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

const responseColumns = `id, vacancy_id, candidate_user_id, cover_letter,
	contact, status, created_at, withdrawn_at`

func scanResponse(row rowScanner) (*jobpb.VacancyResponse, error) {
	var r jobpb.VacancyResponse
	var contact sql.NullString
	var createdAt time.Time
	var withdrawnAt sql.NullTime
	err := row.Scan(&r.Id, &r.VacancyId, &r.CandidateUserId, &r.CoverLetter,
		&contact, &r.Status, &createdAt, &withdrawnAt)
	if err != nil {
		return nil, err
	}
	r.Contact = contact.String
	r.CreatedAt = timestamppb.New(createdAt)
	if withdrawnAt.Valid {
		r.WithdrawnAt = timestamppb.New(withdrawnAt.Time)
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
	var candidate string
	var status_ string
	err := s.db.QueryRowContext(ctx, `SELECT candidate_user_id, status FROM vacancy_responses
		WHERE id = $1 AND vacancy_id = $2`, req.ResponseId, req.VacancyId).Scan(&candidate, &status_)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound()
	}
	if err != nil {
		return nil, mapDBError(err)
	}
	if candidate != req.ActorUserId {
		return nil, notFound()
	}
	if status_ == "withdrawn" {
		return nil, precondition("response is already withdrawn")
	}
	row := s.db.QueryRowContext(ctx, `UPDATE vacancy_responses
		SET status = 'withdrawn', withdrawn_at = CURRENT_TIMESTAMP
		WHERE id = $1 AND vacancy_id = $2 AND candidate_user_id = $3 AND status = 'active'
		RETURNING `+responseColumns, req.ResponseId, req.VacancyId, req.ActorUserId)
	r, err := scanResponse(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, precondition("response changed concurrently; reload and retry")
	}
	if err != nil {
		return nil, mapDBError(err)
	}
	return r, nil
}

func (s *jobService) GetMyVacancyResponse(ctx context.Context, req *jobpb.GetMyVacancyResponseRequest) (*jobpb.VacancyResponse, error) {
	if err := requireActor(req.ActorUserId); err != nil {
		return nil, err
	}
	if !validUUID(req.VacancyId) {
		return nil, invalid("vacancy_id must be a UUID")
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+responseColumns+` FROM vacancy_responses
		WHERE vacancy_id = $1 AND candidate_user_id = $2 AND status = 'active'`,
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
