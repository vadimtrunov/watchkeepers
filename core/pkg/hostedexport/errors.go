package hostedexport

import "errors"

// ErrInvalidExportRequest is returned by [ExportRequest.Validate] /
// [Exporter.Export] when the request fails the shape contract
// (empty source / tool name, missing reason, missing operator id,
// over-bound field). Wraps with field context via fmt.Errorf for
// triage; callers `errors.Is` to triage at the input boundary.
// Mirrors `localpatch.ErrInvalidInstallRequest`.
var ErrInvalidExportRequest = errors.New("hostedexport: invalid export request")

// ErrInvalidSourceKind is returned by [Exporter.Export] when the
// named source's [toolregistry.SourceKind] is NOT
// [toolregistry.SourceKindHosted]. Hosted-export operations are
// scoped to hosted sources by design — exporting a `git` source
// would be redundant (the operator already has it in git) and
// exporting `local` would expose a hand-patched tree the M9.5
// runbook ties to the operator's local environment. Mirrors
// `localpatch.ErrInvalidSourceKind`.
var ErrInvalidSourceKind = errors.New("hostedexport: source not kind=hosted")

// ErrUnknownSource is returned by [Exporter.Export] when the
// requested source name does not resolve via the configured
// [SourceLookup]. Mirrors `localpatch.ErrUnknownSource` /
// `toolregistry.ErrUnknownSource` for pattern parity.
var ErrUnknownSource = errors.New("hostedexport: unknown source")

// ErrSourceLookupMismatch is returned when the [SourceLookup]
// returns a [toolregistry.SourceConfig] whose `Name` field does not
// match the requested source name. Mirror `localpatch.ErrSourceLookupMismatch`.
var ErrSourceLookupMismatch = errors.New("hostedexport: source lookup contract violated")

// ErrToolMissing is returned by [Exporter.Export] when the hosted
// source's on-disk live directory has no entry for the requested
// tool. The M9.4 hosted-storage pipeline populates the on-disk live
// tree; an absent tool implies either an unconfigured tool name or
// an unpopulated hosted source. Mirrors
// `toolregistry.ErrLocalSourceMissing`.
var ErrToolMissing = errors.New("hostedexport: tool tree missing under hosted source")

// ErrManifestRead is returned by [Exporter.Export] when the live
// tool tree's `manifest.json` is missing OR fails
// [toolregistry.DecodeManifest]. The export refuses BEFORE any write
// to the destination so an undecidable tree never lands on the
// operator's disk under a misleading version directory. Wraps the
// underlying decode sentinel so callers can `errors.Is` either kind.
// Mirrors `localpatch.ErrManifestRead`.
var ErrManifestRead = errors.New("hostedexport: manifest read failed")

// ErrSourceRead is returned when the on-disk tool tree under
// `<DataDir>/tools/<source>/<tool>/` cannot be opened, walked, or
// read. The wrapped underlying error carries the I/O cause for
// triage. Mirrors `localpatch.ErrFolderRead`.
var ErrSourceRead = errors.New("hostedexport: source tree read failed")

// ErrDestinationWrite is returned when the operator-supplied
// destination directory cannot be created or populated. By the time
// this fires no audit publish has happened; the partial destination
// tree is inspectable for operator triage. Mirrors
// `localpatch.ErrLiveWrite`.
var ErrDestinationWrite = errors.New("hostedexport: destination write failed")

// ErrDestinationNotEmpty is returned when the operator-supplied
// destination directory exists AND contains entries. Exporting onto
// a non-empty destination would silently overlay onto an arbitrary
// pre-existing on-disk tree; refuse so the operator triages
// explicitly (delete the directory, pass an empty sibling, etc.).
// No analogue in localpatch — exclusive to the export use case
// where the destination is operator-owned, not tool-registry-managed.
var ErrDestinationNotEmpty = errors.New("hostedexport: destination directory is not empty")

// ErrUnsafePath is returned when a derived path (live tool dir or
// destination dir) resolves outside its expected parent after
// [filepath.Clean]. Belt-and-braces over the per-field allowlists;
// firing it indicates either a successful traversal attempt OR a
// programmer error in path composition. Mirror
// `localpatch.ErrUnsafePath`.
var ErrUnsafePath = errors.New("hostedexport: unsafe path")

// ErrIdentityResolution wraps an [OperatorIdentityResolver] failure.
// The resolver runs once per [Exporter.Export] invocation; a
// resolution error aborts the operation BEFORE any on-disk write.
// Mirrors `localpatch.ErrIdentityResolution`.
var ErrIdentityResolution = errors.New("hostedexport: operator identity resolution failed")

// ErrEmptyResolvedIdentity is returned when the
// [OperatorIdentityResolver] returns `("", nil)` for a non-empty
// resolution attempt. Silently demoting to an empty operator id
// would leave the audit event without accountability. Mirrors
// `localpatch.ErrEmptyResolvedIdentity`.
var ErrEmptyResolvedIdentity = errors.New("hostedexport: resolver returned empty operator identity")

// ErrInvalidOperatorID is returned when the resolved operator id
// exceeds [MaxOperatorIDLength] OR contains characters outside the
// stable-public-identifier allowlist. Mirrors
// `localpatch.ErrInvalidOperatorID`.
var ErrInvalidOperatorID = errors.New("hostedexport: invalid operator id")

// ErrPublishHostedToolExported is returned when the on-disk export
// succeeded but the subsequent [Publisher.Publish] of
// [TopicHostedToolExported] failed. The wrapped underlying error
// chains so callers can distinguish "destination written, audit
// notification missed" (`errors.Is(err, ErrPublishHostedToolExported)`)
// from "operation aborted, no change". Mirrors
// `localpatch.ErrPublishLocalPatchApplied`.
var ErrPublishHostedToolExported = errors.New("hostedexport: publish hosted_tool_exported failed")
