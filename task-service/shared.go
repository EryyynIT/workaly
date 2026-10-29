package main

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Shared plumbing of the order catalog: field formats mirroring the named
// CHECK constraints, UUID and cursor helpers, uniform gRPC errors and the
// mapping of storage failures. The database stays the last backstop; the
// service reports precise InvalidArgument before hitting it.
var (
	moneyPattern = regexp.MustCompile(`^[0-9]{1,10}(\.[0-9]{1,2})?$`)
	currencyPatt = regexp.MustCompile(`^[A-Z]{3}$`)
)

type rowScanner interface{ Scan(dest ...any) error }

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

// validDate accepts an ISO calendar date ("2026-12-31") or the empty string
// for an absent optional. Dates have no time component in the order domain.
func validDate(value string) bool {
	if value == "" {
		return true
	}
	parsed, err := time.Parse("2006-01-02", value)
	return err == nil && parsed.Format("2006-01-02") == value
}

func invalid(format string, args ...any) error {
	return status.Errorf(codes.InvalidArgument, format, args...)
}

func precondition(format string, args ...any) error {
	return status.Errorf(codes.FailedPrecondition, format, args...)
}

func notFound() error {
	return status.Error(codes.NotFound, "order not found")
}

// isDuplicateKey reports a unique-violation of the named constraint.
func isDuplicateKey(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}

// mapDBError turns storage failures into gRPC codes; messages stay generic so
// no database detail reaches the edge, which logs only the code.
func mapDBError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return status.Error(codes.DeadlineExceeded, "task database timed out")
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "57014" {
		return status.Error(codes.DeadlineExceeded, "task database timed out")
	}
	msg := err.Error()
	if strings.Contains(msg, "failed to connect") || strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "connection reset") || strings.Contains(msg, "no such host") {
		slog.Warn("task database unavailable", "error", err)
		return status.Error(codes.Unavailable, "task database unavailable")
	}
	slog.Error("task database error", "error", err)
	return status.Error(codes.Internal, "task database error")
}
