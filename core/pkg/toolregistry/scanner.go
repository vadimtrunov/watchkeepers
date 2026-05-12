package toolregistry

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"time"
)

// ScanSourceDir walks `<dataDir>/tools/<sourceName>/` for immediate
// child directories, calls [LoadManifestFromFile] on each, and
// returns the decoded manifests in alphabetical order by manifest
// name.
//
// Failure modes:
//
//   - The source directory does not exist on disk: returns
//     `(nil, nil)`. A configured source that has not yet been synced
//     by the [Scheduler] is a legitimate state — the scanner skips it
//     so [BuildEffective] can still produce a coherent snapshot from
//     the other sources.
//   - A [FS.ReadDir] failure OTHER than not-exist: wrapped
//     [ErrScanReadDir] returned. Caller (typically [BuildEffective])
//     logs it and moves on to the next source — one bad source does
//     not poison the whole recompute.
//   - A single per-tool manifest is malformed / missing: logged via
//     `logger` (if non-nil) and skipped. The remaining manifests in
//     the same source still surface.
//
// The `sourceName` is used by [LoadManifestFromFile] both for path
// composition (it is already baked into `dataDir/tools/<sourceName>`)
// AND for the source-stamping rule (manifest.json's `source` field
// must match `sourceName` or be empty — see [LoadManifestFromFile]).
//
// `ctx` is the propagated [Registry.Recompute] cancellation context;
// `logger` is the optional diagnostic sink for per-tool decode
// failures. Nil-logger safe.
//
// Intra-source duplicate manifest names: when two per-tool
// subdirectories within the same source declare the same
// [Manifest.Name], the alphabetically-first toolDir wins
// deterministically and the loser is dropped + logged via
// [ErrIntraSourceDuplicateManifestName]. The deterministic tiebreaker
// comes from pre-sorting `entries` by their on-disk name BEFORE
// decoding — [FS.ReadDir]'s order is not guaranteed across
// implementations so we cannot rely on it. The cross-source variant
// (same name across two sources) is handled by [BuildEffective]'s
// priority flattening, not here.
func ScanSourceDir(ctx context.Context, filesystem FS, dataDir, sourceName string, logger Logger) ([]Manifest, error) {
	if filesystem == nil {
		panic("toolregistry: ScanSourceDir: fs must not be nil")
	}
	sourceDir := filepath.Join(dataDir, "tools", sourceName)
	entries, err := filesystem.ReadDir(sourceDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: %q: %w", ErrScanReadDir, sourceDir, err)
	}

	// Deterministic iteration order — [FS.ReadDir]'s order is
	// implementation-defined (Linux ext4, macOS APFS, and Go's
	// in-memory fakes all differ). Pre-sorting by entry name pins
	// the intra-source duplicate tiebreaker to "alpha-first toolDir
	// wins" regardless of the underlying file system.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	seen := make(map[string]string, len(entries))
	manifests := make([]Manifest, 0, len(entries))
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !e.IsDir() {
			continue
		}
		toolDir := filepath.Join(sourceDir, e.Name())
		m, err := LoadManifestFromFile(filesystem, toolDir, sourceName)
		if err != nil {
			logScanFailure(ctx, logger, sourceName, e.Name(), err)
			continue
		}
		if firstDir, dup := seen[m.Name]; dup {
			logIntraSourceDuplicate(ctx, logger, sourceName, m.Name, firstDir, e.Name())
			continue
		}
		seen[m.Name] = e.Name()
		manifests = append(manifests, m)
	}
	// Stable sort by manifest name. Since `entries` was pre-sorted
	// by toolDir name and the intra-source dedupe above retained the
	// first occurrence, the relative order of equal-name (impossible
	// after dedupe) and the secondary order are both deterministic.
	sort.SliceStable(manifests, func(i, j int) bool {
		return manifests[i].Name < manifests[j].Name
	})
	return manifests, nil
}

// logScanFailure emits a redaction-safe diagnostic for a per-tool
// manifest failure. The toolDir name is the on-disk path component
// (operator-controlled today; future M9.4 / M9.5 will land
// AI-authored / lead-clicked manifest paths here — at that point the
// redaction discipline of `tool_dir` SHOULD be revisited because an
// AI-authored name could carry an injection payload designed to
// poison subscriber logs).
//
// The error TYPE is the leaf type via [leafErrType]; the underlying
// error MESSAGE is NOT logged because [LoadManifestFromFile] wraps a
// [FS.ReadFile] error whose message may contain a fully-qualified
// path.
func logScanFailure(ctx context.Context, logger Logger, sourceName, toolDir string, err error) {
	if logger == nil {
		return
	}
	logger.Log(
		ctx, "toolregistry: manifest scan failed",
		"source", sourceName,
		"tool_dir", toolDir,
		"err_type", leafErrType(err),
	)
}

// logIntraSourceDuplicate logs the deterministic-drop of a
// duplicate manifest name within a single configured source. Both
// the winning toolDir (alpha-first) and the losing toolDir are
// surfaced so an operator can immediately see "two tool directories
// claim the same name, the registry kept this one."
func logIntraSourceDuplicate(ctx context.Context, logger Logger, sourceName, manifestName, winnerDir, loserDir string) {
	if logger == nil {
		return
	}
	logger.Log(
		ctx, "toolregistry: "+ErrIntraSourceDuplicateManifestName.Error(),
		"source", sourceName,
		"manifest_name", manifestName,
		"winner_tool_dir", winnerDir,
		"loser_tool_dir", loserDir,
	)
}

// BuildEffective scans every configured source's directory, flattens
// the manifests by name in source-priority order (earlier source
// wins for a duplicated name), and returns the resulting
// [EffectiveToolset] alongside the list of [ShadowedTool] entries
// dropped by the flattening. The returned snapshot's [EffectiveToolset.Revision]
// is zero — callers that need a monotonic revision (the
// [Registry]) stamp it post-construction; standalone callers
// (M9.4 dry-run, M9.5 local-patch validation) ignore the field.
// `builtAt` is the timestamp imprinted on the snapshot.
//
// Shadow detection (M9.2): when the same [Manifest.Name] appears in
// two configured sources, the EARLIER-listed source's manifest
// becomes the winner and the LATER source's manifest is appended to
// the returned `shadows` slice (in the order it was detected) instead
// of the snapshot. Three lower-priority sources contributing the same
// name produce three entries with the same WinnerSource/Version
// repeated. Intra-source duplicates are NOT surfaced here — they
// land in the [ScanSourceDir] log via
// [ErrIntraSourceDuplicateManifestName] before reaching this loop.
//
// Per-source ReadDir errors are logged (via `logger`) and the source
// contributes zero manifests; the rest of the sources still build.
// This isolation is intentional: a single corrupt source must not
// brick the runtime's effective toolset.
func BuildEffective(
	ctx context.Context,
	filesystem FS,
	dataDir string,
	sources []SourceConfig,
	builtAt time.Time,
	logger Logger,
) (*EffectiveToolset, []ShadowedTool, error) {
	if filesystem == nil {
		panic("toolregistry: BuildEffective: fs must not be nil")
	}
	// Capacity hints — a tool name index that anticipates one
	// manifest per source per "tools-per-source" bucket of 8 keeps
	// the map from rehashing across the loop. The merged + shadows
	// slices share the same bound because (sources × tools_per_src)
	// is the worst-case combined cardinality and the split between
	// them depends on conflict density.
	hint := len(sources) * 8
	winnerIdx := make(map[string]int, hint)
	merged := make([]EffectiveTool, 0, hint)
	shadows := make([]ShadowedTool, 0)

	for _, src := range sources {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		manifests, err := ScanSourceDir(ctx, filesystem, dataDir, src.Name, logger)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, nil, err
			}
			logBuildSourceFailure(ctx, logger, src.Name, err)
			continue
		}
		for _, m := range manifests {
			if idx, dup := winnerIdx[m.Name]; dup {
				winner := merged[idx]
				shadows = append(shadows, ShadowedTool{
					ToolName:        m.Name,
					WinnerSource:    winner.Source,
					WinnerVersion:   winner.Manifest.Version,
					ShadowedSource:  src.Name,
					ShadowedVersion: m.Version,
				})
				continue
			}
			merged = append(merged, EffectiveTool{
				Source:   src.Name,
				Manifest: m,
			})
			winnerIdx[m.Name] = len(merged) - 1
		}
	}
	return newEffectiveToolset(0, builtAt, merged), shadows, nil
}

// logBuildSourceFailure logs the per-source ReadDir failure surfaced
// by [ScanSourceDir]. Same redaction discipline as
// [logScanFailure] — error TYPE only, no message body.
func logBuildSourceFailure(ctx context.Context, logger Logger, sourceName string, err error) {
	if logger == nil {
		return
	}
	logger.Log(
		ctx, "toolregistry: source scan failed",
		"source", sourceName,
		"err_type", leafErrType(err),
	)
}
