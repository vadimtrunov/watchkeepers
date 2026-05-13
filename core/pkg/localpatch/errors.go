package localpatch

import "errors"

// ErrInvalidInstallRequest is returned by [InstallRequest.Validate] /
// [Installer.Install] when the request fails the shape contract
// (empty source / tool name, missing reason, missing operator id,
// over-bound field). Wraps with field context via fmt.Errorf for
// triage; callers `errors.Is` to triage at the input boundary.
var ErrInvalidInstallRequest = errors.New("localpatch: invalid install request")

// ErrInvalidRollbackRequest is returned by [RollbackRequest.Validate]
// / [Rollbacker.Rollback] when the request fails the shape contract
// (empty source / tool name, empty target version, over-bound
// operator id / reason). Symmetric with [ErrInvalidInstallRequest].
var ErrInvalidRollbackRequest = errors.New("localpatch: invalid rollback request")

// ErrInvalidSourceKind is returned by both [Installer.Install] and
// [Rollbacker.Rollback] when the named source's [toolregistry.SourceKind]
// is NOT [toolregistry.SourceKindLocal]. Local-patch operations are
// scoped to local sources by design — patching a `git` source would
// be silently overwritten on the next sync, and patching `hosted`
// would short-circuit the M9.4 hosted-storage pipeline.
var ErrInvalidSourceKind = errors.New("localpatch: source not kind=local")

// ErrUnknownSource is returned by both [Installer.Install] and
// [Rollbacker.Rollback] when the requested source name does not
// resolve via the configured [SourceLookup]. Mirrors
// `toolregistry.ErrUnknownSource` for pattern parity.
var ErrUnknownSource = errors.New("localpatch: unknown source")

// ErrSourceLookupMismatch is returned when the [SourceLookup]
// returns a [toolregistry.SourceConfig] whose `Name` field does not
// match the requested source name. The source IS known to the
// resolver — the contract violation is "lookup returned the wrong
// record". Distinct from [ErrUnknownSource] (the source name does
// not resolve at all) so the operator's `errors.Is` triage routes
// to the right place. Iter-1 codex M5 + critic M5 fix.
var ErrSourceLookupMismatch = errors.New("localpatch: source lookup contract violated")

// ErrFolderRead is returned by [Installer.Install] when the
// operator-supplied folder cannot be opened, walked, or read. The
// wrapped underlying error carries the I/O cause for triage.
var ErrFolderRead = errors.New("localpatch: folder read failed")

// ErrManifestRead is returned by [Installer.Install] when the
// operator-supplied folder is missing a `manifest.json` or the
// manifest fails [toolregistry.DecodeManifest]. Wraps the underlying
// decode sentinel so callers can `errors.Is` either kind.
var ErrManifestRead = errors.New("localpatch: manifest read failed")

// ErrSnapshotWrite is returned by both operations when a snapshot
// directory cannot be created or populated. The on-disk staging
// surface is `<DataDir>/_history/<source>/<tool>/<version>/`; a
// failure here aborts the operation BEFORE any change to the live
// `<DataDir>/tools/<source>/<tool>/` tree.
var ErrSnapshotWrite = errors.New("localpatch: snapshot write failed")

// ErrLiveWrite is returned by both operations when the live tools
// directory cannot be cleared or populated. By the time this fires
// the snapshot is already on disk; the partial live tree is
// inspectable for operator triage.
var ErrLiveWrite = errors.New("localpatch: live tools write failed")

// ErrSnapshotMissing is returned by [Rollbacker.Rollback] when the
// requested target version has no on-disk snapshot under
// `<DataDir>/_history/<source>/<tool>/<version>/`. A rollback target
// MUST have been previously snapshotted by an [Installer.Install]
// call (or an out-of-band restore staged into the same path).
var ErrSnapshotMissing = errors.New("localpatch: snapshot missing")

// ErrUnsafePath is returned by both operations when a derived path
// (live source dir, snapshot dir, or operator-supplied folder)
// resolves outside its expected parent after [filepath.Clean].
// Belt-and-braces over the per-field allowlists; firing it indicates
// either a successful traversal attempt OR a programmer error in
// path composition. Mirrors `toolregistry.ErrUnsafeLocalPath`.
var ErrUnsafePath = errors.New("localpatch: unsafe path")

// ErrIdentityResolution wraps an [OperatorIdentityResolver] failure.
// The resolver runs once per [Installer.Install] / [Rollbacker.Rollback]
// invocation; a resolution error aborts the operation BEFORE any
// on-disk change. Mirrors `approval.ErrIdentityResolution`.
var ErrIdentityResolution = errors.New("localpatch: operator identity resolution failed")

// ErrEmptyResolvedIdentity is returned when the [OperatorIdentityResolver]
// returns `("", nil)` for a non-empty resolution attempt. Silently
// demoting to an empty operator id would leave the audit event
// without accountability; mirror `approval.ErrEmptyResolvedIdentity`'s
// fail-loud discipline.
var ErrEmptyResolvedIdentity = errors.New("localpatch: resolver returned empty operator identity")

// ErrInvalidOperatorID is returned when the resolved operator id
// exceeds [MaxOperatorIDLength] OR contains characters outside the
// stable-public-identifier allowlist (alphanumerics + `_-.@:`). The
// resolver SHOULD return a stable public identifier (an agent uuid
// or human handle), NEVER a credential or session token; a buggy
// resolver leaking a token-shaped string is rejected here BEFORE the
// id reaches the [LocalPatchApplied] payload or the diagnostic
// logger.
var ErrInvalidOperatorID = errors.New("localpatch: invalid operator id")

// ErrInvalidOperation is returned by [Operation.Validate] when an
// audit-decoder receives an [Operation] value outside the closed
// set (`install`, `rollback`). Iter-1 critic n3 fix — symmetric with
// the closed-set Validate() discipline elsewhere in M9.
var ErrInvalidOperation = errors.New("localpatch: invalid operation")

// ErrPublishLocalPatchApplied is returned by [Installer.Install] /
// [Rollbacker.Rollback] when the on-disk operation succeeded but the
// subsequent [Publisher.Publish] of [TopicLocalPatchApplied] failed.
// The wrapped underlying error chains so callers can distinguish
// "state committed, audit notification missed"
// (`errors.Is(err, ErrPublishLocalPatchApplied)`) from "operation
// aborted, no change". Mirrors `toolregistry.ErrPublishAfterSwap`.
var ErrPublishLocalPatchApplied = errors.New("localpatch: publish local_patch_applied failed")
