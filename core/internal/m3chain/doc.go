// Package m3chain is the in-process integration-test home for the M3
// event chain: cron scheduler → eventbus → handler → Keeper's Log.
//
// It exists to host one verification scenario from
// `docs/ROADMAP-phase1.md` §M3 (bullet line 256 / §10 DoD-closure-plan
// item B2):
//
//	Integration test: spawn a mock Watchkeeper, fire a cron event,
//	Watchkeeper receives it, Keeper's Log contains both the cron-fired
//	and the handler-ran events with matching correlation IDs.
//
// No production code lives in this package — only `*_test.go` files
// and this doc.go. The intent is a neutral cross-package home where
// `cron`, `eventbus`, `lifecycle`, and `keeperslog` can be wired
// together without any of those packages taking a dependency on each
// other (preserving the one-way `LocalKeepClient` / `LocalPublisher`
// import-cycle-break pattern documented in `docs/lessons/M3.md`).
//
// Package location follows the `core/internal/keep/<feature>_wiring/`
// convention — internal helpers + their tests live under
// `core/internal/`, separate from the public packages under
// `core/pkg/`.
package m3chain
