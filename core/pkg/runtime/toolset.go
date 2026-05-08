package runtime

// ToolEntry pairs a tool name with its manifest-declared version. The
// version is OPTIONAL: legacy `manifest_version.tools` jsonb rows
// predating per-tool versioning (anything written before M5.6.e.a) carry
// no `version` field and surface here with [ToolEntry.Version] == "".
// Version-aware callers (the M5.6.e.b boot-time superseded-lesson scan)
// project this value through to recall queries that compare against the
// active manifest's per-tool version.
type ToolEntry struct {
	// Name is the manifest-declared tool name (e.g. "sandbox.exec").
	// Required at decode time — an empty Name surfaces as
	// `manifest: toolset: entry N has empty name` per the M5.6.e.a
	// loader contract on [github.com/vadimtrunov/watchkeepers/core/pkg/manifest.LoadManifest].
	Name string

	// Version is the OPTIONAL manifest-declared tool version (e.g.
	// "v1.2.3"). Empty for legacy rows; populated when the
	// manifest_version.tools entry carries a `version` field. The
	// runtime forwards Version verbatim through [ToolCall.ToolVersion]
	// and the M5.6.b reflector to per-tool lesson rows.
	Version string
}

// Toolset is the manifest-projected per-agent tool list. It carries
// both names and (optional) versions. Consumers that historically
// consumed the [Manifest.Toolset] []string shape (the M5.5.b.a ACL
// gate, the LLM request builder's `tools` projection, every test
// fixture that iterates the field) call [Toolset.Names] to recover
// the prior shape without changing call-site signatures. Version-aware
// callers (M5.6.e.b boot-time superseded-lesson scan) iterate the
// slice directly.
//
// Empty / nil Toolset still means "no tools" per the runtime ACL
// deny-all default documented on [Manifest.Toolset].
type Toolset []ToolEntry

// Names projects the [Toolset] back to the legacy []string of names,
// preserving order. Callers that fed `[]string` to ACL maps, LLM tool
// schemas, or test assertions use this to migrate without altering
// downstream code shape. A nil receiver returns nil; an empty receiver
// returns nil; otherwise the result has len(t) entries (no dedupe —
// duplicate Names propagate as-is so the caller's existing semantics
// are preserved). Order matches the manifest_version.tools jsonb
// array order; the loader does NOT sort.
func (t Toolset) Names() []string {
	if len(t) == 0 {
		return nil
	}
	names := make([]string, len(t))
	for i, e := range t {
		names[i] = e.Name
	}
	return names
}
