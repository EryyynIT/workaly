package main

import (
	"context"
	"database/sql"
	"errors"
	"math/big"
	"time"
	"unicode/utf8"

	taskpb "github.com/EryyynIT/workaly/task-service/pb/taskpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// The deal pipeline of S3.2 (docs/s3-hiring.md, decisions 2-3). Every
// transition is a CAS on the expected deal status with the acting side
// verified against the deal row: a stranger gets the uniform 404, a lost
// race or a repeated command gets 409 with the current and required status.
// The multi-row steps (selection, declined handshake, acceptance,
// cancellation) are single transactions, so the order and its deal never
// diverge.

const (
	maxDealReasonLen     = 1000
	maxDealCriteriaLen   = 4000
	maxSubmissionTextLen = 10000
	// proposalWithdrawnReason marks the proposals auto-withdrawn when the
	// customer selected another executor; a declined handshake restores
	// exactly the proposals carrying this mark.
	proposalWithdrawnReason = "executor_selected"
)

// validateTerms mirrors the named CHECK constraints of the deals table.
func validateTerms(amount, currency, deadline string, revisions int32, criteria string) error {
	if !moneyPattern.MatchString(amount) {
		return invalid("amount must be a decimal amount like 150000 or 150000.50")
	}
	parsed, _ := new(big.Rat).SetString(amount)
	if parsed.Sign() <= 0 {
		return invalid("amount must be greater than zero")
	}
	if !currencyPatt.MatchString(currency) {
		return invalid("currency must be three uppercase letters, for example RUB")
	}
	if !validDate(deadline) {
		return invalid("deadline must be an ISO date like 2026-12-31")
	}
	if revisions < 0 {
		return invalid("revisions_included must not be negative")
	}
	if runes := utf8.RuneCountInString(criteria); runes > maxDealCriteriaLen {
		return invalid("acceptance_criteria must contain at most 4000 characters")
	}
	return nil
}

func validateDealReason(reason string, required bool) error {
	if required && utf8.RuneCountInString(reason) == 0 {
		return invalid("reason is required")
	}
	if utf8.RuneCountInString(reason) > maxDealReasonLen {
		return invalid("reason must contain at most 1000 characters")
	}
	return nil
}

func validateSubmission(result string) error {
	if runes := utf8.RuneCountInString(result); runes < 1 || runes > maxSubmissionTextLen {
		return invalid("result must contain 1..10000 characters")
	}
	return nil
}

func validateIdempotencyKey(key string) error {
	if len(key) < 1 || len(key) > 128 {
		return invalid("idempotency_key must contain 1..128 bytes")
	}
	return nil
}

const dealColumns = `id, order_id, customer_user_id, executor_user_id,
	proposal_id, status, amount::text, currency, to_char(deadline, 'YYYY-MM-DD'),
	revisions_included, acceptance_criteria, reason, version, created_at,
	started_at, submitted_at, decided_at`

// dealRow carries the proposal reference alongside the protobuf fields; the
// selection restore needs it, the wire format does not expose it.
type dealRow struct {
	deal       taskpb.Deal
	proposalID string
}

func scanDeal(row rowScanner) (*dealRow, error) {
	d := &dealRow{}
	d.deal.Terms = &taskpb.DealTerms{}
	var deadline, criteria, reason sql.NullString
	var createdAt time.Time
	var startedAt, submittedAt, decidedAt sql.NullTime
	err := row.Scan(&d.deal.Id, &d.deal.OrderId, &d.deal.CustomerUserId, &d.deal.ExecutorUserId,
		&d.proposalID, &d.deal.Status, &d.deal.Terms.Amount, &d.deal.Terms.Currency, &deadline,
		&d.deal.Terms.RevisionsIncluded, &criteria, &reason, &d.deal.Version,
		&createdAt, &startedAt, &submittedAt, &decidedAt)
	if err != nil {
		return nil, err
	}
	d.deal.Terms.Deadline = deadline.String
	d.deal.Terms.AcceptanceCriteria = criteria.String
	d.deal.Reason = reason.String
	d.deal.CreatedAt = timestamppb.New(createdAt)
	if startedAt.Valid {
		d.deal.StartedAt = timestamppb.New(startedAt.Time)
	}
	if submittedAt.Valid {
		d.deal.SubmittedAt = timestamppb.New(submittedAt.Time)
	}
	if decidedAt.Valid {
		d.deal.DecidedAt = timestamppb.New(decidedAt.Time)
	}
	return d, nil
}

// loadSubmissions fills the immutable delivery history of the deal, oldest
// first; submissions live in a separate table and never change after insert,
// except revision_reason set once when the author sends the delivery back.
type submissionQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func loadSubmissions(ctx context.Context, db submissionQuerier, deal *taskpb.Deal) error {
	rows, err := db.QueryContext(ctx, `SELECT iteration, result, revision_reason, created_at
		FROM deal_submissions WHERE deal_id = $1 ORDER BY iteration`, deal.Id)
	if err != nil {
		return mapDBError(err)
	}
	defer rows.Close()
	for rows.Next() {
		var s taskpb.DealSubmission
		var revisionReason sql.NullString
		var createdAt time.Time
		if err := rows.Scan(&s.Iteration, &s.Result, &revisionReason, &createdAt); err != nil {
			return mapDBError(err)
		}
		s.RevisionReason = revisionReason.String
		s.SubmittedAt = timestamppb.New(createdAt)
		deal.Submissions = append(deal.Submissions, &s)
	}
	if err := rows.Err(); err != nil {
		return mapDBError(err)
	}
	return nil
}

// requireDealIDs validates the path identifiers shared by every deal call.
func requireDealIDs(orderID string) error {
	if !validUUID(orderID) {
		return invalid("order_id must be a UUID")
	}
	return nil
}

// execExactlyOne keeps a successful deal transition and its order mirror in
// the same transaction; a missing or unexpected order state rolls both back.
func execExactlyOne(ctx context.Context, tx *sql.Tx, query, description string, args ...any) error {
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return mapDBError(err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return mapDBError(err)
	}
	if rows != 1 {
		return precondition("order state changed unexpectedly while %s", description)
	}
	return nil
}

func (s *taskService) SelectProposal(ctx context.Context, req *taskpb.SelectProposalRequest) (*taskpb.Deal, error) {
	if err := requireActor(req.ActorUserId); err != nil {
		return nil, err
	}
	if !validUUID(req.OrderId) || !validUUID(req.ProposalId) {
		return nil, invalid("order_id and proposal_id must be UUIDs")
	}
	if err := validateTerms(req.Amount, req.Currency, req.Deadline, req.RevisionsIncluded, req.AcceptanceCriteria); err != nil {
		return nil, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = tx.Rollback() }()

	var author, orderStatus string
	err = tx.QueryRowContext(ctx, `SELECT author_user_id, status FROM orders WHERE id = $1 FOR UPDATE`, req.OrderId).
		Scan(&author, &orderStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound()
	}
	if err != nil {
		return nil, mapDBError(err)
	}
	if author != req.ActorUserId {
		return nil, notFound()
	}
	if orderStatus != "published" {
		return nil, precondition("order is %s; proposals can be selected on a published order only", orderStatus)
	}

	var executor string
	err = tx.QueryRowContext(ctx, `SELECT executor_user_id FROM order_proposals
		WHERE id = $1 AND order_id = $2 AND status = 'active' FOR UPDATE`, req.ProposalId, req.OrderId).
		Scan(&executor)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound()
	}
	if err != nil {
		return nil, mapDBError(err)
	}

	// The status is computed in PostgreSQL: the proposal defines amount,
	// currency and deadline; it does not offer a revision budget or acceptance
	// criteria, so non-default values for either also require executor consent.
	// The unique deals_one_per_order index turns a concurrent second
	// selection into a unique violation reported as AlreadyExists.
	row := tx.QueryRowContext(ctx, `INSERT INTO deals
		(order_id, customer_user_id, executor_user_id, proposal_id, status,
		 amount, currency, deadline, revisions_included, acceptance_criteria, started_at)
		SELECT $1, $2, $3, $4,
			CASE WHEN $5::numeric = p.proposed_amount AND $6::char(3) = p.currency
					AND NULLIF($7, '')::date IS NOT DISTINCT FROM p.proposed_deadline
					AND $8::integer = 0 AND NULLIF($9, '') IS NULL
				THEN 'in_progress' ELSE 'terms_pending' END,
			$5::numeric, $6::char(3), NULLIF($7, '')::date, $8, NULLIF($9, ''),
			CASE WHEN $5::numeric = p.proposed_amount AND $6::char(3) = p.currency
					AND NULLIF($7, '')::date IS NOT DISTINCT FROM p.proposed_deadline
					AND $8::integer = 0 AND NULLIF($9, '') IS NULL
				THEN CURRENT_TIMESTAMP END
		FROM order_proposals p WHERE p.id = $4
		RETURNING `+dealColumns,
		req.OrderId, req.ActorUserId, executor, req.ProposalId,
		req.Amount, req.Currency, req.Deadline, req.RevisionsIncluded, req.AcceptanceCriteria)
	selected, err := scanDeal(row)
	if err != nil {
		if isDuplicateKey(err, "deals_one_per_order") {
			return nil, status.Error(codes.AlreadyExists, "this order already has a deal")
		}
		return nil, mapDBError(err)
	}

	// The order moves with its deal: in_progress at once when the terms
	// match, staying published while the executor decides.
	if _, err := tx.ExecContext(ctx, `UPDATE orders SET status = 'in_progress'
		WHERE id = $1 AND status = 'published' AND
			EXISTS (SELECT 1 FROM deals WHERE id = $2 AND status = 'in_progress')`,
		req.OrderId, selected.deal.Id); err != nil {
		return nil, mapDBError(err)
	}
	// The other active proposals leave the funnel until the selection is
	// settled; a declined handshake restores exactly these rows.
	if _, err := tx.ExecContext(ctx, `UPDATE order_proposals
		SET status = 'withdrawn', withdrawn_at = CURRENT_TIMESTAMP, withdrawn_reason = $3
		WHERE order_id = $1 AND status = 'active' AND id <> $2`,
		req.OrderId, req.ProposalId, proposalWithdrawnReason); err != nil {
		return nil, mapDBError(err)
	}
	if err := tx.Commit(); err != nil {
		return nil, mapDBError(err)
	}
	return &selected.deal, nil
}

// lockDealForParty reads the deal with its row lock for a mutation. Only a
// party may act: a stranger gets the same 404 as a missing deal, so the
// existence of a deal is never disclosed.
func (s *taskService) lockDealForParty(ctx context.Context, tx *sql.Tx, orderID, actor, side string) (string, string, error) {
	var dealID, status_ string
	partyColumn := "customer_user_id"
	if side == "executor" {
		partyColumn = "executor_user_id"
	}
	err := tx.QueryRowContext(ctx, `SELECT id, status FROM deals
		WHERE order_id = $1 AND `+partyColumn+` = $2 FOR UPDATE`, orderID, actor).
		Scan(&dealID, &status_)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", notFound()
	}
	if err != nil {
		return "", "", mapDBError(err)
	}
	return dealID, status_, nil
}

// transitionDeal is the shared CAS body of the single-row transitions.
// expected lists the legal source statuses; the UPDATE bumps the version so
// a lost race or a repeated command is always observable.
func (s *taskService) transitionDeal(ctx context.Context, tx *sql.Tx, dealID, to, extraSet string, args []any, expected ...string) (*dealRow, error) {
	query := `UPDATE deals SET status = $2, version = version + 1`
	if extraSet != "" {
		query += `, ` + extraSet
	}
	query += ` WHERE id = $1 AND status = ANY($3) RETURNING ` + dealColumns
	params := append([]any{dealID, to, expected}, args...)
	row := tx.QueryRowContext(ctx, query, params...)
	deal, err := scanDeal(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, s.classifyDeal(ctx, tx, dealID, to, expected)
	}
	if err != nil {
		return nil, mapDBError(err)
	}
	return deal, nil
}

// classifyDeal runs only after a guarded UPDATE matched nothing: the caller
// holds the row lock and is a party, so the current status is safe to name.
func (s *taskService) classifyDeal(ctx context.Context, tx *sql.Tx, dealID, to string, expected []string) error {
	var current string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM deals WHERE id = $1`, dealID).Scan(&current); err != nil {
		return mapDBError(err)
	}
	if current == to {
		return precondition("deal is already %s", to)
	}
	return precondition("deal is %s; this transition requires %s", current, expected[0])
}

func (s *taskService) ConfirmDealTerms(ctx context.Context, req *taskpb.ConfirmDealTermsRequest) (*taskpb.Deal, error) {
	if err := requireActor(req.ActorUserId); err != nil {
		return nil, err
	}
	if err := requireDealIDs(req.OrderId); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = tx.Rollback() }()
	var orderStatus string
	err = tx.QueryRowContext(ctx, `SELECT status FROM orders WHERE id = $1 FOR UPDATE`, req.OrderId).
		Scan(&orderStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound()
	}
	if err != nil {
		return nil, mapDBError(err)
	}
	dealID, current, err := s.lockDealForParty(ctx, tx, req.OrderId, req.ActorUserId, "executor")
	if err != nil {
		return nil, err
	}
	if current != "terms_pending" || orderStatus != "published" {
		return nil, s.classifyDeal(ctx, tx, dealID, "in_progress", []string{"terms_pending"})
	}
	selected, err := s.transitionDeal(ctx, tx, dealID, "in_progress",
		`started_at = CURRENT_TIMESTAMP`, nil, "terms_pending")
	if err != nil {
		return nil, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE orders SET status = 'in_progress'
		WHERE id = $1 AND status = 'published'`, req.OrderId)
	if err != nil {
		return nil, mapDBError(err)
	}
	if rows, err := result.RowsAffected(); err != nil {
		return nil, mapDBError(err)
	} else if rows != 1 {
		return nil, precondition("order changed concurrently; reload and retry")
	}
	if err := tx.Commit(); err != nil {
		return nil, mapDBError(err)
	}
	return &selected.deal, nil
}

// DeclineDealTerms cancels the selection atomically: the deal and its terms
// v1 disappear, the order returns to published, the chosen proposal becomes
// active again and the proposals auto-withdrawn by the selection are
// restored. The order never keeps a dead deal.
func (s *taskService) DeclineDealTerms(ctx context.Context, req *taskpb.DeclineDealTermsRequest) (*taskpb.Deal, error) {
	if err := requireActor(req.ActorUserId); err != nil {
		return nil, err
	}
	if err := requireDealIDs(req.OrderId); err != nil {
		return nil, err
	}
	if err := validateDealReason(req.Reason, false); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = tx.Rollback() }()
	// Use the same lock order as CloseOrder, SelectProposal and confirm:
	// order first, then deal. This prevents a concurrent close from deleting
	// the pending deal between decline and funnel restoration.
	var orderStatus string
	err = tx.QueryRowContext(ctx, `SELECT status FROM orders WHERE id = $1 FOR UPDATE`, req.OrderId).Scan(&orderStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound()
	}
	if err != nil {
		return nil, mapDBError(err)
	}
	dealID, current, err := s.lockDealForParty(ctx, tx, req.OrderId, req.ActorUserId, "executor")
	if err != nil {
		return nil, err
	}
	if current != "terms_pending" || orderStatus != "published" {
		return nil, s.classifyDeal(ctx, tx, dealID, "declined", []string{"terms_pending"})
	}
	deal, err := scanDeal(tx.QueryRowContext(ctx, `SELECT `+dealColumns+` FROM deals WHERE id = $1`, dealID))
	if err != nil {
		return nil, mapDBError(err)
	}
	deal.deal.Status = "declined"
	deal.deal.Reason = req.Reason
	// Submissions cannot exist in terms_pending; the delete takes the deal
	// row only, cascades nothing.
	result, err := tx.ExecContext(ctx, `UPDATE orders SET status = 'published'
		WHERE id = $1 AND status = 'published'`, req.OrderId)
	if err != nil {
		return nil, mapDBError(err)
	}
	if rows, err := result.RowsAffected(); err != nil {
		return nil, mapDBError(err)
	} else if rows != 1 {
		return nil, precondition("order is no longer published; the selection cannot be declined")
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM deals WHERE id = $1`, dealID); err != nil {
		return nil, mapDBError(err)
	}
	// Restore the funnel exactly as the selection found it: the chosen
	// proposal and the auto-withdrawn ones become active again.
	if _, err := tx.ExecContext(ctx, `UPDATE order_proposals
		SET status = 'active', withdrawn_at = NULL, withdrawn_reason = NULL
		WHERE order_id = $1 AND status = 'withdrawn'
			AND (id = $2 OR withdrawn_reason = $3)`,
		req.OrderId, deal.proposalID, proposalWithdrawnReason); err != nil {
		return nil, mapDBError(err)
	}
	if err := tx.Commit(); err != nil {
		return nil, mapDBError(err)
	}
	return &deal.deal, nil
}

func (s *taskService) SubmitResult(ctx context.Context, req *taskpb.SubmitResultRequest) (*taskpb.Deal, error) {
	if err := requireActor(req.ActorUserId); err != nil {
		return nil, err
	}
	if err := requireDealIDs(req.OrderId); err != nil {
		return nil, err
	}
	if err := validateSubmission(req.Result); err != nil {
		return nil, err
	}
	if err := validateIdempotencyKey(req.IdempotencyKey); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = tx.Rollback() }()
	dealID, current, err := s.lockDealForParty(ctx, tx, req.OrderId, req.ActorUserId, "executor")
	if err != nil {
		return nil, err
	}
	var existingIteration int
	var existingResult string
	existingErr := tx.QueryRowContext(ctx, `SELECT iteration, result FROM deal_submissions
		WHERE deal_id = $1 AND idempotency_key = $2`, dealID, req.IdempotencyKey).
		Scan(&existingIteration, &existingResult)
	if existingErr != nil && !errors.Is(existingErr, sql.ErrNoRows) {
		return nil, mapDBError(existingErr)
	}
	if existingErr == nil {
		if existingResult != req.Result {
			return nil, precondition("idempotency_key was already used with different submission content")
		}
		recovered, err := scanDeal(tx.QueryRowContext(ctx, `SELECT `+dealColumns+` FROM deals WHERE id = $1`, dealID))
		if err != nil {
			return nil, mapDBError(err)
		}
		if err := loadSubmissions(ctx, tx, &recovered.deal); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, mapDBError(err)
		}
		return &recovered.deal, nil
	}
	if current == "submitted" {
		return nil, precondition("deal is already submitted; use a new idempotency_key for a new delivery")
	}
	if current != "in_progress" && current != "revision_requested" {
		return nil, s.classifyDeal(ctx, tx, dealID, "submitted", []string{"in_progress"})
	}
	var orderStatus string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM orders WHERE id = $1`, req.OrderId).Scan(&orderStatus); err != nil {
		return nil, mapDBError(err)
	}
	deal, err := s.transitionDeal(ctx, tx, dealID, "submitted",
		`submitted_at = CURRENT_TIMESTAMP`, nil, "in_progress", "revision_requested")
	if err != nil {
		return nil, err
	}
	var iteration int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(iteration), 0) + 1 FROM deal_submissions
		WHERE deal_id = $1`, dealID).Scan(&iteration); err != nil {
		return nil, mapDBError(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO deal_submissions (deal_id, iteration, result, idempotency_key)
		VALUES ($1, $2, $3, $4)`, dealID, iteration, req.Result, req.IdempotencyKey); err != nil {
		return nil, mapDBError(err)
	}
	// The order mirrors the deal: in_progress -> submitted on the first
	// delivery; a re-delivery keeps the order submitted.
	if orderStatus == "in_progress" {
		if err := execExactlyOne(ctx, tx, `UPDATE orders SET status = 'submitted' WHERE id = $1 AND status = 'in_progress'`, "submitting the result", req.OrderId); err != nil {
			return nil, err
		}
	} else if orderStatus != "submitted" {
		return nil, precondition("order is %s; a submitted deal requires an in_progress or submitted order", orderStatus)
	}
	if err := loadSubmissions(ctx, tx, &deal.deal); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, mapDBError(err)
	}
	return &deal.deal, nil
}

func (s *taskService) AcceptResult(ctx context.Context, req *taskpb.AcceptResultRequest) (*taskpb.Deal, error) {
	if err := requireActor(req.ActorUserId); err != nil {
		return nil, err
	}
	if err := requireDealIDs(req.OrderId); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = tx.Rollback() }()
	dealID, _, err := s.lockDealForParty(ctx, tx, req.OrderId, req.ActorUserId, "customer")
	if err != nil {
		return nil, err
	}
	deal, err := s.transitionDeal(ctx, tx, dealID, "completed",
		`decided_at = CURRENT_TIMESTAMP`, nil, "submitted")
	if err != nil {
		return nil, err
	}
	// Acceptance finishes the order in the same transaction.
	if err := execExactlyOne(ctx, tx, `UPDATE orders SET status = 'completed'
		WHERE id = $1 AND status = 'submitted'`, "accepting the result", req.OrderId); err != nil {
		return nil, err
	}
	if err := loadSubmissions(ctx, tx, &deal.deal); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, mapDBError(err)
	}
	return &deal.deal, nil
}

// RequestRevision sends the last delivery back, bounded by the revisions
// included in the confirmed terms v1: a request beyond the budget is
// FailedPrecondition, acceptance and cancellation stay possible.
func (s *taskService) RequestRevision(ctx context.Context, req *taskpb.RequestRevisionRequest) (*taskpb.Deal, error) {
	if err := requireActor(req.ActorUserId); err != nil {
		return nil, err
	}
	if err := requireDealIDs(req.OrderId); err != nil {
		return nil, err
	}
	if err := validateDealReason(req.Reason, true); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = tx.Rollback() }()
	dealID, current, err := s.lockDealForParty(ctx, tx, req.OrderId, req.ActorUserId, "customer")
	if err != nil {
		return nil, err
	}
	if current != "submitted" {
		return nil, s.classifyDeal(ctx, tx, dealID, "revision_requested", []string{"submitted"})
	}
	var revisionsUsed, revisionsIncluded int
	err = tx.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM deal_submissions WHERE deal_id = $1 AND revision_reason IS NOT NULL),
		(SELECT revisions_included FROM deals WHERE id = $1)`, dealID).
		Scan(&revisionsUsed, &revisionsIncluded)
	if err != nil {
		return nil, mapDBError(err)
	}
	if revisionsUsed >= revisionsIncluded {
		return nil, precondition("the revision budget of the confirmed terms is exhausted (%d of %d); accept or cancel the deal",
			revisionsUsed, revisionsIncluded)
	}
	deal, err := s.transitionDeal(ctx, tx, dealID, "revision_requested",
		"", nil, "submitted")
	if err != nil {
		return nil, err
	}
	// The reason is stored on the delivery it sent back.
	if _, err := tx.ExecContext(ctx, `UPDATE deal_submissions SET revision_reason = $2
		WHERE deal_id = $1 AND iteration = (SELECT MAX(iteration) FROM deal_submissions WHERE deal_id = $1)`,
		dealID, req.Reason); err != nil {
		return nil, mapDBError(err)
	}
	if err := execExactlyOne(ctx, tx, `UPDATE orders SET status = 'in_progress'
		WHERE id = $1 AND status = 'submitted'`, "requesting a revision", req.OrderId); err != nil {
		return nil, err
	}
	if err := loadSubmissions(ctx, tx, &deal.deal); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, mapDBError(err)
	}
	return &deal.deal, nil
}

// CancelDeal is the only way out of a started order: the customer cancels
// before the first delivery with a mandatory reason, the order is cancelled
// in the same transaction. Terminal deals keep their history untouched.
func (s *taskService) CancelDeal(ctx context.Context, req *taskpb.CancelDealRequest) (*taskpb.Deal, error) {
	if err := requireActor(req.ActorUserId); err != nil {
		return nil, err
	}
	if err := requireDealIDs(req.OrderId); err != nil {
		return nil, err
	}
	if err := validateDealReason(req.Reason, true); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer func() { _ = tx.Rollback() }()
	dealID, current, err := s.lockDealForParty(ctx, tx, req.OrderId, req.ActorUserId, "customer")
	if err != nil {
		return nil, err
	}
	if current != "in_progress" && current != "revision_requested" {
		return nil, s.classifyDeal(ctx, tx, dealID, "cancelled", []string{"in_progress"})
	}
	deal, err := s.transitionDeal(ctx, tx, dealID, "cancelled",
		`reason = $4, decided_at = CURRENT_TIMESTAMP`, []any{req.Reason},
		"in_progress", "revision_requested")
	if err != nil {
		return nil, err
	}
	if err := execExactlyOne(ctx, tx, `UPDATE orders SET status = 'cancelled'
		WHERE id = $1 AND status = 'in_progress'`, "cancelling the deal", req.OrderId); err != nil {
		return nil, err
	}
	if err := loadSubmissions(ctx, tx, &deal.deal); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, mapDBError(err)
	}
	return &deal.deal, nil
}

func (s *taskService) GetDeal(ctx context.Context, req *taskpb.GetDealRequest) (*taskpb.Deal, error) {
	if err := requireActor(req.ActorUserId); err != nil {
		return nil, err
	}
	if err := requireDealIDs(req.OrderId); err != nil {
		return nil, err
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+dealColumns+` FROM deals
		WHERE order_id = $1 AND (customer_user_id = $2 OR executor_user_id = $2)`,
		req.OrderId, req.ActorUserId)
	deal, err := scanDeal(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound()
	}
	if err != nil {
		return nil, mapDBError(err)
	}
	if err := loadSubmissions(ctx, s.db, &deal.deal); err != nil {
		return nil, err
	}
	return &deal.deal, nil
}
