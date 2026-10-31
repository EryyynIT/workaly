package main

import (
	"context"
	"net/http"
	"time"

	jobpb "github.com/EryyynIT/workaly/auth-service/pb/jobpb"
	"github.com/gin-gonic/gin"
)

// responseRequest is the body of creating a vacancy response. Field
// validation lives in job-service; the edge only translates transport.
type responseRequest struct {
	CoverLetter string  `json:"cover_letter"`
	Contact     *string `json:"contact"`
}

func (app *application) createVacancyResponse(c *gin.Context) {
	actor, ok := app.actor(c)
	if !ok {
		return
	}
	var req responseRequest
	if err := bindJSON(c, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if !app.limiter.Allow("vacancy-response:"+c.ClientIP(), 60, time.Hour) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many requests; try again later"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), requestTimeout)
	defer cancel()
	// Responding requires a confirmed email, same as publishing (S2 decision 4).
	verified, err := app.emailVerified(ctx, actor)
	if err != nil {
		writeDBError(c, err)
		return
	}
	if !verified {
		c.JSON(http.StatusForbidden, gin.H{"error": "email_not_verified"})
		return
	}
	r, err := app.vacancies.CreateVacancyResponse(ctx, &jobpb.CreateVacancyResponseRequest{
		ActorUserId: actor,
		VacancyId:   c.Param("id"),
		CoverLetter: req.CoverLetter,
		Contact:     textOrEmpty(req.Contact),
	})
	if err != nil {
		app.writeVacancyError(c, err)
		return
	}
	c.JSON(http.StatusCreated, responseJSON(r))
}

// listVacancyResponses serves the author of the vacancy; job-service rejects
// every other actor with NotFound, so the edge adds no ownership logic.
func (app *application) listVacancyResponses(c *gin.Context) {
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
	list, err := app.vacancies.ListVacancyResponses(ctx, &jobpb.ListVacancyResponsesRequest{
		ActorUserId: actor,
		VacancyId:   c.Param("id"),
		PageToken:   c.Query("page_token"),
		PageSize:    pageSize,
	})
	if err != nil {
		app.writeVacancyError(c, err)
		return
	}
	views := make([]*responseView, 0, len(list.Responses))
	for _, r := range list.Responses {
		views = append(views, responseJSONForAuthor(r))
	}
	c.JSON(http.StatusOK, gin.H{"responses": views, "next_page_token": list.NextPageToken})
}

func (app *application) withdrawVacancyResponse(c *gin.Context) {
	actor, ok := app.actor(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), requestTimeout)
	defer cancel()
	r, err := app.vacancies.WithdrawVacancyResponse(ctx, &jobpb.WithdrawVacancyResponseRequest{
		ActorUserId: actor,
		VacancyId:   c.Param("id"),
		ResponseId:  c.Param("rid"),
	})
	if err != nil {
		app.writeVacancyError(c, err)
		return
	}
	c.JSON(http.StatusOK, responseJSONForCandidate(r))
}

// getMyVacancyResponse serves the candidate; the response JSON is theirs.
func (app *application) getMyVacancyResponse(c *gin.Context) {
	actor, ok := app.actor(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), requestTimeout)
	defer cancel()
	r, err := app.vacancies.GetMyVacancyResponse(ctx, &jobpb.GetMyVacancyResponseRequest{
		ActorUserId: actor,
		VacancyId:   c.Param("id"),
	})
	if err != nil {
		app.writeVacancyError(c, err)
		return
	}
	c.JSON(http.StatusOK, responseJSONForCandidate(r))
}

// decisionRequest carries the optional reason of reject/decline calls.
type decisionRequest struct {
	Reason *string `json:"reason"`
}

// bindReason reads the optional JSON body of a decision call; an empty body
// is valid and means "no reason". Non-empty bodies go through the same
// hardened parser as every other JSON endpoint (invalid UTF-8 and trailing
// documents are rejected).
func bindReason(c *gin.Context) (string, bool) {
	if c.Request.Body == nil || c.Request.ContentLength == 0 {
		return "", true
	}
	var req decisionRequest
	if err := bindJSON(c, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return "", false
	}
	return textOrEmpty(req.Reason), true
}

// rejectVacancyResponse: author, active -> rejected; the reason reaches the
// candidate only (the author list view strips it, see responseJSONForAuthor).
func (app *application) rejectVacancyResponse(c *gin.Context) {
	actor, ok := app.actor(c)
	if !ok {
		return
	}
	reason, ok := bindReason(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), requestTimeout)
	defer cancel()
	r, err := app.vacancies.RejectVacancyResponse(ctx, &jobpb.RejectVacancyResponseRequest{
		ActorUserId: actor,
		VacancyId:   c.Param("id"),
		ResponseId:  c.Param("rid"),
		Reason:      reason,
	})
	if err != nil {
		app.writeVacancyError(c, err)
		return
	}
	c.JSON(http.StatusOK, responseJSONForAuthor(r))
}

func (app *application) sendOffer(c *gin.Context) {
	app.simpleTransition(c, func(ctx context.Context, actor string) (*jobpb.VacancyResponse, error) {
		return app.vacancies.SendOffer(ctx, &jobpb.SendOfferRequest{
			ActorUserId: actor, VacancyId: c.Param("id"), ResponseId: c.Param("rid"),
		})
	})
}

func (app *application) withdrawOffer(c *gin.Context) {
	app.simpleTransition(c, func(ctx context.Context, actor string) (*jobpb.VacancyResponse, error) {
		return app.vacancies.WithdrawOffer(ctx, &jobpb.WithdrawOfferRequest{
			ActorUserId: actor, VacancyId: c.Param("id"), ResponseId: c.Param("rid"),
		})
	})
}

func (app *application) markHired(c *gin.Context) {
	app.simpleTransition(c, func(ctx context.Context, actor string) (*jobpb.VacancyResponse, error) {
		return app.vacancies.MarkHired(ctx, &jobpb.MarkHiredRequest{
			ActorUserId: actor, VacancyId: c.Param("id"), ResponseId: c.Param("rid"),
		})
	})
}

func (app *application) acceptOffer(c *gin.Context) {
	actor, ok := app.actor(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), requestTimeout)
	defer cancel()
	r, err := app.vacancies.AcceptOffer(ctx, &jobpb.AcceptOfferRequest{
		ActorUserId: actor,
		VacancyId:   c.Param("id"),
		ResponseId:  c.Param("rid"),
	})
	if err != nil {
		app.writeVacancyError(c, err)
		return
	}
	c.JSON(http.StatusOK, responseJSONForCandidate(r))
}

func (app *application) declineOffer(c *gin.Context) {
	actor, ok := app.actor(c)
	if !ok {
		return
	}
	reason, ok := bindReason(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), requestTimeout)
	defer cancel()
	r, err := app.vacancies.DeclineOffer(ctx, &jobpb.DeclineOfferRequest{
		ActorUserId: actor,
		VacancyId:   c.Param("id"),
		ResponseId:  c.Param("rid"),
		Reason:      reason,
	})
	if err != nil {
		app.writeVacancyError(c, err)
		return
	}
	c.JSON(http.StatusOK, responseJSONForCandidate(r))
}

// simpleTransition serves the author-side hiring calls without a request
// body (send/withdraw/hired). Author-side calls strip the reject reason that
// belongs to the candidate — matching the dedicated reject handler.
func (app *application) simpleTransition(c *gin.Context, call func(context.Context, string) (*jobpb.VacancyResponse, error)) {
	actor, ok := app.actor(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), requestTimeout)
	defer cancel()
	r, err := call(ctx, actor)
	if err != nil {
		app.writeVacancyError(c, err)
		return
	}
	c.JSON(http.StatusOK, responseJSONForAuthor(r))
}

// responseView is the HTTP representation of a vacancy response. It is
// served only to the vacancy author (list) or to the responding candidate
// themselves (get/withdraw), so the contact field is never exposed publicly.
type responseView struct {
	ID              string  `json:"id"`
	VacancyID       string  `json:"vacancy_id"`
	CandidateUserID string  `json:"candidate_user_id"`
	CoverLetter     string  `json:"cover_letter"`
	Contact         *string `json:"contact"`
	Status          string  `json:"status"`
	Reason          *string `json:"reason,omitempty"`
	CreatedAt       string  `json:"created_at"`
	WithdrawnAt     *string `json:"withdrawn_at"`
	DecidedAt       *string `json:"decided_at"`
}

func responseJSON(r *jobpb.VacancyResponse) *responseView {
	view := &responseView{
		ID:              r.Id,
		VacancyID:       r.VacancyId,
		CandidateUserID: r.CandidateUserId,
		CoverLetter:     r.CoverLetter,
		Status:          r.Status,
		CreatedAt:       utcString(r.CreatedAt),
		WithdrawnAt:     optionalTimestamp(r.WithdrawnAt),
		DecidedAt:       optionalTimestamp(r.DecidedAt),
	}
	if r.Contact != "" {
		view.Contact = &r.Contact
	}
	if r.Reason != "" {
		view.Reason = &r.Reason
	}
	return view
}

// responseJSONForAuthor strips reasons meant for the candidate: the author
// sees the decline reason of their offer, never the reject reason they gave
// (their own) — the author's own reason is already known to them, and the
// candidate must not learn it back through a shared view. The reject reason
// is stripped because the author view is also the shape reused by
// simpleTransition responses.
func responseJSONForAuthor(r *jobpb.VacancyResponse) *responseView {
	view := responseJSON(r)
	if r.Status == "rejected" {
		view.Reason = nil
	}
	return view
}

// responseJSONForCandidate strips nothing the candidate may not see: the
// candidate sees the reject reason and their own decline reason; the contact
// is their own.
func responseJSONForCandidate(r *jobpb.VacancyResponse) *responseView {
	return responseJSON(r)
}
