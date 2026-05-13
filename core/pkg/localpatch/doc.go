// Package localpatch implements the M9.5 operator-driven local-patch
// installer + rollback runtime that the `make tools-local-install`
// Makefile target and the `wk tool rollback` CLI subcommand wrap.
//
// Two operator-facing operations land here:
//
//  1. Install — copies an operator-supplied folder into a configured
//     `kind: local` source's tools directory under
//     `<DataDir>/tools/<source>/<tool>/`, snapshotting the prior
//     contents (when present) under `<DataDir>/_history/<source>/<tool>/<version>/`
//     so a later rollback can restore them. Emits
//     [TopicLocalPatchApplied] with operator identity, content
//     SHA256 (`diff_hash`), reason, and metadata.
//
//  2. Rollback — restores a previously-snapshotted version of a tool
//     into the same on-disk source/tool directory and emits the same
//     [TopicLocalPatchApplied] topic with `reason="rollback to vX from
//     vY"` (single audit channel, M9.7 lists only `local_patch_applied`).
//
// Both operations land snapshots on the same disk layout: a
// `<DataDir>/_history/<source>/<tool>/<version>/` tree, sibling to the
// `<DataDir>/tools/<source>/` source root the M9.1.b
// [toolregistry.Registry] scanner walks. The `_history` parent is
// SIBLING to `tools/` so the scanner never observes snapshot trees
// (which would attempt to load them as tool manifests and emit decode
// failures into the registry log).
//
// audit discipline (mirrors M9.4.b/c): the package never imports
// `keeperslog` and never calls `.Append(`. The single audit channel for
// install + rollback is the [TopicLocalPatchApplied] event the M9.7
// audit subscriber observes.
//
// PII discipline: the eventbus payload [LocalPatchApplied] carries
// metadata + accountability fields ONLY — `SourceName`, `ToolName`,
// `ToolVersion`, `OperatorID`, `Reason`, `DiffHash`, `AppliedAt`,
// `CorrelationID`. The `Reason` field is operator-supplied free-form
// text — it IS the audit purpose (the operator MUST justify a
// local-patch / rollback in plain language) so the
// metadata-only-payload rule from M9.4.a/b/c does not apply: the
// operator's accountability text MUST land verbatim. The `OperatorID`
// is a stable public identifier (an agent / human uuid), NEVER a
// credential or session token. The bounded length on `Reason` and
// `OperatorID` prevents an unbounded payload from a buggy CLI invocation.
// Tool source code, file paths under `<DataDir>/_history/`, and snapshot
// trees never land on the bus payload — `DiffHash` is the
// cryptographically-bounded summary the audit subscriber consumes
// instead.
//
// Installer / Rollbacker resolution order (mirrors M9.4.c
// [approval.Executor.Execute]):
//
//   - nil-dep check (panic with named-field message)
//   - input.Validate (sentinel pass-through)
//   - ctx.Err pass-through
//   - source-config lookup (must be `kind: local`)
//   - prior-version snapshot (install only, when target dir present)
//   - on-disk write (install: copy folder; rollback: copy from snapshot)
//   - Publish [TopicLocalPatchApplied] with [context.WithoutCancel]
//     (durable record contract — a mid-flight cancel must not split
//     the on-disk-then-publish pipeline)
//
// Per-call seam discipline: the operator-identity resolver, the
// per-call sources lookup, and the diff-hash function are all
// per-invocation seams (not pinned-at-construction values) so a
// per-tenant rotation takes effect on the next operation without a
// process restart — same lesson as M9.1.a's `AuthSecretResolver`,
// M9.4.a's `IdentityResolver`, M9.4.b's `WebhookSecretResolver` /
// `SourceForTarget`, M9.4.c's `ScopeResolver`.
//
// Snapshot retention (iter-1 critic M6 fix): every install + every
// rollback adds a directory under
// `<DataDir>/_history/<source>/<tool>/<version>/`. There is no
// automated GC, no max-versions cap, and no scheduled cleanup. The
// operator's contract: snapshots accumulate indefinitely until the
// operator manually prunes via
// `rm -rf <DataDir>/_history/<source>/<tool>/<version>/` for the
// versions they no longer want as a rollback target. A future
// roadmap item lands an automated retention sweep; until then,
// operators MUST schedule periodic manual pruning OR a cron-driven
// cleanup script — sized to the disk budget of the deployment.
package localpatch
