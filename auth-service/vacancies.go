package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	jobpb "github.com/EryyynIT/workaly/auth-service/pb/jobpb"
	"github.com/gin-gonic/gin"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// vacancyRequest is the shared body of create and update. Field validation
// lives in job-service; the edge only translates transport, so its errors
// keep the precise wording the service produced.
type vacancyRequest struct {
	Title          string  `json:"title"`
	Description    string  `json:"description"`
	EmploymentType string  `json:"employment_type"`
	WorkFormat     string  `json:"work_format"`
	Location       *string `json:"location"`
	SalaryMin      *string `json:"salary_min"`
	SalaryMax      *string `json:"salary_max"`
	Currency       *string `json:"currency"`
}

type vacancyPatchRequest struct {
	vacancyRequest
	Version int64 `json:"version"`
}

// actor authenticates the session and stops with 401 when it is missing or
// invalid. The returned value is the JWT subject, the only source of identity.
func (app *application) actor(c *gin.Context) (string, bool) {
	accountID, err := app.requireAuth(c)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return "", false
	}
	return accountID, true
}

func (app *application) createVacancy(c *gin.Context) {
	actor, ok := app.actor(c)
	if !ok {
		return
	}
	var req vacancyRequest
	if err := bindJSON(c, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if !app.limiter.Allow("vacancy-create:"+c.ClientIP(), 60, time.Hour) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many requests; try again later"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), requestTimeout)
	defer cancel()
	v, err := app.vacancies.CreateVacancy(ctx, &jobpb.CreateVacancyRequest{
		ActorUserId:    actor,
		Title:          req.Title,
		Description:    req.Description,
		EmploymentType: req.EmploymentType,
		WorkFormat:     req.WorkFormat,
		Location:       textOrEmpty(req.Location),
		SalaryMin:      textOrEmpty(req.SalaryMin),
		SalaryMax:      textOrEmpty(req.SalaryMax),
		Currency:       textOrEmpty(req.Currency),
	})
	if err != nil {
		app.writeVacancyError(c, err)
		return
	}
	c.JSON(http.StatusCreated, vacancyJSON(v))
}

func (app *application) getVacancy(c *gin.Context) {
	// Public read: an invalid or absent session degrades to anonymous instead
	// of failing, so public views do not depend on the caller at all.
	actor, _ := app.requireAuth(c)
	ctx, cancel := context.WithTimeout(c.Request.Context(), requestTimeout)
	defer cancel()
	v, err := app.vacancies.GetVacancy(ctx, &jobpb.GetVacancyRequest{
		ActorUserId: actor,
		VacancyId:   c.Param("id"),
	})
	if err != nil {
		app.writeVacancyError(c, err)
		return
	}
	c.JSON(http.StatusOK, vacancyJSON(v))
}

func (app *application) patchVacancy(c *gin.Context) {
	actor, ok := app.actor(c)
	if !ok {
		return
	}
	var req vacancyPatchRequest
	if err := bindJSON(c, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), requestTimeout)
	defer cancel()
	v, err := app.vacancies.UpdateVacancy(ctx, &jobpb.UpdateVacancyRequest{
		ActorUserId:    actor,
		VacancyId:      c.Param("id"),
		Version:        req.Version,
		Title:          req.Title,
		Description:    req.Description,
		EmploymentType: req.EmploymentType,
		WorkFormat:     req.WorkFormat,
		Location:       textOrEmpty(req.Location),
		SalaryMin:      textOrEmpty(req.SalaryMin),
		SalaryMax:      textOrEmpty(req.SalaryMax),
		Currency:       textOrEmpty(req.Currency),
	})
	if err != nil {
		app.writeVacancyError(c, err)
		return
	}
	c.JSON(http.StatusOK, vacancyJSON(v))
}

func (app *application) publishVacancy(c *gin.Context) {
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
	v, err := app.vacancies.PublishVacancy(ctx, &jobpb.PublishVacancyRequest{
		ActorUserId: actor,
		VacancyId:   c.Param("id"),
	})
	if err != nil {
		app.writeVacancyError(c, err)
		return
	}
	c.JSON(http.StatusOK, vacancyJSON(v))
}

func (app *application) closeVacancy(c *gin.Context) {
	actor, ok := app.actor(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), requestTimeout)
	defer cancel()
	v, err := app.vacancies.CloseVacancy(ctx, &jobpb.CloseVacancyRequest{
		ActorUserId: actor,
		VacancyId:   c.Param("id"),
	})
	if err != nil {
		app.writeVacancyError(c, err)
		return
	}
	c.JSON(http.StatusOK, vacancyJSON(v))
}

func (app *application) deleteVacancy(c *gin.Context) {
	if !app.sameOrigin(c) {
		return
	}
	actor, ok := app.actor(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), requestTimeout)
	defer cancel()
	v, err := app.vacancies.DeleteDraft(ctx, &jobpb.DeleteDraftRequest{
		ActorUserId: actor,
		VacancyId:   c.Param("id"),
	})
	if err != nil {
		app.writeVacancyError(c, err)
		return
	}
	c.JSON(http.StatusOK, vacancyJSON(v))
}

func (app *application) listMyVacancies(c *gin.Context) {
	actor, ok := app.actor(c)
	if !ok {
		return
	}
	var pageSize int32
	if raw := c.Query("page_size"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 32)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "page_size must be an integer between 1 and 50"})
			return
		}
		pageSize = int32(value)
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), requestTimeout)
	defer cancel()
	list, err := app.vacancies.ListMyVacancies(ctx, &jobpb.ListMyVacanciesRequest{
		ActorUserId: actor,
		PageToken:   c.Query("page_token"),
		PageSize:    pageSize,
	})
	if err != nil {
		app.writeVacancyError(c, err)
		return
	}
	views := make([]*vacancyView, 0, len(list.Vacancies))
	for _, v := range list.Vacancies {
		views = append(views, vacancyJSON(v))
	}
	c.JSON(http.StatusOK, gin.H{"vacancies": views, "next_page_token": list.NextPageToken})
}

// writeVacancyError maps job-service failures to HTTP. Domain messages of our
// own service are safe to show; the 404 body stays uniform so that a draft's
// existence is never disclosed.
func (app *application) writeVacancyError(c *gin.Context, err error) {
	switch status.Code(err) {
	case codes.InvalidArgument:
		c.JSON(http.StatusBadRequest, gin.H{"error": status.Convert(err).Message()})
	case codes.NotFound:
		c.JSON(http.StatusNotFound, gin.H{"error": "vacancy not found"})
	case codes.FailedPrecondition:
		c.JSON(http.StatusConflict, gin.H{"error": status.Convert(err).Message()})
	case codes.AlreadyExists:
		c.JSON(http.StatusConflict, gin.H{"error": status.Convert(err).Message()})
	case codes.ResourceExhausted:
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "service busy; try again later"})
	case codes.Unavailable:
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "vacancy service unavailable"})
	case codes.DeadlineExceeded:
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": "operation timed out; result may be unknown"})
	default:
		// Only the code is logged: upstream errors can contain database details.
		slog.Warn("vacancy request failed", "grpc_code", status.Code(err).String())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal service error"})
	}
}

// vacancyView is the HTTP representation of a vacancy: absent optionals are
// JSON null, timestamps are RFC 3339 UTC.
type vacancyView struct {
	ID             string  `json:"id"`
	AuthorUserID   string  `json:"author_user_id"`
	Title          string  `json:"title"`
	Description    string  `json:"description"`
	EmploymentType string  `json:"employment_type"`
	WorkFormat     string  `json:"work_format"`
	Location       *string `json:"location"`
	SalaryMin      *string `json:"salary_min"`
	SalaryMax      *string `json:"salary_max"`
	Currency       *string `json:"currency"`
	Status         string  `json:"status"`
	Version        int64   `json:"version"`
	CreatedAt      string  `json:"created_at"`
	UpdatedAt      string  `json:"updated_at"`
	PublishedAt    *string `json:"published_at"`
	ClosedAt       *string `json:"closed_at"`
}

func vacancyJSON(v *jobpb.Vacancy) *vacancyView {
	view := &vacancyView{
		ID:             v.Id,
		AuthorUserID:   v.AuthorUserId,
		Title:          v.Title,
		Description:    v.Description,
		EmploymentType: v.EmploymentType,
		WorkFormat:     v.WorkFormat,
		Status:         v.Status,
		Version:        v.Version,
		CreatedAt:      utcString(v.CreatedAt),
		UpdatedAt:      utcString(v.UpdatedAt),
		PublishedAt:    optionalTimestamp(v.PublishedAt),
		ClosedAt:       optionalTimestamp(v.ClosedAt),
	}
	if v.Location != "" {
		view.Location = &v.Location
	}
	if v.SalaryMin != "" {
		view.SalaryMin = &v.SalaryMin
	}
	if v.SalaryMax != "" {
		view.SalaryMax = &v.SalaryMax
	}
	if v.Currency != "" {
		view.Currency = &v.Currency
	}
	return view
}

func utcString(ts *timestamppb.Timestamp) string {
	if ts == nil {
		return ""
	}
	return ts.AsTime().UTC().Format(time.RFC3339)
}

func optionalTimestamp(ts *timestamppb.Timestamp) *string {
	if ts == nil {
		return nil
	}
	value := utcString(ts)
	return &value
}

func textOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// emailVerified reports whether the account confirmed its email address.
// A vanished account counts as unverified: a session outliving its account
// cannot publish anything.
func (app *application) emailVerified(ctx context.Context, accountID string) (bool, error) {
	if app.db == nil {
		return true, nil // unit tests stub every storage call
	}
	var verified bool
	err := app.db.QueryRowContext(ctx,
		`SELECT email_verified_at IS NOT NULL FROM accounts WHERE id = $1`, accountID).Scan(&verified)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return verified, nil
}
