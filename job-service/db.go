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

func migrate(ctx context.Context, conn *sql.DB) error {
	migrations := []string{
		`CREATE EXTENSION IF NOT EXISTS pgcrypto`,
		`CREATE TABLE IF NOT EXISTS vacancies (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			author_user_id UUID NOT NULL,
			title TEXT NOT NULL CONSTRAINT vacancies_title_len CHECK (char_length(title) BETWEEN 1 AND 120),
			description TEXT NOT NULL CONSTRAINT vacancies_description_len CHECK (char_length(description) BETWEEN 1 AND 5000),
			employment_type TEXT NOT NULL CONSTRAINT vacancies_employment_type CHECK (employment_type IN ('full_time', 'part_time', 'contract', 'internship')),
			work_format TEXT NOT NULL CONSTRAINT vacancies_work_format CHECK (work_format IN ('onsite', 'remote', 'hybrid')),
			location TEXT CONSTRAINT vacancies_location_len CHECK (location IS NULL OR char_length(location) <= 120),
			salary_min NUMERIC(12, 2),
			salary_max NUMERIC(12, 2),
			currency CHAR(3) CONSTRAINT vacancies_currency_format CHECK (currency IS NULL OR currency ~ '^[A-Z]{3}$'),
			status TEXT NOT NULL DEFAULT 'draft' CONSTRAINT vacancies_status CHECK (status IN ('draft', 'published', 'closed')),
			version INTEGER NOT NULL DEFAULT 1 CONSTRAINT vacancies_version_positive CHECK (version > 0),
			created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
			published_at TIMESTAMP WITH TIME ZONE,
			closed_at TIMESTAMP WITH TIME ZONE,
			CONSTRAINT vacancies_salary_order CHECK (NOT (salary_min IS NOT NULL AND salary_max IS NOT NULL AND salary_min > salary_max)),
			CONSTRAINT vacancies_salary_currency CHECK ((salary_min IS NULL AND salary_max IS NULL) OR currency IS NOT NULL)
		)`,
		`CREATE INDEX IF NOT EXISTS vacancies_public ON vacancies (published_at DESC, id DESC) WHERE status = 'published'`,
		`CREATE INDEX IF NOT EXISTS vacancies_author ON vacancies (author_user_id, created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS vacancy_responses (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			vacancy_id UUID NOT NULL REFERENCES vacancies (id) ON DELETE CASCADE,
			candidate_user_id UUID NOT NULL,
			cover_letter TEXT NOT NULL CONSTRAINT vacancy_responses_letter_len CHECK (char_length(cover_letter) BETWEEN 1 AND 4000),
			contact TEXT CONSTRAINT vacancy_responses_contact_len CHECK (contact IS NULL OR char_length(contact) BETWEEN 3 AND 200),
			status TEXT NOT NULL DEFAULT 'active' CONSTRAINT vacancy_responses_status CHECK (status IN ('active', 'withdrawn')),
			created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
			withdrawn_at TIMESTAMP WITH TIME ZONE,
			CONSTRAINT vacancy_responses_state CHECK (
				(status = 'active' AND withdrawn_at IS NULL)
				OR (status = 'withdrawn' AND withdrawn_at IS NOT NULL)
			)
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS vacancy_responses_one_active ON vacancy_responses (vacancy_id, candidate_user_id) WHERE status = 'active'`,
		`CREATE INDEX IF NOT EXISTS vacancy_responses_vacancy ON vacancy_responses (vacancy_id, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS vacancy_responses_candidate ON vacancy_responses (candidate_user_id, created_at DESC)`,
	}
	for _, query := range migrations {
		if _, err := conn.ExecContext(ctx, query); err != nil {
			return fmt.Errorf("migration: %w", err)
		}
	}
	return nil
}
