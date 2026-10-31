package main

import (
	"context"
	"net/http"

	taskpb "github.com/EryyynIT/workaly/auth-service/pb/taskpb"
	"github.com/gin-gonic/gin"
)

// The HTTP edge of the deal pipeline (S3.2). Selection terms arrive
// optional: an absent amount/currency pair means "the proposal's terms as
// they are", so the edge reads the proposal and forwards its values
// verbatim; the handshake decision itself is computed in PostgreSQL, never
// here. Validation of the terms lives in task-service; the edge only
// translates transport.

type selectProposalRequest struct {
	Amount             *string `json:"amount"`
	Currency           *string `json:"currency"`
	Deadline           *string `json:"deadline"`
	RevisionsIncluded  *int32  `json:"revisions_included"`
	AcceptanceCriteria *string `json:"acceptance_criteria"`
}

func (app *application) selectProposal(c *gin.Context) {
	actor, ok := app.actor(c)
	if !ok {
		return
	}
	var req selectProposalRequest
	if err := bindJSON(c, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), requestTimeout)
	defer cancel()

	var proposal *taskpb.OrderProposal
	if (req.Amount == nil) != (req.Currency == nil) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "amount and currency are required together with custom terms"})
		return
	}
	{
		// Default terms: the proposal as the executor offered it. The
		// proposals list is author-only, so the read doubles as the
		// ownership check of the path.
		pageToken := ""
		seenPageTokens := make(map[string]struct{})
		for {
			list, err := app.orders.ListOrderProposals(ctx, &taskpb.ListOrderProposalsRequest{
				ActorUserId: actor,
				OrderId:     c.Param("id"),
				PageSize:    50,
				PageToken:   pageToken,
			})
			if err != nil {
				app.writeOrderError(c, err)
				return
			}
			for _, p := range list.Proposals {
				if p.Id == c.Param("pid") {
					proposal = p
					break
				}
			}
			if proposal != nil || list.NextPageToken == "" {
				break
			}
			if _, repeated := seenPageTokens[list.NextPageToken]; repeated {
				c.JSON(http.StatusBadGateway, gin.H{"error": "order service returned an invalid page cursor"})
				return
			}
			seenPageTokens[list.NextPageToken] = struct{}{}
			pageToken = list.NextPageToken
		}
		if proposal == nil {
			// Same undisclosing 404 as every other missing or foreign
			// resource of the order.
			c.JSON(http.StatusNotFound, gin.H{"error": "order not found"})
			return
		}
	}

	amount, currency, deadline := proposal.ProposedAmount, proposal.Currency, proposal.ProposedDeadline
	var revisions int32
	criteria := ""
	if req.Amount != nil {
		amount = textOrEmpty(req.Amount)
	}
	if req.Currency != nil {
		currency = textOrEmpty(req.Currency)
	}
	if req.Deadline != nil {
		deadline = textOrEmpty(req.Deadline)
	}
	if req.RevisionsIncluded != nil {
		revisions = *req.RevisionsIncluded
	}
	if req.AcceptanceCriteria != nil {
		criteria = *req.AcceptanceCriteria
	}
	deal, err := app.orders.SelectProposal(ctx, &taskpb.SelectProposalRequest{
		ActorUserId:        actor,
		OrderId:            c.Param("id"),
		ProposalId:         c.Param("pid"),
		Amount:             amount,
		Currency:           currency,
		Deadline:           deadline,
		RevisionsIncluded:  revisions,
		AcceptanceCriteria: criteria,
	})
	if err != nil {
		app.writeOrderError(c, err)
		return
	}
	c.JSON(http.StatusCreated, dealJSON(deal))
}

// dealAction serves the deal calls without a request body. The RPC runs
// before the session check aborts only when the caller is authenticated.
func (app *application) dealAction(c *gin.Context, call func(context.Context, string) (*taskpb.Deal, error)) {
	actor, ok := app.actor(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), requestTimeout)
	defer cancel()
	deal, err := call(ctx, actor)
	if err != nil {
		if !c.IsAborted() {
			app.writeOrderError(c, err)
		}
		return
	}
	c.JSON(http.StatusOK, dealJSON(deal))
}

func (app *application) confirmDealTerms(c *gin.Context) {
	app.dealAction(c, func(ctx context.Context, actor string) (*taskpb.Deal, error) {
		return app.orders.ConfirmDealTerms(ctx, &taskpb.ConfirmDealTermsRequest{
			ActorUserId: actor, OrderId: c.Param("id"),
		})
	})
}

func (app *application) declineDealTerms(c *gin.Context) {
	actor, ok := app.actor(c)
	if !ok {
		return
	}
	// The decline reason is optional; an empty body is valid.
	reason, ok := bindReason(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), requestTimeout)
	defer cancel()
	deal, err := app.orders.DeclineDealTerms(ctx, &taskpb.DeclineDealTermsRequest{
		ActorUserId: actor,
		OrderId:     c.Param("id"),
		Reason:      reason,
	})
	if err != nil {
		app.writeOrderError(c, err)
		return
	}
	c.JSON(http.StatusOK, dealJSON(deal))
}

type submitResultRequest struct {
	Result         string `json:"result"`
	IdempotencyKey string `json:"idempotency_key"`
}

func (app *application) submitDealResult(c *gin.Context) {
	actor, ok := app.actor(c)
	if !ok {
		return
	}
	var req submitResultRequest
	if err := bindJSON(c, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), requestTimeout)
	defer cancel()
	if req.IdempotencyKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "idempotency_key is required; reuse it when retrying the same submission"})
		return
	}
	deal, err := app.orders.SubmitResult(ctx, &taskpb.SubmitResultRequest{
		ActorUserId:    actor,
		OrderId:        c.Param("id"),
		Result:         req.Result,
		IdempotencyKey: req.IdempotencyKey,
	})
	if err != nil {
		app.writeOrderError(c, err)
		return
	}
	c.JSON(http.StatusOK, dealJSON(deal))
}

func (app *application) acceptDealResult(c *gin.Context) {
	app.dealAction(c, func(ctx context.Context, actor string) (*taskpb.Deal, error) {
		return app.orders.AcceptResult(ctx, &taskpb.AcceptResultRequest{
			ActorUserId: actor, OrderId: c.Param("id"),
		})
	})
}

// reasonRequest carries the mandatory reason of revision and cancel calls.
type reasonRequest struct {
	Reason string `json:"reason"`
}

func (app *application) requestDealRevision(c *gin.Context) {
	actor, ok := app.actor(c)
	if !ok {
		return
	}
	var req reasonRequest
	if err := bindJSON(c, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), requestTimeout)
	defer cancel()
	deal, err := app.orders.RequestRevision(ctx, &taskpb.RequestRevisionRequest{
		ActorUserId: actor,
		OrderId:     c.Param("id"),
		Reason:      req.Reason,
	})
	if err != nil {
		app.writeOrderError(c, err)
		return
	}
	c.JSON(http.StatusOK, dealJSON(deal))
}

func (app *application) cancelDeal(c *gin.Context) {
	actor, ok := app.actor(c)
	if !ok {
		return
	}
	var req reasonRequest
	if err := bindJSON(c, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), requestTimeout)
	defer cancel()
	deal, err := app.orders.CancelDeal(ctx, &taskpb.CancelDealRequest{
		ActorUserId: actor,
		OrderId:     c.Param("id"),
		Reason:      req.Reason,
	})
	if err != nil {
		app.writeOrderError(c, err)
		return
	}
	c.JSON(http.StatusOK, dealJSON(deal))
}

func (app *application) getDeal(c *gin.Context) {
	actor, ok := app.actor(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), requestTimeout)
	defer cancel()
	deal, err := app.orders.GetDeal(ctx, &taskpb.GetDealRequest{
		ActorUserId: actor,
		OrderId:     c.Param("id"),
	})
	if err != nil {
		app.writeOrderError(c, err)
		return
	}
	c.JSON(http.StatusOK, dealJSON(deal))
}

// dealView is the HTTP representation of a deal. It is served to the two
// parties only; every field is visible to both — the cancel reason by
// decision 3 of docs/s3-hiring.md, revision reasons ride on the submissions
// they sent back.
type dealView struct {
	ID             string                `json:"id"`
	OrderID        string                `json:"order_id"`
	CustomerUserID string                `json:"customer_user_id"`
	ExecutorUserID string                `json:"executor_user_id"`
	Status         string                `json:"status"`
	Terms          dealTermsView         `json:"terms"`
	Submissions    []*dealSubmissionView `json:"submissions"`
	Version        int64                 `json:"version"`
	CreatedAt      string                `json:"created_at"`
	StartedAt      *string               `json:"started_at"`
	SubmittedAt    *string               `json:"submitted_at"`
	DecidedAt      *string               `json:"decided_at"`
	Reason         *string               `json:"reason"`
}

type dealTermsView struct {
	Amount             string  `json:"amount"`
	Currency           string  `json:"currency"`
	Deadline           *string `json:"deadline"`
	RevisionsIncluded  int32   `json:"revisions_included"`
	AcceptanceCriteria *string `json:"acceptance_criteria"`
}

type dealSubmissionView struct {
	Iteration      int64   `json:"iteration"`
	Result         string  `json:"result"`
	SubmittedAt    string  `json:"submitted_at"`
	RevisionReason *string `json:"revision_reason"`
}

func dealJSON(d *taskpb.Deal) *dealView {
	view := &dealView{
		ID:             d.Id,
		OrderID:        d.OrderId,
		CustomerUserID: d.CustomerUserId,
		ExecutorUserID: d.ExecutorUserId,
		Status:         d.Status,
		Version:        d.Version,
		CreatedAt:      utcString(d.CreatedAt),
		StartedAt:      optionalTimestamp(d.StartedAt),
		SubmittedAt:    optionalTimestamp(d.SubmittedAt),
		DecidedAt:      optionalTimestamp(d.DecidedAt),
		Submissions:    []*dealSubmissionView{},
		Terms: dealTermsView{
			Amount:            d.Terms.Amount,
			Currency:          d.Terms.Currency,
			RevisionsIncluded: d.Terms.RevisionsIncluded,
		},
	}
	if d.Terms.Deadline != "" {
		view.Terms.Deadline = &d.Terms.Deadline
	}
	if d.Terms.AcceptanceCriteria != "" {
		view.Terms.AcceptanceCriteria = &d.Terms.AcceptanceCriteria
	}
	if d.Reason != "" {
		view.Reason = &d.Reason
	}
	for _, s := range d.Submissions {
		item := &dealSubmissionView{
			Iteration:   s.Iteration,
			Result:      s.Result,
			SubmittedAt: utcString(s.SubmittedAt),
		}
		if s.RevisionReason != "" {
			item.RevisionReason = &s.RevisionReason
		}
		view.Submissions = append(view.Submissions, item)
	}
	return view
}
