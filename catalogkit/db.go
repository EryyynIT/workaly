package catalogkit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// OpenDB opens the service database with the pool limits shared by every
// catalog service and verifies connectivity.
func OpenDB(ctx context.Context, dsn string) (*sql.DB, error) {
	if dsn == "" {
		return nil, errors.New("DATABASE_URL is required")
	}
	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("invalid DATABASE_URL: %w", err)
	}
	conn.SetMaxOpenConns(10)
	conn.SetMaxIdleConns(2)
	conn.SetConnMaxLifetime(5 * time.Minute)

	if err := conn.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("database ping: %w", err)
	}
	return conn, nil
}

// Migrate applies idempotent statements in order. The statement list is owned
// by the calling service and never edited retroactively: a schema change
// appends a new statement, so restarts converge without a migration tool.
func Migrate(ctx context.Context, db *sql.DB, statements []string) error {
	for _, query := range statements {
		if _, err := db.ExecContext(ctx, query); err != nil {
			return fmt.Errorf("migration: %w", err)
		}
	}
	return nil
}
