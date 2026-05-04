// Package capability provides the broker primitive for issuing and
// validating opaque scoped capability tokens with TTL. ROADMAP §M3 →
// M3.5.
//
// A [Broker] mints cryptographically random tokens via [Broker.Issue],
// each bound to a single string scope (the `pkg:verb` convention — e.g.
// `keep:write`, `notebook:read`) and a wall-clock expiry computed at
// issue time as `clock() + ttl`. Callers later present a token at
// invocation sites and the broker decides admit / deny via
// [Broker.Validate].
//
// # Scope convention: pkg:verb
//
// Scopes are opaque strings to the broker — only equality matters. The
// project-wide convention is `pkg:verb`: the package being protected
// followed by the verb being authorised. Examples:
//
//   - `keep:write` — write to the keepclient backend.
//   - `keep:read`  — read from the keepclient backend.
//   - `notebook:append` — append to the notebook log.
//   - `archivestore:write` — append to an archivestore.
//
// Single-string equality keeps Phase 1 simple; richer permission models
// (sets, hierarchies, JWT-style claims) are deferred to Phase 2+.
//
// # Expiry boundary semantics
//
// `now() >= expiry` is treated as expired — i.e. the boundary is
// INCLUSIVE on the right. A Validate at exactly the expiry instant
// returns [ErrTokenExpired]; a Validate strictly before returns nil
// (assuming scope matches). This convention removes the off-by-one
// ambiguity at the boundary and aligns with the project-wide "expiry
// is the first moment the token is dead" reading.
//
// # Lifecycle and cleanup
//
// Expired entries are removed via two complementary mechanisms:
//
//  1. Lazy cleanup on [Broker.Validate] — when a Validate call lands on
//     an expired entry, the entry is removed before [ErrTokenExpired] is
//     returned. The next Validate of the same token returns
//     [ErrInvalidToken].
//  2. Optional background reaper — when [WithReaperInterval] is supplied
//     with a positive duration the broker spawns a single goroutine
//     that periodically sweeps the map and removes any expired entries.
//     Useful for issue-heavy / validate-light workloads where lazy
//     cleanup alone would let expired entries accumulate.
//
// The reaper is OFF by default — most Phase 1 callers see balanced
// issue / validate traffic and the lazy path keeps the map bounded.
//
// # Redaction discipline (security contract)
//
// This is a security-sensitive package. Two unconditional rules:
//
//  1. **The full 44-character token is NEVER logged.** When a [Logger]
//     is wired via [WithLogger], log entries carry only a `token_prefix`
//     field with the FIRST 8 CHARACTERS of the token. The full token
//     value never appears in any log payload, key, or value — not on
//     issue, not on validate, not on revoke, not on expiry pruning, not
//     on close.
//  2. **Token bytes never appear in error messages or error values.**
//     [ErrInvalidToken], [ErrTokenExpired], and [ErrScopeMismatch] are
//     bare sentinels. No formatting (`fmt.Errorf("token %s ...", token)`)
//     ever wraps the input token bytes. A caller's `err.Error()` string
//     is therefore safe to log even when the caller itself is uncareful.
//
// Test asserts both rules: the recording logger's serialized entries
// never contain the full token, and `err.Error()` never contains the
// input token bytes.
//
// # Out of scope (deferred)
//
//   - Wiring this broker into `keepclient` / `notebook` / `archivestore`
//     so callers must present a valid token to invoke methods. The
//     planner verdict at Gate 1 explicitly defers those integrations
//     until a real harness consumer (post-M5) exists; speculative
//     integration would balloon the M3.5 diff and risk
//     unused-API churn.
//   - Signed / JWT-style self-contained tokens — a Phase 2+ multi-host
//     concern. Phase 1 is single-process so an in-memory map suffices.
//   - Rich permission models (sets, hierarchies, claims). The single
//     `pkg:verb` string scope is sufficient for Phase 1.
//   - Cross-process token distribution and revocation broadcast.
//   - HMAC key rotation.
package capability
