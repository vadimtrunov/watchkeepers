#!/usr/bin/env bash
# Integration test for the migration Makefile surface (AC3, AC6, AC7, negative).
#
# Contract: caller supplies WATCHKEEPER_DB_URL pointing at a reachable, empty
# Postgres 16 database. The script exercises every `make migrate-*` target
# against it, asserts idempotency, then stages a deliberately broken SQL file
# in a temp dir and asserts `migrate-up` rejects it with non-zero exit.
#
# CI calls this after starting a `postgres:16-alpine` service container.
# Local devs can run it via `make migrate-round-trip` + manual `migrate-up`
# or by exporting WATCHKEEPER_DB_URL and invoking this file directly.
#
# Exit status: 0 on full pass, non-zero on first failing assertion.

set -euo pipefail

if [[ -z "${WATCHKEEPER_DB_URL:-}" ]]; then
  echo "ERROR: WATCHKEEPER_DB_URL is not set" >&2
  exit 2
fi

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "${repo_root}"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

echo "== AC3 happy path: migrate-up applies the placeholder schema =="
make migrate-up
make migrate-status

echo "== AC7 idempotent: second migrate-up is a no-op and exits 0 =="
second_run=$(make migrate-up 2>&1)
printf '%s\n' "${second_run}"
echo "${second_run}" | grep -qE 'no migrations to run|up to date|already applied|nothing to apply' \
  || fail "migrate-up second run did not report idempotency marker"

echo "== AC6 round-trip: schema dump identical before/after down+up =="
make migrate-round-trip

echo "== Negative path: a broken SQL file causes migrate-up to exit non-zero =="
tmp_dir=$(mktemp -d)
trap 'rm -rf "${tmp_dir}"' EXIT
cp deploy/migrations/*.sql "${tmp_dir}/"
cat >"${tmp_dir}/999_bad.sql" <<'BAD'
-- +goose Up
THIS IS NOT VALID SQL;

-- +goose Down
BAD
if make migrate-up MIGRATIONS_DIR="${tmp_dir}" >/tmp/migrate-bad.log 2>&1; then
  cat /tmp/migrate-bad.log
  fail "migrate-up with broken SQL unexpectedly succeeded"
fi
echo "OK: broken SQL rejected as expected"

echo "ALL migrate tests passed"
