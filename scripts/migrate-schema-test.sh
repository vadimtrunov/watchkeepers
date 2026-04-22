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
-- mirrors the reverse-dependency FK chain (newest-leaf tables first).
-- `knowledge_chunk` is standalone (no FKs to the core chain) so it sits at
-- the top without affecting ordering. The append-only trigger on
-- `keepers_log` fires on DELETE but not TRUNCATE, so the TRUNCATE chain
-- continues to clear it cleanly.
TRUNCATE TABLE
  watchkeeper.knowledge_chunk,
  watchkeeper.keepers_log,
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

echo ">> migrate-schema-test: (d) keepers_log happy-path insert (system-emitted event)"
"${PSQL[@]}" >/dev/null <<'SQL'
BEGIN;

-- Both actor_* columns left NULL to represent a system-emitted event
-- (AC1 nullability + edge case from the TASK test plan).
INSERT INTO watchkeeper.keepers_log (event_type, payload)
VALUES ('watchkeeper_spawned', '{"agent":"x"}'::jsonb);

COMMIT;
SQL

kl_count=$("${PSQL[@]}" -tA -c "SELECT count(*) FROM watchkeeper.keepers_log;")
if [[ "${kl_count}" != "1" ]]; then
  fail "keepers_log happy-path insert count expected 1, got ${kl_count}"
fi
echo "OK: keepers_log happy-path insert accepted (count = ${kl_count})"

echo ">> migrate-schema-test: (d-update) UPDATE on keepers_log rejected by append-only trigger"
kl_update_output=$("${PSQL[@]}" <<'SQL' 2>&1 || true
BEGIN;

SAVEPOINT before_kl_update;

UPDATE watchkeeper.keepers_log SET event_type = 'x';

ROLLBACK TO SAVEPOINT before_kl_update;
ROLLBACK;
SQL
)

if ! printf '%s' "${kl_update_output}" | grep -q 'append-only'; then
  fail "expected UPDATE on keepers_log to be rejected with 'append-only'; got: ${kl_update_output}"
fi
kl_count_after_update=$("${PSQL[@]}" -tA -c "SELECT count(*) FROM watchkeeper.keepers_log;")
if [[ "${kl_count_after_update}" != "1" ]]; then
  fail "keepers_log row count after UPDATE attempt expected 1, got ${kl_count_after_update}"
fi
echo "OK: keepers_log UPDATE rejected (append-only) and row count unchanged"

echo ">> migrate-schema-test: (d-delete) DELETE on keepers_log rejected by append-only trigger"
kl_delete_output=$("${PSQL[@]}" <<'SQL' 2>&1 || true
BEGIN;

SAVEPOINT before_kl_delete;

DELETE FROM watchkeeper.keepers_log;

ROLLBACK TO SAVEPOINT before_kl_delete;
ROLLBACK;
SQL
)

if ! printf '%s' "${kl_delete_output}" | grep -q 'append-only'; then
  fail "expected DELETE on keepers_log to be rejected with 'append-only'; got: ${kl_delete_output}"
fi
kl_count_after_delete=$("${PSQL[@]}" -tA -c "SELECT count(*) FROM watchkeeper.keepers_log;")
if [[ "${kl_count_after_delete}" != "1" ]]; then
  fail "keepers_log row count after DELETE attempt expected 1, got ${kl_count_after_delete}"
fi
echo "OK: keepers_log DELETE rejected (append-only) and row count unchanged"

echo ">> migrate-schema-test: (e) knowledge_chunk HNSW plan"
# Seed 100 random-vector rows server-side via generate_series so we don't have
# to embed a 1536-element literal. `vector(1536)` accepts a text literal of the
# form `[a,b,...,c]`; we build one from 1536 random doubles per row.
"${PSQL[@]}" >/dev/null <<'SQL'
BEGIN;

INSERT INTO watchkeeper.knowledge_chunk (scope, content, embedding)
SELECT
  'org',
  'seed row ' || gs,
  (
    '[' || string_agg(random()::text, ',') || ']'
  )::vector
FROM generate_series(1, 100) AS gs,
  LATERAL generate_series(1, 1536) AS dim
GROUP BY gs;

COMMIT;

ANALYZE watchkeeper.knowledge_chunk;
SQL

# Build a 1536-element query vector as a string (1536 zeros joined by commas)
# and EXPLAIN the KNN query. The HNSW index is built with `vector_cosine_ops`,
# so the ORDER BY must use the cosine-distance operator `<=>` to match the
# operator class; `<->` (L2) or `<#>` (inner product) will not hit this index.
# SET LOCAL enable_seqscan = off hardens the assertion against planner cost
# flips on tiny datasets.
query_vec=$(python3 -c "print('[' + ','.join(['0']*1536) + ']')")
plan_output=$("${PSQL[@]}" <<SQL
BEGIN;
SET LOCAL enable_seqscan = off;
EXPLAIN (FORMAT TEXT) SELECT id FROM watchkeeper.knowledge_chunk
ORDER BY embedding <=> '${query_vec}'::vector LIMIT 5;
ROLLBACK;
SQL
)

if ! printf '%s' "${plan_output}" | grep -q 'knowledge_chunk_embedding_hnsw_idx'; then
  fail "expected EXPLAIN plan to reference knowledge_chunk_embedding_hnsw_idx; got: ${plan_output}"
fi
echo "OK: HNSW index chosen for KNN plan (knowledge_chunk_embedding_hnsw_idx)"

# ---------------------------------------------------------------------------
# M2.1.d — RLS assertions
#
# The `(e)` block above left 100 `scope='org'` rows in `knowledge_chunk`. We
# add two more rows under distinct `agent:<uuid>` scopes so the visibility
# tests below have something to discriminate on. Each test opens its own psql
# transaction because `SET LOCAL` is scoped to a transaction.
# ---------------------------------------------------------------------------
"${PSQL[@]}" >/dev/null <<'SQL'
BEGIN;
INSERT INTO watchkeeper.knowledge_chunk (scope, content, embedding) VALUES
  (
    'agent:aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa',
    'agent-A row',
    ('[' || repeat('0,', 1535) || '0]')::vector
  ),
  (
    'agent:bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb',
    'agent-B row',
    ('[' || repeat('0,', 1535) || '0]')::vector
  );
COMMIT;
SQL

echo ">> migrate-schema-test: (f) RLS cross-scope SELECT invisibility"
rls_select_counts=$("${PSQL[@]}" -tA <<'SQL'
BEGIN;
SET ROLE wk_agent_role;
SET LOCAL watchkeeper.scope = 'agent:aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa';
SELECT
  (SELECT count(*) FROM watchkeeper.knowledge_chunk) || ',' ||
  (SELECT count(*) FROM watchkeeper.knowledge_chunk WHERE scope LIKE 'agent:bbbb%');
RESET ROLE;
ROLLBACK;
SQL
)

# Expect: visible = 100 'org' rows + 1 matching agent:aaaa row = 101; bbbb = 0.
if [[ "${rls_select_counts}" != "101,0" ]]; then
  fail "RLS cross-scope SELECT expected '101,0' (visible, bbbb-visible); got '${rls_select_counts}'"
fi
echo "OK: RLS hides out-of-scope rows (visible=101, bbbb-visible=0)"

echo ">> migrate-schema-test: (g) RLS WITH CHECK on INSERT"
rls_insert_output=$("${PSQL[@]}" <<'SQL' 2>&1 || true
BEGIN;
SET ROLE wk_agent_role;
SET LOCAL watchkeeper.scope = 'agent:aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa';
INSERT INTO watchkeeper.knowledge_chunk (scope, content, embedding)
VALUES (
  'agent:bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb',
  'cross-scope leak attempt',
  ('[' || repeat('0,', 1535) || '0]')::vector
);
RESET ROLE;
ROLLBACK;
SQL
)

# Test accepts either the stable phrase `row-level security` or the SQLSTATE
# `42501` (insufficient_privilege) — Postgres raises the former on its own
# policy-violation path. A CHECK-constraint violation (23514) would also pass
# since both the policy WITH CHECK and the column CHECK reject the row.
if ! printf '%s' "${rls_insert_output}" | grep -Eq 'row-level security|42501|23514'; then
  fail "expected RLS INSERT to be rejected with policy/privilege error; got: ${rls_insert_output}"
fi
echo "OK: RLS INSERT rejected by WITH CHECK"

echo ">> migrate-schema-test: (h) RLS empty session setting"
# No `SET LOCAL watchkeeper.scope` — current_setting(…, true) returns empty
# string, so `scope = current_setting(…, true)` is false and only the
# `scope = 'org'` branch of USING holds. Expect 100 rows visible.
rls_empty_count=$("${PSQL[@]}" -tA <<'SQL'
BEGIN;
SET ROLE wk_agent_role;
SELECT count(*) FROM watchkeeper.knowledge_chunk;
RESET ROLE;
ROLLBACK;
SQL
)

if [[ "${rls_empty_count}" != "100" ]]; then
  fail "RLS empty-scope visibility expected 100 (org rows only); got '${rls_empty_count}'"
fi
echo "OK: RLS with unset scope sees only scope='org' rows (count=100)"

echo "ALL schema assertions passed"
