package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
	"unicode/utf8"

	taskpb "github.com/EryyynIT/workaly/task-service/pb/taskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type taskService struct {
	taskpb.UnimplementedTaskServiceServer
	db *sql.DB
}

var budgetTypes = map[string]bool{"fixed": true, "hourly": true}

type orderFields struct {
	title, description, budgetType, budgetAmount, currency, deadline string
}

// validate mirrors the named CHECK constraints of the orders table.
func (f orderFields) validate() error {
	if runes := utf8.RuneCountInString(f.title); runes < 1 || runes > 120 {
		return invalid("title must contain 1..120 characters")
	}
	if runes := utf8.RuneCountInString(f.description); runes < 1 || runes > 5000 {
		return invalid("description must contain 1..5000 characters")
	}
	if !budgetTypes[f.budgetType] {
		return invalid("budget_type must be one of: fixed, hourly")
	}
	if !moneyPattern.MatchString(f.budgetAmount) {
		return invalid("budget_amount must be a decimal amount like 150000 or 150000.50")
	}
	amount, _ := new(big.Rat).SetString(f.budgetAmount)
	if amount.Sign() <= 0 {
		return invalid("budget_amount must be greater than zero")
	}
	if !currencyPatt.MatchString(f.currency) {
		return invalid("currency must be three uppercase letters, for example RUB")
	}
	if !validDate(f.deadline) {
		return invalid("deadline must be an ISO date like 2026-12-31")
	}
	return nil
}

// Money and date columns are formatted explicitly: the pool must not depend
// on the session DateStyle for the ISO contract of the edge.
const orderColumns = `id, author_user_id, title, description, budget_type,
	budget_amount::text, currency, to_char(deadline, 'YYYY-MM-DD'), status, version,
	created_at, updated_at, published_at, closed_at`

func scanOrder(row rowScanner) (*taskpb.Order, error) {
	var o taskpb.Order
	var deadline sql.NullString
	var createdAt, updatedAt time.Time
	var publishedAt, closedAt sql.NullTime
	err := row.Scan(&o.Id, &o.AuthorUserId, &o.Title, &o.Description, &o.BudgetType,
		&o.BudgetAmount, &o.Currency, &deadline, &o.Status, &o.Version,
		&createdAt, &updatedAt, &publishedAt, &closedAt)
	if err != nil {
		return nil, err
	}
	o.Deadline = deadline.String
	o.CreatedAt = timestamppb.New(createdAt)
	o.UpdatedAt = timestamppb.New(updatedAt)
	if publishedAt.Valid {
		o.PublishedAt = timestamppb.New(publishedAt.Time)
	}
	if closedAt.Valid {
		o.ClosedAt = timestamppb.New(closedAt.Time)
	}
	return &o, nil
}

func (s *taskService) CreateOrder(ctx context.Context, req *taskpb.CreateOrderRequest) (*taskpb.Order, error) {
	if err := requireActor(req.ActorUserId); err != nil {
		return nil, err
	}
	f := orderFields{
		title: req.Title, description: req.Description, budgetType: req.BudgetType,
		budgetAmount: req.BudgetAmount, currency: req.Currency, deadline: req.Deadline,
	}
	if err := f.validate(); err != nil {
		return nil, err
	}
	row := s.db.QueryRowContext(ctx, `INSERT INTO orders
		(author_user_id, title, description, budget_type, budget_amount, currency, deadline)
		VALUES ($1, $2, $3, $4, $5::numeric, $6::char(3), NULLIF($7, '')::date)
		RETURNING `+orderColumns,
		req.ActorUserId, req.Title, req.Description, req.BudgetType,
		req.BudgetAmount, req.Currency, req.Deadline)
	o, err := scanOrder(row)
	if err != nil {
		return nil, mapDBError(err)
	}
	return o, nil
}

func (s *taskService) GetOrder(ctx context.Context, req *taskpb.GetOrderRequest) (*taskpb.Order, error) {
	if !validUUID(req.OrderId) {
		return nil, invalid("order_id must be a UUID")
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+orderColumns+` FROM orders WHERE id = $1`, req.OrderId)
	o, err := scanOrder(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound()
	}
	if err != nil {
		return nil, mapDBError(err)
	}
	// Published orders are public; everything else is for the author only.
	// A stranger gets the same 404 for drafts, closed and missing orders.
	if o.Status != "published" && o.AuthorUserId != req.ActorUserId {
		return nil, notFound()
	}
	return o, nil
}

func (s *taskService) UpdateOrder(ctx context.Context, req *taskpb.UpdateOrderRequest) (*taskpb.Order, error) {
	if err := requireActor(req.ActorUserId); err != nil {
		return nil, err
	}
	if !validUUID(req.OrderId) {
		return nil, invalid("order_id must be a UUID")
	}
	if req.Version < 1 {
		return nil, invalid("version is the order version you last read (at least 1)")
	}
	f := orderFields{
		title: req.Title, description: req.Description, budgetType: req.BudgetType,
		budgetAmount: req.BudgetAmount, currency: req.Currency, deadline: req.Deadline,
	}
	if err := f.validate(); err != nil {
		return nil, err
	}
	var currentAuthor string
	var currentVersion int64
	var currentStatus string
	err := s.db.QueryRowContext(ctx, `SELECT author_user_id, version, status FROM orders WHERE id = $1`,
		req.OrderId).Scan(&currentAuthor, &currentVersion, &currentStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound()
	}
	if err != nil {
		return nil, mapDBError(err)
	}
	if currentAuthor != req.ActorUserId {
		return nil, notFound()
	}
	if currentStatus == "closed" {
		return nil, precondition("closed order cannot be edited")
	}
	if currentVersion != req.Version {
		return nil, precondition(
			"order changed since you last read it (current version %d, sent %d); reload and retry",
			currentVersion, req.Version)
	}
	row := s.db.QueryRowContext(ctx, `UPDATE orders SET
			title = $4, description = $5, budget_type = $6, budget_amount = $7::numeric,
			currency = $8::char(3), deadline = NULLIF($9, '')::date,
			version = version + 1, updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 AND author_user_id = $2 AND version = $3
		RETURNING `+orderColumns,
		req.OrderId, req.ActorUserId, req.Version, req.Title, req.Description,
		req.BudgetType, req.BudgetAmount, req.Currency, req.Deadline)
	o, err := scanOrder(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, precondition("order changed concurrently; reload and retry")
	}
	if err != nil {
		return nil, mapDBError(err)
	}
	return o, nil
}

// transition is the CAS body shared by PublishOrder and CloseOrder:
// classify the current row, then flip the status in one guarded UPDATE.
func (s *taskService) transition(ctx context.Context, orderID, actor, from, to, alreadyMsg, wrongStateMsg string) (*taskpb.Order, error) {
	var currentAuthor, currentStatus string
	err := s.db.QueryRowContext(ctx, `SELECT author_user_id, status FROM orders WHERE id = $1`, orderID).
		Scan(&currentAuthor, &currentStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound()
	}
	if err != nil {
		return nil, mapDBError(err)
	}
	if currentAuthor != actor {
		return nil, notFound()
	}
	switch currentStatus {
	case to:
		return nil, precondition("%s", alreadyMsg)
	case from:
		// proceed below
	default:
		return nil, precondition("%s", wrongStateMsg)
	}
	row := s.db.QueryRowContext(ctx, `UPDATE orders SET
			status = $3,
			published_at = CASE WHEN $3 = 'published' THEN CURRENT_TIMESTAMP ELSE published_at END,
			closed_at = CASE WHEN $3 = 'closed' THEN CURRENT_TIMESTAMP ELSE closed_at END
		WHERE id = $1 AND author_user_id = $2 AND status = $4
		RETURNING `+orderColumns,
		orderID, actor, to, from)
	o, err := scanOrder(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, precondition("order changed concurrently; reload and retry")
	}
	if err != nil {
		return nil, mapDBError(err)
	}
	return o, nil
}

func (s *taskService) PublishOrder(ctx context.Context, req *taskpb.PublishOrderRequest) (*taskpb.Order, error) {
	if err := requireActor(req.ActorUserId); err != nil {
		return nil, err
	}
	if !validUUID(req.OrderId) {
		return nil, invalid("order_id must be a UUID")
	}
	return s.transition(ctx, req.OrderId, req.ActorUserId, "draft", "published",
		"order is already published", "closed order cannot be published")
}

func (s *taskService) CloseOrder(ctx context.Context, req *taskpb.CloseOrderRequest) (*taskpb.Order, error) {
	if err := requireActor(req.ActorUserId); err != nil {
		return nil, err
	}
	if !validUUID(req.OrderId) {
		return nil, invalid("order_id must be a UUID")
	}
	return s.transition(ctx, req.OrderId, req.ActorUserId, "published", "closed",
		"order is already closed", "draft must be published before it can be closed")
}

func (s *taskService) ListMyOrders(ctx context.Context, req *taskpb.ListMyOrdersRequest) (*taskpb.ListMyOrdersResponse, error) {
	if err := requireActor(req.ActorUserId); err != nil {
		return nil, err
	}
	pageSize := int(req.PageSize)
	if pageSize == 0 {
		pageSize = 20
	}
	if pageSize < 0 || pageSize > 50 {
		return nil, invalid("page_size must be between 1 and 50")
	}

	const base = `SELECT ` + orderColumns + ` FROM orders WHERE author_user_id = $1`
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
			req.ActorUserId, pageSize+1)
	} else {
		rows, err = s.db.QueryContext(ctx, base+` AND (created_at, id) < ($2, $3::uuid)
			ORDER BY created_at DESC, id DESC LIMIT $4`,
			req.ActorUserId, cursorAt, cursorID, pageSize+1)
	}
	if err != nil {
		return nil, mapDBError(err)
	}
	defer rows.Close()

	response := &taskpb.ListMyOrdersResponse{}
	for i := 0; rows.Next(); i++ {
		if i == pageSize {
			// One extra row proves a next page exists; the token sits on the
			// last row returned to the client.
			last := response.Orders[pageSize-1]
			response.NextPageToken = encodeCursor(last.CreatedAt.AsTime(), last.Id)
			break
		}
		o, err := scanOrder(rows)
		if err != nil {
			return nil, mapDBError(err)
		}
		response.Orders = append(response.Orders, o)
	}
	if err := rows.Err(); err != nil {
		return nil, mapDBError(err)
	}
	return response, nil
}

// ListPublishedOrders serves the public catalog: only published rows,
// newest first, keyset by (published_at, id) matching the orders_public
// index. Filters are validated before touching the database.
func (s *taskService) ListPublishedOrders(ctx context.Context, req *taskpb.ListPublishedOrdersRequest) (*taskpb.ListPublishedOrdersResponse, error) {
	pageSize := int(req.PageSize)
	if pageSize == 0 {
		pageSize = 20
	}
	if pageSize < 0 || pageSize > 50 {
		return nil, invalid("page_size must be between 1 and 50")
	}
	if req.BudgetType != "" && !budgetTypes[req.BudgetType] {
		return nil, invalid("budget_type must be one of: fixed, hourly")
	}
	if req.BudgetMin != "" && !moneyPattern.MatchString(req.BudgetMin) {
		return nil, invalid("budget_min must be a decimal amount like 50000")
	}
	if req.BudgetMax != "" && !moneyPattern.MatchString(req.BudgetMax) {
		return nil, invalid("budget_max must be a decimal amount like 250000")
	}
	if runes := utf8.RuneCountInString(req.TitleContains); runes > 120 {
		return nil, invalid("title_contains must not exceed 120 characters")
	}

	var cursorAt time.Time
	var cursorID string
	if req.PageToken != "" {
		at, id, err := decodeCursor(req.PageToken)
		if err != nil {
			return nil, invalid("page_token is not a valid cursor")
		}
		cursorAt, cursorID = at, id
	}

	// Same construction as the vacancy catalog: constant fragments and
	// numbered parameters only. The budget is a single amount, so both
	// bounds apply to budget_amount directly. ILIKE metacharacters of the
	// search term are escaped to literals.
	var conditions strings.Builder
	args := []any{}
	next := 1
	push := func(fragment string, value any) {
		conditions.WriteString(" AND ")
		conditions.WriteString(fragment)
		args = append(args, value)
		next++
	}
	if !cursorAt.IsZero() {
		push(fmt.Sprintf("(published_at, id) < ($%d, $%d::uuid)", next, next+1), cursorAt)
		args = append(args, cursorID)
		next++
	}
	if req.BudgetType != "" {
		push(fmt.Sprintf("budget_type = $%d", next), req.BudgetType)
	}
	if req.BudgetMin != "" {
		push(fmt.Sprintf("budget_amount >= $%d::numeric", next), req.BudgetMin)
	}
	if req.BudgetMax != "" {
		push(fmt.Sprintf("budget_amount <= $%d::numeric", next), req.BudgetMax)
	}
	if req.TitleContains != "" {
		push(fmt.Sprintf("title ILIKE '%%' || $%d || '%%'", next), ilikeEscape.Replace(req.TitleContains))
	}
	args = append(args, pageSize+1)
	query := `SELECT ` + orderColumns + ` FROM orders WHERE status = 'published'` +
		conditions.String() +
		fmt.Sprintf(` ORDER BY published_at DESC, id DESC LIMIT $%d`, next)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapDBError(err)
	}
	defer rows.Close()

	response := &taskpb.ListPublishedOrdersResponse{}
	for i := 0; rows.Next(); i++ {
		if i == pageSize {
			// One extra row proves a next page exists; the token sits on the
			// last row returned to the client.
			last := response.Orders[pageSize-1]
			response.NextPageToken = encodeCursor(last.PublishedAt.AsTime(), last.Id)
			break
		}
		o, err := scanOrder(rows)
		if err != nil {
			return nil, mapDBError(err)
		}
		response.Orders = append(response.Orders, o)
	}
	if err := rows.Err(); err != nil {
		return nil, mapDBError(err)
	}
	return response, nil
}

func (s *taskService) DeleteDraft(ctx context.Context, req *taskpb.DeleteDraftRequest) (*taskpb.Order, error) {
	if err := requireActor(req.ActorUserId); err != nil {
		return nil, err
	}
	if !validUUID(req.OrderId) {
		return nil, invalid("order_id must be a UUID")
	}
	var currentAuthor, currentStatus string
	err := s.db.QueryRowContext(ctx, `SELECT author_user_id, status FROM orders WHERE id = $1`, req.OrderId).
		Scan(&currentAuthor, &currentStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound()
	}
	if err != nil {
		return nil, mapDBError(err)
	}
	if currentAuthor != req.ActorUserId {
		return nil, notFound()
	}
	if currentStatus == "published" {
		return nil, precondition("published order cannot be deleted; close it first")
	}
	if currentStatus == "closed" {
		return nil, precondition("closed order is history and cannot be deleted")
	}
	row := s.db.QueryRowContext(ctx, `DELETE FROM orders
		WHERE id = $1 AND author_user_id = $2 AND status = 'draft'
		RETURNING `+orderColumns, req.OrderId, req.ActorUserId)
	o, err := scanOrder(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, precondition("order changed concurrently; reload and retry")
	}
	if err != nil {
		return nil, mapDBError(err)
	}
	return o, nil
}
