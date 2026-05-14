#!/usr/bin/env bash
# Watchkeeper Phase 1 smoke test — M10.4.
#
# This file is bash (not POSIX sh) — relies on arrays and `local`
# declarations. Run via `make smoke` or `bash scripts/smoke.sh`.
#
# Reproduces the M7 + M8 + M9 success scenarios against an isolated dev
# environment by:
#
#   1. Building every Go binary (`go build ./...`) so a compile break in
#      any package surfaces before the test pass.
#   2. Running `go test -race -count=1` against the curated package set
#      that owns the M7 / M8 / M9 happy-path coverage:
#
#        M7 spawn + retire saga:
#          core/pkg/spawn      core/pkg/spawn/saga    core/pkg/lifecycle
#          core/pkg/notebook   core/pkg/archivestore
#
#        M8 coordinator + jira + cron-driven daily briefing:
#          core/pkg/coordinator  core/pkg/jira  core/pkg/cron
#
#        M9 tool authoring + approval + dry-run + hosted/local + share:
#          core/pkg/approval     core/pkg/toolregistry  core/pkg/toolshare
#          core/pkg/hostedexport core/pkg/capdict       core/pkg/localpatch
#
#        Operator CLI seam (every M10.2 subcommand routes through it):
#          core/cmd/wk
#
# The package list is also pinned by the Go contract test
# `core/internal/deploy/smoke_test.go` so a rename in either place
# fails loudly in the same PR.
#
# The smoke target is intentionally **host-only** — no docker compose,
# no Postgres, no real Slack. Every M7 / M8 / M9 success path has
# fake-driven unit tests in the listed packages; the smoke contract is
# "these tests pass on a clean checkout with only the Go toolchain
# available". Heavier end-to-end coverage (the M10.3 compose stack
# bringing up real Postgres + Keep + Prometheus + Grafana) is the
# parallel DoD bullet, not this one.
#
# Required tools on PATH: go.
# Required env: none.
#
# Optional env:
#   WK_SMOKE_TIMEOUT   per-phase `go test -timeout=…` value (Go duration
#                      like `120s`, `2m`, `1h`; default: 120s). Validated
#                      below so an unsuffixed value (e.g. `WK_SMOKE_TIMEOUT=120`)
#                      fails the smoke gate with a script-level diagnostic
#                      rather than the cryptic `go test` parser error.
#   WK_SMOKE_GO        Go binary (default: go)
#   WK_REPO_ROOT       override the auto-detected repo root (mirrors the
#                      `core/internal/deploy/compose_test.go:repoRoot`
#                      helper iter-1 #15; useful when the script is
#                      invoked from a symlink under $PATH or a sandboxed
#                      runner where `BASH_SOURCE` resolution does not
#                      land on the canonical tree).
#
# Exit status:
#   0 on full pass.
#   non-zero on first failing build or test step.

set -euo pipefail

if [[ -n "${WK_REPO_ROOT:-}" ]]; then
  repo_root=${WK_REPO_ROOT}
else
  # Resolve symlinks so `ln -s $(pwd)/scripts/smoke.sh ~/bin/wk-smoke`
  # invocations still find the canonical tree. macOS BSD `readlink` and
  # GNU `readlink` both accept `-f`; iter-1 #7 closed the symlink hole.
  resolved=$(readlink -f -- "${BASH_SOURCE[0]}" 2>/dev/null || true)
  if [[ -z "${resolved}" ]]; then
    # macOS without coreutils' `readlink -f` — fall back to the raw
    # path; this is the pre-iter-1 behaviour and works for non-symlink
    # checkouts (the common case).
    resolved=${BASH_SOURCE[0]}
  fi
  repo_root=$(cd -- "$(dirname -- "${resolved}")/.." && pwd)
fi
cd "${repo_root}"

go_bin=${WK_SMOKE_GO:-go}
timeout=${WK_SMOKE_TIMEOUT:-120s}

# Validate WK_SMOKE_TIMEOUT — iter-1 #11. A bare integer is a common
# mistake (`WK_SMOKE_TIMEOUT=120`); without this gate `go test` fails
# with "parse error" and the operator chases the wrong diagnostic.
case "${timeout}" in
  *[0-9]ns|*[0-9]us|*[0-9]µs|*[0-9]ms|*[0-9]s|*[0-9]m|*[0-9]h) ;;
  *)
    echo "ERROR: WK_SMOKE_TIMEOUT=${timeout} must be a Go duration (e.g. 120s, 2m, 1h)" >&2
    exit 2
    ;;
esac

if ! command -v "${go_bin}" >/dev/null 2>&1; then
  echo "ERROR: ${go_bin} not found on PATH" >&2
  exit 2
fi

# Curated package sets per phase. Order matters only for output
# legibility: M7 -> M8 -> M9 -> CLI seam mirrors the runbook section
# layout so a failed phase points the operator at the failing M*.
m7_pkgs=(
  ./core/pkg/spawn/...
  ./core/pkg/lifecycle/...
  ./core/pkg/notebook/...
  ./core/pkg/archivestore/...
)
m8_pkgs=(
  ./core/pkg/coordinator/...
  ./core/pkg/jira/...
  ./core/pkg/cron/...
)
m9_pkgs=(
  ./core/pkg/approval/...
  ./core/pkg/toolregistry/...
  ./core/pkg/toolshare/...
  ./core/pkg/hostedexport/...
  ./core/pkg/capdict/...
  ./core/pkg/localpatch/...
)
cli_pkgs=(
  ./core/cmd/wk/...
)

run_phase() {
  if [[ "$#" -lt 2 ]]; then
    echo "ERROR: run_phase needs <label> <pkg...>; got $#" >&2
    exit 2
  fi
  local label=$1
  shift
  echo ">> smoke: ${label}"
  "${go_bin}" test -race -count=1 -timeout="${timeout}" "$@"
}

echo ">> smoke: build (go build ./...)"
"${go_bin}" build ./...

run_phase "M7 spawn + retire saga (notebook + archivestore)" "${m7_pkgs[@]}"
run_phase "M8 coordinator + jira + cron daily briefing"       "${m8_pkgs[@]}"
run_phase "M9 tool authoring + approval + dry-run + share"    "${m9_pkgs[@]}"
run_phase "Operator CLI seam (wk)"                            "${cli_pkgs[@]}"

echo "OK: Phase 1 smoke passed (M7 + M8 + M9 success scenarios + wk CLI)"
