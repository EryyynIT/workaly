package main

// The list is applied by catalogkit.Migrate on every start; statements are
// idempotent and never edited retroactively — schema changes append.
var migrations = []string{
	`CREATE EXTENSION IF NOT EXISTS pgcrypto`,
	`CREATE TABLE IF NOT EXISTS orders (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		author_user_id UUID NOT NULL,
		title TEXT NOT NULL CONSTRAINT orders_title_len CHECK (char_length(title) BETWEEN 1 AND 120),
		description TEXT NOT NULL CONSTRAINT orders_description_len CHECK (char_length(description) BETWEEN 1 AND 5000),
		budget_type TEXT NOT NULL CONSTRAINT orders_budget_type CHECK (budget_type IN ('fixed', 'hourly')),
		budget_amount NUMERIC(12, 2) NOT NULL CONSTRAINT orders_budget_positive CHECK (budget_amount > 0),
		currency CHAR(3) NOT NULL CONSTRAINT orders_currency_format CHECK (currency ~ '^[A-Z]{3}$'),
		deadline DATE,
		status TEXT NOT NULL DEFAULT 'draft' CONSTRAINT orders_status CHECK (status IN ('draft', 'published', 'closed')),
		version INTEGER NOT NULL DEFAULT 1 CONSTRAINT orders_version_positive CHECK (version > 0),
		created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
		published_at TIMESTAMP WITH TIME ZONE,
		closed_at TIMESTAMP WITH TIME ZONE
	)`,
	`CREATE INDEX IF NOT EXISTS orders_public ON orders (published_at DESC, id DESC) WHERE status = 'published'`,
	`CREATE INDEX IF NOT EXISTS orders_author ON orders (author_user_id, created_at DESC)`,
	`CREATE TABLE IF NOT EXISTS order_proposals (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		order_id UUID NOT NULL REFERENCES orders (id) ON DELETE CASCADE,
		executor_user_id UUID NOT NULL,
		message TEXT NOT NULL CONSTRAINT order_proposals_message_len CHECK (char_length(message) BETWEEN 1 AND 4000),
		proposed_amount NUMERIC(12, 2) NOT NULL CONSTRAINT order_proposals_amount_positive CHECK (proposed_amount > 0),
		currency CHAR(3) NOT NULL CONSTRAINT order_proposals_currency_format CHECK (currency ~ '^[A-Z]{3}$'),
		proposed_deadline DATE,
		status TEXT NOT NULL DEFAULT 'active' CONSTRAINT order_proposals_status CHECK (status IN ('active', 'withdrawn')),
		created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
		withdrawn_at TIMESTAMP WITH TIME ZONE,
		CONSTRAINT order_proposals_state CHECK (
			(status = 'active' AND withdrawn_at IS NULL)
			OR (status = 'withdrawn' AND withdrawn_at IS NOT NULL)
		)
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS order_proposals_one_active ON order_proposals (order_id, executor_user_id) WHERE status = 'active'`,
	`CREATE INDEX IF NOT EXISTS order_proposals_order ON order_proposals (order_id, created_at DESC)`,
	`CREATE INDEX IF NOT EXISTS order_proposals_executor ON order_proposals (executor_user_id, created_at DESC)`,
}
