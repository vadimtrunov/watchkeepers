#!/usr/bin/env bash
# Watchkeeper migration round-trip sanity check.
#
# Up -> down-to-zero -> up, with two pg_dump --schema-only snapshots compared
# byte-for-byte. The snapshots ignore the goose tracking table because its
# version_id values change between runs (filtered via --exclude-table).
#
# Required env: WATCHKEEPER_DB_URL (postgres://user:pass@host:port/db?sslmode=...)
# Required tools on PATH: pg_dump, go (for `go run goose`).
#
# Exit status:
#   0 - schema dumps match (AC6 satisfied).
#   1 - dumps differ; diff is printed to stderr.
#   2 - preconditions unmet (env var or tool missing).
#
# Used by `make migrate-round-trip` and the `migrate-ci` GitHub Actions job.

set -euo pipefail

if [[ -z "${WATCHKEEPER_DB_URL:-}" ]]; then
  echo "ERROR: WATCHKEEPER_DB_URL is not set" >&2
  exit 2
fi

if ! command -v pg_dump >/dev/null 2>&1; then
  echo "ERROR: pg_dump not found on PATH (install postgresql-client matching server major version)" >&2
  exit 2
fi

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "${repo_root}"

migrations_dir=${MIGRATIONS_DIR:-deploy/migrations}
goose_version=${GOOSE_VERSION:-v3.27.0}

goose() {
  go run "github.com/pressly/goose/v3/cmd/goose@${goose_version}" \
    -dir "${migrations_dir}" postgres "${WATCHKEEPER_DB_URL}" "$@"
}

# Schema dump excluding goose's bookkeeping table so version_id churn does not
# create a false negative. The `\restrict`/`\unrestrict` lines (pg_dump 18+)
# embed a random per-dump token and are stripped for the same reason.
dump_schema() {
  pg_dump --schema-only --no-owner --no-privileges \
    --exclude-table='*goose_db_version*' "${WATCHKEEPER_DB_URL}" \
    | grep -Ev '^\\(restrict|unrestrict) '
}

tmpdir=$(mktemp -d)
trap 'rm -rf "${tmpdir}"' EXIT

echo ">> migrate-round-trip: initial up"
goose up

echo ">> migrate-round-trip: snapshot 1"
dump_schema >"${tmpdir}/dump1.sql"

echo ">> migrate-round-trip: down to zero"
goose down-to 0

echo ">> migrate-round-trip: re-apply up"
goose up

echo ">> migrate-round-trip: snapshot 2"
dump_schema >"${tmpdir}/dump2.sql"

echo ">> migrate-round-trip: diff"
if diff -u "${tmpdir}/dump1.sql" "${tmpdir}/dump2.sql" >&2; then
  echo "OK: round-trip schema matches"
  exit 0
fi

echo "FAIL: schema dumps differ after round-trip" >&2
exit 1
