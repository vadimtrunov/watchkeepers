#!/usr/bin/env bash
# Watchkeeper schema smoke assertions (M2.1.a AC5).
#
# Runs three psql scenarios against an already-migrated database to assert:
#   (a) Happy path: insert chain organization -> human -> manifest ->
#       manifest_version -> watchkeeper -> watch_order commits and each
#       table reports exactly one row.
#   (b) Unique violation: a second manifest_version with the same
#       (manifest_id, version_no) is rejected by the UNIQUE constraint.
#   (c) FK violation: a watch_order referencing a random non-existent
#       watchkeeper_id UUID is rejected by the foreign key.
#
# Required env: WATCHKEEPER_DB_URL (postgres://user:pass@host:port/db?sslmode=...)
# Required tools on PATH: psql (from postgresql-client matching the server).
#
# Exit status:
#   0 - every assertion passed.
#   1 - an assertion failed; message on stderr.
#   2 - preconditions unmet (env var or tool missing).
#
# Used by `make migrate-test` and the Migrate CI job.

set -euo pipefail

if [[ -z "${WATCHKEEPER_DB_URL:-}" ]]; then
  echo "ERROR: WATCHKEEPER_DB_URL is not set" >&2
  exit 2
fi

if ! command -v psql >/dev/null 2>&1; then
  echo "ERROR: psql not found on PATH (install postgresql-client matching server major version)" >&2
  exit 2
fi

# psql flags: -v ON_ERROR_STOP=1 turns any SQL error into a non-zero exit so
# set -e catches it; -q suppresses the command echo; -X ignores ~/.psqlrc so
# developer overrides cannot taint assertions.
PSQL=(psql -v ON_ERROR_STOP=1 -X -q "${WATCHKEEPER_DB_URL}")

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

cleanup_sql=$(cat <<'SQL'
-- Leave the schema empty so re-runs start from the same baseline. Order
-- mirrors the reverse-dependency FK chain.
TRUNCATE TABLE
  watchkeeper.watch_order,
  watchkeeper.watchkeeper,
  watchkeeper.manifest_version,
  watchkeeper.manifest,
  watchkeeper.human,
  watchkeeper.organization
RESTART IDENTITY CASCADE;
SQL
)

echo ">> migrate-schema-test: truncating watchkeeper tables"
"${PSQL[@]}" -c "${cleanup_sql}" >/dev/null

echo ">> migrate-schema-test: (a) happy-path insert chain"
"${PSQL[@]}" >/dev/null <<'SQL'
BEGIN;

WITH
  org AS (
    INSERT INTO watchkeeper.organization (display_name)
    VALUES ('Acme Watchers')
    RETURNING id
  ),
  hum AS (
    INSERT INTO watchkeeper.human (organization_id, display_name, email)
    SELECT id, 'Alice Lead', 'alice@example.com' FROM org
    RETURNING id
  ),
  man AS (
    INSERT INTO watchkeeper.manifest (display_name, created_by_human_id)
    SELECT 'Alpha Manifest', id FROM hum
    RETURNING id
  ),
  mv AS (
    INSERT INTO watchkeeper.manifest_version (
      manifest_id, version_no, system_prompt, personality, language
    )
    SELECT id, 1, 'you are a watchkeeper', 'calm', 'en' FROM man
    RETURNING id, manifest_id
  ),
  wk AS (
    INSERT INTO watchkeeper.watchkeeper (
      manifest_id, lead_human_id, active_manifest_version_id, status, spawned_at
    )
    SELECT mv.manifest_id, hum.id, mv.id, 'active', now() FROM mv, hum
    RETURNING id, lead_human_id
  )
INSERT INTO watchkeeper.watch_order (watchkeeper_id, lead_human_id, content)
SELECT wk.id, wk.lead_human_id, 'observe the perimeter' FROM wk;

COMMIT;
SQL

happy_counts=$("${PSQL[@]}" -tA <<'SQL'
SELECT
  (SELECT count(*) FROM watchkeeper.organization) || ',' ||
  (SELECT count(*) FROM watchkeeper.human) || ',' ||
  (SELECT count(*) FROM watchkeeper.manifest) || ',' ||
  (SELECT count(*) FROM watchkeeper.manifest_version) || ',' ||
  (SELECT count(*) FROM watchkeeper.watchkeeper) || ',' ||
  (SELECT count(*) FROM watchkeeper.watch_order);
SQL
)

if [[ "${happy_counts}" != "1,1,1,1,1,1" ]]; then
  fail "happy-path row counts expected '1,1,1,1,1,1', got '${happy_counts}'"
fi
echo "OK: happy-path row counts = ${happy_counts}"

echo ">> migrate-schema-test: (d) nullable active_manifest_version_id accepted (pending state)"
"${PSQL[@]}" >/dev/null <<'SQL'
BEGIN;

-- Insert a second watchkeeper using the existing manifest and human rows,
-- leaving active_manifest_version_id as NULL to represent pre-approval state.
INSERT INTO watchkeeper.watchkeeper (
  manifest_id, lead_human_id, status
)
SELECT
  (SELECT id FROM watchkeeper.manifest LIMIT 1),
  (SELECT id FROM watchkeeper.human LIMIT 1),
  'pending';

COMMIT;
SQL

null_count=$("${PSQL[@]}" -tA -c "SELECT count(*) FROM watchkeeper.watchkeeper WHERE active_manifest_version_id IS NULL;")
if [[ "${null_count}" != "1" ]]; then
  fail "nullable active_manifest_version_id count expected 1, got ${null_count}"
fi
echo "OK: watchkeeper with active_manifest_version_id = NULL accepted (count = ${null_count})"

echo ">> migrate-schema-test: (b) duplicate (manifest_id, version_no) rejected"
dup_output=$("${PSQL[@]}" <<'SQL' 2>&1 || true
BEGIN;

SAVEPOINT before_dup;

-- Same (manifest_id, version_no) pair as the happy-path row.
INSERT INTO watchkeeper.manifest_version (
  manifest_id, version_no, system_prompt
)
SELECT id, 1, 'duplicate attempt' FROM watchkeeper.manifest LIMIT 1;

-- Should not reach this line because the INSERT above raises an error
-- which, combined with ON_ERROR_STOP=1, aborts the psql session.
ROLLBACK TO SAVEPOINT before_dup;
ROLLBACK;
SQL
)

if ! printf '%s' "${dup_output}" | grep -qi 'duplicate key value'; then
  fail "expected a unique-violation error from duplicate (manifest_id, version_no); got: ${dup_output}"
fi
echo "OK: duplicate (manifest_id, version_no) rejected by unique constraint"

# Verify the happy-path row is still the only manifest_version row.
mv_count=$("${PSQL[@]}" -tA -c "SELECT count(*) FROM watchkeeper.manifest_version;")
if [[ "${mv_count}" != "1" ]]; then
  fail "manifest_version count after duplicate attempt expected 1, got ${mv_count}"
fi

echo ">> migrate-schema-test: (c) FK violation on non-existent watchkeeper_id"
fk_output=$("${PSQL[@]}" <<'SQL' 2>&1 || true
BEGIN;

SAVEPOINT before_fk;

INSERT INTO watchkeeper.watch_order (watchkeeper_id, lead_human_id, content)
SELECT
  gen_random_uuid(),
  (SELECT id FROM watchkeeper.human LIMIT 1),
  'order to nowhere';

ROLLBACK TO SAVEPOINT before_fk;
ROLLBACK;
SQL
)

if ! printf '%s' "${fk_output}" | grep -qi 'violates foreign key constraint'; then
  fail "expected a foreign-key-violation error for non-existent watchkeeper_id; got: ${fk_output}"
fi
echo "OK: watch_order with non-existent watchkeeper_id rejected by FK"

echo "ALL schema assertions passed"
