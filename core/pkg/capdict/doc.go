// Package capdict loads and serves the per-capability plain-language
// dictionary the M9.4.b approval card surfaces to non-engineer leads.
//
// # Resolution order
//
// Caller flow (production wiring + tests):
//
//  1. [LoadFromFile] reads `dict/capabilities.yaml` via the supplied
//     [FS] seam (or [LoadFromBytes] when the caller already has the
//     payload in memory). I/O failures surface under
//     [ErrDictionaryRead] (distinct from [ErrInvalidDictionary]) so
//     operator triage code can branch on "path wrong" vs "content
//     malformed".
//  2. The strict YAML decoder ([yaml.v3] `KnownFields(true)`) rejects
//     unknown top-level / per-capability keys so a typo or future-
//     protocol field surfaces loudly rather than silently dropping.
//     Note: additive yaml-schema evolution requires a matching Go-
//     field change on [Capability] — strict decode buys typo-catching
//     at the cost of strict-deploy on additive fields.
//  3. Per-row validation enforces non-empty descriptions, valid utf-8
//     (via [utf8.ValidString]), no ASCII control bytes, length caps,
//     and the lower-snake-case + colon-namespace id grammar (stricter
//     than `approval.proposal.go`'s capability-id validator; every
//     capdict id is a legal proposer id, NOT vice-versa). Validation
//     walks the decoded map in DETERMINISTIC sort order so the
//     surfaced first-failure message is reproducible across runs.
//  4. [Translator] returns a closure satisfying the M9.4.b
//     `approval.CapabilityTranslator` seam — the closure looks up the
//     id and returns the description; misses return the empty string
//     (the card renderer falls back to a "no translation registered"
//     placeholder).
//
// # Bijection with CanonicalCapabilities
//
// The closed-set authority is the Go-side [CanonicalCapabilities]
// slice. A two-way test in `canonical_test.go` pins both directions
// AND the length symmetry — same shape as the M9.7
// `TestEventTypeVocabulary_RoadmapNames` pattern. Adding a capability
// requires one yaml row AND one Go-slice entry; missing either side
// fails the CI bijection check.
//
// # Audit discipline
//
// This package is a PURE DECODER. It never imports `keeperslog` and
// never calls `.Append(`. The dictionary loader is configuration-
// shaped, not event-shaped — there is no audit row to emit on a
// successful load. Operator-facing visibility on the loader path
// lives in the production wiring (the caller decides whether to log
// "dictionary loaded" at boot).
//
// # PII discipline
//
// The dictionary's contents are public copy intended for lead-facing
// rendering. The loader does NOT consume tenant data, secret data, or
// any operator-supplied free-form text. The strict-byte validation
// (no NUL, no control-byte runs) is a defense against accidental
// binary smuggling into the yaml file, not a redaction layer.
//
// # Per-call seam shape
//
// [Translator] mirrors the M9.1.a `AuthSecretResolver` / M9.4.a
// `IdentityResolver` pattern: a function-typed return value bound to
// a specific Dictionary at construction time. A process-global static
// would force every tenant to share one dictionary; the closure
// shape lets a multi-tenant deployment load per-tenant dictionaries
// without per-call rebuilds.
package capdict
