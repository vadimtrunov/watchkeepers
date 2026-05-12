package toolregistry

import (
	"sort"
	"time"
)

// ShadowedTool is one entry in the slice [BuildEffective] returns
// alongside the [EffectiveToolset]: a manifest whose name collided
// with an earlier-priority source's manifest and was dropped from
// the snapshot. The struct carries enough metadata for a downstream
// subscriber to construct the lead-facing DM documented on
// [ToolShadowed.Message] — both the winner's source/version AND the
// shadowed entry's source/version.
//
// Same-priority semantics: the FIRST source in the [SourceConfig]
// list that contributes a given name wins; every later same-name
// contribution is shadowed in the order it was seen. A single tool
// name shadowed by three lower-priority sources produces three
// distinct [ShadowedTool] entries (the same WinnerSource /
// WinnerVersion repeated, distinct ShadowedSource / ShadowedVersion
// per entry). Intra-source duplicates are NOT surfaced here — they
// land in the [ScanSourceDir] log via
// [ErrIntraSourceDuplicateManifestName] before reaching the
// precedence-flattening loop.
type ShadowedTool struct {
	// ToolName is the [Manifest.Name] of the conflicting tool.
	ToolName string

	// WinnerSource is the [SourceConfig.Name] whose manifest landed
	// in the snapshot.
	WinnerSource string

	// WinnerVersion is the [Manifest.Version] of the winning entry.
	WinnerVersion string

	// ShadowedSource is the [SourceConfig.Name] whose manifest was
	// dropped by precedence flattening.
	ShadowedSource string

	// ShadowedVersion is the [Manifest.Version] of the dropped entry.
	ShadowedVersion string
}

// EffectiveTool is one entry in an [EffectiveToolset]: a manifest
// loaded from a configured source, projected after precedence
// flattening. The struct is intentionally a shallow shape — M9.2
// records same-name conflicts on the side via [ShadowedTool] (returned
// from [BuildEffective] alongside the snapshot) rather than embedding
// per-tool conflict metadata onto every entry.
//
// The [Manifest.Source] field is already stamped by
// [LoadManifestFromFile]; callers can read it directly off the
// manifest without consulting [EffectiveTool] separately. The field
// is duplicated onto [EffectiveTool] purely as a convenience so a
// caller that only has an [EffectiveTool] does not need to deference
// the manifest.
type EffectiveTool struct {
	// Source is the [SourceConfig.Name] that supplied this tool.
	// Equal to [Manifest.Source]; duplicated for ergonomics.
	Source string

	// Manifest is the decoded per-tool manifest. Defensive-copied at
	// [EffectiveToolset] construction so caller mutation post-build
	// cannot bleed into the snapshot.
	Manifest Manifest
}

// EffectiveToolset is an immutable snapshot of the registry's view of
// the platform-wide toolset. Constructed via [newEffectiveToolset] and
// observed via [Registry.Snapshot] / [Registry.Acquire]; callers MUST
// NOT mutate the value through unsafe casts or reflection.
//
// Tools is in stable order — sorted by [Manifest.Name] ascending. The
// internal name→index map enables constant-time [EffectiveToolset.Lookup]
// without exposing it to callers; mutation through the map would break
// the snapshot's immutability guarantee.
type EffectiveToolset struct {
	// Revision is the monotonic version number assigned by
	// [Registry.Recompute]. Strictly increases across successive
	// snapshots; never reused. Subscribers can use it to detect
	// missed updates (e.g. a "last seen rev" gauge) but MUST NOT
	// reason about absolute values across process restarts —
	// revisions reset to 1 on each [NewRegistry].
	Revision int64

	// BuiltAt is the wall-clock timestamp of the [Registry.Recompute]
	// call that produced this snapshot, sourced from [Clock.Now].
	BuiltAt time.Time

	// Tools is the flattened, name-sorted list of tools. Length zero
	// is legitimate (no sources configured OR every source's
	// directory is empty / unreadable). The slice is allocated fresh
	// on construction; callers MUST treat it as read-only.
	Tools []EffectiveTool

	// byName is the constant-time [EffectiveToolset.Lookup] index.
	// Internal — never exposed; mutation through it would corrupt
	// the snapshot.
	byName map[string]int
}

// newEffectiveToolset builds an [EffectiveToolset] from `tools`.
// Every per-tool [Manifest] is deep-copied at construction —
// `Capabilities` (string slice) and `Schema` (byte slice) get fresh
// backing arrays so a caller mutating their input post-build cannot
// bleed into the immutable snapshot. The outer `[]EffectiveTool` is
// also copied. Combined with the no-op `Source` / `Name` /
// `Version` / `Signature` strings (immutable by Go semantics), this
// makes the snapshot a true value-deep-copy.
//
// Sorts by name ascending. Constructs the lookup index. Returns a
// non-nil pointer even when `tools` is empty.
func newEffectiveToolset(revision int64, builtAt time.Time, tools []EffectiveTool) *EffectiveToolset {
	sorted := make([]EffectiveTool, len(tools))
	for i, t := range tools {
		sorted[i] = EffectiveTool{
			Source:   t.Source,
			Manifest: cloneManifest(t.Manifest),
		}
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Manifest.Name < sorted[j].Manifest.Name
	})
	byName := make(map[string]int, len(sorted))
	for i, t := range sorted {
		byName[t.Manifest.Name] = i
	}
	return &EffectiveToolset{
		Revision: revision,
		BuiltAt:  builtAt,
		Tools:    sorted,
		byName:   byName,
	}
}

// cloneManifest returns a deep-copy of `m`. Scalar fields are
// copied by Go value semantics; the reference-typed `Capabilities`
// and `Schema` are duplicated via the manifest.go helpers so the
// returned [Manifest] aliases no backing array of the input.
func cloneManifest(m Manifest) Manifest {
	out := m
	out.Capabilities = cloneStringSlice(m.Capabilities)
	out.Schema = cloneBytes(m.Schema)
	return out
}

// Lookup returns the [EffectiveTool] with the given name and a
// boolean indicating presence. Constant-time against the internal
// name index.
//
// Callers MUST NOT mutate the returned [EffectiveTool] — although
// the value type is passed by value, the embedded [Manifest] carries
// slice / byte-slice fields whose backing arrays are shared with
// every other call site holding the same snapshot. Treat the result
// as read-only.
func (s *EffectiveToolset) Lookup(name string) (EffectiveTool, bool) {
	if s == nil {
		return EffectiveTool{}, false
	}
	idx, ok := s.byName[name]
	if !ok {
		return EffectiveTool{}, false
	}
	return s.Tools[idx], true
}

// Names returns the tool names in the snapshot's stable order
// (alphabetical by [Manifest.Name]). The returned slice is freshly
// allocated — callers can mutate it freely without disturbing the
// snapshot.
func (s *EffectiveToolset) Names() []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s.Tools))
	for i, t := range s.Tools {
		out[i] = t.Manifest.Name
	}
	return out
}

// Len reports the number of tools in the snapshot. Constant-time.
// Convenience over `len(snap.Tools)` for callers that only have an
// [*EffectiveToolset] interface-typed value.
func (s *EffectiveToolset) Len() int {
	if s == nil {
		return 0
	}
	return len(s.Tools)
}
