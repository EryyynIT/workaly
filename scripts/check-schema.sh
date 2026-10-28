#!/usr/bin/env bash
set -euo pipefail

# Catalog schema probes: every rule from the S2 design (docs/s2-catalogs.md)
# must be enforced by the database itself. Bad inserts must fail with the
# named constraint; withdraw-then-respond must work. Probe data is deleted
# first, so reruns on the same stack are safe.
# Uses docker compose (CI) or podman-compose (local stand). For a local
# stand started with a custom project name, pass it:
#   PODMAN_PROJECT=workaly-s21 scripts/check-schema.sh

if command -v docker >/dev/null 2>&1; then
  read -ra J <<<"docker compose exec -T job-db psql -U workaly_job -d workaly_job -tA"
  read -ra T <<<"docker compose exec -T task-db psql -U workaly_task -d workaly_task -tA"
else
  read -ra J <<<"podman-compose -p ${PODMAN_PROJECT:-workaly} exec -T job-db psql -U workaly_job -d workaly_job -tA"
  read -ra T <<<"podman-compose -p ${PODMAN_PROJECT:-workaly} exec -T task-db psql -U workaly_task -d workaly_task -tA"
fi

fail() { echo "check-schema: $*" >&2; exit 1; }

# podman-compose interpolates compose.yaml even for exec; these probes never
# touch auth, so any non-empty value satisfies the :? requirement.
export JWT_SECRET=${JWT_SECRET:-check-schema-interpolation-only}

must_succeed() { "$@" >/dev/null 2>&1 || fail "expected success: $*"; }

must_reject() {
  local want="$1"; shift
  local out
  if out=$("$@" 2>&1); then
    fail "expected rejection ($want), but the insert succeeded"
  fi
  case "$out" in
    *"$want"*) ;;
    *) fail "rejected for the wrong reason (wanted $want): $out" ;;
  esac
}

AUTHOR=00000000-0000-0000-0000-000000000001
RESPONDER=00000000-0000-0000-0000-000000000002

# Fresh probe data on every run; the vacancy delete cascades its responses.
must_succeed "${J[@]}" -c "DELETE FROM vacancy_responses WHERE candidate_user_id = '$RESPONDER'"
must_succeed "${J[@]}" -c "DELETE FROM vacancies WHERE title = '__schema_probe__'"
must_succeed "${T[@]}" -c "DELETE FROM order_proposals WHERE executor_user_id = '$RESPONDER'"
must_succeed "${T[@]}" -c "DELETE FROM orders WHERE title = '__schema_probe__'"

# job-db: vacancies.
must_succeed "${J[@]}" -c "INSERT INTO vacancies (author_user_id, title, description, employment_type, work_format) VALUES ('$AUTHOR', '__schema_probe__', 'probe', 'full_time', 'remote')"
must_reject "vacancies_title_len" "${J[@]}" -c "INSERT INTO vacancies (author_user_id, title, description, employment_type, work_format) VALUES ('$AUTHOR', repeat('x', 121), 'probe', 'full_time', 'remote')"
must_reject "vacancies_employment_type" "${J[@]}" -c "INSERT INTO vacancies (author_user_id, title, description, employment_type, work_format) VALUES ('$AUTHOR', 'probe', 'probe', 'night_shift', 'remote')"
must_reject "vacancies_salary_currency" "${J[@]}" -c "INSERT INTO vacancies (author_user_id, title, description, employment_type, work_format, salary_min) VALUES ('$AUTHOR', 'probe', 'probe', 'full_time', 'remote', 100)"
must_reject "vacancies_salary_order" "${J[@]}" -c "INSERT INTO vacancies (author_user_id, title, description, employment_type, work_format, salary_min, salary_max, currency) VALUES ('$AUTHOR', 'probe', 'probe', 'full_time', 'remote', 200, 100, 'RUB')"
must_reject "vacancies_currency_format" "${J[@]}" -c "INSERT INTO vacancies (author_user_id, title, description, employment_type, work_format, salary_min, currency) VALUES ('$AUTHOR', 'probe', 'probe', 'full_time', 'remote', 100, 'rub')"

VID=$("${J[@]}" -c "SELECT id FROM vacancies WHERE title = '__schema_probe__' LIMIT 1" | head -1)
[ -n "$VID" ] || fail "probe vacancy id missing"
must_succeed "${J[@]}" -c "INSERT INTO vacancy_responses (vacancy_id, candidate_user_id, cover_letter) VALUES ('$VID', '$RESPONDER', 'probe')"
must_reject "vacancy_responses_one_active" "${J[@]}" -c "INSERT INTO vacancy_responses (vacancy_id, candidate_user_id, cover_letter) VALUES ('$VID', '$RESPONDER', 'probe')"
must_succeed "${J[@]}" -c "UPDATE vacancy_responses SET status = 'withdrawn', withdrawn_at = now() WHERE candidate_user_id = '$RESPONDER' AND status = 'active'"
must_succeed "${J[@]}" -c "INSERT INTO vacancy_responses (vacancy_id, candidate_user_id, cover_letter) VALUES ('$VID', '$RESPONDER', 'probe after withdraw')"

# task-db: orders.
must_succeed "${T[@]}" -c "INSERT INTO orders (author_user_id, title, description, budget_type, budget_amount, currency) VALUES ('$AUTHOR', '__schema_probe__', 'probe', 'fixed', 500, 'RUB')"
must_reject "orders_title_len" "${T[@]}" -c "INSERT INTO orders (author_user_id, title, description, budget_type, budget_amount, currency) VALUES ('$AUTHOR', repeat('x', 121), 'probe', 'fixed', 100, 'RUB')"
must_reject "orders_budget_positive" "${T[@]}" -c "INSERT INTO orders (author_user_id, title, description, budget_type, budget_amount, currency) VALUES ('$AUTHOR', 'probe', 'probe', 'fixed', -5, 'RUB')"
must_reject "orders_currency_format" "${T[@]}" -c "INSERT INTO orders (author_user_id, title, description, budget_type, budget_amount, currency) VALUES ('$AUTHOR', 'probe', 'probe', 'fixed', 100, 'rub')"

OID=$("${T[@]}" -c "SELECT id FROM orders WHERE title = '__schema_probe__' LIMIT 1" | head -1)
[ -n "$OID" ] || fail "probe order id missing"
must_succeed "${T[@]}" -c "INSERT INTO order_proposals (order_id, executor_user_id, message, proposed_amount, currency) VALUES ('$OID', '$RESPONDER', 'probe', 450, 'RUB')"
must_reject "order_proposals_one_active" "${T[@]}" -c "INSERT INTO order_proposals (order_id, executor_user_id, message, proposed_amount, currency) VALUES ('$OID', '$RESPONDER', 'probe', 400, 'RUB')"
must_succeed "${T[@]}" -c "UPDATE order_proposals SET status = 'withdrawn', withdrawn_at = now() WHERE executor_user_id = '$RESPONDER' AND status = 'active'"
must_succeed "${T[@]}" -c "INSERT INTO order_proposals (order_id, executor_user_id, message, proposed_amount, currency) VALUES ('$OID', '$RESPONDER', 'probe after withdraw', 450, 'RUB')"

echo "check-schema: PASS (catalog constraints enforced)"
