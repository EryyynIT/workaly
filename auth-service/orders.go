package main

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	taskpb "github.com/EryyynIT/workaly/auth-service/pb/taskpb"
	"github.com/gin-gonic/gin"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// orderRequest is the shared body of create and update. Field validation
// lives in task-service; the edge only translates transport, so its errors
// keep the precise wording the service produced.
type orderRequest struct {
	Title        string  `json:"title"`
	Description  string  `json:"description"`
	BudgetType   string  `json:"budget_type"`
	BudgetAmount *string `json:"budget_amount"`
	Currency     *string `json:"currency"`
	Deadline     *string `json:"deadline"`
}

type orderPatchRequest struct {
	orderRequest
	Version int64 `json:"version"`
}

func (app *application) createOrder(c *gin.Context) {
	actor, ok := app.actor(c)
	if !ok {
		return
	}
	var req orderRequest
	if err := bindJSON(c, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if !app.limiter.Allow("order-create:"+c.ClientIP(), 60, time.Hour) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many requests; try again later"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), requestTimeout)
	defer cancel()
	o, err := app.orders.CreateOrder(ctx, &taskpb.CreateOrderRequest{
		ActorUserId:  actor,
		Title:        req.Title,
		Description:  req.Description,
		BudgetType:   req.BudgetType,
		BudgetAmount: textOrEmpty(req.BudgetAmount),
		Currency:     textOrEmpty(req.Currency),
		Deadline:     textOrEmpty(req.Deadline),
	})
	if err != nil {
		app.writeOrderError(c, err)
		return
	}
	c.JSON(http.StatusCreated, orderJSON(o))
}

func (app *application) getOrder(c *gin.Context) {
	// Public read: an invalid or absent session degrades to anonymous instead
	// of failing, so public views do not depend on the caller at all.
	actor, _ := app.requireAuth(c)
	ctx, cancel := context.WithTimeout(c.Request.Context(), requestTimeout)
	defer cancel()
	o, err := app.orders.GetOrder(ctx, &taskpb.GetOrderRequest{
		ActorUserId: actor,
		OrderId:     c.Param("id"),
	})
	if err != nil {
		app.writeOrderError(c, err)
		return
	}
	c.JSON(http.StatusOK, orderJSON(o))
}

func (app *application) patchOrder(c *gin.Context) {
	actor, ok := app.actor(c)
	if !ok {
		return
	}
	var req orderPatchRequest
	if err := bindJSON(c, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), requestTimeout)
	defer cancel()
	o, err := app.orders.UpdateOrder(ctx, &taskpb.UpdateOrderRequest{
		ActorUserId:  actor,
		OrderId:      c.Param("id"),
		Version:      req.Version,
		Title:        req.Title,
		Description:  req.Description,
		BudgetType:   req.BudgetType,
		BudgetAmount: textOrEmpty(req.BudgetAmount),
		Currency:     textOrEmpty(req.Currency),
		Deadline:     textOrEmpty(req.Deadline),
	})
	if err != nil {
		app.writeOrderError(c, err)
		return
	}
	c.JSON(http.StatusOK, orderJSON(o))
}

func (app *application) publishOrder(c *gin.Context) {
	actor, ok := app.actor(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), requestTimeout)
	defer cancel()
	// Publication requires a confirmed email (S2 decision 4); drafts do not.
	verified, err := app.emailVerified(ctx, actor)
	if err != nil {
		writeDBError(c, err)
		return
	}
	if !verified {
		c.JSON(http.StatusForbidden, gin.H{"error": "email_not_verified"})
		return
	}
	o, err := app.orders.PublishOrder(ctx, &taskpb.PublishOrderRequest{
		ActorUserId: actor,
		OrderId:     c.Param("id"),
	})
	if err != nil {
		app.writeOrderError(c, err)
		return
	}
	c.JSON(http.StatusOK, orderJSON(o))
}

func (app *application) closeOrder(c *gin.Context) {
	actor, ok := app.actor(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), requestTimeout)
	defer cancel()
	o, err := app.orders.CloseOrder(ctx, &taskpb.CloseOrderRequest{
		ActorUserId: actor,
		OrderId:     c.Param("id"),
	})
	if err != nil {
		app.writeOrderError(c, err)
		return
	}
	c.JSON(http.StatusOK, orderJSON(o))
}

func (app *application) deleteOrder(c *gin.Context) {
	if !app.sameOrigin(c) {
		return
	}
	actor, ok := app.actor(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), requestTimeout)
	defer cancel()
	o, err := app.orders.DeleteDraft(ctx, &taskpb.DeleteDraftRequest{
		ActorUserId: actor,
		OrderId:     c.Param("id"),
	})
	if err != nil {
		app.writeOrderError(c, err)
		return
	}
	c.JSON(http.StatusOK, orderJSON(o))
}

func (app *application) listMyOrders(c *gin.Context) {
	actor, ok := app.actor(c)
	if !ok {
		return
	}
	pageSize, ok := parsePageSize(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), requestTimeout)
	defer cancel()
	list, err := app.orders.ListMyOrders(ctx, &taskpb.ListMyOrdersRequest{
		ActorUserId: actor,
		PageToken:   c.Query("page_token"),
		PageSize:    pageSize,
	})
	if err != nil {
		app.writeOrderError(c, err)
		return
	}
	views := make([]*orderView, 0, len(list.Orders))
	for _, o := range list.Orders {
		views = append(views, orderJSON(o))
	}
	c.JSON(http.StatusOK, gin.H{"orders": views, "next_page_token": list.NextPageToken})
}

// proposalRequest is the body of creating an order proposal: the executor's
// offer, a draft of the deal terms confirmed later in S3.
type proposalRequest struct {
	Message          string  `json:"message"`
	ProposedAmount   *string `json:"proposed_amount"`
	Currency         *string `json:"currency"`
	ProposedDeadline *string `json:"proposed_deadline"`
}

func (app *application) createOrderProposal(c *gin.Context) {
	actor, ok := app.actor(c)
	if !ok {
		return
	}
	var req proposalRequest
	if err := bindJSON(c, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if !app.limiter.Allow("order-proposal:"+c.ClientIP(), 60, time.Hour) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many requests; try again later"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), requestTimeout)
	defer cancel()
	// Proposing requires a confirmed email, same as publishing (S2 decision 4).
	verified, err := app.emailVerified(ctx, actor)
	if err != nil {
		writeDBError(c, err)
		return
	}
	if !verified {
		c.JSON(http.StatusForbidden, gin.H{"error": "email_not_verified"})
		return
	}
	p, err := app.orders.CreateOrderProposal(ctx, &taskpb.CreateOrderProposalRequest{
		ActorUserId:      actor,
		OrderId:          c.Param("id"),
		Message:          req.Message,
		ProposedAmount:   textOrEmpty(req.ProposedAmount),
		Currency:         textOrEmpty(req.Currency),
		ProposedDeadline: textOrEmpty(req.ProposedDeadline),
	})
	if err != nil {
		app.writeOrderError(c, err)
		return
	}
	c.JSON(http.StatusCreated, proposalJSON(p))
}

// listOrderProposals serves the author of the order; task-service rejects
// every other actor with NotFound, so the edge adds no ownership logic.
func (app *application) listOrderProposals(c *gin.Context) {
	actor, ok := app.actor(c)
	if !ok {
		return
	}
	pageSize, ok := parsePageSize(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), requestTimeout)
	defer cancel()
	list, err := app.orders.ListOrderProposals(ctx, &taskpb.ListOrderProposalsRequest{
		ActorUserId: actor,
		OrderId:     c.Param("id"),
		PageToken:   c.Query("page_token"),
		PageSize:    pageSize,
	})
	if err != nil {
		app.writeOrderError(c, err)
		return
	}
	views := make([]*proposalView, 0, len(list.Proposals))
	for _, p := range list.Proposals {
		views = append(views, proposalJSON(p))
	}
	c.JSON(http.StatusOK, gin.H{"proposals": views, "next_page_token": list.NextPageToken})
}

func (app *application) withdrawOrderProposal(c *gin.Context) {
	actor, ok := app.actor(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), requestTimeout)
	defer cancel()
	p, err := app.orders.WithdrawOrderProposal(ctx, &taskpb.WithdrawOrderProposalRequest{
		ActorUserId: actor,
		OrderId:     c.Param("id"),
		ProposalId:  c.Param("pid"),
	})
	if err != nil {
		app.writeOrderError(c, err)
		return
	}
	c.JSON(http.StatusOK, proposalJSON(p))
}

func (app *application) getMyOrderProposal(c *gin.Context) {
	actor, ok := app.actor(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), requestTimeout)
	defer cancel()
	p, err := app.orders.GetMyOrderProposal(ctx, &taskpb.GetMyOrderProposalRequest{
		ActorUserId: actor,
		OrderId:     c.Param("id"),
	})
	if err != nil {
		app.writeOrderError(c, err)
		return
	}
	c.JSON(http.StatusOK, proposalJSON(p))
}

// writeOrderError maps task-service failures to HTTP. Domain messages of our
// own service are safe to show; the 404 body stays uniform so that a draft's
// existence is never disclosed.
func (app *application) writeOrderError(c *gin.Context, err error) {
	switch status.Code(err) {
	case codes.InvalidArgument:
		c.JSON(http.StatusBadRequest, gin.H{"error": status.Convert(err).Message()})
	case codes.NotFound:
		c.JSON(http.StatusNotFound, gin.H{"error": "order not found"})
	case codes.FailedPrecondition:
		c.JSON(http.StatusConflict, gin.H{"error": status.Convert(err).Message()})
	case codes.AlreadyExists:
		c.JSON(http.StatusConflict, gin.H{"error": status.Convert(err).Message()})
	case codes.ResourceExhausted:
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "service busy; try again later"})
	case codes.Unavailable:
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "order service unavailable"})
	case codes.DeadlineExceeded:
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": "operation timed out; result may be unknown"})
	default:
		// Only the code is logged: upstream errors can contain database details.
		slog.Warn("order request failed", "grpc_code", status.Code(err).String())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal service error"})
	}
}

// orderView is the HTTP representation of an order: absent optionals are
// JSON null, timestamps are RFC 3339 UTC.
type orderView struct {
	ID           string  `json:"id"`
	AuthorUserID string  `json:"author_user_id"`
	Title        string  `json:"title"`
	Description  string  `json:"description"`
	BudgetType   string  `json:"budget_type"`
	BudgetAmount string  `json:"budget_amount"`
	Currency     string  `json:"currency"`
	Deadline     *string `json:"deadline"`
	Status       string  `json:"status"`
	Version      int64   `json:"version"`
	CreatedAt    string  `json:"created_at"`
	UpdatedAt    string  `json:"updated_at"`
	PublishedAt  *string `json:"published_at"`
	ClosedAt     *string `json:"closed_at"`
}

func orderJSON(o *taskpb.Order) *orderView {
	view := &orderView{
		ID:           o.Id,
		AuthorUserID: o.AuthorUserId,
		Title:        o.Title,
		Description:  o.Description,
		BudgetType:   o.BudgetType,
		BudgetAmount: o.BudgetAmount,
		Currency:     o.Currency,
		Status:       o.Status,
		Version:      o.Version,
		CreatedAt:    utcString(o.CreatedAt),
		UpdatedAt:    utcString(o.UpdatedAt),
		PublishedAt:  optionalTimestamp(o.PublishedAt),
		ClosedAt:     optionalTimestamp(o.ClosedAt),
	}
	if o.Deadline != "" {
		view.Deadline = &o.Deadline
	}
	return view
}

// proposalView is the HTTP representation of an order proposal. It is served
// only to the order author (list) or to the proposing executor themselves
// (get/withdraw); proposals never appear in public views.
type proposalView struct {
	ID               string  `json:"id"`
	OrderID          string  `json:"order_id"`
	ExecutorUserID   string  `json:"executor_user_id"`
	Message          string  `json:"message"`
	ProposedAmount   string  `json:"proposed_amount"`
	Currency         string  `json:"currency"`
	ProposedDeadline *string `json:"proposed_deadline"`
	Status           string  `json:"status"`
	CreatedAt        string  `json:"created_at"`
	WithdrawnAt      *string `json:"withdrawn_at"`
}

func proposalJSON(p *taskpb.OrderProposal) *proposalView {
	view := &proposalView{
		ID:             p.Id,
		OrderID:        p.OrderId,
		ExecutorUserID: p.ExecutorUserId,
		Message:        p.Message,
		ProposedAmount: p.ProposedAmount,
		Currency:       p.Currency,
		Status:         p.Status,
		CreatedAt:      utcString(p.CreatedAt),
		WithdrawnAt:    optionalTimestamp(p.WithdrawnAt),
	}
	if p.ProposedDeadline != "" {
		view.ProposedDeadline = &p.ProposedDeadline
	}
	return view
}
