package toolregistry

import "time"

// TopicSourceSynced is the [eventbus.Bus] topic the [Scheduler] emits
// to after a successful clone / pull. M9.1.b's effective-toolset
// recompute subscribes here.
const TopicSourceSynced = "toolregistry.source_synced"

// TopicSourceFailed is the [eventbus.Bus] topic the [Scheduler] emits
// to on a sync failure. Subscribers (M9.7 audit + future operator
// alerting) can `errors.Is` on the wrapped sentinel via the
// `ErrorType` field — the event payload deliberately omits the
// underlying error message so a tampered URL or auth-resolver
// diagnostic cannot leak credentials downstream.
const TopicSourceFailed = "toolregistry.source_failed"

// TopicEffectiveToolsetUpdated is the [eventbus.Bus] topic the M9.1.b
// [Registry] emits to after a successful [Registry.Recompute] installs
// a new [EffectiveToolset]. Running runtimes subscribe here to learn
// that "new invocations should resolve against a new snapshot"; the
// in-flight calls already hold a reference to the old snapshot and
// keep running on it until they release.
const TopicEffectiveToolsetUpdated = "toolregistry.effective_toolset_updated"

// SourceSynced is the payload published on [TopicSourceSynced] after
// the scheduler has finished cloning / pulling a source AND the
// [SignatureVerifier] (if any) has accepted the result.
//
// PII discipline: the payload carries the source NAME (a public
// identifier) and the LocalPath (also derived from the public name);
// it never carries the resolved auth-secret value or the upstream URL
// (which may contain embedded credentials for git over HTTPS).
type SourceSynced struct {
	// SourceName is the [SourceConfig.Name] of the synced source.
	SourceName string

	// SyncedAt is the wall-clock timestamp captured AFTER the sync
	// finished and BEFORE the event was published. Sourced from
	// [Clock.Now] so tests can pin it deterministically.
	SyncedAt time.Time

	// LocalPath is the on-disk directory the source was synced into,
	// `<DataDir>/tools/<SourceName>/`. Subscribers (M9.1.b) walk this
	// directory to discover per-tool `manifest.json` files.
	LocalPath string

	// CorrelationID is a process-monotonic identifier for this sync
	// cycle. Subscribers MAY join it with their own logs to trace a
	// sync end-to-end; the format is opaque (currently a
	// `time.Now().UnixNano()` value formatted via [strconv.FormatInt])
	// — callers MUST NOT parse it.
	CorrelationID string
}

// SourceFailed is the payload published on [TopicSourceFailed] when a
// sync attempt cannot complete. Either the [GitClient], the
// [AuthSecretResolver], the [SignatureVerifier], or a filesystem
// primitive returned an error.
//
// PII discipline: only the source NAME and the error TYPE
// (`fmt.Sprintf("%T", err)`) are carried. The underlying error
// message — which may contain a token, a URL with embedded creds, or
// a fully-qualified file path — is deliberately omitted so a
// subscriber that logs the event verbatim never leaks credentials.
// Callers that need the underlying error consult the scheduler's
// optional [Logger] (which itself follows the same redaction rule).
type SourceFailed struct {
	// SourceName is the [SourceConfig.Name] of the source that
	// failed to sync.
	SourceName string

	// FailedAt is the wall-clock timestamp of the failure, sourced
	// from [Clock.Now].
	FailedAt time.Time

	// ErrorType is the Go type name of the underlying error,
	// captured via `fmt.Sprintf("%T", err)`. Subscribers SHOULD NOT
	// branch on the exact string — it is intended for human triage
	// only. Programmatic error classification consumes the wrapped
	// sentinel returned from [Scheduler.SyncOnce] instead.
	ErrorType string

	// Phase identifies the sync phase the failure occurred in:
	// `auth`, `mkdir`, `stat`, `clone`, `pull`, `signature`. Empty
	// when the phase is genuinely indeterminate.
	Phase string

	// CorrelationID is the process-monotonic identifier of the
	// failed sync cycle — same shape as [SourceSynced.CorrelationID].
	CorrelationID string
}

// EffectiveToolsetUpdated is the payload published on
// [TopicEffectiveToolsetUpdated] when [Registry.Recompute] installs a
// new [EffectiveToolset]. The payload is intentionally small — it
// carries enough for a subscriber to decide "I should re-read the
// snapshot" without dragging the entire manifest set onto the bus
// (which would defeat the snapshot/refcount pattern and explode the
// event-bus channel buffers under churn).
//
// PII discipline: only the revision counter, tool count, source
// count, build timestamp, and correlation id are carried. The
// manifest contents — which can be operator-authored / AI-authored
// and may include capability ids or schema blobs that a verbose
// subscriber log would dump — are deliberately NOT in the payload.
// Subscribers that need them call [Registry.Snapshot] /
// [Registry.Acquire] on the registry directly.
type EffectiveToolsetUpdated struct {
	// Revision matches [EffectiveToolset.Revision] for the newly
	// installed snapshot. Strictly monotonic across publishes from
	// the same [Registry].
	Revision int64

	// BuiltAt is the wall-clock timestamp captured by
	// [Registry.Recompute] before the swap, sourced from [Clock.Now].
	BuiltAt time.Time

	// ToolCount is the [EffectiveToolset.Len] of the new snapshot
	// (number of tools after precedence flattening).
	ToolCount int

	// SourceCount is the number of [SourceConfig] entries the
	// scanner walked to build this snapshot — equal to the
	// registry's configured source count, not the number of
	// sources that actually contributed tools (an empty source
	// still counts).
	SourceCount int

	// CorrelationID is a process-monotonic identifier for this
	// recompute cycle — same opaque shape as
	// [SourceSynced.CorrelationID].
	CorrelationID string
}
