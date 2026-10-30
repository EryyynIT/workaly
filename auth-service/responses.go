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
		views = append(views, responseJSON(r))
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
	c.JSON(http.StatusOK, responseJSON(r))
}

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
	c.JSON(http.StatusOK, responseJSON(r))
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
	CreatedAt       string  `json:"created_at"`
	WithdrawnAt     *string `json:"withdrawn_at"`
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
	}
	if r.Contact != "" {
		view.Contact = &r.Contact
	}
	return view
}
