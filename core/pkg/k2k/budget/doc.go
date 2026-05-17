// Package budget ships the M1.5 K2K token-budget enforcement seam.
//
// The peer-tool layer (`core/pkg/peer/ask.go`, `core/pkg/peer/reply.go`)
// composes the [Enforcer] interface after every successful
// [k2k.Repository.AppendMessage] call to:
//
//  1. Atomically advance the conversation's `tokens_used` counter via
//     [k2k.Repository.IncTokens].
//  2. Compare the post-increment counter against the conversation's
//     persisted `token_budget` (forwarded by the caller — the conversation
//     row is the source of truth, the caller pre-reads it so this package
//     does not burn an extra `Get` round-trip per Charge).
//  3. On a crossing (`tokensUsed > budget && budget > 0`), emit a
//     `k2k_over_budget` keeperslog row via the M1.4 [audit.Emitter] seam
//     AND notify the M1.6 escalation saga via the [EscalationTrigger]
//     seam.
//
// resolution order:
//
//	Charge → ctx.Err → param validation →
//	         k2k.Repository.IncTokens(delta) (atomic) →
//	         compute over-budget bool (post-increment > budget && budget > 0) →
//	         IF over-budget: detached-ctx emit (audit.EmitOverBudget) →
//	                         detached-ctx trigger (EscalationTrigger.TriggerOverBudget) →
//	         return ChargeResult{TokensUsed, TokenBudget, OverBudget}.
//
// audit discipline: this package owns the `k2k_over_budget` emission
// site (the M1.4 [audit.Emitter] seam defined the constant + the payload
// struct; the production caller landed here). The emit runs under a
// detached `context.WithoutCancel` ctx with a 5-second cap (same
// discipline as `k2k.Lifecycle` and `peer.Tool.Ask`'s audit emit sites)
// so a caller-side cancellation arriving after IncTokens succeeded does
// NOT systematically drop the over-budget audit row.
//
// escalation discipline: M1.5 owns the emit + trigger sites; the M1.6
// escalation saga owns the [EscalationTrigger] implementation. M1.5
// ships a no-op default [NoopEscalationTrigger] so the seam is
// exercised end-to-end without M1.6's wiring. The trigger runs under
// the same detached-ctx discipline as the audit emit so a caller-side
// cancel does not systematically drop the escalation notification
// either.
//
// PII discipline: the package never observes message body bytes — the
// caller supplies a pre-computed token delta (int64) and the persisted
// row's budget (int64). The emitter / trigger payloads are constructed
// from typed numeric primitives + conversation/organization ids; no
// free-form text reaches either surface. Mirrors the M1.3.\* peer-tool
// PII discipline at the budget enforcement seam.
//
// configurable default + per-Watchkeeper override:
//
//   - [DefaultTokenBudget] is the package-wide default (0 means
//     "enforcement disabled at the default level"; the project config can
//     override at wiring time via a per-call resolver — mirrors the
//     M1.3.d `FilterResolver` per-call seam discipline).
//   - The peer-tool layer's `Deps.TokenBudgetResolver` (a per-call
//     resolver func passed at `peer.NewTool` time) consults the acting
//     watchkeeper's [runtime.Manifest.ImmutableCore.CostLimits] via the
//     `manifest.K2KTokenBudget` helper to fetch the per-Watchkeeper
//     override, falling back to [DefaultTokenBudget] when the override
//     is unset. The resolver is OPTIONAL: nil falls back to
//     [DefaultTokenBudget] verbatim so M1.3.\*-era wirings stay valid
//     without re-plumbing the manifest loader.
//
// out-of-scope:
//   - This package does NOT count tokens against a real LLM tokenizer.
//     A future M2.\* leaf may layer a tokenizer-driven counter behind
//     the same seam without touching the [Enforcer] contract; for M1.5
//     the caller pre-computes the delta (typically `len(body)` bytes or
//     a length-derived estimate — see [EstimateTokensFromBody]) so the
//     enforcement plumbing is exercised end-to-end without a real
//     tokenizer.
//   - This package does NOT own the `k2k_escalated` audit row — that is
//     M1.6's responsibility (the escalation saga emits the row after
//     resolving the lead / Watchmaster routing). M1.5 emits ONLY the
//     `k2k_over_budget` row and pokes the [EscalationTrigger] seam; the
//     trigger's implementation owns subsequent state.
//
// References:
//   - `docs/ROADMAP-phase2.md` §M1 → M1.5.
//   - `docs/lessons/M1.md` 2026-05-17 entry for M1.4 (audit taxonomy).
//   - `core/pkg/k2k/audit/events.go` for the `EventOverBudget` constant.
//   - `core/pkg/peer/ask.go` / `core/pkg/peer/reply.go` for the call
//     sites.
package budget
