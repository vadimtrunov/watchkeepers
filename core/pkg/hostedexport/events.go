package hostedexport

import "time"

// TopicHostedToolExported is the [eventbus.Bus] topic
// [Exporter.Export] emits to. M9.7's audit subscriber consumes this
// name verbatim — the prose roadmap text (M9.6 + M9.7) names
// `hosted_tool_exported` as the audit channel for the
// hosted-export lifecycle.
//
// The `hostedexport.` namespace prefix mirrors
// `localpatch.local_patch_applied` / `approval.tool_proposed` /
// `toolregistry.source_synced` — every M9 topic is package-prefixed
// so an audit subscriber subscribing by topic glob
// (`hostedexport.*`) gets the whole package's surface without naming
// each topic individually.
const TopicHostedToolExported = "hostedexport.hosted_tool_exported"

// HostedToolExported is the payload published on
// [TopicHostedToolExported] after [Exporter.Export] commits the
// on-disk write AND before returning to the caller. Named verbatim
// to match the audit-vocabulary topic
// (`hostedexport.hosted_tool_exported`) so a downstream M9.7 audit
// subscriber's row-schema field name maps 1:1 to the package's Go
// type name — the stutter is the deliberate parity surface.
//
// PII discipline (see also the package doc): metadata +
// accountability fields ONLY.
//
//   - `SourceName` / `ToolName` / `ToolVersion` are public identifiers
//     (the operator-config source name, the manifest's tool name, the
//     manifest's SemVer version).
//   - `OperatorID` is the resolved stable public identifier of the
//     operator who initiated the export — bounded length, allowlisted
//     character set (see [ErrInvalidOperatorID]).
//   - `Reason` is operator-supplied free-form audit text — verbatim,
//     bounded length. The operator-attested justification for the
//     export IS the audit purpose; metadata-only-payload rule from
//     M9.4.a/b/c does not apply here. Mirrors
//     `localpatch.LocalPatchApplied.Reason`.
//   - `BundleDigest` is the lower-hex SHA256 of the canonicalised
//     bundle contents — bounded 64-char string; downstream
//     subscribers compare against the source-tree digest to detect
//     tampering between read and write OR future cross-deployment
//     drift.
//   - `ExportedAt` is the [Clock] timestamp captured AFTER the
//     destination write committed and BEFORE the publish.
//   - `CorrelationID` is process-monotonic — same opaque shape as
//     `localpatch.LocalPatchApplied.CorrelationID`
//     (`Clock.Now().UnixNano()` + per-receiver atomic nonce,
//     formatted as base-10 int64 with a `-` separator). Subscribers
//     MUST NOT parse it.
//
// Tool source code, the on-disk live path under
// `<DataDir>/tools/<source>/<tool>/`, and the operator-supplied
// destination path NEVER land on the payload. The reflection-based
// field-allowlist test pins the shape so a future addition forces
// the author to bump the allowlist AND consciously document the new
// field's PII shape.
//
//nolint:revive // Type name parity with TopicHostedToolExported is load-bearing for the M9.7 audit subscriber's row-schema join.
type HostedToolExported struct {
	// SourceName is the [toolregistry.SourceConfig.Name] the export
	// read from. Always a `kind: hosted` source (other kinds are
	// rejected at [Exporter.Export] entry).
	SourceName string

	// ToolName is the [toolregistry.Manifest.Name] of the exported
	// tool.
	ToolName string

	// ToolVersion is the [toolregistry.Manifest.Version] of the
	// exported tool, read from the live tree's `manifest.json`.
	ToolVersion string

	// OperatorID is the resolved stable public identifier of the
	// operator who initiated the export. Bounded
	// [MaxOperatorIDLength] bytes, allowlisted characters
	// (alphanumerics + `_-.@:`).
	OperatorID string

	// Reason is the operator-supplied audit text explaining WHY the
	// export was taken. Required, bounded [MaxReasonLength] bytes.
	// Verbatim — the operator's accountability statement IS the
	// audit purpose. Mirrors `localpatch.LocalPatchApplied.Reason`.
	Reason string

	// BundleDigest is the lower-hex SHA256 of the canonicalised
	// exported bundle contents. Always 64 chars.
	BundleDigest string

	// ExportedAt is the wall-clock timestamp captured AFTER the
	// destination write committed AND BEFORE the publish, sourced
	// from [Clock.Now].
	ExportedAt time.Time

	// CorrelationID is a process-monotonic identifier for this
	// operation. Opaque format; callers MUST NOT parse it.
	CorrelationID string
}

// newHostedToolExportedEvent constructs a [HostedToolExported]
// payload from the per-operation fields. Centralising the mapping
// keeps the field-parity contract auditable: a future addition to
// the payload flows through this constructor, and the
// reflection-based allowlist test on [HostedToolExported] catches
// silent drift. Mirrors `localpatch.newLocalPatchAppliedEvent`.
func newHostedToolExportedEvent(
	sourceName, toolName, toolVersion, operatorID, reason, bundleDigest string,
	exportedAt time.Time,
	correlationID string,
) HostedToolExported {
	return HostedToolExported{
		SourceName:    sourceName,
		ToolName:      toolName,
		ToolVersion:   toolVersion,
		OperatorID:    operatorID,
		Reason:        reason,
		BundleDigest:  bundleDigest,
		ExportedAt:    exportedAt,
		CorrelationID: correlationID,
	}
}
