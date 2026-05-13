// Doc-block at file head documenting the [Installer] seam contract.
//
// resolution order: nil-dep check (panic) → InstallRequest.Validate
// (ErrInvalidInstallRequest pass-through) → ctx.Err pass-through →
// SourceLookup (ErrUnknownSource / ErrInvalidSourceKind) →
// OperatorIdentityResolver (ErrIdentityResolution /
// ErrEmptyResolvedIdentity / ErrInvalidOperatorID) →
// new-folder manifest read (ErrManifestRead) →
// new-folder ContentDigest (ErrFolderRead) →
// optional snapshot of prior live tree (ErrSnapshotWrite; reads the
// PRIOR manifest's version to name the snapshot dir; refuses install
// when the prior tree is undecidable so a rollback target is always
// available) →
// replaceLive (ErrLiveWrite; live tree atomically replaced with the
// new folder's contents) →
// Publish [TopicLocalPatchApplied] with [context.WithoutCancel]
// (durable record contract — a mid-flight cancel must not split the
// on-disk-then-publish pipeline; M9.4.b webhook iter-1 M1 lesson).
//
// audit discipline: the installer never imports `keeperslog` and
// never calls `.Append(` (see source-grep AC). The audit log entry
// for `local_patch_applied` lives in the M9.7 audit subscriber that
// observes [TopicLocalPatchApplied].
//
// PII discipline: the optional [Logger] is invoked with the source /
// tool / operator id ONLY — never with the operator-supplied folder
// path content, the diff hash bytes (hex string is OK; the digest
// itself is a cryptographic summary), or the raw [InstallRequest]
// fields. The [LocalPatchApplied] payload allowlist pins the bus
// boundary.

package localpatch

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/toolregistry"
)

// targetMutexes is the per-(source,tool) mutex registry shared by
// [Installer] and [Rollbacker]. Concurrent invocations on the SAME
// (source,tool) target serialise via a per-key `sync.Mutex` so
// `snapshotIfPresent`'s Stat-then-write TOCTOU and `replaceLive`'s
// RemoveAll-vs-copyTree race cannot both fire on the same on-disk
// surface. Concurrent invocations on DIFFERENT targets proceed in
// parallel — the registry hands out distinct mutexes per key.
//
// Iter-1 codex M12 fix. The prior package doc claimed
// "safe for concurrent use" but the per-target races were real on
// the OSFS path; the in-memory fakeFS hid them under a single
// package-level mutex.
type targetMutexes struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

func newTargetMutexes() *targetMutexes {
	return &targetMutexes{m: map[string]*sync.Mutex{}}
}

// lock acquires (and creates if absent) the mutex for the given
// (source, tool) target, returns an unlock func the caller defers.
// The returned unlock unlocks once — repeated calls panic per
// `sync.Mutex` semantics.
func (t *targetMutexes) lock(source, tool string) func() {
	key := source + "/" + tool
	t.mu.Lock()
	mu, ok := t.m[key]
	if !ok {
		mu = &sync.Mutex{}
		t.m[key] = mu
	}
	t.mu.Unlock()
	mu.Lock()
	return mu.Unlock
}

// fmtInt64 is inlined at call sites (iter-1 codex n1 nit fix —
// removed the one-line wrapper).

// Bounds enforced by [InstallRequest.Validate] / [RollbackRequest.Validate].
// The numbers defend against an unbounded operator-supplied string
// landing on the [LocalPatchApplied] payload (the audit subscriber's
// row-store has its own bounds; ours are belt-and-braces).
const (
	// MaxSourceNameLength bounds [InstallRequest.SourceName] /
	// [RollbackRequest.SourceName]. A source name is already
	// allowlisted via [validIdentifier] so 64 covers any reasonable
	// operator-config name.
	MaxSourceNameLength = 64

	// MaxToolNameLength bounds [InstallRequest.ToolName] /
	// [RollbackRequest.ToolName]. Same allowlist + bound rationale
	// as [MaxSourceNameLength].
	MaxToolNameLength = 64

	// MaxVersionLength bounds [RollbackRequest.TargetVersion] AND
	// the version derived from the operator-supplied folder's
	// `manifest.json` Version field. SemVer is well under 32 bytes;
	// 64 covers any pre-release / build-metadata suffix.
	MaxVersionLength = 64

	// MaxOperatorIDLength bounds the resolved operator identity. A
	// stable public id (UUID, agent handle, human handle) fits in
	// 64 bytes; a longer string suggests the resolver leaked a
	// credential.
	MaxOperatorIDLength = 64

	// MaxReasonLength bounds [InstallRequest.Reason] /
	// [RollbackRequest.Reason]. The audit text MUST be the operator's
	// justification, not a full PR description; 1 KiB covers any
	// reasonable explanation.
	MaxReasonLength = 1024

	// MaxDataDirLength bounds [InstallerDeps.DataDir] /
	// [RollbackerDeps.DataDir]. A 4 KiB DataDir composes into a
	// multi-KiB final path the OS rejects with a generic
	// `ENAMETOOLONG` rather than a clean `ErrUnsafePath`; bounding
	// at the construction boundary lands a stable diagnostic.
	// Iter-1 critic m11 fix.
	MaxDataDirLength = 1024
)

// validIdentifier is the character allowlist applied to source names
// and tool names. Mirrors `toolregistry`'s `validSourceName` exactly
// (forbids `.`, `/`, `\`, `:` so a path component cannot escape the
// data dir AND cannot impersonate a URL scheme on stringification).
// Version strings have their own (looser) allowlist via [validVersion].
var validIdentifier = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// validVersion is the character allowlist applied to a manifest's
// `Version` string. Permits SemVer + pre-release / build-metadata
// suffixes (`1.2.3-rc1+build.42`, `~unstable`). Explicitly refuses
// the bare `.` / `..` strings so a tampered manifest cannot
// impersonate a path-traversal component on filesystem composition.
var validVersion = regexp.MustCompile(`^[a-zA-Z0-9_~+-][a-zA-Z0-9_.~+-]*$`)

// validOperatorID is the character allowlist applied to a resolved
// operator identity. Permits alphanumerics + `_-.@:` so handles like
// `alice@example.com` or `agent:42` work; rejects whitespace / shell
// metacharacters / path separators that could land on an audit row
// or in a log message and confuse a downstream parser. Exposed
// indirectly via [ValidateOperatorID] for CLI flag-level validation
// (iter-1 critic M2 fix).
var validOperatorID = regexp.MustCompile(`^[a-zA-Z0-9_.@:-]+$`)

// ValidateOperatorID applies the same bound + character-allowlist
// the [Installer] / [Rollbacker] apply post-resolver. Exposed so a
// CLI / wrapper can refuse a malformed `--operator` value BEFORE
// it reaches a wrapped error string on stderr (iter-1 critic M2 fix).
// Returns [ErrInvalidOperatorID] on failure; nil otherwise.
func ValidateOperatorID(id string) error {
	if id == "" {
		return fmt.Errorf("%w: empty", ErrInvalidOperatorID)
	}
	if len(id) > MaxOperatorIDLength {
		return fmt.Errorf("%w: %d bytes (max %d)", ErrInvalidOperatorID, len(id), MaxOperatorIDLength)
	}
	if !validOperatorID.MatchString(id) {
		return fmt.Errorf("%w: disallowed characters", ErrInvalidOperatorID)
	}
	return nil
}

// Publisher is the [eventbus.Bus] subset both [Installer.Install] and
// [Rollbacker.Rollback] consume — only the [Publisher.Publish]
// method. Defined locally so tests substitute a hand-rolled fake and
// production wiring never has to import the concrete `*eventbus.Bus`.
// Mirror `toolregistry.Publisher` / `approval.Publisher`.
type Publisher interface {
	Publish(ctx context.Context, topic string, event any) error
}

// Clock is the time seam. Production wiring uses [ClockFunc] wrapping
// [time.Now]; tests pin a deterministic value. Mirror
// `toolregistry.Clock` / `approval.Clock`.
type Clock interface {
	Now() time.Time
}

// ClockFunc adapts a plain `func() time.Time` to [Clock]. The
// `time.Now` wrapper is the production default; tests pass in a
// closure capturing a `*time.Time` they advance manually.
type ClockFunc func() time.Time

// Now implements [Clock].
func (f ClockFunc) Now() time.Time { return f() }

// Logger is the optional structured-log seam. Mirror
// `toolregistry.Logger` / `approval.Logger`. PII discipline:
// implementations MUST NOT include operator-supplied folder content,
// raw [InstallRequest.Reason] (the reason IS audit content but the
// LOG path is for diagnostics only — keep it metadata-flavoured),
// or any path under `<DataDir>/_history/`.
type Logger interface {
	Log(ctx context.Context, msg string, kv ...any)
}

// SourceLookup is the per-call seam for resolving a configured
// [toolregistry.SourceConfig] by name. Production wiring closes over
// a `*toolregistry.Scheduler` (or a per-deployment config struct);
// tests inject a hand-rolled fake. Per-call (not pinned-at-construction)
// so a config rotation takes effect on the next operation. Same
// shape lesson as M9.4.b's `SourceForTarget`.
//
// Contract:
//
//   - Return the resolved [toolregistry.SourceConfig] on success.
//   - Return [ErrUnknownSource] (or any wrapping of it) when the
//     source name does not match any configured source.
//   - Returning a [toolregistry.SourceConfig] whose
//     [toolregistry.SourceConfig.Kind] is NOT
//     [toolregistry.SourceKindLocal] surfaces [ErrInvalidSourceKind] —
//     the caller (Install / Rollback) re-validates the kind at the
//     entry boundary regardless.
type SourceLookup func(ctx context.Context, sourceName string) (toolregistry.SourceConfig, error)

// OperatorIdentityResolver is the per-call seam for resolving the
// operator identity from the request context. Same shape as
// `approval.IdentityResolver`. The resolver receives a hint (the
// CLI's `--operator <id>` flag or an env-supplied default); the
// resolver decides whether to honour it (e.g. a hosted deployment
// might verify it against an OIDC token bound to the request and
// reject if mismatched).
//
// Contract:
//
//   - Return a non-empty identifier on success.
//   - Return `("", <cause>)` on resolution failure. [Installer.Install]
//     / [Rollbacker.Rollback] wraps the cause with [ErrIdentityResolution]
//     for the caller — implementers MUST NOT pre-wrap.
//   - Returning `("", nil)` is a programmer error caught as
//     [ErrEmptyResolvedIdentity].
//
// Bound discipline: the resolved identifier MUST be at most
// [MaxOperatorIDLength] bytes AND match [validOperatorID]; the
// caller surfaces [ErrInvalidOperatorID] otherwise so a buggy
// resolver cannot land an unbounded credential-shaped string on
// the audit payload.
type OperatorIdentityResolver func(ctx context.Context, hint string) (string, error)

// InstallRequest is the [Installer.Install] input.
type InstallRequest struct {
	// SourceName is the configured `kind: local` source the patch
	// targets. Required; non-empty; bounded; allowlisted. Rejected
	// with [ErrInvalidInstallRequest] otherwise.
	SourceName string

	// FolderPath is the operator-supplied folder containing the new
	// tool tree. Required; non-empty; bounded by the OS path limit
	// (no per-package bound — the FS layer rejects on read).
	FolderPath string

	// Reason is the operator-supplied audit text. Required; non-
	// empty; bounded [MaxReasonLength].
	Reason string

	// OperatorIDHint is the operator's self-declared id, passed
	// through to [OperatorIdentityResolver] as a hint. The resolver
	// decides whether to honour it (a trust-everyone resolver
	// returns it verbatim; a verified-OIDC resolver verifies and
	// rejects on mismatch). Empty hint is acceptable — the resolver
	// may still produce a non-empty resolved id from ctx.
	OperatorIDHint string
}

// Validate enforces the shape contract on an [InstallRequest].
// Returns [ErrInvalidInstallRequest] wrapped with field context.
func (r InstallRequest) Validate() error {
	if strings.TrimSpace(r.SourceName) == "" {
		return fmt.Errorf("%w: empty source_name", ErrInvalidInstallRequest)
	}
	if len(r.SourceName) > MaxSourceNameLength {
		return fmt.Errorf("%w: source_name has %d bytes (max %d)", ErrInvalidInstallRequest, len(r.SourceName), MaxSourceNameLength)
	}
	if !validIdentifier.MatchString(r.SourceName) {
		return fmt.Errorf("%w: source_name %q has disallowed characters", ErrInvalidInstallRequest, r.SourceName)
	}
	if strings.TrimSpace(r.FolderPath) == "" {
		return fmt.Errorf("%w: empty folder_path", ErrInvalidInstallRequest)
	}
	if strings.TrimSpace(r.Reason) == "" {
		return fmt.Errorf("%w: empty reason", ErrInvalidInstallRequest)
	}
	if len(r.Reason) > MaxReasonLength {
		return fmt.Errorf("%w: reason has %d bytes (max %d)", ErrInvalidInstallRequest, len(r.Reason), MaxReasonLength)
	}
	if len(r.OperatorIDHint) > MaxOperatorIDLength {
		return fmt.Errorf("%w: operator_id_hint has %d bytes (max %d)", ErrInvalidInstallRequest, len(r.OperatorIDHint), MaxOperatorIDLength)
	}
	return nil
}

// InstallerDeps bundles the required + optional dependencies for
// [NewInstaller].
type InstallerDeps struct {
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

	// DataDir is the deployment-level data root the on-disk paths
	// are composed under (`<DataDir>/tools/<source>/<tool>/` for
	// live, `<DataDir>/_history/<source>/<tool>/<version>/` for
	// snapshots). Required, non-empty.
	DataDir string

	// Logger receives diagnostic log entries. Optional; nil discards.
	Logger Logger
}

// Installer is the M9.5 local-patch install orchestrator. Construct
// via [NewInstaller]; the zero value is not usable. Safe for
// concurrent use across goroutines: distinct (source, tool) targets
// run in parallel; concurrent invocations on the SAME (source, tool)
// target serialise via the per-target mutex (iter-1 codex M12 fix).
//
// The corollary: a CLI invocation should never need cross-process
// coordination — the Makefile target is one-shot per invocation and
// the per-target mutex inside one process handles the in-process
// goroutine case. Cross-process concurrent installs of the same
// target on the same operator host remain the operator's
// responsibility (file-system locks live outside this package's scope).
type Installer struct {
	deps    InstallerDeps
	targets *targetMutexes

	// correlationSeq is the monotonically-increasing per-Installer
	// nonce woven into `newCorrelationID` so two operations within
	// the same nanosecond produce distinct correlation ids (iter-1
	// codex m9 fix).
	correlationSeq atomic.Uint64
}

// NewInstaller constructs an [*Installer]. Panics with a named-field
// message when any required dependency is nil; mirrors
// `toolregistry.New` / `approval.NewExecutor`.
func NewInstaller(deps InstallerDeps) *Installer {
	if deps.FS == nil {
		panic("localpatch: NewInstaller: deps.FS must not be nil")
	}
	if deps.Publisher == nil {
		panic("localpatch: NewInstaller: deps.Publisher must not be nil")
	}
	if deps.Clock == nil {
		panic("localpatch: NewInstaller: deps.Clock must not be nil")
	}
	if deps.SourceLookup == nil {
		panic("localpatch: NewInstaller: deps.SourceLookup must not be nil")
	}
	if deps.OperatorIdentityResolver == nil {
		panic("localpatch: NewInstaller: deps.OperatorIdentityResolver must not be nil")
	}
	if strings.TrimSpace(deps.DataDir) == "" {
		panic("localpatch: NewInstaller: deps.DataDir must not be empty")
	}
	if len(deps.DataDir) > MaxDataDirLength {
		panic(fmt.Sprintf("localpatch: NewInstaller: deps.DataDir has %d bytes (max %d)", len(deps.DataDir), MaxDataDirLength))
	}
	return &Installer{deps: deps, targets: newTargetMutexes()}
}

// Install runs the install pipeline on `req` and returns the emitted
// [LocalPatchApplied] payload alongside any error. The resolution
// order is documented at the top of this file.
//
// On a successful publish the returned event is the verbatim payload
// the bus saw; on any pre-publish failure the returned event is the
// zero value AND the error is the wrapped sentinel. On a publish
// failure the on-disk install IS committed AND the snapshot IS taken;
// the error wraps [ErrPublishLocalPatchApplied] AND the returned
// event carries the populated payload (so the caller can re-publish
// out-of-band if desired).
func (i *Installer) Install(ctx context.Context, req InstallRequest) (LocalPatchApplied, error) {
	if err := req.Validate(); err != nil {
		return LocalPatchApplied{}, err
	}
	if err := ctx.Err(); err != nil {
		return LocalPatchApplied{}, err
	}
	if err := resolveLocalSource(ctx, i.deps.SourceLookup, req.SourceName); err != nil {
		return LocalPatchApplied{}, err
	}
	operatorID, err := i.resolveOperator(ctx, req.OperatorIDHint)
	if err != nil {
		return LocalPatchApplied{}, err
	}
	folderManifest, err := loadAndValidateFolderManifest(i.deps.FS, req.FolderPath, req.SourceName)
	if err != nil {
		return LocalPatchApplied{}, err
	}

	// Per-target serialisation (iter-1 codex M12 fix). Acquired
	// AFTER folder manifest decode so we know the tool name — the
	// lock key is (source, tool); concurrent installs of DIFFERENT
	// tools under the same source proceed in parallel.
	unlock := i.targets.lock(req.SourceName, folderManifest.Name)
	defer unlock()

	diffHash, err := ContentDigest(i.deps.FS, req.FolderPath)
	if err != nil {
		return LocalPatchApplied{}, err
	}

	// Snapshot the prior live tree (when present) BEFORE the
	// destructive overwrite. The snapshot version comes from the
	// PRIOR manifest — refuse install when the prior tree is
	// undecidable so a rollback target is always available.
	livePath, err := liveToolPath(i.deps.DataDir, req.SourceName, folderManifest.Name)
	if err != nil {
		return LocalPatchApplied{}, err
	}
	priorVersion, hasPrior, err := readLiveVersion(i.deps.FS, livePath, req.SourceName)
	if err != nil {
		return LocalPatchApplied{}, err
	}
	if hasPrior {
		if _, err := snapshotIfPresent(i.deps.FS, i.deps.DataDir, req.SourceName, folderManifest.Name, priorVersion); err != nil {
			return LocalPatchApplied{}, err
		}
	}

	// Write the new tree. By the time replaceLive returns, the live
	// tools directory contains the freshly-installed tool and the
	// snapshot of the prior version (if any) has already landed.
	if err := replaceLive(i.deps.FS, i.deps.DataDir, req.SourceName, folderManifest.Name, req.FolderPath); err != nil {
		return LocalPatchApplied{}, err
	}

	now := i.deps.Clock.Now()
	correlationID := i.newCorrelationID(now)
	event := newLocalPatchAppliedEvent(
		req.SourceName,
		folderManifest.Name,
		folderManifest.Version,
		operatorID,
		req.Reason,
		diffHash,
		OperationInstall,
		now,
		correlationID,
	)
	publishCtx := context.WithoutCancel(ctx)
	if err := i.deps.Publisher.Publish(publishCtx, TopicLocalPatchApplied, event); err != nil {
		i.logErr(ctx, "publish local_patch_applied failed (install)", "source", req.SourceName, "tool", folderManifest.Name, "operator", operatorID)
		return event, fmt.Errorf("%w: %w", ErrPublishLocalPatchApplied, err)
	}
	return event, nil
}

// resolveOperator runs the [OperatorIdentityResolver] and applies the
// length / character-set bounds. Mirrors `approval.Proposer.resolveProposer`.
func (i *Installer) resolveOperator(ctx context.Context, hint string) (string, error) {
	id, err := i.deps.OperatorIdentityResolver(ctx, hint)
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

// readLiveVersion attempts to decode the manifest at
// `<livePath>/manifest.json` and returns its Version field plus a
// presence boolean. Returns `("", false, nil)` when the live tools
// directory does not exist (a brand-new tool); returns
// `("", false, ErrManifestRead)` when the directory exists but the
// manifest is missing / undecidable (the caller MUST refuse the
// install — overwriting an undecidable tree would lose the prior
// contents without a restorable snapshot).
func readLiveVersion(filesystem FS, livePath, sourceName string) (string, bool, error) {
	if _, err := filesystem.Stat(livePath); err != nil {
		// Live tree absent → first install, no prior to snapshot.
		// Any other stat failure is left to the caller's later
		// replaceLive call to surface as ErrLiveWrite (mkdir/copy
		// will hit it on the next access).
		return "", false, nil
	}
	m, err := toolregistry.LoadManifestFromFile(filesystem, livePath, sourceName)
	if err != nil {
		return "", false, fmt.Errorf("%w: prior tree at %q: %w", ErrManifestRead, livePath, err)
	}
	if !validVersion.MatchString(m.Version) {
		return "", false, fmt.Errorf("%w: prior manifest version %q has disallowed characters", ErrManifestRead, m.Version)
	}
	return m.Version, true, nil
}

// logErr forwards a diagnostic message to the optional [Logger].
// Nil-logger safe. PII discipline: caller passes only metadata kv
// pairs.
func (i *Installer) logErr(ctx context.Context, msg string, kv ...any) {
	if i.deps.Logger == nil {
		return
	}
	i.deps.Logger.Log(ctx, msg, kv...)
}

// newCorrelationID returns the process-monotonic identifier for one
// install / rollback cycle. The format is the `Clock.Now().UnixNano()`
// value joined with a per-receiver monotonic nonce (iter-1 critic m9
// fix: two operations within the same nanosecond collide on bare
// UnixNano under concurrent dispatch). Subscribers MUST NOT parse it.
func (i *Installer) newCorrelationID(now time.Time) string {
	return strconv.FormatInt(now.UnixNano(), 10) + "-" + strconv.FormatUint(i.correlationSeq.Add(1), 10)
}

// resolveLocalSource resolves a source via `lookup` AND verifies its
// kind is `local` AND its returned name matches the requested name.
// Shared by [Installer.Install] and [Rollbacker.Rollback] so the
// 3-case branch stays in one place. Errors wrap the appropriate
// sentinel ([ErrUnknownSource] / [ErrInvalidSourceKind] /
// [ErrSourceLookupMismatch]).
func resolveLocalSource(ctx context.Context, lookup SourceLookup, sourceName string) error {
	src, err := lookup(ctx, sourceName)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnknownSource, err)
	}
	if src.Kind != toolregistry.SourceKindLocal {
		return fmt.Errorf("%w: source %q kind %q", ErrInvalidSourceKind, src.Name, src.Kind)
	}
	if src.Name != sourceName {
		return fmt.Errorf("%w: lookup returned %q for request %q", ErrSourceLookupMismatch, src.Name, sourceName)
	}
	return nil
}

// loadAndValidateFolderManifest reads + decodes a folder's manifest
// AND applies the bound + character-allowlist checks to the
// resulting Name and Version. Hoisted out of [Installer.Install] so
// the orchestrator stays under the cyclomatic-complexity budget.
func loadAndValidateFolderManifest(fs FS, folderPath, sourceName string) (toolregistry.Manifest, error) {
	m, err := toolregistry.LoadManifestFromFile(fs, folderPath, sourceName)
	if err != nil {
		return toolregistry.Manifest{}, fmt.Errorf("%w: %w", ErrManifestRead, err)
	}
	if !validIdentifier.MatchString(m.Name) {
		return toolregistry.Manifest{}, fmt.Errorf("%w: manifest tool name %q has disallowed characters", ErrManifestRead, m.Name)
	}
	if len(m.Name) > MaxToolNameLength {
		return toolregistry.Manifest{}, fmt.Errorf("%w: manifest tool name has %d bytes (max %d)", ErrManifestRead, len(m.Name), MaxToolNameLength)
	}
	if !validVersion.MatchString(m.Version) {
		return toolregistry.Manifest{}, fmt.Errorf("%w: manifest version %q has disallowed characters", ErrManifestRead, m.Version)
	}
	if len(m.Version) > MaxVersionLength {
		return toolregistry.Manifest{}, fmt.Errorf("%w: manifest version has %d bytes (max %d)", ErrManifestRead, len(m.Version), MaxVersionLength)
	}
	return m, nil
}
