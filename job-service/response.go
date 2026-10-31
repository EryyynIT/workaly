package main

import (
	"context"
	"database/sql"
	"errors"
	"time"
	"unicode/utf8"

	jobpb "github.com/EryyynIT/workaly/job-service/pb/jobpb"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// validateResponse mirrors the named CHECK constraints of vacancy_responses:
// precise 400s here, the database stays the last backstop.
func validateResponse(coverLetter, contact string) error {
	if runes := utf8.RuneCountInString(coverLetter); runes < 1 || runes > 4000 {
		return invalid("cover_letter must contain 1..4000 characters")
	}
	if runes := utf8.RuneCountInString(contact); contact != "" && (runes < 3 || runes > 200) {
		return invalid("contact must be empty or contain 3..200 characters")
	}
	return nil
}

const maxReasonLen = 1000

// validateReason mirrors vacancy_responses_reason_len; empty is allowed on
// every decision that carries a reason.
func validateReason(reason string) error {
	if runes := utf8.RuneCountInString(reason); runes > maxReasonLen {
		return invalid("reason must contain at most 1000 characters")
	}
	return nil
}

const responseColumns = `id, vacancy_id, candidate_user_id, cover_letter,
	contact, status, created_at, withdrawn_at, decided_at, reason`

func scanResponse(row rowScanner) (*jobpb.VacancyResponse, error) {
	var r jobpb.VacancyResponse
	var contact, reason sql.NullString
	var createdAt time.Time
	var withdrawnAt, decidedAt sql.NullTime
	err := row.Scan(&r.Id, &r.VacancyId, &r.CandidateUserId, &r.CoverLetter,
		&contact, &r.Status, &createdAt, &withdrawnAt, &decidedAt, &reason)
	if err != nil {
		return nil, err
	}
	r.Contact = contact.String
	r.Reason = reason.String
	r.CreatedAt = timestamppb.New(createdAt)
	if withdrawnAt.Valid {
		r.WithdrawnAt = timestamppb.New(withdrawnAt.Time)
	}
	if decidedAt.Valid {
		r.DecidedAt = timestamppb.New(decidedAt.Time)
	}
	return &r, nil
}

// requireOwnVacancy verifies that the vacancy belongs to the actor. A
// stranger and a missing vacancy get the same 404, so the existence of a
// draft is never disclosed through the responses API.
func (s *jobService) requireOwnVacancy(ctx context.Context, vacancyID, actor string) error {
	var author string
	err := s.db.QueryRowContext(ctx, `SELECT author_user_id FROM vacancies WHERE id = $1`, vacancyID).
		Scan(&author)
	if errors.Is(err, sql.ErrNoRows) {
		return notFound()
	}
	if err != nil {
		return mapDBError(err)
	}
	if author != actor {
		return notFound()
	}
	return nil
}

func (s *jobService) CreateVacancyResponse(ctx context.Context, req *jobpb.CreateVacancyResponseRequest) (*jobpb.VacancyResponse, error) {
	if err := requireActor(req.ActorUserId); err != nil {
		return nil, err
	}
	if !validUUID(req.VacancyId) {
		return nil, invalid("vacancy_id must be a UUID")
	}
	if err := validateResponse(req.CoverLetter, req.Contact); err != nil {
		return nil, err
	}
	var author string
	err := s.db.QueryRowContext(ctx, `SELECT author_user_id FROM vacancies WHERE id = $1`, req.VacancyId).
		Scan(&author)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound()
	}
	if err != nil {
		return nil, mapDBError(err)
	}
	if author == req.ActorUserId {
		return nil, invalid("the vacancy author cannot respond to their own vacancy")
	}
	// The status gate is part of the INSERT itself: a vacancy closing between
	// a read and the write cannot accept a response. A draft, closed or
	// vanished vacancy reports the same 404 — existence stays undisclosed.
	row := s.db.QueryRowContext(ctx, `INSERT INTO vacancy_responses (vacancy_id, candidate_user_id, cover_letter, contact)
		SELECT id, $2, $3, NULLIF($4, '')::text FROM vacancies
		WHERE id = $1 AND status = 'published'
		RETURNING `+responseColumns,
		req.VacancyId, req.ActorUserId, req.CoverLetter, req.Contact)
	r, err := scanResponse(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound()
	}
	if err != nil {
		if isDuplicateKey(err, "vacancy_responses_one_active") {
			return nil, status.Error(codes.AlreadyExists, "you already have an active response to this vacancy")
		}
		return nil, mapDBError(err)
	}
	return r, nil
}

// isDuplicateKey reports a unique-violation of the named constraint.
func isDuplicateKey(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}

func (s *jobService) ListVacancyResponses(ctx context.Context, req *jobpb.ListVacancyResponsesRequest) (*jobpb.ListVacancyResponsesResponse, error) {
	if err := requireActor(req.ActorUserId); err != nil {
		return nil, err
	}
	if !validUUID(req.VacancyId) {
		return nil, invalid("vacancy_id must be a UUID")
	}
	pageSize := int(req.PageSize)
	if pageSize == 0 {
		pageSize = 20
	}
	if pageSize < 0 || pageSize > 50 {
		return nil, invalid("page_size must be between 1 and 50")
	}
	if err := s.requireOwnVacancy(ctx, req.VacancyId, req.ActorUserId); err != nil {
		return nil, err
	}

	const base = `SELECT ` + responseColumns + ` FROM vacancy_responses WHERE vacancy_id = $1`
	var cursorAt time.Time
	var cursorID string
	if req.PageToken != "" {
		at, id, err := decodeCursor(req.PageToken)
		if err != nil {
			return nil, invalid("page_token is not a valid cursor")
		}
		cursorAt, cursorID = at, id
	}
	var (
		rows *sql.Rows
		err  error
	)
	if cursorAt.IsZero() {
		rows, err = s.db.QueryContext(ctx, base+` ORDER BY created_at DESC, id DESC LIMIT $2`,
			req.VacancyId, pageSize+1)
	} else {
		rows, err = s.db.QueryContext(ctx, base+` AND (created_at, id) < ($2, $3::uuid)
			ORDER BY created_at DESC, id DESC LIMIT $4`,
			req.VacancyId, cursorAt, cursorID, pageSize+1)
	}
	if err != nil {
		return nil, mapDBError(err)
	}
	defer rows.Close()

	response := &jobpb.ListVacancyResponsesResponse{}
	for i := 0; rows.Next(); i++ {
		if i == pageSize {
			// One extra row proves a next page exists; the token sits on the
			// last row returned to the client.
			last := response.Responses[pageSize-1]
			response.NextPageToken = encodeCursor(last.CreatedAt.AsTime(), last.Id)
			break
		}
		r, err := scanResponse(rows)
		if err != nil {
			return nil, mapDBError(err)
		}
		response.Responses = append(response.Responses, r)
	}
	if err := rows.Err(); err != nil {
		return nil, mapDBError(err)
	}
	return response, nil
}

func (s *jobService) WithdrawVacancyResponse(ctx context.Context, req *jobpb.WithdrawVacancyResponseRequest) (*jobpb.VacancyResponse, error) {
	if err := requireActor(req.ActorUserId); err != nil {
		return nil, err
	}
	if !validUUID(req.VacancyId) || !validUUID(req.ResponseId) {
		return nil, invalid("vacancy_id and response_id must be UUIDs")
	}
	// Withdrawal is possible from any non-final status: terminal records
	// (rejected, offer_declined, withdrawn, hired) stay untouched as history
	// (docs/s3-hiring.md, decision 1).
	row := s.db.QueryRowContext(ctx, `UPDATE vacancy_responses
		SET status = 'withdrawn', withdrawn_at = CURRENT_TIMESTAMP
		WHERE id = $1 AND vacancy_id = $2 AND candidate_user_id = $3
			AND status IN ('active', 'offer_sent', 'offer_accepted')
		RETURNING `+responseColumns, req.ResponseId, req.VacancyId, req.ActorUserId)
	r, err := scanResponse(row)
	if errors.Is(err, sql.ErrNoRows) {
		// Not owned, not found, or in a state without a withdrawal path
		// (already withdrawn, hired). The candidate sees their own record, so
		// a state conflict is reported as such; anything else stays hidden.
		return nil, s.classifyOwnResponse(ctx, req.ResponseId, req.VacancyId, req.ActorUserId)
	}
	if err != nil {
		return nil, mapDBError(err)
	}
	return r, nil
}

// classifyOwnResponse distinguishes "no such response of mine" (404) from
// "my response is in a state that forbids the transition" (409). It runs
// only after a guarded UPDATE matched nothing, so a concurrent change is
// still reported as a state conflict.
func (s *jobService) classifyOwnResponse(ctx context.Context, responseID, vacancyID, actor string) error {
	var candidate, status_ string
	err := s.db.QueryRowContext(ctx, `SELECT candidate_user_id, status FROM vacancy_responses
		WHERE id = $1 AND vacancy_id = $2`, responseID, vacancyID).Scan(&candidate, &status_)
	if errors.Is(err, sql.ErrNoRows) {
		return notFound()
	}
	if err != nil {
		return mapDBError(err)
	}
	if candidate != actor {
		return notFound()
	}
	if status_ == "withdrawn" {
		return precondition("response is already withdrawn")
	}
	return precondition("response is %s and cannot be withdrawn", status_)
}

func (s *jobService) GetMyVacancyResponse(ctx context.Context, req *jobpb.GetMyVacancyResponseRequest) (*jobpb.VacancyResponse, error) {
	if err := requireActor(req.ActorUserId); err != nil {
		return nil, err
	}
	if !validUUID(req.VacancyId) {
		return nil, invalid("vacancy_id must be a UUID")
	}
	// The candidate's current pipeline state: every status except withdrawn.
	row := s.db.QueryRowContext(ctx, `SELECT `+responseColumns+` FROM vacancy_responses
		WHERE vacancy_id = $1 AND candidate_user_id = $2 AND status != 'withdrawn'
		ORDER BY created_at DESC LIMIT 1`,
		req.VacancyId, req.ActorUserId)
	r, err := scanResponse(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound()
	}
	if err != nil {
		return nil, mapDBError(err)
	}
	return r, nil
}

// hiringTransition is the shared CAS body of the offer pipeline. Side is
// "author" or "candidate": the actor must be the vacancy author or the
// responding candidate respectively; a stranger gets the same 404 as a
// missing response. The UPDATE is guarded by the expected from-status, so a
// lost race is a 409, never a silent overwrite. Transitions out of "active"
// additionally require the vacancy to be published: a closed vacancy stops
// its pipeline, so an outstanding offer can never be created on it. reason
// and decided_at are set only when the transition carries them.
func (s *jobService) hiringTransition(ctx context.Context, responseID, vacancyID, actor, side, from, to, reason string) (*jobpb.VacancyResponse, error) {
	var sideClause, sideArg string
	if side == "author" {
		sideClause = `vacancy_id IN (SELECT id FROM vacancies WHERE id = $2 AND author_user_id = $3)`
		sideArg = actor
	} else {
		sideClause = `candidate_user_id = $3`
		sideArg = actor
	}
	if from == "active" {
		sideClause += ` AND vacancy_id IN (SELECT id FROM vacancies WHERE id = $2 AND status = 'published')`
	}
	row := s.db.QueryRowContext(ctx, `UPDATE vacancy_responses
		SET status = $4,
			decided_at = CASE WHEN $4 IN ('rejected', 'offer_declined', 'hired') THEN CURRENT_TIMESTAMP ELSE decided_at END,
			reason = CASE WHEN $5 <> '' THEN $5 ELSE reason END
		WHERE id = $1 AND vacancy_id = $2 AND status = $6 AND `+sideClause+`
		RETURNING `+responseColumns,
		responseID, vacancyID, sideArg, to, reason, from)
	r, err := scanResponse(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, s.classifyTransition(ctx, responseID, vacancyID, actor, side, from, to, from == "active")
	}
	if err != nil {
		if isDuplicateKey(err, "vacancy_responses_one_offer") {
			return nil, status.Error(codes.AlreadyExists, "another response already holds an outstanding offer for this vacancy")
		}
		return nil, mapDBError(err)
	}
	return r, nil
}

// classifyTransition mirrors classifyOwnResponse for both sides and reports
// a precise conflict when the record is in the target state already (a
// retried command) or any other state (a lost race). When the transition
// required a published vacancy and the vacancy is closed, the conflict says
// so instead of exposing the record state.
func (s *jobService) classifyTransition(ctx context.Context, responseID, vacancyID, actor, side, from, to string, requiresPublished bool) error {
	var currentAuthor, currentCandidate, currentStatus, vacancyStatus string
	err := s.db.QueryRowContext(ctx, `SELECT v.author_user_id, r.candidate_user_id, r.status, v.status
		FROM vacancy_responses r JOIN vacancies v ON v.id = r.vacancy_id
		WHERE r.id = $1 AND r.vacancy_id = $2`, responseID, vacancyID).
		Scan(&currentAuthor, &currentCandidate, &currentStatus, &vacancyStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return notFound()
	}
	if err != nil {
		return mapDBError(err)
	}
	if (side == "author" && currentAuthor != actor) || (side == "candidate" && currentCandidate != actor) {
		return notFound()
	}
	if requiresPublished && vacancyStatus != "published" {
		return precondition("the vacancy is %s; the hiring pipeline stopped", vacancyStatus)
	}
	if currentStatus == to {
		return precondition("response is already %s", to)
	}
	return precondition("response is %s; this transition requires %s", currentStatus, from)
}

func requireResponseIDs(vacancyID, responseID string) error {
	if !validUUID(vacancyID) || !validUUID(responseID) {
		return invalid("vacancy_id and response_id must be UUIDs")
	}
	return nil
}

func (s *jobService) RejectVacancyResponse(ctx context.Context, req *jobpb.RejectVacancyResponseRequest) (*jobpb.VacancyResponse, error) {
	if err := requireActor(req.ActorUserId); err != nil {
		return nil, err
	}
	if err := requireResponseIDs(req.VacancyId, req.ResponseId); err != nil {
		return nil, err
	}
	if err := validateReason(req.Reason); err != nil {
		return nil, err
	}
	return s.hiringTransition(ctx, req.ResponseId, req.VacancyId, req.ActorUserId,
		"author", "active", "rejected", req.Reason)
}

func (s *jobService) SendOffer(ctx context.Context, req *jobpb.SendOfferRequest) (*jobpb.VacancyResponse, error) {
	if err := requireActor(req.ActorUserId); err != nil {
		return nil, err
	}
	if err := requireResponseIDs(req.VacancyId, req.ResponseId); err != nil {
		return nil, err
	}
	return s.hiringTransition(ctx, req.ResponseId, req.VacancyId, req.ActorUserId,
		"author", "active", "offer_sent", "")
}

func (s *jobService) WithdrawOffer(ctx context.Context, req *jobpb.WithdrawOfferRequest) (*jobpb.VacancyResponse, error) {
	if err := requireActor(req.ActorUserId); err != nil {
		return nil, err
	}
	if err := requireResponseIDs(req.VacancyId, req.ResponseId); err != nil {
		return nil, err
	}
	return s.hiringTransition(ctx, req.ResponseId, req.VacancyId, req.ActorUserId,
		"author", "offer_sent", "active", "")
}

func (s *jobService) AcceptOffer(ctx context.Context, req *jobpb.AcceptOfferRequest) (*jobpb.VacancyResponse, error) {
	if err := requireActor(req.ActorUserId); err != nil {
		return nil, err
	}
	if err := requireResponseIDs(req.VacancyId, req.ResponseId); err != nil {
		return nil, err
	}
	return s.hiringTransition(ctx, req.ResponseId, req.VacancyId, req.ActorUserId,
		"candidate", "offer_sent", "offer_accepted", "")
}

func (s *jobService) DeclineOffer(ctx context.Context, req *jobpb.DeclineOfferRequest) (*jobpb.VacancyResponse, error) {
	if err := requireActor(req.ActorUserId); err != nil {
		return nil, err
	}
	if err := requireResponseIDs(req.VacancyId, req.ResponseId); err != nil {
		return nil, err
	}
	if err := validateReason(req.Reason); err != nil {
		return nil, err
	}
	return s.hiringTransition(ctx, req.ResponseId, req.VacancyId, req.ActorUserId,
		"candidate", "offer_sent", "offer_declined", req.Reason)
}

func (s *jobService) MarkHired(ctx context.Context, req *jobpb.MarkHiredRequest) (*jobpb.VacancyResponse, error) {
	if err := requireActor(req.ActorUserId); err != nil {
		return nil, err
	}
	if err := requireResponseIDs(req.VacancyId, req.ResponseId); err != nil {
		return nil, err
	}
	return s.hiringTransition(ctx, req.ResponseId, req.VacancyId, req.ActorUserId,
		"author", "offer_accepted", "hired", "")
}
