// Doc-block at file head documenting the [Exporter] seam contract.
//
// resolution order: nil-dep check (panic) → ExportRequest.Validate
// (ErrInvalidExportRequest pass-through) → ctx.Err pass-through →
// SourceLookup (ErrUnknownSource / ErrInvalidSourceKind /
// ErrSourceLookupMismatch) → OperatorIdentityResolver
// (ErrIdentityResolution / ErrEmptyResolvedIdentity /
// ErrInvalidOperatorID) → live tool manifest read (ErrManifestRead /
// ErrToolMissing) → destination emptiness check
// (ErrDestinationNotEmpty / ErrDestinationWrite) → live tree
// ContentDigest (ErrSourceRead) → copy tree to destination
// (ErrDestinationWrite) → Publish [TopicHostedToolExported] with
// [context.WithoutCancel] (durable record contract — a mid-flight
// cancel must not split the on-disk-then-publish pipeline;
// M9.4.b webhook iter-1 M1 lesson).
//
// audit discipline: the exporter never imports `keeperslog` and
// never calls `.Append(` (see source-grep AC). The audit log entry
// for `hosted_tool_exported` lives in the M9.7 audit subscriber
// that observes [TopicHostedToolExported].
//
// PII discipline: the optional [Logger] is invoked with the source /
// tool / operator id ONLY — never with the live tool source bytes,
// the operator-supplied destination path, or the raw
// [ExportRequest.Reason]. The [HostedToolExported] payload allowlist
// pins the bus boundary.

package hostedexport

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/localpatch"
	"github.com/vadimtrunov/watchkeepers/core/pkg/toolregistry"
)

// FS is the filesystem seam consumed by [Exporter.Export]. Aliased
// to [localpatch.FS] for one source of truth; production wiring uses
// [localpatch.OSFS]; tests substitute a hand-rolled in-memory fake
// satisfying the same interface.
type FS = localpatch.FS

// OSFS is the production [FS] implementation, re-exported from
// localpatch so callers of this package never have to reach into
// the sibling package directly. Mirrors how multiple core packages
// re-export `toolregistry.OSFS`.
type OSFS = localpatch.OSFS

// Bounds enforced by [ExportRequest.Validate]. The numbers defend
// against an unbounded operator-supplied string landing on the
// [HostedToolExported] payload. Identical bounds to
// `localpatch.{MaxSourceNameLength, ...}` for cross-package
// consistency; redefined here so a future drift on either side
// surfaces in the per-package test rather than silently coupling.
const (
	// MaxSourceNameLength bounds [ExportRequest.SourceName].
	MaxSourceNameLength = 64

	// MaxToolNameLength bounds [ExportRequest.ToolName].
	MaxToolNameLength = 64

	// MaxOperatorIDLength bounds the resolved operator identity.
	MaxOperatorIDLength = 64

	// MaxReasonLength bounds [ExportRequest.Reason].
	MaxReasonLength = 1024

	// MaxDataDirLength bounds [ExporterDeps.DataDir]. A 4 KiB
	// DataDir composes into a multi-KiB final path the OS rejects
	// with a generic `ENAMETOOLONG` rather than a clean
	// [ErrUnsafePath]; bounding at the construction boundary lands
	// a stable diagnostic. Mirror `localpatch.MaxDataDirLength`.
	MaxDataDirLength = 1024

	// MaxDestinationLength bounds [ExportRequest.Destination]. Same
	// rationale as [MaxDataDirLength].
	MaxDestinationLength = 1024
)

// validIdentifier is the character allowlist applied to source names
// and tool names. Mirrors `localpatch.validIdentifier` /
// `toolregistry`'s `validSourceName` exactly.
var validIdentifier = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// validOperatorID is the character allowlist applied to a resolved
// operator identity. Mirrors `localpatch.validOperatorID`.
var validOperatorID = regexp.MustCompile(`^[a-zA-Z0-9_.@:-]+$`)

// ValidateOperatorID applies the same bound + character-allowlist
// the [Exporter] applies post-resolver. Exposed so a CLI / wrapper
// can refuse a malformed `--operator` value BEFORE it reaches a
// wrapped error string on stderr (mirror M9.5 iter-1 critic M2
// fix). Returns [ErrInvalidOperatorID] on failure; nil otherwise.
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

// Publisher is the [eventbus.Bus] subset [Exporter.Export] consumes.
// Defined locally so tests substitute a hand-rolled fake and
// production wiring never has to import the concrete `*eventbus.Bus`.
// Mirror `localpatch.Publisher` / `toolregistry.Publisher`.
type Publisher interface {
	Publish(ctx context.Context, topic string, event any) error
}

// Clock is the time seam. Mirror `localpatch.Clock`.
type Clock interface {
	Now() time.Time
}

// ClockFunc adapts a plain `func() time.Time` to [Clock]. Mirror
// `localpatch.ClockFunc`.
type ClockFunc func() time.Time

// Now implements [Clock].
func (f ClockFunc) Now() time.Time { return f() }

// Logger is the optional structured-log seam. Mirror
// `localpatch.Logger`. PII discipline: implementations MUST NOT
// include the operator-supplied destination path, the operator-
// supplied reason, OR the live tool source bytes.
type Logger interface {
	Log(ctx context.Context, msg string, kv ...any)
}

// SourceLookup is the per-call seam for resolving a configured
// [toolregistry.SourceConfig] by name. Mirror
// `localpatch.SourceLookup`.
//
// Contract:
//
//   - Return the resolved [toolregistry.SourceConfig] on success.
//   - Return [ErrUnknownSource] (or any wrapping of it) when the
//     source name does not match any configured source.
//   - Returning a [toolregistry.SourceConfig] whose
//     [toolregistry.SourceConfig.Kind] is NOT
//     [toolregistry.SourceKindHosted] surfaces [ErrInvalidSourceKind].
type SourceLookup func(ctx context.Context, sourceName string) (toolregistry.SourceConfig, error)

// OperatorIdentityResolver is the per-call seam for resolving the
// operator identity. Mirror `localpatch.OperatorIdentityResolver`.
//
// Contract:
//
//   - Return a non-empty identifier on success.
//   - Return `("", <cause>)` on resolution failure. [Exporter.Export]
//     wraps the cause with [ErrIdentityResolution]; implementers
//     MUST NOT pre-wrap.
//   - Returning `("", nil)` is a programmer error caught as
//     [ErrEmptyResolvedIdentity].
//
// Bound discipline: the resolved identifier MUST be at most
// [MaxOperatorIDLength] bytes AND match [validOperatorID].
type OperatorIdentityResolver func(ctx context.Context, hint string) (string, error)

// ExportRequest is the [Exporter.Export] input.
type ExportRequest struct {
	// SourceName is the configured `kind: hosted` source the export
	// reads from. Required; non-empty; bounded; allowlisted.
	SourceName string

	// ToolName is the manifest name of the tool to export. Required;
	// non-empty; bounded; allowlisted.
	ToolName string

	// Destination is the operator-supplied absolute path to write
	// the bundle into. Required; non-empty; bounded; MUST be absent
	// OR empty at export time (refuse-on-non-empty discipline).
	Destination string

	// Reason is the operator-supplied audit text. Required; non-
	// empty; bounded [MaxReasonLength].
	Reason string

	// OperatorIDHint is the operator's self-declared id, passed
	// through to [OperatorIdentityResolver] as a hint.
	OperatorIDHint string
}

// Validate enforces the shape contract on an [ExportRequest].
// Returns [ErrInvalidExportRequest] wrapped with field context.
func (r ExportRequest) Validate() error {
	if strings.TrimSpace(r.SourceName) == "" {
		return fmt.Errorf("%w: empty source_name", ErrInvalidExportRequest)
	}
	if len(r.SourceName) > MaxSourceNameLength {
		return fmt.Errorf("%w: source_name %d bytes (max %d)", ErrInvalidExportRequest, len(r.SourceName), MaxSourceNameLength)
	}
	if !validIdentifier.MatchString(r.SourceName) {
		return fmt.Errorf("%w: source_name disallowed characters", ErrInvalidExportRequest)
	}
	if strings.TrimSpace(r.ToolName) == "" {
		return fmt.Errorf("%w: empty tool_name", ErrInvalidExportRequest)
	}
	if len(r.ToolName) > MaxToolNameLength {
		return fmt.Errorf("%w: tool_name %d bytes (max %d)", ErrInvalidExportRequest, len(r.ToolName), MaxToolNameLength)
	}
	if !validIdentifier.MatchString(r.ToolName) {
		return fmt.Errorf("%w: tool_name disallowed characters", ErrInvalidExportRequest)
	}
	if strings.TrimSpace(r.Destination) == "" {
		return fmt.Errorf("%w: empty destination", ErrInvalidExportRequest)
	}
	if len(r.Destination) > MaxDestinationLength {
		return fmt.Errorf("%w: destination %d bytes (max %d)", ErrInvalidExportRequest, len(r.Destination), MaxDestinationLength)
	}
	if !filepath.IsAbs(r.Destination) {
		return fmt.Errorf("%w: destination must be absolute", ErrInvalidExportRequest)
	}
	if strings.TrimSpace(r.Reason) == "" {
		return fmt.Errorf("%w: empty reason", ErrInvalidExportRequest)
	}
	if len(r.Reason) > MaxReasonLength {
		return fmt.Errorf("%w: reason %d bytes (max %d)", ErrInvalidExportRequest, len(r.Reason), MaxReasonLength)
	}
	return nil
}

// ExporterDeps groups the [Exporter] constructor's required +
// optional dependencies. Every field marked required is checked at
// [NewExporter] time — passing a nil [FS] / [Publisher] / [Clock] /
// [SourceLookup] / [OperatorIdentityResolver] panics with a named-
// field message so a wiring bug surfaces at startup, not on first
// export. [Logger] is optional.
type ExporterDeps struct {
	// FS is the filesystem seam. Required.
	FS FS

	// Publisher emits [TopicHostedToolExported] events. Required.
	Publisher Publisher

	// Clock sources [HostedToolExported.ExportedAt] and the
	// correlation-id nanosecond suffix. Required.
	Clock Clock

	// SourceLookup resolves [ExportRequest.SourceName] to a
	// [toolregistry.SourceConfig]. Required.
	SourceLookup SourceLookup

	// OperatorIdentityResolver resolves the operator id from the
	// request context + [ExportRequest.OperatorIDHint]. Required.
	OperatorIdentityResolver OperatorIdentityResolver

	// Logger receives metadata-only diagnostic log entries on the
	// publish-failure path. Optional. PII discipline: never the
	// operator-supplied destination or reason; never tool source
	// bytes.
	Logger Logger

	// DataDir is the deployment's data-root path. The hosted tool
	// tree lives at `<DataDir>/tools/<source>/<tool>/`. Required;
	// non-empty; bounded [MaxDataDirLength]; absolute (relative
	// data dirs are a wiring bug — the M9.1.a config loader rejects
	// them upstream).
	DataDir string
}

// Exporter is the [Exporter.Export] orchestrator.
type Exporter struct {
	fs               FS
	publisher        Publisher
	clock            Clock
	sourceLookup     SourceLookup
	identityResolver OperatorIdentityResolver
	logger           Logger
	dataDir          string

	// nonce is a per-Exporter atomic counter joined onto the
	// nanosecond timestamp inside [Exporter.newCorrelationID].
	// Mirror M9.5 iter-1 critic m6 fix: same-nanosecond dispatch on
	// distinct (source, tool) targets must produce distinct
	// correlation ids. A per-Exporter counter (not per-target) is
	// sufficient because correlation ids are scoped to the
	// receiver-instance lifetime.
	nonce uint64
}

// NewExporter constructs an [Exporter] from [ExporterDeps]. Panics
// on nil required deps (FS / Publisher / Clock / SourceLookup /
// OperatorIdentityResolver) so a wiring bug surfaces at startup.
// Panics on empty / over-bound / relative [ExporterDeps.DataDir]
// (the M9.1.a config loader's invariant — surfaced here as belt-
// and-braces).
func NewExporter(deps ExporterDeps) *Exporter {
	if deps.FS == nil {
		panic("hostedexport: NewExporter: deps.FS must not be nil")
	}
	if deps.Publisher == nil {
		panic("hostedexport: NewExporter: deps.Publisher must not be nil")
	}
	if deps.Clock == nil {
		panic("hostedexport: NewExporter: deps.Clock must not be nil")
	}
	if deps.SourceLookup == nil {
		panic("hostedexport: NewExporter: deps.SourceLookup must not be nil")
	}
	if deps.OperatorIdentityResolver == nil {
		panic("hostedexport: NewExporter: deps.OperatorIdentityResolver must not be nil")
	}
	if strings.TrimSpace(deps.DataDir) == "" {
		panic("hostedexport: NewExporter: deps.DataDir must not be empty")
	}
	if len(deps.DataDir) > MaxDataDirLength {
		panic(fmt.Sprintf("hostedexport: NewExporter: deps.DataDir %d bytes (max %d)", len(deps.DataDir), MaxDataDirLength))
	}
	if !filepath.IsAbs(deps.DataDir) {
		panic("hostedexport: NewExporter: deps.DataDir must be absolute")
	}
	return &Exporter{
		fs:               deps.FS,
		publisher:        deps.Publisher,
		clock:            deps.Clock,
		sourceLookup:     deps.SourceLookup,
		identityResolver: deps.OperatorIdentityResolver,
		logger:           deps.Logger,
		dataDir:          deps.DataDir,
	}
}

// ExportResult is the [Exporter.Export] return value.
type ExportResult struct {
	// ToolVersion is the [toolregistry.Manifest.Version] of the
	// exported tool, read from the live tree's `manifest.json`.
	ToolVersion string

	// BundleDigest is the lower-hex SHA256 of the canonicalised
	// exported bundle. Always 64 chars. Same content as
	// [HostedToolExported.BundleDigest].
	BundleDigest string

	// ExportedAt is the wall-clock timestamp captured AFTER the
	// destination write committed. Same content as
	// [HostedToolExported.ExportedAt].
	ExportedAt time.Time

	// CorrelationID is process-monotonic identifier for this
	// operation. Same content as [HostedToolExported.CorrelationID].
	CorrelationID string
}

// Export reads the configured hosted-source's tool tree under
// `<DataDir>/tools/<source>/<tool>/`, writes a self-contained
// bundle to [ExportRequest.Destination], and emits
// [TopicHostedToolExported]. See the file-head doc-block for the
// resolution order + audit + PII discipline.
//
// sequence of validate-then-side-effect steps; mirroring M9.5's
// localpatch.Installer.Install shape. Decomposing into helpers
// would obscure the resolution-order discipline.
//
//nolint:gocyclo // Export's high cyclomatic complexity is a straight-line
func (e *Exporter) Export(ctx context.Context, req ExportRequest) (ExportResult, error) {
	if err := req.Validate(); err != nil {
		return ExportResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return ExportResult{}, err
	}

	src, err := e.sourceLookup(ctx, req.SourceName)
	if err != nil {
		// Iter-1 m5 fix (reviewer A): only wrap-as-ErrUnknownSource
		// when the resolver actually signalled "unknown". Other
		// causes (DB outage, config-read failure) propagate
		// verbatim so the operator's errors.Is triage routes
		// correctly.
		if errors.Is(err, ErrUnknownSource) {
			return ExportResult{}, err
		}
		return ExportResult{}, fmt.Errorf("source lookup %q: %w", req.SourceName, err)
	}
	if src.Name != req.SourceName {
		return ExportResult{}, fmt.Errorf("%w: requested %q got %q", ErrSourceLookupMismatch, req.SourceName, src.Name)
	}
	if src.Kind != toolregistry.SourceKindHosted {
		return ExportResult{}, fmt.Errorf("%w: source %q kind=%q", ErrInvalidSourceKind, req.SourceName, src.Kind)
	}

	operatorID, err := e.identityResolver(ctx, req.OperatorIDHint)
	if err != nil {
		return ExportResult{}, fmt.Errorf("%w: %w", ErrIdentityResolution, err)
	}
	if strings.TrimSpace(operatorID) == "" {
		return ExportResult{}, ErrEmptyResolvedIdentity
	}
	if vErr := ValidateOperatorID(operatorID); vErr != nil {
		return ExportResult{}, vErr
	}

	livePath, err := liveToolPath(e.dataDir, req.SourceName, req.ToolName)
	if err != nil {
		return ExportResult{}, err
	}

	if _, statErr := e.fs.Stat(livePath); statErr != nil {
		if errors.Is(statErr, fs.ErrNotExist) {
			return ExportResult{}, fmt.Errorf("%w: %q", ErrToolMissing, livePath)
		}
		return ExportResult{}, fmt.Errorf("%w: stat live %q: %w", ErrSourceRead, livePath, statErr)
	}

	manifestPath := filepath.Join(livePath, "manifest.json")
	manifestBytes, err := e.fs.ReadFile(manifestPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ExportResult{}, fmt.Errorf("%w: manifest.json absent at %q", ErrManifestRead, manifestPath)
		}
		return ExportResult{}, fmt.Errorf("%w: read %q: %w", ErrManifestRead, manifestPath, err)
	}
	manifest, err := toolregistry.DecodeManifest(manifestBytes)
	if err != nil {
		return ExportResult{}, fmt.Errorf("%w: %w", ErrManifestRead, err)
	}

	cleanDest := filepath.Clean(req.Destination)
	if err := assertDestinationEmpty(e.fs, cleanDest); err != nil {
		return ExportResult{}, err
	}

	digest, err := localpatch.ContentDigest(e.fs, livePath)
	if err != nil {
		return ExportResult{}, fmt.Errorf("%w: %w", ErrSourceRead, err)
	}

	if err := copyTreeRefuseSymlinks(e.fs, livePath, cleanDest); err != nil {
		return ExportResult{}, err
	}

	exportedAt := e.clock.Now()
	correlationID := e.newCorrelationID(exportedAt)

	event := newHostedToolExportedEvent(
		req.SourceName, req.ToolName, manifest.Version,
		operatorID, req.Reason, digest,
		exportedAt, correlationID,
	)

	publishCtx := context.WithoutCancel(ctx)
	if err := e.publisher.Publish(publishCtx, TopicHostedToolExported, event); err != nil {
		if e.logger != nil {
			e.logger.Log(
				ctx, "hostedexport: publish failed",
				"source", req.SourceName,
				"tool", req.ToolName,
				"version", manifest.Version,
				"operator_id", operatorID,
				"err_class", classify(err),
			)
		}
		return ExportResult{
			ToolVersion:   manifest.Version,
			BundleDigest:  digest,
			ExportedAt:    exportedAt,
			CorrelationID: correlationID,
		}, fmt.Errorf("%w: %w", ErrPublishHostedToolExported, err)
	}

	return ExportResult{
		ToolVersion:   manifest.Version,
		BundleDigest:  digest,
		ExportedAt:    exportedAt,
		CorrelationID: correlationID,
	}, nil
}

// newCorrelationID returns a process-monotonic identifier. Iter-1
// pattern carried forward from M9.5: joined `Clock.Now().UnixNano()`
// with a per-Exporter atomic nonce to defend against
// same-nanosecond dispatch on distinct (source, tool) targets.
func (e *Exporter) newCorrelationID(now time.Time) string {
	nonce := atomic.AddUint64(&e.nonce, 1)
	return strconv.FormatInt(now.UnixNano(), 10) + "-" + strconv.FormatUint(nonce, 10)
}

// classify returns a short error class for diagnostic logging.
// Keeps the operator-supplied reason / destination out of log
// output. Mirror M9.5 `localpatch`-style classification.
func classify(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "ctx_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "ctx_deadline"
	default:
		return "publish"
	}
}

// liveToolPath returns the absolute on-disk directory for the
// hosted tool's live tree. Same path layout as
// `localpatch.liveToolPath`. Belt-and-braces over the per-field
// allowlists; firing [ErrUnsafePath] indicates either a successful
// traversal attempt OR a programmer error.
func liveToolPath(dataDir, sourceName, toolName string) (string, error) {
	if strings.TrimSpace(dataDir) == "" {
		return "", fmt.Errorf("%w: empty data dir", ErrUnsafePath)
	}
	for _, part := range []struct{ kind, value string }{
		{"source", sourceName},
		{"tool", toolName},
	} {
		if !validIdentifier.MatchString(part.value) {
			return "", fmt.Errorf("%w: %s %q", ErrUnsafePath, part.kind, part.value)
		}
	}
	parent := filepath.Clean(filepath.Join(dataDir, "tools", sourceName))
	cand := filepath.Clean(filepath.Join(parent, toolName))
	rel, err := filepath.Rel(parent, cand)
	if err != nil || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." || filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: live tool %q/%q", ErrUnsafePath, sourceName, toolName)
	}
	return cand, nil
}

// assertDestinationEmpty refuses if `dest` exists AND is non-empty.
// Absent destination is accepted (the export will create it via
// [FS.MkdirAll] downstream). An existing-but-empty destination is
// also accepted — operators commonly `mkdir -p` ahead of time.
// Mirror M9.5 install's "refuse-when-undecidable" discipline.
func assertDestinationEmpty(filesystem FS, dest string) error {
	info, err := filesystem.Stat(dest)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("%w: stat destination: %w", ErrDestinationWrite, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: destination is not a directory", ErrDestinationWrite)
	}
	entries, err := filesystem.ReadDir(dest)
	if err != nil {
		return fmt.Errorf("%w: readdir destination: %w", ErrDestinationWrite, err)
	}
	if len(entries) > 0 {
		return fmt.Errorf("%w: %d entries", ErrDestinationNotEmpty, len(entries))
	}
	return nil
}

// copyTreeRefuseSymlinks copies every regular file under `src` to
// `dst`, preserving the relative directory layout and the
// executable bit. Symlinks (and other non-regular non-directory
// entries) are SKIPPED silently — same refusal-to-follow discipline
// as the M9.5 [localpatch.ContentDigest] walker. Returns a wrapped
// [ErrDestinationWrite] / [ErrSourceRead] depending on which side
// failed.
func copyTreeRefuseSymlinks(filesystem FS, src, dst string) error {
	if err := filesystem.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("%w: mkdir %q: %w", ErrDestinationWrite, dst, err)
	}
	return walkAndCopy(filesystem, src, src, dst)
}

// over fs.DirEntry types. Each case is a leaf operation.
//
//nolint:gocyclo // Recursive walker; complexity is structural switch
func walkAndCopy(filesystem FS, root, dir, dstRoot string) error {
	entries, err := filesystem.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("%w: readdir %q: %w", ErrSourceRead, dir, err)
	}
	for _, e := range entries {
		t := e.Type()
		full := filepath.Join(dir, e.Name())
		switch {
		case t.IsDir():
			if err := walkAndCopy(filesystem, root, full, dstRoot); err != nil {
				return err
			}
		case t&fs.ModeSymlink != 0:
			// Skip — refuse-to-follow.
		case t.IsRegular() || t == 0:
			// Iter-1 m22 fix (reviewer B): avoid per-file Lstat when
			// ReadDir already typed the entry as a regular file.
			// Only fall back when Type() returned 0 (FS impls that
			// don't fill the mode bits from ReadDir). Mirror the
			// M9.5 m1 iter-1 fix.
			var info fs.FileInfo
			if t == 0 {
				info, err = e.Info()
				if err != nil {
					return fmt.Errorf("%w: info %q: %w", ErrSourceRead, full, err)
				}
				if !info.Mode().IsRegular() {
					continue
				}
			} else {
				// Type() reported regular — still need Info() for the
				// exec-bit.
				info, err = e.Info()
				if err != nil {
					return fmt.Errorf("%w: info %q: %w", ErrSourceRead, full, err)
				}
			}
			content, err := filesystem.ReadFile(full)
			if err != nil {
				return fmt.Errorf("%w: readfile %q: %w", ErrSourceRead, full, err)
			}
			rel, err := filepath.Rel(root, full)
			if err != nil {
				return fmt.Errorf("%w: rel %q: %w", ErrSourceRead, full, err)
			}
			dstPath := filepath.Join(dstRoot, filepath.FromSlash(filepath.ToSlash(rel)))
			if err := filesystem.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
				return fmt.Errorf("%w: mkdir %q: %w", ErrDestinationWrite, filepath.Dir(dstPath), err)
			}
			mode := fs.FileMode(0o644)
			if info.Mode()&0o111 != 0 {
				mode = 0o755
			}
			if err := filesystem.WriteFile(dstPath, content, mode); err != nil {
				return fmt.Errorf("%w: write %q: %w", ErrDestinationWrite, dstPath, err)
			}
		default:
			// Skip non-regular, non-directory entries.
		}
	}
	return nil
}
