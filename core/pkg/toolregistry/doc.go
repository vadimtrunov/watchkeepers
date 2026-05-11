// Package toolregistry implements the M9.1.a data + sync layer for the
// multi-source Tool Registry: operator-supplied `tool_sources` config,
// per-tool `manifest.json` schema, and a [Scheduler] that clones / pulls
// each configured source into the operator's `$DATA_DIR/tools/<source>/`
// directory according to the source's pull policy. Successful syncs emit
// a [SourceSynced] event on [TopicSourceSynced]; failures emit a
// [SourceFailed] event on [TopicSourceFailed]. Effective-toolset recompute
// and the runtime hot-reload signal land in M9.1.b and are deliberately
// out of scope here.
//
// # Seams
//
// All side-effecting primitives are interfaces so tests can substitute
// hand-rolled fakes:
//
//   - [FS] — file-system stat / mkdir / read / readdir.
//   - [GitClient] — clone + pull for git-kind sources.
//   - [Clock] — Now() for event timestamps.
//   - [SignatureVerifier] — verifies a source directory after sync. The
//     default ([NoopSignatureVerifier]) returns nil for every input;
//     real cosign / minisign integration lands in M9.3.
//   - [AuthSecretResolver] — per-call resolver for per-source auth
//     tokens. Tenant-scoped tokens MUST flow through this resolver, not
//     a process-global static, so multi-tenant deployments stay safe.
//   - [Publisher] — eventbus subset for emitting source-sync events.
//
// # PII discipline
//
// The package never logs, returns, or embeds resolved auth-secret values
// in any error, event, or diagnostic. Event payloads carry the source
// NAME (a public identifier) and the error TYPE (`fmt.Sprintf("%T", err)`)
// — never error messages, never tokens, never URLs that may have
// embedded credentials.
//
// # Atomic-ship boundary
//
// M9.1.a defines the data + sync layer. The effective-toolset recompute
// (scanning the synced directory and projecting per-Watchkeeper
// toolsets), the runtime hot-reload signal, and the in-flight-vs-new
// grace period live in M9.1.b. Subscribers to [TopicSourceSynced] in
// M9.1.b will do that work; this package only emits the events.
package toolregistry
