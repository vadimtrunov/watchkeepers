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
-- `knowledge_chunk` and `outbox` are standalone (no FKs to the core chain)
-- so they sit at the top without affecting ordering. The append-only
-- trigger on `keepers_log` fires on DELETE but not TRUNCATE, so the
-- TRUNCATE chain continues to clear it cleanly.
TRUNCATE TABLE
  watchkeeper.k2k_messages,
  watchkeeper.k2k_conversations,
  watchkeeper.outbox,
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
    RETURNING id, organization_id
  ),
  man AS (
    INSERT INTO watchkeeper.manifest (display_name, created_by_human_id, organization_id)
    SELECT 'Alpha Manifest', id, organization_id FROM hum
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

-- `ORDER BY dim` pins a deterministic component order within each vector so
-- the seed is reproducible if we ever want to anchor a test on a specific
-- row/plan. `(random() + 0.001)::text` keeps every component strictly > 0;
-- a pure-zero component combined with a zero query vector would leave cosine
-- distance at 0/0 undefined on some planner paths (vector extension returns
-- NaN, which the HNSW operator then skips).
INSERT INTO watchkeeper.knowledge_chunk (scope, content, embedding)
SELECT
  'org',
  'seed row ' || gs,
  (
    '[' || string_agg((random() + 0.001)::text, ',' ORDER BY dim) || ']'
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
# seed three RLS-specific rows (tagged `subject LIKE 'rls-%'`) so the
# visibility tests below have something to discriminate on: one extra `'org'`
# row plus two rows under distinct `agent:<uuid>` scopes. The seeding block
# is hermetic — it deletes any prior `rls-%` rows first so re-running the
# script (without `migrate-down`) between the (e) block and here keeps the
# RLS row counts deterministic. Each test opens its own psql transaction
# because `SET LOCAL` is scoped to a transaction.
# ---------------------------------------------------------------------------
"${PSQL[@]}" >/dev/null <<'SQL'
BEGIN;
-- Hermetic reseed: remove any stale RLS-tagged rows before inserting fresh
-- ones so repeated script runs (or partial failures) don't drift counts.
DELETE FROM watchkeeper.knowledge_chunk WHERE subject LIKE 'rls-%';
INSERT INTO watchkeeper.knowledge_chunk (scope, subject, content, embedding) VALUES
  (
    'org',
    'rls-org',
    'rls org row',
    ('[' || repeat('0,', 1535) || '0]')::vector
  ),
  (
    'agent:aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa',
    'rls-agent-a',
    'agent-A row',
    ('[' || repeat('0,', 1535) || '0]')::vector
  ),
  (
    'agent:bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb',
    'rls-agent-b',
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

# Expect: visible = 100 HNSW 'org' + 1 rls-org + 1 rls-agent-a = 102; bbbb = 0.
if [[ "${rls_select_counts}" != "102,0" ]]; then
  fail "RLS cross-scope SELECT expected '102,0' (visible, bbbb-visible); got '${rls_select_counts}'"
fi
echo "OK: RLS hides out-of-scope rows (visible=102, bbbb-visible=0)"

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
# `scope = 'org'` branch of USING holds. Expect 101 rows visible:
# 100 HNSW-seeded 'org' rows + 1 RLS-seeded `rls-org` row.
rls_empty_count=$("${PSQL[@]}" -tA <<'SQL'
BEGIN;
SET ROLE wk_agent_role;
SELECT count(*) FROM watchkeeper.knowledge_chunk;
RESET ROLE;
ROLLBACK;
SQL
)

if [[ "${rls_empty_count}" != "101" ]]; then
  fail "RLS empty-scope visibility expected 101 (org rows only); got '${rls_empty_count}'"
fi
echo "OK: RLS with unset scope sees only scope='org' rows (count=101)"

echo ">> migrate-schema-test: (j) RLS owner baseline (default role sees all rows)"
# Document owner-bypass baseline: the connecting CI/superuser role,
# without SET ROLE, still sees all rows in knowledge_chunk (RLS policies
# apply to non-owner roles only; FORCE RLS becomes relevant the moment
# the owner issues SET ROLE into a policy-subject role, as exercised by
# (f)/(g)/(h) above). This assertion documents the baseline and will
# catch a regression where policies accidentally apply to the owner
# default-role path (which would break admin tooling).
#
# Expected row count: 100 HNSW-seeded 'org' rows (block e) + 3 RLS-seeded
# scope rows (rls-org, rls-agent-a, rls-agent-b from the hermetic RLS seed
# block above) = 103.
owner_baseline=$("${PSQL[@]}" -tA -c "SELECT count(*) FROM watchkeeper.knowledge_chunk;")
if [[ "${owner_baseline}" != "103" ]]; then
  fail "owner baseline count expected 103 (100 org-seeded HNSW rows + 3 RLS-seeded scope rows), got ${owner_baseline}"
fi
echo "OK: owner default-role sees all 103 rows (baseline for FORCE RLS policy application)"

echo ">> migrate-schema-test: (i) outbox happy path + partial-index presence"
"${PSQL[@]}" >/dev/null <<'SQL'
BEGIN;
INSERT INTO watchkeeper.outbox (aggregate_type, aggregate_id, event_type, payload)
VALUES ('watchkeeper', gen_random_uuid(), 'spawned', '{"k":"v"}'::jsonb);
COMMIT;
SQL

outbox_count=$("${PSQL[@]}" -tA -c "SELECT count(*) FROM watchkeeper.outbox;")
if [[ "${outbox_count}" != "1" ]]; then
  fail "outbox happy-path insert count expected 1, got ${outbox_count}"
fi

outbox_indexdef=$("${PSQL[@]}" -tA -c \
  "SELECT indexdef FROM pg_indexes WHERE indexname = 'outbox_unpublished_idx';")
if ! printf '%s' "${outbox_indexdef}" | grep -q 'WHERE (published_at IS NULL)'; then
  fail "expected outbox_unpublished_idx to carry 'WHERE (published_at IS NULL)'; got: ${outbox_indexdef}"
fi
echo "OK: outbox insert accepted and partial-index predicate present"

echo ">> migrate-schema-test: (i-plan) outbox partial-index planner use (soft)"
# Soft planner-path check: matches the discipline of the HNSW (e) assertion
# by confirming the partial index is planner-usable. Disabled seqscan hardens
# against planner cost flips on a near-empty table. Documented as nice-to-have
# per the TASK test plan §Security — a WARN here is not a failure.
outbox_plan=$("${PSQL[@]}" -tA <<'SQL'
BEGIN;
SET LOCAL enable_seqscan = off;
EXPLAIN (FORMAT TEXT)
SELECT id FROM watchkeeper.outbox WHERE published_at IS NULL LIMIT 10;
ROLLBACK;
SQL
)
if printf '%s' "${outbox_plan}" | grep -q 'outbox_unpublished_idx'; then
  echo "OK: outbox partial index used by planner (soft check)"
else
  echo "WARN: outbox_unpublished_idx not chosen by planner on empty/near-empty table (soft; see TASK test plan §Security)"
fi

# ---------------------------------------------------------------------------
# M2.7.b+c — read-grants assertions (migration 007).
#
# The Keep service runs read endpoints under `SET LOCAL ROLE wk_*_role`
# (M2.7.b), so every read path must have SELECT on the audit-log and
# manifest tables (no RLS there) plus the lookup tables (`organization`,
# `human`, `watchkeeper`). We assert per role + per table via
# `has_table_privilege` so a future accidental REVOKE trips this gate.
# ---------------------------------------------------------------------------
echo ">> migrate-schema-test: (k) read-grants for wk_*_role on manifest/log/lookup tables"

read_grant_tables=(
  'watchkeeper.organization'
  'watchkeeper.human'
  'watchkeeper.watchkeeper'
  'watchkeeper.manifest'
  'watchkeeper.manifest_version'
  'watchkeeper.keepers_log'
)
read_grant_roles=(wk_org_role wk_user_role wk_agent_role)

for role in "${read_grant_roles[@]}"; do
  for tbl in "${read_grant_tables[@]}"; do
    has=$("${PSQL[@]}" -tA -c "SELECT has_table_privilege('${role}', '${tbl}', 'SELECT');")
    if [[ "${has}" != "t" ]]; then
      fail "expected ${role} to have SELECT on ${tbl}; got '${has}'"
    fi
  done
done
echo "OK: read-grants present for all three roles across manifest/log/lookup tables"

# ---------------------------------------------------------------------------
# M3.5.a.3.1 — manifest tenant column + RLS assertions (migration 013).
#
# Seeds two organizations (orgA / orgB) plus one manifest each and asserts:
#   (l) cross-tenant SELECT invisibility under SET LOCAL watchkeeper.org;
#   (m) cross-tenant INSERT rejected by WITH CHECK on manifest;
#   (n) cross-tenant INSERT rejected by WITH CHECK on manifest_version
#       (manifest_id resolves to a row not visible under the caller's GUC);
#   (o) FK violation: manifest.organization_id pointing at a non-existent
#       organization id raises 23503;
#   (p) unset GUC visibility: with no SET LOCAL watchkeeper.org, the policy
#       fails closed and zero manifest rows are visible.
# ---------------------------------------------------------------------------
echo ">> migrate-schema-test: (l-prep) seed two orgs + one manifest each"
# Hermetic reseed: prior runs may have left rls-* rows in place. Order
# respects FK chain (manifest_version -> manifest -> organization). The
# happy-path rows from block (a) are not touched (they live under display
# names that never start with 'rls-').
"${PSQL[@]}" >/dev/null <<'SQL'
BEGIN;
DELETE FROM watchkeeper.manifest_version
WHERE manifest_id IN (
  SELECT id FROM watchkeeper.manifest WHERE display_name LIKE 'rls-mf-%'
);
DELETE FROM watchkeeper.manifest WHERE display_name LIKE 'rls-mf-%';
DELETE FROM watchkeeper.organization WHERE display_name LIKE 'rls-org-%';
WITH
  org_a AS (
    INSERT INTO watchkeeper.organization (display_name)
    VALUES ('rls-org-a') RETURNING id
  ),
  org_b AS (
    INSERT INTO watchkeeper.organization (display_name)
    VALUES ('rls-org-b') RETURNING id
  ),
  mf_a AS (
    INSERT INTO watchkeeper.manifest (display_name, organization_id)
    SELECT 'rls-mf-a', id FROM org_a RETURNING id, organization_id
  ),
  mf_b AS (
    INSERT INTO watchkeeper.manifest (display_name, organization_id)
    SELECT 'rls-mf-b', id FROM org_b RETURNING id, organization_id
  )
INSERT INTO watchkeeper.manifest_version (manifest_id, version_no, system_prompt)
SELECT id, 1, 'a-v1' FROM mf_a
UNION ALL
SELECT id, 1, 'b-v1' FROM mf_b;
COMMIT;
SQL

org_a_id=$("${PSQL[@]}" -tA -c "SELECT id FROM watchkeeper.organization WHERE display_name = 'rls-org-a';")
org_b_id=$("${PSQL[@]}" -tA -c "SELECT id FROM watchkeeper.organization WHERE display_name = 'rls-org-b';")
if [[ -z "${org_a_id}" || -z "${org_b_id}" ]]; then
  fail "rls org seed failed: orgA='${org_a_id}' orgB='${org_b_id}'"
fi
echo "OK: rls seed inserted (orgA=${org_a_id}, orgB=${org_b_id})"

echo ">> migrate-schema-test: (l) RLS cross-tenant SELECT invisibility on manifest/manifest_version"
mf_visible=$("${PSQL[@]}" -tA <<SQL
BEGIN;
SET ROLE wk_org_role;
SET LOCAL watchkeeper.org = '${org_a_id}';
SELECT
  (SELECT count(*) FROM watchkeeper.manifest) || ',' ||
  (SELECT count(*) FROM watchkeeper.manifest_version);
RESET ROLE;
ROLLBACK;
SQL
)
if [[ "${mf_visible}" != "1,1" ]]; then
  fail "RLS cross-tenant SELECT expected '1,1' for orgA visibility; got '${mf_visible}'"
fi
echo "OK: under orgA GUC, only orgA's manifest+manifest_version are visible (counts=${mf_visible})"

echo ">> migrate-schema-test: (m) RLS WITH CHECK rejects cross-tenant manifest INSERT"
mf_insert_output=$("${PSQL[@]}" <<SQL 2>&1 || true
BEGIN;
SET ROLE wk_org_role;
SET LOCAL watchkeeper.org = '${org_a_id}';
INSERT INTO watchkeeper.manifest (display_name, organization_id)
VALUES ('rls-cross-tenant', '${org_b_id}');
RESET ROLE;
ROLLBACK;
SQL
)
if ! printf '%s' "${mf_insert_output}" | grep -Eq 'row-level security|42501'; then
  fail "expected RLS INSERT on manifest to be rejected; got: ${mf_insert_output}"
fi
echo "OK: cross-tenant manifest INSERT rejected by WITH CHECK"

echo ">> migrate-schema-test: (n) RLS WITH CHECK rejects cross-tenant manifest_version INSERT"
mf_b_id=$("${PSQL[@]}" -tA -c "SELECT id FROM watchkeeper.manifest WHERE display_name = 'rls-mf-b';")
mv_insert_output=$("${PSQL[@]}" <<SQL 2>&1 || true
BEGIN;
SET ROLE wk_org_role;
SET LOCAL watchkeeper.org = '${org_a_id}';
INSERT INTO watchkeeper.manifest_version (manifest_id, version_no, system_prompt)
VALUES ('${mf_b_id}', 99, 'cross-tenant attempt');
RESET ROLE;
ROLLBACK;
SQL
)
if ! printf '%s' "${mv_insert_output}" | grep -Eq 'row-level security|42501'; then
  fail "expected RLS INSERT on manifest_version to be rejected; got: ${mv_insert_output}"
fi
echo "OK: cross-tenant manifest_version INSERT rejected by WITH CHECK"

echo ">> migrate-schema-test: (o) FK violation on non-existent manifest.organization_id"
fk_mf_output=$("${PSQL[@]}" <<'SQL' 2>&1 || true
BEGIN;
SAVEPOINT before_fk;
INSERT INTO watchkeeper.manifest (display_name, organization_id)
VALUES ('rls-fk-attempt', gen_random_uuid());
ROLLBACK TO SAVEPOINT before_fk;
ROLLBACK;
SQL
)
if ! printf '%s' "${fk_mf_output}" | grep -qi 'violates foreign key constraint'; then
  fail "expected FK violation on manifest.organization_id; got: ${fk_mf_output}"
fi
echo "OK: manifest with non-existent organization_id rejected by FK"

echo ">> migrate-schema-test: (p) RLS empty GUC fails closed on manifest"
mf_empty=$("${PSQL[@]}" -tA <<'SQL'
BEGIN;
SET ROLE wk_org_role;
-- No SET LOCAL watchkeeper.org. nullif('','')::uuid is NULL; comparison
-- against NULL is never true so zero rows are visible.
SELECT count(*) FROM watchkeeper.manifest;
RESET ROLE;
ROLLBACK;
SQL
)
if [[ "${mf_empty}" != "0" ]]; then
  fail "RLS empty-GUC manifest visibility expected 0 (fail-closed); got '${mf_empty}'"
fi
echo "OK: unset watchkeeper.org GUC sees zero manifest rows (fail-closed)"

# ---------------------------------------------------------------------------
# M1.1.a — k2k_conversations RLS assertions (migration 029).
#
# Seeds two K2K conversations under the existing rls-org-a / rls-org-b
# organizations (re-used from block (l-prep)) and asserts:
#   (q) cross-tenant SELECT invisibility under SET LOCAL watchkeeper.org;
#   (r) cross-tenant INSERT rejected by WITH CHECK;
#   (s) FK violation on non-existent organization_id raises 23503;
#   (t) unset GUC visibility fails closed (zero rows visible);
#   (u) CHECK constraints reject empty participants and empty subject;
#   (v) CHECK constraint rejects invalid status enum value.
# ---------------------------------------------------------------------------
echo ">> migrate-schema-test: (q-prep) seed one k2k conversation per org"
# Hermetic reseed: remove any prior rls-k2k-* rows so re-runs stay
# deterministic. Order is independent — k2k_conversations has no
# inbound FKs at this layer.
"${PSQL[@]}" >/dev/null <<SQL
BEGIN;
DELETE FROM watchkeeper.k2k_conversations WHERE subject LIKE 'rls-k2k-%';
INSERT INTO watchkeeper.k2k_conversations (
  id, organization_id, participants, subject, status,
  token_budget, tokens_used
) VALUES
  (gen_random_uuid(), '${org_a_id}', ARRAY['bot-a','bot-b'],
   'rls-k2k-a', 'open', 1000, 0),
  (gen_random_uuid(), '${org_b_id}', ARRAY['bot-c','bot-d'],
   'rls-k2k-b', 'open', 1000, 0);
COMMIT;
SQL
echo "OK: seeded one k2k conversation per org"

echo ">> migrate-schema-test: (q) RLS cross-tenant SELECT invisibility on k2k_conversations"
k2k_visible=$("${PSQL[@]}" -tA <<SQL
BEGIN;
SET ROLE wk_org_role;
SET LOCAL watchkeeper.org = '${org_a_id}';
SELECT count(*) FROM watchkeeper.k2k_conversations WHERE subject LIKE 'rls-k2k-%';
RESET ROLE;
ROLLBACK;
SQL
)
if [[ "${k2k_visible}" != "1" ]]; then
  fail "RLS cross-tenant SELECT on k2k_conversations expected 1 (orgA-only); got '${k2k_visible}'"
fi
echo "OK: under orgA GUC, only orgA's k2k conversation visible (count=${k2k_visible})"

echo ">> migrate-schema-test: (r) RLS WITH CHECK rejects cross-tenant k2k INSERT"
k2k_insert_output=$("${PSQL[@]}" <<SQL 2>&1 || true
BEGIN;
SET ROLE wk_org_role;
SET LOCAL watchkeeper.org = '${org_a_id}';
INSERT INTO watchkeeper.k2k_conversations (
  id, organization_id, participants, subject
) VALUES (
  gen_random_uuid(), '${org_b_id}', ARRAY['bot-x'], 'rls-k2k-cross-tenant'
);
RESET ROLE;
ROLLBACK;
SQL
)
if ! printf '%s' "${k2k_insert_output}" | grep -Eq 'row-level security|42501'; then
  fail "expected RLS INSERT on k2k_conversations to be rejected; got: ${k2k_insert_output}"
fi
echo "OK: cross-tenant k2k_conversations INSERT rejected by WITH CHECK"

echo ">> migrate-schema-test: (s) FK violation on non-existent k2k.organization_id"
fk_k2k_output=$("${PSQL[@]}" <<'SQL' 2>&1 || true
BEGIN;
SAVEPOINT before_fk;
INSERT INTO watchkeeper.k2k_conversations (
  id, organization_id, participants, subject
) VALUES (
  gen_random_uuid(), gen_random_uuid(), ARRAY['bot-x'], 'rls-k2k-fk-attempt'
);
ROLLBACK TO SAVEPOINT before_fk;
ROLLBACK;
SQL
)
if ! printf '%s' "${fk_k2k_output}" | grep -qi 'violates foreign key constraint'; then
  fail "expected FK violation on k2k_conversations.organization_id; got: ${fk_k2k_output}"
fi
echo "OK: k2k_conversations with non-existent organization_id rejected by FK"

echo ">> migrate-schema-test: (t) RLS empty GUC fails closed on k2k_conversations"
k2k_empty=$("${PSQL[@]}" -tA <<'SQL'
BEGIN;
SET ROLE wk_org_role;
-- No SET LOCAL watchkeeper.org. Policy evaluates against NULL → zero rows.
SELECT count(*) FROM watchkeeper.k2k_conversations;
RESET ROLE;
ROLLBACK;
SQL
)
if [[ "${k2k_empty}" != "0" ]]; then
  fail "RLS empty-GUC k2k_conversations visibility expected 0 (fail-closed); got '${k2k_empty}'"
fi
echo "OK: unset watchkeeper.org GUC sees zero k2k_conversations rows (fail-closed)"

echo ">> migrate-schema-test: (u) CHECK constraints reject empty participants and empty subject"
chk_participants=$("${PSQL[@]}" <<SQL 2>&1 || true
BEGIN;
SAVEPOINT before_chk;
INSERT INTO watchkeeper.k2k_conversations (
  id, organization_id, participants, subject
) VALUES (
  gen_random_uuid(), '${org_a_id}', ARRAY[]::text[], 'rls-k2k-empty-participants'
);
ROLLBACK TO SAVEPOINT before_chk;
ROLLBACK;
SQL
)
if ! printf '%s' "${chk_participants}" | grep -qi 'check constraint'; then
  fail "expected CHECK violation on empty participants; got: ${chk_participants}"
fi
echo "OK: empty participants rejected by CHECK"

chk_subject=$("${PSQL[@]}" <<SQL 2>&1 || true
BEGIN;
SAVEPOINT before_chk;
INSERT INTO watchkeeper.k2k_conversations (
  id, organization_id, participants, subject
) VALUES (
  gen_random_uuid(), '${org_a_id}', ARRAY['bot-x'], '   '
);
ROLLBACK TO SAVEPOINT before_chk;
ROLLBACK;
SQL
)
if ! printf '%s' "${chk_subject}" | grep -qi 'check constraint'; then
  fail "expected CHECK violation on whitespace-only subject; got: ${chk_subject}"
fi
echo "OK: whitespace-only subject rejected by CHECK"

echo ">> migrate-schema-test: (v) CHECK constraint rejects invalid status enum"
chk_status=$("${PSQL[@]}" <<SQL 2>&1 || true
BEGIN;
SAVEPOINT before_chk;
INSERT INTO watchkeeper.k2k_conversations (
  id, organization_id, participants, subject, status
) VALUES (
  gen_random_uuid(), '${org_a_id}', ARRAY['bot-x'], 'rls-k2k-bogus-status', 'bogus'
);
ROLLBACK TO SAVEPOINT before_chk;
ROLLBACK;
SQL
)
if ! printf '%s' "${chk_status}" | grep -qi 'check constraint'; then
  fail "expected CHECK violation on invalid status enum; got: ${chk_status}"
fi
echo "OK: invalid status enum rejected by CHECK"

# ---------------------------------------------------------------------------
# M1.3.a — k2k_messages RLS assertions (migration 030).
#
# Seeds one K2K conversation per org (re-using the org_a_id / org_b_id
# fixtures from block (l-prep)), inserts one request-direction message
# per org, and asserts:
#   (w) cross-tenant SELECT invisibility on k2k_messages;
#   (x) cross-tenant INSERT rejected by WITH CHECK;
#   (y) unset GUC visibility fails closed on k2k_messages;
#   (z) CHECK rejects invalid direction enum and empty sender id.
# ---------------------------------------------------------------------------
echo ">> migrate-schema-test: (w-prep) seed one k2k_messages row per org"
"${PSQL[@]}" >/dev/null <<SQL
BEGIN;
DELETE FROM watchkeeper.k2k_messages
WHERE encode(body, 'escape') LIKE 'rls-k2k-msg-%';
DELETE FROM watchkeeper.k2k_conversations
WHERE subject LIKE 'rls-k2k-msg-%';
WITH ca AS (
  INSERT INTO watchkeeper.k2k_conversations (
    id, organization_id, participants, subject, status
  ) VALUES (
    gen_random_uuid(), '${org_a_id}', ARRAY['bot-a','bot-b'],
    'rls-k2k-msg-a', 'open'
  ) RETURNING id
), cb AS (
  INSERT INTO watchkeeper.k2k_conversations (
    id, organization_id, participants, subject, status
  ) VALUES (
    gen_random_uuid(), '${org_b_id}', ARRAY['bot-c','bot-d'],
    'rls-k2k-msg-b', 'open'
  ) RETURNING id
)
INSERT INTO watchkeeper.k2k_messages (
  id, conversation_id, organization_id, sender_watchkeeper_id, body, direction
)
SELECT gen_random_uuid(), ca.id, '${org_a_id}', 'bot-a', convert_to('rls-k2k-msg-a-body', 'UTF8'), 'request'
FROM ca
UNION ALL
SELECT gen_random_uuid(), cb.id, '${org_b_id}', 'bot-c', convert_to('rls-k2k-msg-b-body', 'UTF8'), 'request'
FROM cb;
COMMIT;
SQL
echo "OK: seeded one k2k_messages row per org"

echo ">> migrate-schema-test: (w) RLS cross-tenant SELECT invisibility on k2k_messages"
k2k_msg_visible=$("${PSQL[@]}" -tA <<SQL
BEGIN;
SET ROLE wk_org_role;
SET LOCAL watchkeeper.org = '${org_a_id}';
SELECT count(*) FROM watchkeeper.k2k_messages WHERE encode(body, 'escape') LIKE 'rls-k2k-msg-%';
RESET ROLE;
ROLLBACK;
SQL
)
if [[ "${k2k_msg_visible}" != "1" ]]; then
  fail "RLS cross-tenant SELECT on k2k_messages expected 1 (orgA-only); got '${k2k_msg_visible}'"
fi
echo "OK: under orgA GUC, only orgA's k2k_messages row visible (count=${k2k_msg_visible})"

echo ">> migrate-schema-test: (x) RLS WITH CHECK rejects cross-tenant k2k_messages INSERT"
# Find any orgA conversation to attach the cross-tenant message to so the
# FK survives and only RLS is exercised.
orgA_conv_id=$("${PSQL[@]}" -tA <<SQL
SELECT id FROM watchkeeper.k2k_conversations WHERE organization_id = '${org_a_id}' LIMIT 1;
SQL
)
k2k_msg_insert_output=$("${PSQL[@]}" <<SQL 2>&1 || true
BEGIN;
SET ROLE wk_org_role;
SET LOCAL watchkeeper.org = '${org_a_id}';
INSERT INTO watchkeeper.k2k_messages (
  id, conversation_id, organization_id, sender_watchkeeper_id, body, direction
) VALUES (
  gen_random_uuid(), '${orgA_conv_id}', '${org_b_id}', 'bot-x', convert_to('rls-k2k-msg-cross', 'UTF8'), 'request'
);
RESET ROLE;
ROLLBACK;
SQL
)
if ! printf '%s' "${k2k_msg_insert_output}" | grep -Eq 'row-level security|42501'; then
  fail "expected RLS INSERT on k2k_messages to be rejected; got: ${k2k_msg_insert_output}"
fi
echo "OK: cross-tenant k2k_messages INSERT rejected by WITH CHECK"

echo ">> migrate-schema-test: (y) RLS empty GUC fails closed on k2k_messages"
k2k_msg_empty=$("${PSQL[@]}" -tA <<'SQL'
BEGIN;
SET ROLE wk_org_role;
-- No SET LOCAL watchkeeper.org. Policy evaluates against NULL → zero rows.
SELECT count(*) FROM watchkeeper.k2k_messages;
RESET ROLE;
ROLLBACK;
SQL
)
if [[ "${k2k_msg_empty}" != "0" ]]; then
  fail "RLS empty-GUC k2k_messages visibility expected 0 (fail-closed); got '${k2k_msg_empty}'"
fi
echo "OK: unset watchkeeper.org GUC sees zero k2k_messages rows (fail-closed)"

echo ">> migrate-schema-test: (z) CHECK rejects invalid direction and empty sender"
chk_dir=$("${PSQL[@]}" <<SQL 2>&1 || true
BEGIN;
SAVEPOINT before_chk;
INSERT INTO watchkeeper.k2k_messages (
  id, conversation_id, organization_id, sender_watchkeeper_id, body, direction
) VALUES (
  gen_random_uuid(), '${orgA_conv_id}', '${org_a_id}', 'bot-x', convert_to('b', 'UTF8'), 'bogus'
);
ROLLBACK TO SAVEPOINT before_chk;
ROLLBACK;
SQL
)
if ! printf '%s' "${chk_dir}" | grep -qi 'check constraint'; then
  fail "expected CHECK violation on invalid direction; got: ${chk_dir}"
fi
echo "OK: invalid direction enum rejected by CHECK"

chk_sender=$("${PSQL[@]}" <<SQL 2>&1 || true
BEGIN;
SAVEPOINT before_chk;
INSERT INTO watchkeeper.k2k_messages (
  id, conversation_id, organization_id, sender_watchkeeper_id, body, direction
) VALUES (
  gen_random_uuid(), '${orgA_conv_id}', '${org_a_id}', '   ', convert_to('b', 'UTF8'), 'request'
);
ROLLBACK TO SAVEPOINT before_chk;
ROLLBACK;
SQL
)
if ! printf '%s' "${chk_sender}" | grep -qi 'check constraint'; then
  fail "expected CHECK violation on whitespace-only sender_watchkeeper_id; got: ${chk_sender}"
fi
echo "OK: whitespace-only sender_watchkeeper_id rejected by CHECK"

# M1.3.b — k2k_conversations.close_summary column assertions (migration 031).
#
# Block (aa) pins:
#   * column exists with the expected NOT NULL + default '' shape;
#   * an open row defaults to '' (matches the in-memory adapter);
#   * an UPDATE writing a non-empty summary onto an archived row is
#     accepted under the same per-tenant scope used for the existing
#     k2k_conversations writes (RLS WITH CHECK preserves the org match).

echo ">> migrate-schema-test: (aa) close_summary column shape + default + write"
close_summary_shape=$("${PSQL[@]}" -tA <<'SQL'
SELECT format(
  '%s|%s|%s',
  column_name,
  is_nullable,
  column_default
)
FROM information_schema.columns
WHERE table_schema = 'watchkeeper'
  AND table_name = 'k2k_conversations'
  AND column_name = 'close_summary';
SQL
)
if [[ "${close_summary_shape}" != "close_summary|NO|''::text" ]]; then
  fail "expected close_summary NOT NULL with default ''; got '${close_summary_shape}'"
fi
echo "OK: k2k_conversations.close_summary column has NOT NULL + default ''"

close_summary_default=$("${PSQL[@]}" -tA <<SQL
BEGIN;
SET LOCAL ROLE wk_org_role;
SET LOCAL watchkeeper.org TO '${org_a_id}';
SELECT close_summary FROM watchkeeper.k2k_conversations WHERE id = '${orgA_conv_id}';
ROLLBACK;
SQL
)
if [[ "${close_summary_default}" != "" ]]; then
  fail "expected fresh open row close_summary to default to '' (got '${close_summary_default}')"
fi
echo "OK: open row's close_summary defaults to '' on insert"

close_summary_write=$("${PSQL[@]}" -tA <<SQL
BEGIN;
SET LOCAL ROLE wk_org_role;
SET LOCAL watchkeeper.org TO '${org_a_id}';
UPDATE watchkeeper.k2k_conversations
SET status = 'archived', closed_at = now()
WHERE id = '${orgA_conv_id}';
UPDATE watchkeeper.k2k_conversations
SET close_summary = 'integration-test summary'
WHERE id = '${orgA_conv_id}' AND status = 'archived';
SELECT close_summary FROM watchkeeper.k2k_conversations WHERE id = '${orgA_conv_id}';
ROLLBACK;
SQL
)
if [[ "${close_summary_write}" != "integration-test summary" ]]; then
  fail "expected close_summary write to persist 'integration-test summary'; got '${close_summary_write}'"
fi
echo "OK: close_summary write on archived row persists under wk_org_role"

echo "ALL schema assertions passed"
