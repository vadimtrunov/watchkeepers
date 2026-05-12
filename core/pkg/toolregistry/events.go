package toolregistry

import (
	"fmt"
	"strings"
	"time"
)

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

// TopicToolShadowed is the [eventbus.Bus] topic the M9.2 [Registry]
// emits to once per shadowed tool detected during
// [Registry.Recompute]. A tool is SHADOWED when a higher-priority
// source already contributed the same [Manifest.Name] earlier in the
// [SourceConfig] list — the lower-priority entry is dropped from the
// snapshot AND surfaced via this topic so a downstream subscriber
// can DM the lead with the message documented on
// [ToolShadowed.Message].
//
// Publish order at the bus boundary is `tool_shadowed` events FIRST
// (one per shadow, in the order they were detected) and
// `effective_toolset_updated` LAST. NOTE on cross-topic delivery
// order: [eventbus.Bus] runs a per-topic worker goroutine, so a
// subscriber registered on both topics is NOT guaranteed to receive
// `tool_shadowed` before `effective_toolset_updated` for the same
// revision — the publish-call order is enforced, the
// subscriber-delivery order across topics is not. Subscribers
// requiring a strict before/after relationship correlate via
// [ToolShadowed.CorrelationID] +
// [EffectiveToolsetUpdated.CorrelationID] (set to the same value
// for one recompute cycle) and key per-revision UI state off
// [ToolShadowed.Revision] / [EffectiveToolsetUpdated.Revision].
const TopicToolShadowed = "toolregistry.tool_shadowed"

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

	// BuiltAt is the wall-clock timestamp captured at the START of
	// [Registry.Recompute] (before the scan), sourced from
	// [Clock.Now]. Subscribers measuring "publish-to-event latency"
	// via `time.Since(ev.BuiltAt)` therefore observe the
	// scan + swap + publish round-trip, not just the swap-to-publish
	// delay. Under a healthy scan duration this distinction is
	// sub-millisecond; under filesystem stalls the metric will
	// include the stall time, which is usually the correct
	// observability behaviour (the bump's wall-clock age IS the
	// scan-stall duration).
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

// ToolShadowed is the payload published on [TopicToolShadowed] once
// per shadowed tool detected during [Registry.Recompute]. The
// "winner" is the higher-priority source whose manifest landed in the
// snapshot; the "shadowed" entry is the same-name manifest from a
// lower-priority source that was dropped.
//
// Priority is the order of the [SourceConfig] list passed to
// [NewRegistry]: an earlier entry has HIGHER priority (it wins the
// merge), a later entry has LOWER priority (it gets shadowed).
//
// PII discipline: the payload carries source NAMES, the tool NAME,
// and both Manifest VERSIONS — all operator / authoring-pipeline
// identifiers, not credentials. The manifest's `Schema` / `Capabilities`
// fields are deliberately NOT in the payload (a verbose subscriber
// log would otherwise dump zod-schema bodies which can be
// AI-authored under M9.4 and may contain proprietary code). The
// `Revision` matches the [EffectiveToolset.Revision] this shadow was
// observed on; the `CorrelationID` matches the
// [EffectiveToolsetUpdated.CorrelationID] of the same recompute
// cycle so subscribers can join the two streams.
type ToolShadowed struct {
	// ToolName is the [Manifest.Name] of the conflicting tool.
	ToolName string

	// WinnerSource is the [SourceConfig.Name] whose manifest landed in
	// the snapshot.
	WinnerSource string

	// WinnerVersion is the [Manifest.Version] of the winning entry —
	// duplicated onto the event so a subscriber building the DM
	// message does not have to call back into [Registry.Snapshot].
	WinnerVersion string

	// ShadowedSource is the [SourceConfig.Name] whose manifest was
	// dropped because [WinnerSource] already supplied [ToolName].
	ShadowedSource string

	// ShadowedVersion is the [Manifest.Version] of the dropped entry.
	ShadowedVersion string

	// Revision matches the [EffectiveToolset.Revision] of the
	// snapshot this shadow was observed on. Strictly monotonic across
	// publishes from the same [Registry]. Subscribers dedup
	// repeat-shadow notifications by joining on
	// (ToolName, ShadowedSource, ShadowedVersion, Revision) — the
	// Revision is part of the dedup key because the same
	// (ToolName, ShadowedSource, ShadowedVersion) tuple WILL fire on
	// every subsequent Recompute that re-observes the conflict, and
	// a subscriber that wants "DM the lead exactly once per
	// new-arrival" gates on a max-seen revision rather than on the
	// triple alone.
	Revision int64

	// BuiltAt mirrors [EffectiveToolset.BuiltAt] for this revision —
	// captured at the START of [Registry.Recompute] (before the
	// scan), sourced from [Clock.Now]. See
	// [EffectiveToolsetUpdated.BuiltAt] for the latency-measurement
	// implications.
	BuiltAt time.Time

	// CorrelationID matches the [EffectiveToolsetUpdated.CorrelationID]
	// of the same recompute cycle — same opaque shape as
	// [SourceSynced.CorrelationID]. The join key for subscribers
	// observing both topics that need a strict before/after
	// relationship across the per-topic worker boundary.
	CorrelationID string
}

// newToolShadowedEvent constructs a [ToolShadowed] event payload
// from a [ShadowedTool] (the in-process detection record returned by
// [BuildEffective]) plus the per-recompute revision / builtAt /
// correlationID context. Centralising the mapping keeps the field-
// parity contract auditable: a future addition to [ShadowedTool]
// that needs to land on the event flows through this constructor,
// and the parallel reflection-based allowlist tests on BOTH structs
// catch silent drift.
func newToolShadowedEvent(sh ShadowedTool, revision int64, builtAt time.Time, correlationID string) ToolShadowed {
	return ToolShadowed{
		ToolName:        sh.ToolName,
		WinnerSource:    sh.WinnerSource,
		WinnerVersion:   sh.WinnerVersion,
		ShadowedSource:  sh.ShadowedSource,
		ShadowedVersion: sh.ShadowedVersion,
		Revision:        revision,
		BuiltAt:         builtAt,
		CorrelationID:   correlationID,
	}
}

// dmInjectionScrubber strips characters a malicious authoring
// pipeline could embed in a [Manifest.Name] / [Manifest.Version] to
// break out of the [ToolShadowed.Message] DM template: backticks
// would close the inline-code-span, newlines would forge new lines,
// `<@U…>` / `<!subteam^…>` Slack mention syntax would page an
// unintended user, leading `>` would create a blockquote. The
// scrubber is conservative — it strips rather than escapes — because
// no rendering layer downstream of the bus is guaranteed to honour
// escapes, and the only callers are DM-text builders.
//
// PII discipline: the scrubber operates on already-public identifier
// strings (tool names + versions). It never sees credentials.
var dmInjectionScrubber = strings.NewReplacer(
	"`", "", "\n", " ", "\r", " ", "\t", " ",
	"<", "", ">", "", "&", "", "|", "",
)

// Message returns the lead-facing DM text documented on the M9.2
// roadmap entry: "<shadowed_source> now ships `<tool>` <shadowed_ver>;
// <winner_source>'s `<tool>` <winner_ver> takes precedence. Review?".
// The phrasing is asymmetric on purpose — the SHADOWED source is the
// new arrival ("now ships"), the WINNER is the incumbent
// ("takes precedence"). A subscriber wiring an actual Slack DM calls
// this method to keep the wording consistent across deployments and
// future re-wordings localised to one place.
//
// Untrusted-content discipline: [Manifest.Name] / [Manifest.Version]
// flow into the template after [dmInjectionScrubber] strips
// characters that could break out of the inline-code span or forge
// Slack mention / formatting syntax. A hostile authoring pipeline
// (M9.4 will land AI-authored tools) therefore cannot inject
// markdown / Slack control sequences via the manifest itself.
// Subscribers wrapping the output in further templates SHOULD
// additionally escape per their own renderer's rules.
//
// Version-prefix note: the roadmap-entry example uses the SemVer "v"
// prefix (`v1.2.0`) as illustration; the renderer here passes the
// [Manifest.Version] verbatim, so a manifest authored with or
// without a "v" prefix surfaces in the DM exactly as authored.
func (e ToolShadowed) Message() string {
	tool := dmInjectionScrubber.Replace(e.ToolName)
	winnerVer := dmInjectionScrubber.Replace(e.WinnerVersion)
	shadowedVer := dmInjectionScrubber.Replace(e.ShadowedVersion)
	winnerSrc := dmInjectionScrubber.Replace(e.WinnerSource)
	shadowedSrc := dmInjectionScrubber.Replace(e.ShadowedSource)
	return fmt.Sprintf(
		"%s now ships `%s` %s; %s's `%s` %s takes precedence. Review?",
		shadowedSrc, tool, shadowedVer,
		winnerSrc, tool, winnerVer,
	)
}
