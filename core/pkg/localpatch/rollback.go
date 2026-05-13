// Doc-block at file head documenting the [Rollbacker] seam contract.
//
// resolution order: nil-dep check (panic) → RollbackRequest.Validate
// (ErrInvalidRollbackRequest pass-through) → ctx.Err pass-through →
// SourceLookup (ErrUnknownSource / ErrInvalidSourceKind) →
// OperatorIdentityResolver (ErrIdentityResolution /
// ErrEmptyResolvedIdentity / ErrInvalidOperatorID) →
// snapshot existence check at <DataDir>/_history/<source>/<tool>/<target>/
// (ErrSnapshotMissing) →
// snapshot manifest decode (ErrManifestRead — refuses to restore an
// undecidable snapshot) →
// snapshot ContentDigest (ErrFolderRead — for the audit DiffHash) →
// optional snapshot of CURRENT live tree under
// <DataDir>/_history/<source>/<tool>/<currentVersion>/ before
// overwrite (ErrSnapshotWrite; rollback is itself a patch — leaving
// a forward snapshot lets the operator un-rollback) →
// replaceLive (ErrLiveWrite) →
// Publish [TopicLocalPatchApplied] with [context.WithoutCancel]
// (single audit channel; Operation=`rollback`).
//
// audit discipline: never imports `keeperslog`, never calls `.Append(`.
// PII discipline: same as installer — metadata-only kv on log path,
// allowlisted payload fields, bounded operator id + reason.

package localpatch

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/toolregistry"
)

// RollbackRequest is the [Rollbacker.Rollback] input.
type RollbackRequest struct {
	// SourceName is the configured `kind: local` source the rollback
	// targets. Required; non-empty; bounded; allowlisted.
	SourceName string

	// ToolName is the [toolregistry.Manifest.Name] of the tool to
	// roll back. Required; non-empty; bounded; allowlisted.
	ToolName string

	// TargetVersion is the snapshot version to restore. Required;
	// non-empty; bounded; allowlisted. The snapshot MUST exist on
	// disk at `<DataDir>/_history/<source>/<tool>/<target>/`;
	// [Rollbacker.Rollback] returns [ErrSnapshotMissing] when it
	// does not.
	TargetVersion string

	// Reason is the operator-supplied audit text explaining WHY the
	// rollback was performed (e.g. "v1.2.0 panics on unicode tool
	// names — see incident #4711"). Required; non-empty; bounded
	// [MaxReasonLength].
	Reason string

	// OperatorIDHint is the operator's self-declared id (same shape
	// as [InstallRequest.OperatorIDHint]).
	OperatorIDHint string
}

// Validate enforces the shape contract on a [RollbackRequest].
// Returns [ErrInvalidRollbackRequest] wrapped with field context.
func (r RollbackRequest) Validate() error {
	if strings.TrimSpace(r.SourceName) == "" {
		return fmt.Errorf("%w: empty source_name", ErrInvalidRollbackRequest)
	}
	if len(r.SourceName) > MaxSourceNameLength {
		return fmt.Errorf("%w: source_name has %d bytes (max %d)", ErrInvalidRollbackRequest, len(r.SourceName), MaxSourceNameLength)
	}
	if !validIdentifier.MatchString(r.SourceName) {
		return fmt.Errorf("%w: source_name %q has disallowed characters", ErrInvalidRollbackRequest, r.SourceName)
	}
	if strings.TrimSpace(r.ToolName) == "" {
		return fmt.Errorf("%w: empty tool_name", ErrInvalidRollbackRequest)
	}
	if len(r.ToolName) > MaxToolNameLength {
		return fmt.Errorf("%w: tool_name has %d bytes (max %d)", ErrInvalidRollbackRequest, len(r.ToolName), MaxToolNameLength)
	}
	if !validIdentifier.MatchString(r.ToolName) {
		return fmt.Errorf("%w: tool_name %q has disallowed characters", ErrInvalidRollbackRequest, r.ToolName)
	}
	if strings.TrimSpace(r.TargetVersion) == "" {
		return fmt.Errorf("%w: empty target_version", ErrInvalidRollbackRequest)
	}
	if len(r.TargetVersion) > MaxVersionLength {
		return fmt.Errorf("%w: target_version has %d bytes (max %d)", ErrInvalidRollbackRequest, len(r.TargetVersion), MaxVersionLength)
	}
	if !validVersion.MatchString(r.TargetVersion) {
		return fmt.Errorf("%w: target_version %q has disallowed characters", ErrInvalidRollbackRequest, r.TargetVersion)
	}
	if strings.TrimSpace(r.Reason) == "" {
		return fmt.Errorf("%w: empty reason", ErrInvalidRollbackRequest)
	}
	if len(r.Reason) > MaxReasonLength {
		return fmt.Errorf("%w: reason has %d bytes (max %d)", ErrInvalidRollbackRequest, len(r.Reason), MaxReasonLength)
	}
	if len(r.OperatorIDHint) > MaxOperatorIDLength {
		return fmt.Errorf("%w: operator_id_hint has %d bytes (max %d)", ErrInvalidRollbackRequest, len(r.OperatorIDHint), MaxOperatorIDLength)
	}
	return nil
}

// RollbackerDeps bundles the required + optional dependencies for
// [NewRollbacker]. Same shape as [InstallerDeps] — both share the
// FS / Publisher / Clock / SourceLookup / OperatorIdentityResolver
// surfaces so a wired-once production [InstallerDeps] can be passed
// into both constructors verbatim.
type RollbackerDeps struct {
	// FS is the filesystem seam. Required.
	FS FS

	// Publisher emits [TopicLocalPatchApplied] events. Required.
	Publisher Publisher

	// Clock stamps [LocalPatchApplied.AppliedAt]. Required.
	Clock Clock

	// SourceLookup resolves a [toolregistry.SourceConfig] by name.
	// Required.
	SourceLookup SourceLookup

	// OperatorIdentityResolver resolves the operator identity per
	// call. Required.
	OperatorIdentityResolver OperatorIdentityResolver

	// DataDir is the deployment-level data root (see
	// [InstallerDeps.DataDir] for the path-composition contract).
	// Required, non-empty, bounded by [MaxDataDirLength].
	DataDir string

	// Logger receives diagnostic log entries. Optional; nil discards.
	Logger Logger
}

// Rollbacker is the M9.5 local-patch rollback orchestrator. Construct
// via [NewRollbacker]; the zero value is not usable. Safe for
// concurrent use across goroutines under the same per-target mutex
// contract as [Installer] (iter-1 codex M12 fix).
type Rollbacker struct {
	deps    RollbackerDeps
	targets *targetMutexes

	// correlationSeq mirrors [Installer.correlationSeq] (iter-1
	// critic m9 fix — distinct correlation ids under same-nanosecond
	// concurrent dispatch).
	correlationSeq atomic.Uint64
}

// NewRollbacker constructs an [*Rollbacker]. Panics with a named-field
// message when any required dependency is nil; mirrors [NewInstaller].
func NewRollbacker(deps RollbackerDeps) *Rollbacker {
	if deps.FS == nil {
		panic("localpatch: NewRollbacker: deps.FS must not be nil")
	}
	if deps.Publisher == nil {
		panic("localpatch: NewRollbacker: deps.Publisher must not be nil")
	}
	if deps.Clock == nil {
		panic("localpatch: NewRollbacker: deps.Clock must not be nil")
	}
	if deps.SourceLookup == nil {
		panic("localpatch: NewRollbacker: deps.SourceLookup must not be nil")
	}
	if deps.OperatorIdentityResolver == nil {
		panic("localpatch: NewRollbacker: deps.OperatorIdentityResolver must not be nil")
	}
	if strings.TrimSpace(deps.DataDir) == "" {
		panic("localpatch: NewRollbacker: deps.DataDir must not be empty")
	}
	if len(deps.DataDir) > MaxDataDirLength {
		panic(fmt.Sprintf("localpatch: NewRollbacker: deps.DataDir has %d bytes (max %d)", len(deps.DataDir), MaxDataDirLength))
	}
	return &Rollbacker{deps: deps, targets: newTargetMutexes()}
}

// Rollback runs the rollback pipeline on `req` and returns the
// emitted [LocalPatchApplied] payload alongside any error. The
// resolution order is documented at the top of this file.
//
// Rollback is itself a local-patch operation — the on-disk live tree
// is replaced by the snapshot tree, AND the prior live tree is
// snapshotted under its current version BEFORE the overwrite. The
// operator can therefore un-rollback by running another rollback
// targeting the now-snapshotted prior version (so a "rollback to
// v1.2.0 → tested → forward to v1.3.0" workflow degenerates to
// `wk tool rollback foo --to 1.3.0`).
func (r *Rollbacker) Rollback(ctx context.Context, req RollbackRequest) (LocalPatchApplied, error) {
	if err := req.Validate(); err != nil {
		return LocalPatchApplied{}, err
	}
	if err := ctx.Err(); err != nil {
		return LocalPatchApplied{}, err
	}
	if err := resolveLocalSource(ctx, r.deps.SourceLookup, req.SourceName); err != nil {
		return LocalPatchApplied{}, err
	}

	operatorID, err := r.resolveOperator(ctx, req.OperatorIDHint)
	if err != nil {
		return LocalPatchApplied{}, err
	}

	// Per-target serialisation (iter-1 codex M12 fix). The lock
	// key is (source, tool); concurrent rollbacks of DIFFERENT
	// tools under the same source proceed in parallel.
	unlock := r.targets.lock(req.SourceName, req.ToolName)
	defer unlock()

	snapPath, err := r.locateSnapshot(req.SourceName, req.ToolName, req.TargetVersion)
	if err != nil {
		return LocalPatchApplied{}, err
	}
	if err := r.verifySnapshotManifest(snapPath, req); err != nil {
		return LocalPatchApplied{}, err
	}

	diffHash, err := ContentDigest(r.deps.FS, snapPath)
	if err != nil {
		return LocalPatchApplied{}, err
	}

	// Snapshot the CURRENT live tree under its current version
	// before the destructive overwrite (iter-1 codex M2 fix:
	// refuse rollback over an undecidable live tree so an operator
	// cannot silently destroy a triage-required state without an
	// audit trail). Symmetric with the installer's
	// `TestInstall_PriorTreeUndecidableRefused` discipline.
	livePath, err := liveToolPath(r.deps.DataDir, req.SourceName, req.ToolName)
	if err != nil {
		return LocalPatchApplied{}, err
	}
	currentVersion, hasCurrent, readErr := readLiveVersion(r.deps.FS, livePath, req.SourceName)
	if readErr != nil {
		return LocalPatchApplied{}, readErr
	}
	if hasCurrent && currentVersion != req.TargetVersion {
		// Best-effort snapshot of the soon-to-be-replaced tree.
		// A snapshot failure here aborts rollback (the operator
		// would otherwise lose a forward path); the snapshotIfPresent
		// helper preserves an existing snapshot at the same version
		// so a rollback that follows an install of the same version
		// is idempotent.
		if _, snapErr := snapshotIfPresent(r.deps.FS, r.deps.DataDir, req.SourceName, req.ToolName, currentVersion); snapErr != nil {
			return LocalPatchApplied{}, snapErr
		}
	}

	if err := replaceLive(r.deps.FS, r.deps.DataDir, req.SourceName, req.ToolName, snapPath); err != nil {
		return LocalPatchApplied{}, err
	}

	now := r.deps.Clock.Now()
	correlationID := r.newCorrelationID(now)
	event := newLocalPatchAppliedEvent(
		req.SourceName,
		req.ToolName,
		req.TargetVersion,
		operatorID,
		req.Reason,
		diffHash,
		OperationRollback,
		now,
		correlationID,
	)
	publishCtx := context.WithoutCancel(ctx)
	if err := r.deps.Publisher.Publish(publishCtx, TopicLocalPatchApplied, event); err != nil {
		r.logErr(ctx, "publish local_patch_applied failed (rollback)", "source", req.SourceName, "tool", req.ToolName, "operator", operatorID)
		return event, fmt.Errorf("%w: %w", ErrPublishLocalPatchApplied, err)
	}
	return event, nil
}

// resolveOperator mirrors [Installer.resolveOperator]. Hoisted as a
// receiver method (not a free function) so the receiver-typed
// nil-deps check in [NewRollbacker] is the single source of "deps
// were validated".
func (r *Rollbacker) resolveOperator(ctx context.Context, hint string) (string, error) {
	id, err := r.deps.OperatorIdentityResolver(ctx, hint)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrIdentityResolution, err)
	}
	if id == "" {
		return "", ErrEmptyResolvedIdentity
	}
	if len(id) > MaxOperatorIDLength {
		return "", fmt.Errorf("%w: resolved operator id has %d bytes (max %d)", ErrInvalidOperatorID, len(id), MaxOperatorIDLength)
	}
	if !validOperatorID.MatchString(id) {
		return "", fmt.Errorf("%w: resolved operator id %q has disallowed characters", ErrInvalidOperatorID, id)
	}
	return id, nil
}

// logErr forwards a diagnostic message to the optional [Logger].
// Nil-logger safe.
func (r *Rollbacker) logErr(ctx context.Context, msg string, kv ...any) {
	if r.deps.Logger == nil {
		return
	}
	r.deps.Logger.Log(ctx, msg, kv...)
}

// newCorrelationID mirrors [Installer.newCorrelationID].
func (r *Rollbacker) newCorrelationID(now time.Time) string {
	return strconv.FormatInt(now.UnixNano(), 10) + "-" + strconv.FormatUint(r.correlationSeq.Add(1), 10)
}

// locateSnapshot composes the snapshot path AND verifies the
// snapshot directory exists on disk. On a missing snapshot, surfaces
// [ErrSnapshotMissing] with a friendly diagnostic listing available
// versions (iter-1 codex M9 fix: surfaces a `listSnapshots` failure
// rather than swallowing it).
func (r *Rollbacker) locateSnapshot(sourceName, toolName, version string) (string, error) {
	snapPath, err := snapshotPath(r.deps.DataDir, sourceName, toolName, version)
	if err != nil {
		return "", err
	}
	if _, err := r.deps.FS.Stat(snapPath); err != nil {
		available, listErr := listSnapshots(r.deps.FS, r.deps.DataDir, sourceName, toolName)
		if listErr != nil {
			return "", fmt.Errorf("%w: %q (list available failed: %v): %w", ErrSnapshotMissing, snapPath, listErr, err)
		}
		return "", fmt.Errorf("%w: %q (available: %v): %w", ErrSnapshotMissing, snapPath, available, err)
	}
	return snapPath, nil
}

// verifySnapshotManifest decodes the snapshot's manifest AND
// asserts both Name and Version match the rollback request. A
// tampered / hand-edited snapshot lands [ErrManifestRead] BEFORE
// the destructive overwrite.
func (r *Rollbacker) verifySnapshotManifest(snapPath string, req RollbackRequest) error {
	m, err := toolregistry.LoadManifestFromFile(r.deps.FS, snapPath, req.SourceName)
	if err != nil {
		return fmt.Errorf("%w: snapshot at %q: %w", ErrManifestRead, snapPath, err)
	}
	if m.Name != req.ToolName {
		return fmt.Errorf("%w: snapshot manifest tool name %q != request %q", ErrManifestRead, m.Name, req.ToolName)
	}
	if m.Version != req.TargetVersion {
		return fmt.Errorf("%w: snapshot manifest version %q != target %q", ErrManifestRead, m.Version, req.TargetVersion)
	}
	return nil
}
