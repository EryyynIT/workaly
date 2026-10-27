package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func openDB(ctx context.Context, dsn string) (*sql.DB, error) {
	if dsn == "" {
		return nil, errors.New("AUTH_DATABASE_URL is required")
	}
	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("invalid AUTH_DATABASE_URL: %w", err)
	}
	conn.SetMaxOpenConns(10)
	conn.SetMaxIdleConns(2)
	conn.SetConnMaxLifetime(5 * time.Minute)

	if err := conn.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("database ping: %w", err)
	}
	return conn, nil
}

func migrate(ctx context.Context, conn *sql.DB) error {
	migrations := []string{
		`CREATE EXTENSION IF NOT EXISTS pgcrypto`,
		`CREATE TABLE IF NOT EXISTS accounts (
			id UUID PRIMARY KEY,
			email TEXT NOT NULL,
			password_hash TEXT NOT NULL,
			email_verified_at TIMESTAMP WITH TIME ZONE,
			created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS accounts_email_unique ON accounts (lower(email))`,
		`CREATE TABLE IF NOT EXISTS sessions (
			id UUID PRIMARY KEY,
			account_id UUID NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
			refresh_token_hash TEXT NOT NULL UNIQUE,
			previous_token_hash TEXT,
			created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
			expires_at TIMESTAMP WITH TIME ZONE NOT NULL,
			revoked_at TIMESTAMP WITH TIME ZONE,
			replaced_by UUID REFERENCES sessions (id)
		)`,
		`CREATE INDEX IF NOT EXISTS sessions_account_active ON sessions (account_id) WHERE revoked_at IS NULL`,
		`CREATE TABLE IF NOT EXISTS pending_profiles (
			account_id UUID PRIMARY KEY REFERENCES accounts (id) ON DELETE CASCADE,
			name TEXT NOT NULL,
			username TEXT NOT NULL,
			attempts INT NOT NULL DEFAULT 0,
			last_error TEXT,
			created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`ALTER TABLE pending_profiles ADD COLUMN IF NOT EXISTS last_sent_at TIMESTAMP WITH TIME ZONE`,
		`CREATE TABLE IF NOT EXISTS action_tokens (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			account_id UUID NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
			kind TEXT NOT NULL CHECK (kind IN ('verify_email', 'password_reset')),
			token_hash TEXT NOT NULL UNIQUE,
			expires_at TIMESTAMP WITH TIME ZONE NOT NULL,
			used_at TIMESTAMP WITH TIME ZONE,
			created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS action_tokens_account_kind ON action_tokens (account_id, kind)`,
		`CREATE TABLE IF NOT EXISTS audit_log (
			id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			account_id UUID REFERENCES accounts (id) ON DELETE SET NULL,
			action TEXT NOT NULL,
			detail JSONB NOT NULL DEFAULT '{}',
			created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
	}
	for _, query := range migrations {
		if _, err := conn.ExecContext(ctx, query); err != nil {
			return fmt.Errorf("migration: %w", err)
		}
	}
	return nil
}
