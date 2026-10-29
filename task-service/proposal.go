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

// validateProposal mirrors the named CHECK constraints of order_proposals:
// precise 400s here, the database stays the last backstop.
func validateProposal(message, amount, currency, proposedDeadline string) error {
	if runes := utf8.RuneCountInString(message); runes < 1 || runes > 4000 {
		return invalid("message must contain 1..4000 characters")
	}
	if !moneyPattern.MatchString(amount) {
		return invalid("proposed_amount must be a decimal amount like 150000 or 150000.50")
	}
	parsed, _ := new(big.Rat).SetString(amount)
	if parsed.Sign() <= 0 {
		return invalid("proposed_amount must be greater than zero")
	}
	if !currencyPatt.MatchString(currency) {
		return invalid("currency must be three uppercase letters, for example RUB")
	}
	if !validDate(proposedDeadline) {
		return invalid("proposed_deadline must be an ISO date like 2026-12-31")
	}
	return nil
}

// See orderColumns: to_char pins the ISO format regardless of DateStyle.
const proposalColumns = `id, order_id, executor_user_id, message,
	proposed_amount::text, currency, to_char(proposed_deadline, 'YYYY-MM-DD'), status,
	created_at, withdrawn_at`

func scanProposal(row rowScanner) (*taskpb.OrderProposal, error) {
	var p taskpb.OrderProposal
	var proposedDeadline sql.NullString
	var createdAt time.Time
	var withdrawnAt sql.NullTime
	err := row.Scan(&p.Id, &p.OrderId, &p.ExecutorUserId, &p.Message,
		&p.ProposedAmount, &p.Currency, &proposedDeadline, &p.Status,
		&createdAt, &withdrawnAt)
	if err != nil {
		return nil, err
	}
	p.ProposedDeadline = proposedDeadline.String
	p.CreatedAt = timestamppb.New(createdAt)
	if withdrawnAt.Valid {
		p.WithdrawnAt = timestamppb.New(withdrawnAt.Time)
	}
	return &p, nil
}

func (s *taskService) CreateOrderProposal(ctx context.Context, req *taskpb.CreateOrderProposalRequest) (*taskpb.OrderProposal, error) {
	if err := requireActor(req.ActorUserId); err != nil {
		return nil, err
	}
	if !validUUID(req.OrderId) {
		return nil, invalid("order_id must be a UUID")
	}
	if err := validateProposal(req.Message, req.ProposedAmount, req.Currency, req.ProposedDeadline); err != nil {
		return nil, err
	}
	var author string
	err := s.db.QueryRowContext(ctx, `SELECT author_user_id FROM orders WHERE id = $1`, req.OrderId).
		Scan(&author)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound()
	}
	if err != nil {
		return nil, mapDBError(err)
	}
	if author == req.ActorUserId {
		return nil, invalid("the order author cannot propose for their own order")
	}
	// The status gate is part of the INSERT itself: an order closing between
	// a read and the write cannot accept a proposal. A draft, closed or
	// vanished order reports the same 404 — existence stays undisclosed.
	row := s.db.QueryRowContext(ctx, `INSERT INTO order_proposals
		(order_id, executor_user_id, message, proposed_amount, currency, proposed_deadline)
		SELECT id, $2, $3, $4::numeric, $5::char(3), NULLIF($6, '')::date FROM orders
		WHERE id = $1 AND status = 'published'
		RETURNING `+proposalColumns,
		req.OrderId, req.ActorUserId, req.Message, req.ProposedAmount, req.Currency, req.ProposedDeadline)
	p, err := scanProposal(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound()
	}
	if err != nil {
		if isDuplicateKey(err, "order_proposals_one_active") {
			return nil, status.Error(codes.AlreadyExists, "you already have an active proposal for this order")
		}
		return nil, mapDBError(err)
	}
	return p, nil
}

func (s *taskService) ListOrderProposals(ctx context.Context, req *taskpb.ListOrderProposalsRequest) (*taskpb.ListOrderProposalsResponse, error) {
	if err := requireActor(req.ActorUserId); err != nil {
		return nil, err
	}
	if !validUUID(req.OrderId) {
		return nil, invalid("order_id must be a UUID")
	}
	pageSize := int(req.PageSize)
	if pageSize == 0 {
		pageSize = 20
	}
	if pageSize < 0 || pageSize > 50 {
		return nil, invalid("page_size must be between 1 and 50")
	}
	// Author-only: a stranger and a missing order get the same 404, so the
	// existence of a draft is never disclosed through the proposals API.
	var author string
	ownerErr := s.db.QueryRowContext(ctx, `SELECT author_user_id FROM orders WHERE id = $1`, req.OrderId).
		Scan(&author)
	if errors.Is(ownerErr, sql.ErrNoRows) {
		return nil, notFound()
	}
	if ownerErr != nil {
		return nil, mapDBError(ownerErr)
	}
	if author != req.ActorUserId {
		return nil, notFound()
	}

	const base = `SELECT ` + proposalColumns + ` FROM order_proposals WHERE order_id = $1`
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
			req.OrderId, pageSize+1)
	} else {
		rows, err = s.db.QueryContext(ctx, base+` AND (created_at, id) < ($2, $3::uuid)
			ORDER BY created_at DESC, id DESC LIMIT $4`,
			req.OrderId, cursorAt, cursorID, pageSize+1)
	}
	if err != nil {
		return nil, mapDBError(err)
	}
	defer rows.Close()

	response := &taskpb.ListOrderProposalsResponse{}
	for i := 0; rows.Next(); i++ {
		if i == pageSize {
			// One extra row proves a next page exists; the token sits on the
			// last row returned to the client.
			last := response.Proposals[pageSize-1]
			response.NextPageToken = encodeCursor(last.CreatedAt.AsTime(), last.Id)
			break
		}
		p, err := scanProposal(rows)
		if err != nil {
			return nil, mapDBError(err)
		}
		response.Proposals = append(response.Proposals, p)
	}
	if err := rows.Err(); err != nil {
		return nil, mapDBError(err)
	}
	return response, nil
}

func (s *taskService) WithdrawOrderProposal(ctx context.Context, req *taskpb.WithdrawOrderProposalRequest) (*taskpb.OrderProposal, error) {
	if err := requireActor(req.ActorUserId); err != nil {
		return nil, err
	}
	if !validUUID(req.OrderId) || !validUUID(req.ProposalId) {
		return nil, invalid("order_id and proposal_id must be UUIDs")
	}
	var executor string
	var status_ string
	err := s.db.QueryRowContext(ctx, `SELECT executor_user_id, status FROM order_proposals
		WHERE id = $1 AND order_id = $2`, req.ProposalId, req.OrderId).Scan(&executor, &status_)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound()
	}
	if err != nil {
		return nil, mapDBError(err)
	}
	if executor != req.ActorUserId {
		return nil, notFound()
	}
	if status_ == "withdrawn" {
		return nil, precondition("proposal is already withdrawn")
	}
	row := s.db.QueryRowContext(ctx, `UPDATE order_proposals
		SET status = 'withdrawn', withdrawn_at = CURRENT_TIMESTAMP
		WHERE id = $1 AND order_id = $2 AND executor_user_id = $3 AND status = 'active'
		RETURNING `+proposalColumns, req.ProposalId, req.OrderId, req.ActorUserId)
	p, err := scanProposal(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, precondition("proposal changed concurrently; reload and retry")
	}
	if err != nil {
		return nil, mapDBError(err)
	}
	return p, nil
}

func (s *taskService) GetMyOrderProposal(ctx context.Context, req *taskpb.GetMyOrderProposalRequest) (*taskpb.OrderProposal, error) {
	if err := requireActor(req.ActorUserId); err != nil {
		return nil, err
	}
	if !validUUID(req.OrderId) {
		return nil, invalid("order_id must be a UUID")
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+proposalColumns+` FROM order_proposals
		WHERE order_id = $1 AND executor_user_id = $2 AND status = 'active'`,
		req.OrderId, req.ActorUserId)
	p, err := scanProposal(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound()
	}
	if err != nil {
		return nil, mapDBError(err)
	}
	return p, nil
}
