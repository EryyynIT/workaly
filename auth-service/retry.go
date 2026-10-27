package main

import (
	"context"
	"log/slog"
	"time"

	pb "github.com/EryyynIT/workaly/auth-service/pb/userpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	retryInterval    = 30 * time.Second
	retryMaxAttempts = 20
)

// startProfileRetryWorker periodically retries pending profile deliveries.
// It runs until the context is cancelled. This is a local outbox: when Kafka
// arrives in S4, the same rows become events instead of direct RPC retries.
func (app *application) startProfileRetryWorker(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(retryInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				app.processPendingProfiles(ctx)
			}
		}
	}()
}

// processPendingProfiles takes up to 50 oldest pending rows and retries each
// delivery inside one transaction, so concurrent auth-service replicas skip
// locked rows (FOR UPDATE SKIP LOCKED) instead of doing the same work twice.
// Rows that exceeded retryMaxAttempts are not selected again.
func (app *application) processPendingProfiles(ctx context.Context) {
	if app.db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	tx, err := app.db.BeginTx(ctx, nil)
	if err != nil {
		slog.Warn("profile retry begin failed", "error", err)
		return
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx,
		`SELECT account_id, name, username, attempts FROM pending_profiles
		 WHERE attempts < $1 ORDER BY created_at LIMIT 50 FOR UPDATE SKIP LOCKED`,
		retryMaxAttempts)
	if err != nil {
		slog.Warn("profile retry query failed", "error", err)
		return
	}

	type pending struct {
		accountID, name, username string
		attempts                  int
	}
	var items []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.accountID, &p.name, &p.username, &p.attempts); err != nil {
			slog.Warn("profile retry scan failed", "error", err)
			rows.Close()
			return
		}
		items = append(items, p)
	}
	if err := rows.Err(); err != nil {
		slog.Warn("profile retry rows failed", "error", err)
		rows.Close()
		return
	}
	rows.Close()

	for _, p := range items {
		_, err := app.users.CreateProfile(ctx, &pb.CreateProfileRequest{
			UserId: p.accountID, Name: p.name, Username: p.username,
		})
		attempts := p.attempts + 1
		if err == nil {
			if _, err := tx.ExecContext(ctx,
				"DELETE FROM pending_profiles WHERE account_id = $1", p.accountID); err != nil {
				slog.Warn("pending profile delete failed", "account_id", p.accountID, "error", err)
			}
			app.audit(ctx, p.accountID, "profile_delivered", nil)
			continue
		}
		// Store only the gRPC code: upstream error text can carry internal
		// details (addresses, SQL) that must not land in the database.
		lastErr := status.Code(err).String()
		// AlreadyExists from user-service means the profile is already there
		// with different data — a real conflict, not a retryable error.
		if status.Code(err) == codes.AlreadyExists {
			lastErr = "username conflict"
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE pending_profiles SET attempts = $1, last_error = $2, last_sent_at = CURRENT_TIMESTAMP WHERE account_id = $3",
			attempts, lastErr, p.accountID); err != nil {
			slog.Warn("pending profile update failed", "account_id", p.accountID, "error", err)
		}
		if attempts == retryMaxAttempts {
			slog.Error("profile delivery abandoned", "account_id", p.accountID, "attempts", attempts)
		}
	}
	if err := tx.Commit(); err != nil {
		slog.Warn("profile retry commit failed", "error", err)
	}
}
