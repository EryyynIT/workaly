package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"log/slog"
	"math/big"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	jobpb "github.com/EryyynIT/workaly/job-service/pb/jobpb"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type jobService struct {
	jobpb.UnimplementedJobServiceServer
	db *sql.DB
}

var (
	employmentTypes = map[string]bool{
		"full_time": true, "part_time": true, "contract": true, "internship": true,
	}
	workFormats = map[string]bool{
		"onsite": true, "remote": true, "hybrid": true,
	}
	moneyPattern = regexp.MustCompile(`^[0-9]{1,10}(\.[0-9]{1,2})?$`)
	currencyPatt = regexp.MustCompile(`^[A-Z]{3}$`)
)

type vacancyFields struct {
	title, description, employmentType, workFormat, location string
	salaryMin, salaryMax, currency                           string
}

// validate mirrors the named CHECK constraints of the vacancies table: the
// service reports precise 400s and the database stays the last backstop.
func (f vacancyFields) validate() error {
	if runes := utf8.RuneCountInString(f.title); runes < 1 || runes > 120 {
		return invalid("title must contain 1..120 characters")
	}
	if runes := utf8.RuneCountInString(f.description); runes < 1 || runes > 5000 {
		return invalid("description must contain 1..5000 characters")
	}
	if !employmentTypes[f.employmentType] {
		return invalid("employment_type must be one of: full_time, part_time, contract, internship")
	}
	if !workFormats[f.workFormat] {
		return invalid("work_format must be one of: onsite, remote, hybrid")
	}
	if runes := utf8.RuneCountInString(f.location); runes > 120 {
		return invalid("location must not exceed 120 characters")
	}
	if f.salaryMin != "" && !moneyPattern.MatchString(f.salaryMin) {
		return invalid("salary_min must be a decimal amount like 150000 or 150000.50")
	}
	if f.salaryMax != "" && !moneyPattern.MatchString(f.salaryMax) {
		return invalid("salary_max must be a decimal amount like 250000 or 250000.50")
	}
	if f.salaryMin == "" && f.salaryMax == "" {
		if f.currency != "" {
			return invalid("currency is allowed only together with a salary")
		}
		return nil
	}
	if !currencyPatt.MatchString(f.currency) {
		return invalid("a salary requires currency as three uppercase letters, for example RUB")
	}
	if f.salaryMin != "" && f.salaryMax != "" {
		minAmount, okMin := new(big.Rat).SetString(f.salaryMin)
		maxAmount, okMax := new(big.Rat).SetString(f.salaryMax)
		if !okMin || !okMax || minAmount.Cmp(maxAmount) > 0 {
			return invalid("salary_min must not exceed salary_max")
		}
	}
	return nil
}

const vacancyColumns = `id, author_user_id, title, description, employment_type, work_format,
	location, salary_min::text, salary_max::text, currency, status, version,
	created_at, updated_at, published_at, closed_at`

type rowScanner interface{ Scan(dest ...any) error }

func scanVacancy(row rowScanner) (*jobpb.Vacancy, error) {
	var v jobpb.Vacancy
	var location, salaryMin, salaryMax, currency sql.NullString
	var createdAt, updatedAt time.Time
	var publishedAt, closedAt sql.NullTime
	err := row.Scan(&v.Id, &v.AuthorUserId, &v.Title, &v.Description, &v.EmploymentType,
		&v.WorkFormat, &location, &salaryMin, &salaryMax, &currency, &v.Status, &v.Version,
		&createdAt, &updatedAt, &publishedAt, &closedAt)
	if err != nil {
		return nil, err
	}
	v.Location = location.String
	v.SalaryMin = salaryMin.String
	v.SalaryMax = salaryMax.String
	v.Currency = currency.String
	v.CreatedAt = timestamppb.New(createdAt)
	v.UpdatedAt = timestamppb.New(updatedAt)
	if publishedAt.Valid {
		v.PublishedAt = timestamppb.New(publishedAt.Time)
	}
	if closedAt.Valid {
		v.ClosedAt = timestamppb.New(closedAt.Time)
	}
	return &v, nil
}

func (s *jobService) CreateVacancy(_ context.Context, req *jobpb.CreateVacancyRequest) (*jobpb.Vacancy, error) {
	if err := requireActor(req.ActorUserId); err != nil {
		return nil, err
	}
	f := vacancyFields{
		title: req.Title, description: req.Description,
		employmentType: req.EmploymentType, workFormat: req.WorkFormat,
		location: req.Location, salaryMin: req.SalaryMin, salaryMax: req.SalaryMax,
		currency: req.Currency,
	}
	if err := f.validate(); err != nil {
		return nil, err
	}
	row := s.db.QueryRow(`INSERT INTO vacancies
		(author_user_id, title, description, employment_type, work_format, location, salary_min, salary_max, currency)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, '')::text, NULLIF($7, '')::numeric,
			NULLIF($8, '')::numeric, NULLIF($9, '')::char(3))
		RETURNING `+vacancyColumns,
		req.ActorUserId, req.Title, req.Description, req.EmploymentType, req.WorkFormat,
		req.Location, req.SalaryMin, req.SalaryMax, req.Currency)
	v, err := scanVacancy(row)
	if err != nil {
		return nil, mapDBError(err)
	}
	return v, nil
}

func (s *jobService) GetVacancy(_ context.Context, req *jobpb.GetVacancyRequest) (*jobpb.Vacancy, error) {
	if !validUUID(req.VacancyId) {
		return nil, invalid("vacancy_id must be a UUID")
	}
	row := s.db.QueryRow(`SELECT `+vacancyColumns+` FROM vacancies WHERE id = $1`, req.VacancyId)
	v, err := scanVacancy(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound()
	}
	if err != nil {
		return nil, mapDBError(err)
	}
	// Published vacancies are public; everything else is for the author only.
	// A stranger gets the same 404 for drafts, closed and missing vacancies.
	if v.Status != "published" && v.AuthorUserId != req.ActorUserId {
		return nil, notFound()
	}
	return v, nil
}

func (s *jobService) UpdateVacancy(_ context.Context, req *jobpb.UpdateVacancyRequest) (*jobpb.Vacancy, error) {
	if err := requireActor(req.ActorUserId); err != nil {
		return nil, err
	}
	if !validUUID(req.VacancyId) {
		return nil, invalid("vacancy_id must be a UUID")
	}
	if req.Version < 1 {
		return nil, invalid("version is the vacancy version you last read (at least 1)")
	}
	f := vacancyFields{
		title: req.Title, description: req.Description,
		employmentType: req.EmploymentType, workFormat: req.WorkFormat,
		location: req.Location, salaryMin: req.SalaryMin, salaryMax: req.SalaryMax,
		currency: req.Currency,
	}
	if err := f.validate(); err != nil {
		return nil, err
	}
	var currentAuthor string
	var currentVersion int64
	var currentStatus string
	err := s.db.QueryRow(`SELECT author_user_id, version, status FROM vacancies WHERE id = $1`,
		req.VacancyId).Scan(&currentAuthor, &currentVersion, &currentStatus)
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
		return nil, precondition("closed vacancy cannot be edited")
	}
	if currentVersion != req.Version {
		return nil, precondition(
			"vacancy changed since you last read it (current version %d, sent %d); reload and retry",
			currentVersion, req.Version)
	}
	row := s.db.QueryRow(`UPDATE vacancies SET
			title = $4, description = $5, employment_type = $6, work_format = $7,
			location = NULLIF($8, '')::text, salary_min = NULLIF($9, '')::numeric,
			salary_max = NULLIF($10, '')::numeric, currency = NULLIF($11, '')::char(3),
			version = version + 1, updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 AND author_user_id = $2 AND version = $3
		RETURNING `+vacancyColumns,
		req.VacancyId, req.ActorUserId, req.Version, req.Title, req.Description,
		req.EmploymentType, req.WorkFormat, req.Location, req.SalaryMin, req.SalaryMax, req.Currency)
	v, err := scanVacancy(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, precondition("vacancy changed concurrently; reload and retry")
	}
	if err != nil {
		return nil, mapDBError(err)
	}
	return v, nil
}

// transition is the CAS body shared by PublishVacancy and CloseVacancy:
// classify the current row, then flip the status in one guarded UPDATE.
// alreadyMsg reports from == to (e.g. double publish); wrongStateMsg covers
// any other current status (e.g. closing a draft).
func (s *jobService) transition(vacancyID, actor, from, to, alreadyMsg, wrongStateMsg string) (*jobpb.Vacancy, error) {
	var currentAuthor, currentStatus string
	err := s.db.QueryRow(`SELECT author_user_id, status FROM vacancies WHERE id = $1`, vacancyID).
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
	row := s.db.QueryRow(`UPDATE vacancies SET
			status = $3,
			published_at = CASE WHEN $3 = 'published' THEN CURRENT_TIMESTAMP ELSE published_at END,
			closed_at = CASE WHEN $3 = 'closed' THEN CURRENT_TIMESTAMP ELSE closed_at END
		WHERE id = $1 AND author_user_id = $2 AND status = $4
		RETURNING `+vacancyColumns,
		vacancyID, actor, to, from)
	v, err := scanVacancy(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, precondition("vacancy changed concurrently; reload and retry")
	}
	if err != nil {
		return nil, mapDBError(err)
	}
	return v, nil
}

func (s *jobService) PublishVacancy(_ context.Context, req *jobpb.PublishVacancyRequest) (*jobpb.Vacancy, error) {
	if err := requireActor(req.ActorUserId); err != nil {
		return nil, err
	}
	if !validUUID(req.VacancyId) {
		return nil, invalid("vacancy_id must be a UUID")
	}
	return s.transition(req.VacancyId, req.ActorUserId, "draft", "published",
		"vacancy is already published", "closed vacancy cannot be published")
}

func (s *jobService) CloseVacancy(_ context.Context, req *jobpb.CloseVacancyRequest) (*jobpb.Vacancy, error) {
	if err := requireActor(req.ActorUserId); err != nil {
		return nil, err
	}
	if !validUUID(req.VacancyId) {
		return nil, invalid("vacancy_id must be a UUID")
	}
	return s.transition(req.VacancyId, req.ActorUserId, "published", "closed",
		"vacancy is already closed", "draft must be published before it can be closed")
}

func (s *jobService) ListMyVacancies(_ context.Context, req *jobpb.ListMyVacanciesRequest) (*jobpb.ListMyVacanciesResponse, error) {
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

	const base = `SELECT ` + vacancyColumns + ` FROM vacancies WHERE author_user_id = $1`
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
		rows, err = s.db.Query(base+` ORDER BY created_at DESC, id DESC LIMIT $2`,
			req.ActorUserId, pageSize+1)
	} else {
		rows, err = s.db.Query(base+` AND (created_at, id) < ($2, $3::uuid)
			ORDER BY created_at DESC, id DESC LIMIT $4`,
			req.ActorUserId, cursorAt, cursorID, pageSize+1)
	}
	if err != nil {
		return nil, mapDBError(err)
	}
	defer rows.Close()

	response := &jobpb.ListMyVacanciesResponse{}
	for i := 0; rows.Next(); i++ {
		if i == pageSize {
			// One extra row proves a next page exists; the token sits on the
			// last row returned to the client.
			last := response.Vacancies[pageSize-1]
			response.NextPageToken = encodeCursor(last.CreatedAt.AsTime(), last.Id)
			break
		}
		v, err := scanVacancy(rows)
		if err != nil {
			return nil, mapDBError(err)
		}
		response.Vacancies = append(response.Vacancies, v)
	}
	if err := rows.Err(); err != nil {
		return nil, mapDBError(err)
	}
	return response, nil
}

func (s *jobService) DeleteDraft(_ context.Context, req *jobpb.DeleteDraftRequest) (*jobpb.Vacancy, error) {
	if err := requireActor(req.ActorUserId); err != nil {
		return nil, err
	}
	if !validUUID(req.VacancyId) {
		return nil, invalid("vacancy_id must be a UUID")
	}
	var currentAuthor, currentStatus string
	err := s.db.QueryRow(`SELECT author_user_id, status FROM vacancies WHERE id = $1`, req.VacancyId).
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
		return nil, precondition("published vacancy cannot be deleted; close it first")
	}
	if currentStatus == "closed" {
		return nil, precondition("closed vacancy is history and cannot be deleted")
	}
	row := s.db.QueryRow(`DELETE FROM vacancies
		WHERE id = $1 AND author_user_id = $2 AND status = 'draft'
		RETURNING `+vacancyColumns, req.VacancyId, req.ActorUserId)
	v, err := scanVacancy(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, precondition("vacancy changed concurrently; reload and retry")
	}
	if err != nil {
		return nil, mapDBError(err)
	}
	return v, nil
}

func requireActor(actor string) error {
	if actor == "" {
		return invalid("actor_user_id is required")
	}
	if !validUUID(actor) {
		return invalid("actor_user_id must be a UUID")
	}
	return nil
}

func validUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !isHexDigit(c) {
				return false
			}
		}
	}
	return true
}

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func encodeCursor(createdAt time.Time, id string) string {
	value := createdAt.UTC().Format(time.RFC3339Nano) + "|" + id
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func decodeCursor(token string) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return time.Time{}, "", err
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return time.Time{}, "", errors.New("malformed cursor")
	}
	at, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, "", err
	}
	if !validUUID(parts[1]) {
		return time.Time{}, "", errors.New("malformed cursor id")
	}
	return at, parts[1], nil
}

func invalid(format string, args ...any) error {
	return status.Errorf(codes.InvalidArgument, format, args...)
}

func precondition(format string, args ...any) error {
	return status.Errorf(codes.FailedPrecondition, format, args...)
}

func notFound() error {
	return status.Error(codes.NotFound, "vacancy not found")
}

// mapDBError turns storage failures into gRPC codes; messages stay generic so
// no database detail reaches the edge, which logs only the code.
func mapDBError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return status.Error(codes.DeadlineExceeded, "job database timed out")
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "57014" {
		return status.Error(codes.DeadlineExceeded, "job database timed out")
	}
	msg := err.Error()
	if strings.Contains(msg, "failed to connect") || strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "connection reset") || strings.Contains(msg, "no such host") {
		slog.Warn("job database unavailable", "error", err)
		return status.Error(codes.Unavailable, "job database unavailable")
	}
	slog.Error("job database error", "error", err)
	return status.Error(codes.Internal, "job database error")
}
