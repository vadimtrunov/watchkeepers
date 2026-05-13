package localpatch

import (
	"fmt"
	"time"
)

// Operation is the closed-set discriminator for [LocalPatchApplied.Operation].
// Iter-1 critic n3 fix: previously a bare `string` on the payload —
// asymmetric with `approval.BrokerKind` / `Disposition` /
// `toolregistry.DryRunMode` / `SourceKind`, all of which are typed
// strings with `Validate()`. Audit subscribers decoding the payload
// can now call `Operation.Validate()` to triage a tampered row.
type Operation string

const (
	// OperationInstall is the [LocalPatchApplied.Operation] value
	// emitted by [Installer.Install].
	OperationInstall Operation = "install"

	// OperationRollback is the [LocalPatchApplied.Operation] value
	// emitted by [Rollbacker.Rollback].
	OperationRollback Operation = "rollback"
)

// Validate reports whether `o` is in the closed [Operation] set.
// Returns a wrapped [ErrInvalidOperation] otherwise. Mirrors the
// closed-set `Validate()` discipline of the rest of M9.
func (o Operation) Validate() error {
	switch o {
	case OperationInstall, OperationRollback:
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrInvalidOperation, string(o))
	}
}

// TopicLocalPatchApplied is the [eventbus.Bus] topic both
// [Installer.Install] and [Rollbacker.Rollback] emit to. M9.7's audit
// subscriber consumes this name verbatim — the prose roadmap text
// (M9.5) names `local_patch_applied` as the single audit channel for
// the local-patch lifecycle (install + rollback share the topic so a
// downstream "local-patch ledger" view sorts both kinds in one
// chronological feed).
//
// The `localpatch.` namespace prefix mirrors `approval.tool_proposed`
// / `approval.tool_dry_run_executed` / `toolregistry.source_synced` —
// every M9 topic is package-prefixed so an audit subscriber subscribing
// by topic glob (`localpatch.*`) gets the whole package's surface
// without naming each topic individually.
const TopicLocalPatchApplied = "localpatch.local_patch_applied"

// LocalPatchApplied is the payload published on [TopicLocalPatchApplied]
// after [Installer.Install] / [Rollbacker.Rollback] commits the on-disk
// change AND before returning to the caller. Named verbatim to match
// the audit-vocabulary topic (`localpatch.local_patch_applied`) so a
// downstream M9.7 audit subscriber's row-schema field name maps 1:1
// to the package's Go type name — the stutter is the deliberate parity
// surface.
//
// PII discipline (see also the package doc): metadata + accountability
// fields ONLY.
//
//   - `SourceName` / `ToolName` / `ToolVersion` are public identifiers
//     (the operator-config source name, the manifest's tool name, the
//     manifest's SemVer version).
//   - `OperatorID` is the resolved stable public identifier of the
//     operator who initiated the patch — bounded length, allowlisted
//     character set (see [ErrInvalidOperatorID]).
//   - `Reason` is operator-supplied free-form audit text — verbatim,
//     bounded length. The operator-attested justification for the
//     patch IS the audit purpose; metadata-only-payload rule from
//     M9.4.a/b/c does not apply here.
//   - `DiffHash` is the lower-hex SHA256 of the canonicalised tool
//     contents (see [ContentDigest]) — bounded 64-char string;
//     downstream subscribers compare against the prior diff hash to
//     detect "rollback to a known prior version" vs "fresh patch".
//   - `Operation` discriminates `install` vs `rollback` — closed-set
//     enum so the M9.7 audit subscriber can split the feed without
//     parsing the `Reason` text.
//   - `AppliedAt` is the [Clock] timestamp captured AFTER the on-disk
//     write committed and BEFORE the publish.
//   - `CorrelationID` is process-monotonic — same opaque shape as
//     `toolregistry.SourceSynced.CorrelationID` (`Clock.Now().UnixNano()`
//     formatted as base-10 int64). Subscribers MUST NOT parse it.
//
// Tool source code, file paths under `<DataDir>/_history/`, snapshot
// trees, and the operator-supplied folder path NEVER land on the
// payload. The reflection-based field-allowlist test pins the shape
// so a future addition forces the author to bump the allowlist AND
// consciously document the new field's PII shape.
//
//nolint:revive // Type name parity with TopicLocalPatchApplied is load-bearing for the M9.7 audit subscriber's row-schema join.
type LocalPatchApplied struct {
	// SourceName is the [toolregistry.SourceConfig.Name] the patch
	// targeted. Always a `kind: local` source (other kinds are rejected
	// at [Installer.Install] / [Rollbacker.Rollback] entry).
	SourceName string

	// ToolName is the [toolregistry.Manifest.Name] of the patched tool.
	ToolName string

	// ToolVersion is the [toolregistry.Manifest.Version] of the
	// patched tool. For an install, this is the version of the
	// just-installed manifest; for a rollback, this is the version
	// the on-disk state was rolled back TO.
	ToolVersion string

	// OperatorID is the resolved stable public identifier of the
	// operator who initiated the patch. Bounded
	// [MaxOperatorIDLength] bytes, allowlisted characters
	// (alphanumerics + `_-.@:`).
	OperatorID string

	// Reason is the operator-supplied audit text explaining WHY the
	// patch was applied. Required, bounded [MaxReasonLength] bytes.
	// Verbatim — the operator's accountability statement IS the audit
	// purpose.
	Reason string

	// DiffHash is the lower-hex SHA256 of the canonicalised tool
	// contents (see [ContentDigest]). Always 64 chars.
	DiffHash string

	// Operation discriminates install vs rollback. Typed
	// [Operation] enum with `Validate()`.
	Operation Operation

	// AppliedAt is the wall-clock timestamp captured AFTER the on-disk
	// write committed AND BEFORE the publish, sourced from [Clock.Now].
	AppliedAt time.Time

	// CorrelationID is a process-monotonic identifier for this
	// operation. Opaque format (`Clock.Now().UnixNano()` formatted as
	// base-10 int64); callers MUST NOT parse it.
	CorrelationID string
}

// newLocalPatchAppliedEvent constructs a [LocalPatchApplied] payload
// from the per-operation fields. Centralising the mapping keeps the
// field-parity contract auditable: a future addition to the payload
// flows through this constructor, and the reflection-based allowlist
// test on [LocalPatchApplied] catches silent drift. Mirrors
// `approval.newDryRunExecutedEvent`.
func newLocalPatchAppliedEvent(
	sourceName, toolName, toolVersion, operatorID, reason, diffHash string,
	operation Operation,
	appliedAt time.Time,
	correlationID string,
) LocalPatchApplied {
	return LocalPatchApplied{
		SourceName:    sourceName,
		ToolName:      toolName,
		ToolVersion:   toolVersion,
		OperatorID:    operatorID,
		Reason:        reason,
		DiffHash:      diffHash,
		Operation:     operation,
		AppliedAt:     appliedAt,
		CorrelationID: correlationID,
	}
}
