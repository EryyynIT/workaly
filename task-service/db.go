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
	// S3.2 deal pipeline (docs/s3-hiring.md, decisions 2-3). Appended only;
	// existing orders and proposals stay valid under the widened rules. The
	// status constraint is dropped and re-added (and re-fixed when an early
	// iteration of this migration missed 'submitted').
	`ALTER TABLE orders DROP CONSTRAINT IF EXISTS orders_status`,
	`ALTER TABLE orders ADD CONSTRAINT orders_status CHECK (status IN ('draft', 'published', 'closed', 'in_progress', 'submitted', 'completed', 'cancelled'))`,
	`ALTER TABLE order_proposals ADD COLUMN IF NOT EXISTS withdrawn_reason TEXT`,
	`ALTER TABLE order_proposals DROP CONSTRAINT IF EXISTS order_proposals_withdrawn_reason_len`,
	`ALTER TABLE order_proposals ADD CONSTRAINT order_proposals_withdrawn_reason_len CHECK (withdrawn_reason IS NULL OR char_length(withdrawn_reason) <= 1000)`,
	`CREATE TABLE IF NOT EXISTS deals (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		order_id UUID NOT NULL REFERENCES orders (id) ON DELETE CASCADE,
		customer_user_id UUID NOT NULL,
		executor_user_id UUID NOT NULL,
		proposal_id UUID NOT NULL REFERENCES order_proposals (id),
		status TEXT NOT NULL CONSTRAINT deals_status CHECK (status IN ('terms_pending', 'in_progress', 'submitted', 'revision_requested', 'completed', 'cancelled')),
		amount NUMERIC(12, 2) NOT NULL CONSTRAINT deals_amount_positive CHECK (amount > 0),
		currency CHAR(3) NOT NULL CONSTRAINT deals_currency_format CHECK (currency ~ '^[A-Z]{3}$'),
		deadline DATE,
		revisions_included INTEGER NOT NULL DEFAULT 0 CONSTRAINT deals_revisions_nonnegative CHECK (revisions_included >= 0),
		acceptance_criteria TEXT CONSTRAINT deals_criteria_len CHECK (acceptance_criteria IS NULL OR char_length(acceptance_criteria) <= 4000),
		reason TEXT CONSTRAINT deals_reason_len CHECK (reason IS NULL OR char_length(reason) <= 1000),
		version INTEGER NOT NULL DEFAULT 1 CONSTRAINT deals_version_positive CHECK (version > 0),
		created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
		started_at TIMESTAMP WITH TIME ZONE,
		submitted_at TIMESTAMP WITH TIME ZONE,
		decided_at TIMESTAMP WITH TIME ZONE
	)`,
	// One deal per order, and only while the selection is not settled:
	// the unique index makes a concurrent second selection a unique
	// violation reported as AlreadyExists.
	`CREATE UNIQUE INDEX IF NOT EXISTS deals_one_per_order ON deals (order_id)`,
	`CREATE INDEX IF NOT EXISTS deals_executor ON deals (executor_user_id, created_at DESC)`,
	`CREATE TABLE IF NOT EXISTS deal_submissions (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		deal_id UUID NOT NULL REFERENCES deals (id) ON DELETE CASCADE,
		iteration INTEGER NOT NULL CONSTRAINT deal_submissions_iteration_positive CHECK (iteration > 0),
		result TEXT NOT NULL CONSTRAINT deal_submissions_result_len CHECK (char_length(result) BETWEEN 1 AND 10000),
		revision_reason TEXT CONSTRAINT deal_submissions_revision_len CHECK (revision_reason IS NULL OR char_length(revision_reason) <= 1000),
		created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`,
	`ALTER TABLE deal_submissions ADD COLUMN IF NOT EXISTS idempotency_key TEXT`,
	`CREATE UNIQUE INDEX IF NOT EXISTS deal_submissions_idempotency ON deal_submissions (deal_id, idempotency_key) WHERE idempotency_key IS NOT NULL`,
	`CREATE UNIQUE INDEX IF NOT EXISTS deal_submissions_iteration ON deal_submissions (deal_id, iteration)`,
}
