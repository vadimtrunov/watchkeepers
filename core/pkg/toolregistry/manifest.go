package toolregistry

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

// DryRunMode is the closed-set enum a tool manifest declares to
// describe how the runtime should execute the tool when the operator
// has not yet approved it for real-world side effects (M9.4.c).
//
//   - [DryRunModeGhost] — every write-side broker call is stubbed; the
//     runtime records a "would have done: X, Y, Z" trace.
//   - [DryRunModeScoped] — broker calls run with per-deployment filter
//     injection (Slack sends rerouted to the lead's DM, Jira writes
//     redirected to a sandbox project). Real outbound side effects
//     reach a contained surface.
//   - [DryRunModeNone] — no dry-run is available; the runtime surfaces
//     an explicit pre-approval warning before the first call. Authors
//     declaring `none` MUST justify the choice in the proposal review.
//
// The set is closed by design (M9.1.a [SourceKind] / [PullPolicy]
// pattern): adding a new mode requires a new wiring path in the M9.4.c
// executor. The empty string is NOT a valid value — strict decoding
// refuses an absent / blank `dry_run_mode` so an AI-authored manifest
// cannot silently land without an explicit dry-run choice.
type DryRunMode string

const (
	// DryRunModeGhost stubs every write-side broker call and records
	// the would-have-done trace.
	DryRunModeGhost DryRunMode = "ghost"

	// DryRunModeScoped reroutes broker writes to a per-deployment
	// sandbox surface (Slack → lead DM, Jira → sandbox project).
	DryRunModeScoped DryRunMode = "scoped"

	// DryRunModeNone declares no dry-run is available; the runtime
	// MUST surface an explicit pre-approval warning. Authors choosing
	// `none` accept that the lead reviews real-world side effects on
	// first invocation.
	DryRunModeNone DryRunMode = "none"
)

// Validate reports whether `m` is in the closed [DryRunMode] set.
// Returns [ErrInvalidDryRunMode] otherwise (including the empty
// string — see the [DryRunMode] godoc for why "no value" is rejected
// rather than defaulted).
func (m DryRunMode) Validate() error {
	switch m {
	case DryRunModeGhost, DryRunModeScoped, DryRunModeNone:
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrInvalidDryRunMode, string(m))
	}
}

// Manifest is the wire-format per-tool `manifest.json` decoded by
// [DecodeManifest] / [LoadManifestFromFile]. The schema mirrors the
// roadmap M9.1 description: `name`, `version`, `capabilities`,
// zod-compatible `schema`, auto-filled `source`, and optional
// `signature`. M9.4.a additionally requires `dry_run_mode` on every
// manifest so the M9.4.c executor never has to guess the runtime
// posture. Strict decoding rejects unknown fields so an
// operator-authored typo never silently degrades to defaults.
type Manifest struct {
	// Name is the tool identifier used by the runtime ACL gate at
	// `InvokeTool` time. Required; an empty value yields
	// [ErrManifestMissingRequired]. Convention: lower_snake_case
	// (e.g. `find_overdue_tickets`); enforcement is deferred to a
	// linter rule rather than the decoder.
	Name string `json:"name"`

	// Version is the SemVer-shaped string the runtime ACL projects
	// alongside Name to gate per-tool pin checks (see runtime.ToolEntry).
	// Required; empty yields [ErrManifestMissingRequired]. Strict format
	// enforcement is deferred to M9.4's CI gates so this decoder stays
	// transport-only.
	Version string `json:"version"`

	// Capabilities is the list of capability ids this tool requires.
	// At least one entry is required (an empty list signals "no
	// capabilities declared" which is a non-sensical state for an
	// authored tool — the [capability] package's deny-by-default
	// stance would refuse every call). The plain-language description
	// per id lives in M9.3's `dict/capabilities.yaml`.
	Capabilities []string `json:"capabilities"`

	// Schema is the zod-compatible JSON-schema fragment describing the
	// tool's call arguments. Stored verbatim as [json.RawMessage] so
	// downstream tooling can transform it (e.g. emit a TypeScript
	// `z.object` wrapper or a Go runtime validator) without the
	// decoder picking a normal form. Required; null / empty yields
	// [ErrManifestMissingRequired].
	Schema json.RawMessage `json:"schema"`

	// Source is the name of the [SourceConfig] this manifest was
	// synced from. Auto-filled by [LoadManifestFromFile] from the
	// caller-supplied `sourceName` argument so the runtime always
	// observes a populated value. An operator-authored manifest that
	// hard-codes `source` is treated as a tamper attempt:
	// [LoadManifestFromFile] returns [ErrManifestSourceCollision] when
	// the on-disk value disagrees with the stamping argument.
	Source string `json:"source,omitempty"`

	// Signature is the optional detached-signature blob over the
	// manifest + source-tree (cosign / minisign output, M9.3). Empty
	// signature is acceptable in M9.1.a; the default
	// [NoopSignatureVerifier] accepts both states. Strict decoding
	// still allows the field name to be absent entirely (the JSON tag
	// is `omitempty`).
	Signature string `json:"signature,omitempty"`

	// DryRunMode is the closed-set declaration of how the runtime
	// executes this tool pre-approval (M9.4.a schema extension; M9.4.c
	// runtime executor). Required: a manifest that omits the field —
	// or supplies an unknown value — is refused by [DecodeManifest]
	// with [ErrManifestMissingRequired] / [ErrInvalidDryRunMode]. See
	// the [DryRunMode] godoc for the three valid values.
	DryRunMode DryRunMode `json:"dry_run_mode"`
}

// Validate runs the post-decode required-field check. Called by
// [DecodeManifest] before returning to the caller so a [Manifest] in
// hand is always well-formed.
func (m Manifest) Validate() error {
	if strings.TrimSpace(m.Name) == "" {
		return fmt.Errorf("%w: name", ErrManifestMissingRequired)
	}
	if strings.TrimSpace(m.Version) == "" {
		return fmt.Errorf("%w: version", ErrManifestMissingRequired)
	}
	if len(m.Capabilities) == 0 {
		return fmt.Errorf("%w: capabilities", ErrManifestMissingRequired)
	}
	for i, c := range m.Capabilities {
		if strings.TrimSpace(c) == "" {
			return fmt.Errorf("%w: capabilities[%d]", ErrManifestMissingRequired, i)
		}
	}
	if len(bytes.TrimSpace(m.Schema)) == 0 || bytes.Equal(bytes.TrimSpace(m.Schema), []byte("null")) {
		return fmt.Errorf("%w: schema", ErrManifestMissingRequired)
	}
	// `dry_run_mode` is checked LAST deliberately: the M9.4.a
	// schema extension was added to ~25 pre-existing M9.1.a/b/M9.2
	// happy-path JSON fixtures by adding the field; missing-required
	// test fixtures targeting the earlier fields fire on their own
	// field before reaching this check, so existing assertions on
	// `ErrManifestMissingRequired` for name / version / capabilities
	// / schema remain stable. A future refactor reordering this
	// block would silently shift which sentinel ~25 fixtures return
	// — keep the field LAST.
	if strings.TrimSpace(string(m.DryRunMode)) == "" {
		return fmt.Errorf("%w: dry_run_mode", ErrManifestMissingRequired)
	}
	if err := m.DryRunMode.Validate(); err != nil {
		return err
	}
	return nil
}

// DecodeManifest strictly decodes `raw` into a [Manifest], validates
// required fields, and returns it. Strict mode (`DisallowUnknownFields`)
// rejects any JSON key not declared on [Manifest]; a typo therefore
// surfaces as [ErrManifestUnknownField] rather than silently degrading
// to a default value.
//
// Failure modes:
//
//   - Malformed JSON → wrapped [ErrManifestParse].
//   - Unknown field → wrapped [ErrManifestUnknownField] (chained through
//     [ErrManifestParse] so callers matching either sentinel succeed).
//   - Missing required field → wrapped [ErrManifestMissingRequired].
//
// The returned [Manifest] is a defensive deep-copy of all reference-
// typed fields (`Capabilities`, `Schema`) — the caller's `raw` buffer
// is no longer aliased after this function returns.
func DecodeManifest(raw []byte) (Manifest, error) {
	if len(raw) == 0 {
		return Manifest{}, fmt.Errorf("%w: empty input", ErrManifestParse)
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		if isUnknownFieldErr(err) {
			return Manifest{}, fmt.Errorf("%w: %w: %w", ErrManifestUnknownField, ErrManifestParse, err)
		}
		return Manifest{}, fmt.Errorf("%w: %w", ErrManifestParse, err)
	}

	// Guard against trailing JSON garbage. `json.Decoder.Decode` only
	// reads the first value; a clean payload yields [io.EOF] on the
	// second call. Anything else — a second valid value, syntactically
	// invalid bytes, an unterminated literal — surfaces as
	// [ErrManifestParse] so a manifest with `{...}garbage` is refused
	// just like a manifest with `{...}{...}`.
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Manifest{}, fmt.Errorf("%w: trailing document", ErrManifestParse)
		}
		return Manifest{}, fmt.Errorf("%w: trailing input: %w", ErrManifestParse, err)
	}

	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}

	// Defensive deep-copy of reference-typed fields so a caller
	// mutating `raw` post-decode cannot bleed into the returned
	// value. Mirrors the M7.1.c.c `cloneBotProfile` pattern.
	m.Capabilities = cloneStringSlice(m.Capabilities)
	m.Schema = cloneBytes(m.Schema)
	return m, nil
}

// LoadManifestFromFile reads `<dir>/manifest.json` from `fs`, decodes
// it via [DecodeManifest], stamps `Source` from `sourceName`, and
// returns the result. The stamping rule:
//
//   - On-disk `source` empty: the field is filled from `sourceName`.
//   - On-disk `source` non-empty AND equal to `sourceName`: no-op.
//   - On-disk `source` non-empty AND different from `sourceName`:
//     [ErrManifestSourceCollision].
//
// `sourceName` is required; an empty value yields
// [ErrInvalidSourceName] without touching the filesystem.
func LoadManifestFromFile(filesystem FS, dir, sourceName string) (Manifest, error) {
	if filesystem == nil {
		panic("toolregistry: LoadManifestFromFile: fs must not be nil")
	}
	if strings.TrimSpace(sourceName) == "" {
		return Manifest{}, ErrInvalidSourceName
	}
	path := filepath.Join(dir, "manifest.json")
	raw, err := filesystem.ReadFile(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: %w", ErrManifestParse, err)
	}
	m, err := DecodeManifest(raw)
	if err != nil {
		return Manifest{}, err
	}
	switch {
	case m.Source == "":
		m.Source = sourceName
	case m.Source != sourceName:
		return Manifest{}, fmt.Errorf(
			"%w: on-disk %q != stamping %q",
			ErrManifestSourceCollision, m.Source, sourceName,
		)
	}
	return m, nil
}

// isUnknownFieldErr reports whether `err` is the encoding/json
// "unknown field" diagnostic. The stdlib does not export a typed
// sentinel for this case, so we sniff the message text — same
// pattern as `core/pkg/config/config.go`'s `isUnknownFieldErr`.
func isUnknownFieldErr(err error) bool {
	return strings.Contains(err.Error(), "unknown field")
}

func cloneStringSlice(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func cloneBytes(in []byte) []byte {
	if in == nil {
		return nil
	}
	out := make([]byte, len(in))
	copy(out, in)
	return out
}
