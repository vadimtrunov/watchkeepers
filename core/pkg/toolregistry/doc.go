// Package toolregistry implements the M9.1.a data + sync layer AND the
// M9.1.b runtime layer of the multi-source Tool Registry.
//
// M9.1.a (data + sync): operator-supplied `tool_sources` config,
// per-tool `manifest.json` schema, and a [Scheduler] that clones /
// pulls each configured source into the operator's
// `$DATA_DIR/tools/<source>/` directory according to the source's
// pull policy. Successful syncs emit a [SourceSynced] event on
// [TopicSourceSynced]; failures emit a [SourceFailed] event on
// [TopicSourceFailed].
//
// M9.1.b (runtime): a [Registry] subscribes to [TopicSourceSynced],
// rescans the synced directories via [BuildEffective], builds the
// [EffectiveToolset] under precedence flattening (earlier-source-
// wins on same-name conflicts — M9.2 will add the shadow + Slack-DM
// warning), atomically installs it as current, and (when a
// [Publisher] is wired) emits [TopicEffectiveToolsetUpdated]. The
// in-flight-vs-new boundary is preserved by `atomic.Pointer` capture
// + a per-entry refcount tracked on retiring snapshots; the
// configurable [RegistryDeps.GracePeriod] controls how long the
// registry tracks each retiring entry for telemetry.
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
// M9.1.a defines the data + sync layer; M9.1.b adds the runtime
// [Registry] in-process. M9.2 will add the priority + shadow warning
// path (a SHADOWED lower-priority same-name tool fires a
// `tool_shadowed` event and a Slack DM); M9.3 will plug a real
// cosign / minisign verifier into [SignatureVerifier]. Both extend
// the existing seams without rewriting them.
package toolregistry
