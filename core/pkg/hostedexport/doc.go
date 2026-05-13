// Package hostedexport implements the M9.6 operator-driven hosted-tool
// export runtime that the `wk-tool hosted-export` CLI subcommand wraps.
//
// One operator-facing operation lands here:
//
//   - Export — copies a tool tree from a configured `kind: hosted`
//     source's live directory under `<DataDir>/tools/<source>/<tool>/`
//     into an operator-supplied destination directory as a self-
//     contained plain-directory bundle (manifest.json + the full
//     source tree). The bundle imports cleanly into a fresh git repo
//     via `cd <dest> && git init && git add . && git commit` and
//     CI's against the M9.4.d shared workflow template runs against
//     it without any additional unpacking step.
//
// The boundary at M9.6.a is deliberately upstream of the M9.6.b /
// .c git-client + agent-capability surface: this package emits a
// single audit event ([TopicHostedToolExported]) and never opens a
// git connection, never authenticates against GitHub, never posts to
// Slack. Hosted ↔ git promotion (operator-driven and agent-driven
// flows) lands in the sibling [toolshare] package.
//
// audit discipline (mirrors M9.5 localpatch + M9.4.b/c): the package
// never imports `keeperslog` and never calls `.Append(`. The single
// audit channel for export is the [TopicHostedToolExported] event
// the M9.7 audit subscriber observes.
//
// PII discipline: the eventbus payload [HostedToolExported] carries
// metadata + accountability fields ONLY — `SourceName`, `ToolName`,
// `ToolVersion`, `OperatorID`, `Reason`, `BundleDigest`, `ExportedAt`,
// `CorrelationID`. The `Reason` field is operator-supplied free-form
// text — it IS the audit purpose (the operator MUST justify a
// hosted-export in plain language, mirroring M9.5's
// [localpatch.InstallRequest.Reason]) so the metadata-only-payload
// rule from M9.4.a/b/c does not apply: the operator's accountability
// text MUST land verbatim. The `OperatorID` is a stable public
// identifier (an agent / human uuid), NEVER a credential or session
// token. The bounded length on `Reason` and `OperatorID` prevents an
// unbounded payload from a buggy CLI invocation. Tool source code,
// the on-disk live path, and the operator-supplied destination path
// NEVER land on the payload — `BundleDigest` is the
// cryptographically-bounded SHA256 summary the audit subscriber
// consumes instead.
//
// Exporter resolution order (mirrors M9.5
// [localpatch.Installer.Install]):
//
//   - nil-dep check (panic with named-field message)
//   - input.Validate (sentinel pass-through)
//   - ctx.Err pass-through
//   - source-config lookup (must be `kind: hosted`)
//   - OperatorIdentityResolver
//   - on-disk read of `<DataDir>/tools/<source>/<tool>/manifest.json`
//     (the source must be populated by the M9.4-hosted-storage
//     pipeline; export of a non-existent tool is refused)
//   - bundle content digest via [toolregistry.ContentDigest] shape
//     (canonicalised, exec-bit captured, length-prefix collision
//     defence, symlink-cycle safe via refuse-to-follow)
//   - copy tree to operator-supplied destination
//   - Publish [TopicHostedToolExported] with [context.WithoutCancel]
//     (durable record contract — a mid-flight cancel must not split
//     the on-disk-then-publish pipeline; M9.4.b webhook iter-1 M1
//     lesson)
//
// Per-call seam discipline: the operator-identity resolver, the
// per-call sources lookup, and the destination resolver are all
// per-invocation seams (not pinned-at-construction values) so a
// per-tenant rotation takes effect on the next operation without a
// process restart — same lesson as M9.1.a's `AuthSecretResolver`,
// M9.4.a's `IdentityResolver`, M9.4.b's `WebhookSecretResolver` /
// `SourceForTarget`, M9.4.c's `ScopeResolver`, M9.5's
// `OperatorIdentityResolver`.
//
// Destination contract: the operator-supplied destination directory
// MUST be absent OR empty at export time. Exporting onto a non-empty
// destination is refused with [ErrDestinationNotEmpty] — overwriting
// an arbitrary on-disk tree would be an irreversible operator
// surprise. Mirror M9.5 [localpatch.Installer.Install]'s
// "refuse-when-undecidable" discipline.
package hostedexport
