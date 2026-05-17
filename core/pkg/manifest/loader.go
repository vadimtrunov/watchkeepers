// Package manifest implements the M5.5 loader that promotes a wire-format
// [keepclient.ManifestVersion] into a portable [runtime.Manifest]. This
// sub-package covers the personality/language slice (template Personality
// and Language into SystemPrompt; forward AgentID verbatim), the toolset
// slice (decode the `tools` jsonb column into the [runtime.Toolset] the
// runtime ACL consults at InvokeTool time, M5.5.b.a; per-tool versions
// project alongside names per M5.6.e.a), the AuthorityMatrix projection
// (M5.5.b.c.c.a), the Autonomy projection (M5.5.b.c.c.a), and the notebook
// recall tunables NotebookTopK / NotebookRelevanceThreshold (M5.5.c.b).
// Notebook open and the Remember built-in tool live in sibling milestones
// (M5.5.c.c, M5.5.d) and do NOT belong here.
package manifest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
	"github.com/vadimtrunov/watchkeepers/core/pkg/runtime"
)

// ManifestFetcher is the single-method interface [LoadManifest] consumes
// for retrieving a [keepclient.ManifestVersion] by manifest UUID. The real
// [keepclient.Client] satisfies it via Go's structural typing; tests
// inject a hand-rolled fake without touching the HTTP client. Mirrors the
// same signature as [keepclient.Client.GetManifest] so the call site
// passes the client through verbatim.
//
// The name intentionally retains the `Manifest` prefix per the TASK M5.5.a
// AC1 contract — the loader's caller-facing surface needs the unambiguous
// `manifest.ManifestFetcher` reading at the import site (where it sits
// next to other Fetcher-shaped interfaces in sibling packages).
//
//nolint:revive // name is fixed by TASK M5.5.a AC1.
type ManifestFetcher interface {
	GetManifest(ctx context.Context, manifestID string) (*keepclient.ManifestVersion, error)
}

// LoadManifest retrieves the manifest_version row identified by manifestID
// via kc and returns the [runtime.Manifest] the runtime needs to boot a
// session. The transformation:
//
//   - SystemPrompt is composed deterministically as
//     base + suffix where base = ManifestVersion.SystemPrompt and suffix
//     appends only non-empty fields, in order, each on its own line —
//     "\n\nPersonality: <p>" then "\nLanguage: <l>". Empty Personality
//     and empty Language produce no orphan headers; an empty Language
//     after a non-empty Personality terminates the suffix cleanly; an
//     empty Personality with non-empty Language still emits the leading
//     blank-line block as "\n\nLanguage: <l>" so the language hint is
//     visually separated from base prose.
//   - AgentID is copied from ManifestVersion.ManifestID (the stable
//     identifier on this surface; agent_id ↔ manifest_id resolution
//     lives in M5.5.b).
//   - Personality and Language are copied verbatim onto runtime.Manifest
//     fields so meta-tools can introspect them after templating.
//
// An empty manifestID returns [runtime.ErrInvalidManifest] synchronously,
// before any fetcher call (mirrors keepclient's ErrInvalidRequest shape).
// Fetcher errors are wrapped as fmt.Errorf("manifest: load: %w", err) so
// callers can errors.Is the underlying sentinel (typically
// [keepclient.ErrNotFound]).
//
// Toolset is decoded from mv.Tools via [decodeToolset]: a JSON array of
// `{"name": string, "version": string}` entries (version OPTIONAL)
// projects to a [runtime.Toolset] carrying both fields per
// [runtime.ToolEntry]; null/empty arrays produce a nil Toolset (the
// deny-all default per runtime.go:99-103). Model is copied verbatim from
// [keepclient.ManifestVersion.Model] onto [runtime.Manifest.Model]; the
// loader does NOT supply a default — empty Model propagates as the empty
// string and downstream [llm.composeBaseFields] is the gate that rejects
// it with [llm.ErrInvalidManifest]. AuthorityMatrix is decoded from
// mv.AuthorityMatrix via [decodeAuthorityMatrix]: a JSON object of
// string→string projects to map[string]string; null/empty jsonb produces
// a nil map (per runtime.go:107 "Nil is fine"). Autonomy is cast from
// [keepclient.ManifestVersion.Autonomy] onto
// [runtime.Manifest.Autonomy] verbatim — empty string propagates as the
// empty [runtime.AutonomyLevel]; the runtime defaults to
// [runtime.AutonomySupervised] downstream per runtime.go:97.
// NotebookTopK and NotebookRelevanceThreshold are copied verbatim from
// [keepclient.ManifestVersion] onto [runtime.Manifest]; zero propagates
// as zero ("unset"). Metadata is not set by this loader; its wiring
// lands in a sibling milestone.
func LoadManifest(ctx context.Context, kc ManifestFetcher, manifestID string) (runtime.Manifest, error) {
	if manifestID == "" {
		return runtime.Manifest{}, runtime.ErrInvalidManifest
	}

	mv, err := kc.GetManifest(ctx, manifestID)
	if err != nil {
		return runtime.Manifest{}, fmt.Errorf("manifest: load: %w", err)
	}

	toolset, err := decodeToolset(mv.Tools)
	if err != nil {
		return runtime.Manifest{}, err
	}

	authorityMatrix, err := decodeAuthorityMatrix(mv.AuthorityMatrix)
	if err != nil {
		return runtime.Manifest{}, err
	}

	immutableCore, err := decodeImmutableCore(mv.ImmutableCore)
	if err != nil {
		return runtime.Manifest{}, err
	}

	return runtime.Manifest{
		AgentID:                    mv.ManifestID,
		SystemPrompt:               composeSystemPrompt(mv.SystemPrompt, mv.Personality, mv.Language),
		Personality:                mv.Personality,
		Language:                   mv.Language,
		Model:                      mv.Model,
		Autonomy:                   runtime.AutonomyLevel(mv.Autonomy),
		Toolset:                    toolset,
		AuthorityMatrix:            authorityMatrix,
		NotebookTopK:               mv.NotebookTopK,
		NotebookRelevanceThreshold: mv.NotebookRelevanceThreshold,
		ImmutableCore:              immutableCore,
	}, nil
}

// decodeToolset projects the manifest_version `tools` jsonb column —
// a JSON array of `{"name": string, "version": string, ...}` objects —
// into the portable [runtime.Toolset]. Per-entry `version` is OPTIONAL:
// legacy rows predating M5.6.e.a omit the field and decode with an
// empty [runtime.ToolEntry.Version]. Capability metadata on each entry
// is still intentionally ignored here; its wiring lands in M5.5.b.b/c.
//
// Empty or null inputs (nil RawMessage, the JSON literal `null`, the
// JSON literal `[]`) all return a nil slice — runtime.go:99-103
// documents "An empty / nil Toolset means 'no tools'", which is the
// deny-all default the harness ACL gate enforces (M5.5.b.a AC6).
//
// Decode failures (malformed JSON, non-string `name`, missing/empty
// `name`, non-string `version`) are wrapped as
// `fmt.Errorf("manifest: toolset: %w", err)` (or the entry-N empty-
// name sentinel format) so callers can errors.Is the underlying
// [json.Unmarshal] failure mode. M5.6.e.a adds the non-string-version
// failure mode; legacy entries without `version` still decode cleanly.
func decodeToolset(raw json.RawMessage) (runtime.Toolset, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var entries []struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("manifest: toolset: %w", err)
	}
	if len(entries) == 0 {
		return nil, nil
	}
	out := make(runtime.Toolset, 0, len(entries))
	for i, e := range entries {
		if e.Name == "" {
			return nil, fmt.Errorf("manifest: toolset: entry %d has empty name", i)
		}
		out = append(out, runtime.ToolEntry{Name: e.Name, Version: e.Version})
	}
	return out, nil
}

// decodeAuthorityMatrix projects the manifest_version `authority_matrix`
// jsonb column — a JSON object of string→string — into the portable
// [runtime.Manifest.AuthorityMatrix] map[string]string the runtime
// consults at lifecycle / approval gates.
//
// Empty or null inputs (nil RawMessage, the JSON literal `null`, the
// JSON literal `{}`) all return a nil map — runtime.go:105-110 documents
// "Nil is fine"; an absent or empty authority_matrix on the wire means
// "no entries" and the runtime treats it as such.
//
// Decode failures (malformed JSON, non-object shapes such as arrays,
// non-string values) are wrapped as
// `fmt.Errorf("manifest: authority_matrix: %w", err)` so callers can
// errors.Is the underlying [json.Unmarshal] failure mode (mirrors the
// `manifest: toolset:` precedent on [decodeToolset]).
func decodeAuthorityMatrix(raw json.RawMessage) (map[string]string, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("manifest: authority_matrix: %w", err)
	}
	if len(m) == 0 {
		return nil, nil
	}
	return m, nil
}

// decodeImmutableCore projects the manifest_version `immutable_core`
// jsonb column — a JSON object carrying the five Phase 2 §M3.1 buckets
// (`role_boundaries`, `security_constraints`, `escalation_protocols`,
// `cost_limits`, `audit_requirements`) — into a typed
// [*runtime.ImmutableCore]. The five canonical buckets decode into
// typed fields; any additional top-level keys ride into
// [runtime.ImmutableCore.Extra] verbatim so a forward-only schema
// extension (Phase 2 §M3.4 `merge_fields` / `rollback`) does not
// silently drop bucket data the M3.1 loader was not yet aware of.
//
// Empty or null inputs (nil RawMessage, the JSON literal `null`) return
// a nil pointer — a row predating M3.1 surfaces as "no immutable core
// declared yet", matching the documented contract on
// [runtime.Manifest.ImmutableCore]. An empty JSON object (`{}`) also
// returns nil so callers can treat "all buckets absent" identically
// across both the SQL-NULL and the explicit-empty-object cases (M3.2
// will tighten this once admin-only editability is enforced; M3.6 will
// reject empty objects from the self-tuning path).
//
// Decode failures (malformed JSON, non-object top-level, wrong bucket
// types) are wrapped as `fmt.Errorf("manifest: immutable_core: %w",
// err)` so callers can errors.Is the underlying [json.Unmarshal]
// failure mode (mirrors the `manifest: toolset:` and
// `manifest: authority_matrix:` precedents).
func decodeImmutableCore(raw json.RawMessage) (*runtime.ImmutableCore, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	// First pass: decode into a map[string]json.RawMessage so unknown
	// keys can ride into Extra without forcing a re-marshal. Rejects
	// non-object top-level (arrays, scalars) via the [json.Unmarshal]
	// type-error path; the server CHECK constraint enforces the same
	// shape at the SQL layer (migration 030) as defense-in-depth.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("manifest: immutable_core: %w", err)
	}
	if len(fields) == 0 {
		return nil, nil
	}

	out := &runtime.ImmutableCore{}
	// Decode each canonical bucket with the bucket's typed shape; an
	// absent key leaves the corresponding field as its zero value
	// (nil slice / nil map). Mirrors the strict-but-tolerant decode
	// pattern from [decodeAuthorityMatrix].
	if err := decodeImmutableCoreBuckets(fields, out); err != nil {
		return nil, err
	}
	// Whatever remains is a forward-compatible bucket the M3.1 loader
	// does not yet recognise; preserve it verbatim so a Phase 2 §M3.4
	// `merge_fields` / `rollback` flow can introspect bucket additions
	// without re-decoding the raw jsonb.
	if len(fields) > 0 {
		out.Extra = fields
	}
	return out, nil
}

// decodeImmutableCoreBuckets pulls each canonical M3.1 bucket out of
// the keyed RawMessage map and projects it onto the typed field of
// out. Recognised buckets are deleted from fields so the caller can
// stash whatever remains onto [runtime.ImmutableCore.Extra]. A
// per-bucket decode failure surfaces as
// `fmt.Errorf("manifest: immutable_core: <bucket>: %w", err)` so the
// caller can pinpoint which bucket misshaped.
//
// Extracted from [decodeImmutableCore] to keep that function under
// the gocyclo budget — five identically-shaped lookup+unmarshal+delete
// triples each contribute to cyclomatic complexity, so factoring them
// out is a structural simplification not a code-smell-hide.
func decodeImmutableCoreBuckets(fields map[string]json.RawMessage, out *runtime.ImmutableCore) error {
	if err := decodeBucket(fields, "role_boundaries", &out.RoleBoundaries); err != nil {
		return err
	}
	if err := decodeBucket(fields, "security_constraints", &out.SecurityConstraints); err != nil {
		return err
	}
	if err := decodeBucket(fields, "escalation_protocols", &out.EscalationProtocols); err != nil {
		return err
	}
	if err := decodeBucket(fields, "cost_limits", &out.CostLimits); err != nil {
		return err
	}
	if err := decodeBucket(fields, "audit_requirements", &out.AuditRequirements); err != nil {
		return err
	}
	return nil
}

// decodeBucket is the generic per-bucket helper for
// [decodeImmutableCoreBuckets]: look up the named key in the
// RawMessage map, decode into dst, delete the key on success so the
// caller can collect leftovers into Extra. Wraps the decode error
// with the bucket name so the per-bucket diagnostics survive.
func decodeBucket[T any](fields map[string]json.RawMessage, name string, dst *T) error {
	v, ok := fields[name]
	if !ok {
		return nil
	}
	if err := json.Unmarshal(v, dst); err != nil {
		return fmt.Errorf("manifest: immutable_core: %s: %w", name, err)
	}
	delete(fields, name)
	return nil
}

// composeSystemPrompt is the deterministic templater documented on
// [LoadManifest]. One [strings.Builder], two conditional appends; no
// micro-optimizations. Empty personality and empty language each suppress
// their own line; both empty returns base verbatim.
func composeSystemPrompt(base, personality, language string) string {
	if personality == "" && language == "" {
		return base
	}

	var b strings.Builder
	b.Grow(len(base) + len(personality) + len(language) + 32)
	b.WriteString(base)
	if personality != "" {
		b.WriteString("\n\nPersonality: ")
		b.WriteString(personality)
		if language != "" {
			b.WriteString("\nLanguage: ")
			b.WriteString(language)
		}
		return b.String()
	}
	// personality == "" && language != "": leading blank line attaches
	// to Language alone (explicit precedence rule, AC2 / test plan).
	b.WriteString("\n\nLanguage: ")
	b.WriteString(language)
	return b.String()
}
