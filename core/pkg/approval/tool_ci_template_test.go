package approval

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/vadimtrunov/watchkeepers/core/pkg/toolregistry"
)

// toolCITemplatePath is the location of the M9.4.d shared CI workflow
// template, relative to the approval package's source directory. The
// path leans on `go test`'s CWD invariant (the test binary runs in the
// package directory). Every read in this file goes through
// readTemplateBytes so a future move only changes one constant.
//
// The template is staged here for publication to the platform
// `watchkeeper-tools` repo at release time; this contract test pins
// the parity contract between the M9.4.b in-process [GateName] closed
// set and the M9.4.d real CI implementation.
const toolCITemplatePath = "../../../tools-builtin/ci-template/tool-ci.yml"

// readTemplateBytes returns the staged template's raw contents,
// resolving the relative path against the package working directory
// (which `go test` sets to the source dir). Used for both YAML decode
// and source-grep AC.
func readTemplateBytes(t *testing.T) []byte {
	t.Helper()
	abs, err := filepath.Abs(toolCITemplatePath)
	if err != nil {
		t.Fatalf("resolve template path: %v", err)
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("read %s: %v", abs, err)
	}
	return raw
}

// readToolCITemplate decodes the staged template. `yaml.v3 v3.0.1`
// decodes the unquoted `on:` key as the STRING `"on"`, not as the
// boolean `true` (empirically verified against this exact library
// version — codex/critic iter-1 M4 corrected the original comment
// claiming the opposite). The decode target stays `map[any]any` as a
// defensive hedge against a future library upgrade flipping the
// decode shape, and `castMap` normalises shape downstream.
func readToolCITemplate(t *testing.T) map[any]any {
	t.Helper()
	var doc map[any]any
	if err := yaml.Unmarshal(readTemplateBytes(t), &doc); err != nil {
		t.Fatalf("decode template: %v", err)
	}
	return doc
}

// TestM94D_Template_GateNameParity pins the M9.4.b → M9.4.d
// vocabulary contract: every [GateName] constant MUST appear as a
// step `name:` value in the staged tool-ci.yml. Reviewer.go's
// godoc names this contract explicitly ("the M9.4.d real CI
// implementation will populate the same names"); this test prevents
// a future GateName rename from silently breaking the audit-vocabulary
// join between slack-native review outcomes and git-pr review
// outcomes.
func TestM94D_Template_GateNameParity(t *testing.T) {
	stepNames := extractToolCIStepNames(t)
	want := []GateName{
		GateTypecheck,
		GateUndeclaredFSNet,
		GateVitest,
		GateCapabilityDeclaration,
	}
	for _, gate := range want {
		found := false
		for _, name := range stepNames {
			if name == string(gate) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("tool-ci.yml missing step name %q; gate names present: %v", gate, stepNames)
		}
	}
}

// TestM94D_Template_GateOrderPinned pins the FULL step order in
// jobs.tool-ci, not just the gate-name relative positions. The
// previous filter-then-check shape (iter-1 M5) would have let a
// reorder of setup steps (e.g., `Install dependencies` after
// `typecheck`) pass silently as long as the four gates kept their
// relative order. The current shape pins every step name in
// declaration order.
func TestM94D_Template_GateOrderPinned(t *testing.T) {
	got := extractToolCIStepNames(t)
	want := []string{
		"Checkout",
		"Set up pnpm",
		"Set up Node.js",
		"Install dependencies",
		string(GateTypecheck),
		string(GateUndeclaredFSNet),
		string(GateVitest),
		string(GateCapabilityDeclaration),
		"sign",
	}
	if len(got) != len(want) {
		t.Fatalf("step count: got %d %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("step position %d: got %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

// TestM94D_Template_TriggerIsWorkflowCallOnly enforces the
// reusable-workflow contract: the template is consumed via
// `uses: watchkeeper-tools/.../tool-ci.yml@vX.Y.Z` from caller repos,
// so it MUST NOT register any other trigger (a stray `push` or
// `pull_request` would fire the template on every commit in
// `wathkeepers` itself, which has its own ci.yml).
func TestM94D_Template_TriggerIsWorkflowCallOnly(t *testing.T) {
	doc := readToolCITemplate(t)
	on := lookupOnBlock(t, doc)
	if len(on) != 1 {
		t.Errorf("template `on:` has %d triggers; want exactly 1 (workflow_call)", len(on))
	}
	if _, ok := on["workflow_call"]; !ok {
		t.Errorf("template `on:` missing `workflow_call` trigger; got %v", on)
	}
}

// lookupOnBlock returns the decoded `on:` block as a map[string]any,
// probing both the string key (yaml.v3 1.2 mode — empirically
// observed) and the boolean `true` key (a defensive hedge against a
// future yaml.v3 upgrade that surfaces the YAML 1.1 truthy quirk on
// KEY position).
func lookupOnBlock(t *testing.T, doc map[any]any) map[string]any {
	t.Helper()
	var raw any
	if v, ok := doc["on"]; ok {
		raw = v
	} else if v, ok := doc[true]; ok {
		raw = v
	} else {
		t.Fatalf("template has no `on:` trigger block: %v", doc)
	}
	switch m := raw.(type) {
	case map[string]any:
		return m
	case map[any]any:
		out := make(map[string]any, len(m))
		for k, v := range m {
			ks, ok := k.(string)
			if !ok {
				t.Fatalf("non-string key in `on:` block: %T %v", k, k)
			}
			out[ks] = v
		}
		return out
	default:
		t.Fatalf("`on:` block is not a map: %T %v", raw, raw)
		return nil
	}
}

// TestM94D_Template_InputsSurface enforces the documented input
// shape AND default values: `coverage_threshold` (number, default
// 80) and `enable_signing` (boolean, default false) MUST be declared
// with strict types AND with the documented defaults, so a typo in
// a consumer workflow (`coverage_treshold: 90`) fails at workflow
// validation rather than silently defaulting, AND a future template
// edit silently lowering `coverage_threshold: 80` to `60` is caught
// (iter-1 M6).
func TestM94D_Template_InputsSurface(t *testing.T) {
	doc := readToolCITemplate(t)
	on := lookupOnBlock(t, doc)
	wc := castMap(t, on["workflow_call"], "workflow_call")
	inputs := castMap(t, wc["inputs"], "workflow_call.inputs")
	tests := []struct {
		name        string
		wantType    string
		wantDefault any
	}{
		{"node_version", "string", "24.15.0"},
		{"pnpm_version", "string", "10.33.0"},
		{"coverage_threshold", "number", 80},
		{"enable_signing", "boolean", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := castMap(t, inputs[tt.name], "inputs."+tt.name)
			gotType, _ := spec["type"].(string)
			if gotType != tt.wantType {
				t.Errorf("input %q: type=%q, want %q", tt.name, gotType, tt.wantType)
			}
			gotDefault := spec["default"]
			if gotDefault != tt.wantDefault {
				t.Errorf("input %q: default=%v (%T), want %v (%T)", tt.name, gotDefault, gotDefault, tt.wantDefault, tt.wantDefault)
			}
		})
	}
}

// TestM94D_Template_OutputsWired pins the reusable-workflow outputs
// contract. The optional `sign` step's `tool_bundle_sha256` is only
// consumable by a downstream caller job when the step has an `id:`
// AND the job declares an `outputs:` block AND the workflow_call
// declares a top-level `outputs:` block (iter-1 M1).
func TestM94D_Template_OutputsWired(t *testing.T) {
	doc := readToolCITemplate(t)

	// workflow_call.outputs.tool_bundle_sha256
	on := lookupOnBlock(t, doc)
	wc := castMap(t, on["workflow_call"], "workflow_call")
	wcOut := castMap(t, wc["outputs"], "workflow_call.outputs")
	out := castMap(t, wcOut["tool_bundle_sha256"], "workflow_call.outputs.tool_bundle_sha256")
	if v, _ := out["value"].(string); !strings.Contains(v, "jobs.tool-ci.outputs.tool_bundle_sha256") {
		t.Errorf("workflow_call.outputs.tool_bundle_sha256.value does not reference jobs.tool-ci.outputs: %q", v)
	}

	// jobs.tool-ci.outputs.tool_bundle_sha256
	jobs := castMap(t, doc["jobs"], "jobs")
	job := castMap(t, jobs["tool-ci"], "jobs.tool-ci")
	jobOut := castMap(t, job["outputs"], "jobs.tool-ci.outputs")
	jobOutVal, _ := jobOut["tool_bundle_sha256"].(string)
	if !strings.Contains(jobOutVal, "steps.sign.outputs.tool_bundle_sha256") {
		t.Errorf("jobs.tool-ci.outputs.tool_bundle_sha256 does not reference steps.sign.outputs: %q", jobOutVal)
	}

	// jobs.tool-ci.steps[*sign*].id == "sign"
	steps, _ := job["steps"].([]any)
	for _, raw := range steps {
		s := castMap(t, raw, "step")
		if name, _ := s["name"].(string); name == "sign" {
			id, _ := s["id"].(string)
			if id != "sign" {
				t.Errorf("sign step missing `id: sign` (got %q)", id)
			}
			return
		}
	}
	t.Fatal("no `sign` step found in jobs.tool-ci.steps")
}

// TestM94D_Template_SigningGatedByInput pins the discipline that the
// optional `sign` step MUST be conditional on `inputs.enable_signing`
// AND on a release-relevant ref (default branch or v* tag). The
// regression risk is a future template edit that drops the input
// guard, turning every PR run into an unintended signing attempt.
func TestM94D_Template_SigningGatedByInput(t *testing.T) {
	doc := readToolCITemplate(t)
	jobs := castMap(t, doc["jobs"], "jobs")
	job := castMap(t, jobs["tool-ci"], "jobs.tool-ci")
	steps, ok := job["steps"].([]any)
	if !ok {
		t.Fatalf("`jobs.tool-ci.steps` is not a list: %T", job["steps"])
	}
	for _, raw := range steps {
		s := castMap(t, raw, "step")
		if name, _ := s["name"].(string); name != "sign" {
			continue
		}
		cond, ok := s["if"].(string)
		if !ok {
			t.Fatalf("`sign` step has no `if:` guard")
		}
		if !strings.Contains(cond, "inputs.enable_signing") {
			t.Errorf("`sign` step `if:` does not reference inputs.enable_signing: %q", cond)
		}
		if !strings.Contains(cond, "refs/heads/main") || !strings.Contains(cond, "refs/tags/v") {
			t.Errorf("`sign` step `if:` does not gate on main / v* tag: %q", cond)
		}
		return
	}
	t.Fatal("no `sign` step found in jobs.tool-ci.steps")
}

// TestM94D_Template_CoverageThresholdPlumbed asserts the
// `vitest` step actually consumes the `coverage_threshold` input
// across all four metrics — a regression where the threshold input
// is declared but unused would let a consumer raise the coverage
// floor without effect.
func TestM94D_Template_CoverageThresholdPlumbed(t *testing.T) {
	body := string(readTemplateBytes(t))
	for _, metric := range []string{
		"--coverage.thresholds.lines=${{ inputs.coverage_threshold }}",
		"--coverage.thresholds.functions=${{ inputs.coverage_threshold }}",
		"--coverage.thresholds.branches=${{ inputs.coverage_threshold }}",
		"--coverage.thresholds.statements=${{ inputs.coverage_threshold }}",
	} {
		if !strings.Contains(body, metric) {
			t.Errorf("vitest step missing %q", metric)
		}
	}
}

// TestM94D_Template_PermissionsContentsRead pins the top-level
// `permissions: contents: read` block — a future broadening to
// `write-all` (e.g., a developer enabling a release-publish flow on
// the same template) would slip through the gate-vocabulary tests
// (iter-1 M6).
func TestM94D_Template_PermissionsContentsRead(t *testing.T) {
	doc := readToolCITemplate(t)
	perms := castMap(t, doc["permissions"], "permissions")
	got, _ := perms["contents"].(string)
	if got != "read" {
		t.Errorf("permissions.contents = %q; want %q", got, "read")
	}
	if len(perms) != 1 {
		t.Errorf("permissions has %d keys; want exactly 1 (contents: read) so a future broadening fails the test", len(perms))
	}
}

// TestM94D_Template_ActionSHAsPinned pins every `uses: action@<ref>`
// to a 40-character hex SHA. A future edit to `@main` / `@v4` slips
// past the gate-vocabulary tests; this test catches the supply-chain
// regression where an unpinned ref opens the door to a tag-rewrite
// or branch-tip attack (iter-1 M6).
//
// The pattern below scans top-level `uses:` lines. The reusable-
// workflow self-reference (`uses: watchkeeper-tools/...@v1.0.0`)
// would be flagged but does NOT appear in tool-ci.yml itself —
// only in example-consumer-workflow.yml — so the gate stays
// SHA-only.
func TestM94D_Template_ActionSHAsPinned(t *testing.T) {
	body := string(readTemplateBytes(t))
	usesRe := regexp.MustCompile(`(?m)^\s*-?\s*uses:\s*(\S+)`)
	matches := usesRe.FindAllStringSubmatch(body, -1)
	if len(matches) == 0 {
		t.Fatal("no `uses:` directives found — surely the template uses checkout/setup actions?")
	}
	for _, m := range matches {
		ref := m[1]
		atIdx := strings.LastIndex(ref, "@")
		if atIdx == -1 {
			t.Errorf("uses: %q has no `@<ref>` suffix", ref)
			continue
		}
		sha := ref[atIdx+1:]
		if len(sha) != 40 {
			t.Errorf("uses: %q does not pin a 40-char SHA (got %q, len=%d)", ref, sha, len(sha))
			continue
		}
		if _, err := hex.DecodeString(sha); err != nil {
			t.Errorf("uses: %q SHA suffix is not hex: %v", ref, err)
		}
	}
}

// TestM94D_Template_FSNetPatternParity pins the byte-for-byte
// mirror between the reviewer's `fsNetPattern` table and the
// inlined `undeclared_fs_net` needles. A divergence — a new
// fs/network module landing in reviewer.go's table without a
// matching template needle (or vice versa) — would silently produce
// a slack-native ↔ git-pr asymmetry (iter-1 M6 + Pattern 4 of the
// M9.4.d lesson entry).
func TestM94D_Template_FSNetPatternParity(t *testing.T) {
	body := string(readTemplateBytes(t))
	for _, p := range fsNetPattern {
		// The template renders each row as
		//   { needle: '...', cap: '...' }
		// with single OR double quotes depending on whether the
		// needle itself contains a quote character. Probe for the
		// cap presence first, then for the needle independently;
		// both must be present.
		if !strings.Contains(body, "cap: '"+p.Capability+"'") {
			t.Errorf("fsNetPattern parity: capability %q not present in template", p.Capability)
		}
		// Needle may be quoted with either quote shape; probe both.
		dq := `needle: "` + p.Needle + `"`
		sq := `needle: '` + p.Needle + `'`
		if !strings.Contains(body, dq) && !strings.Contains(body, sq) {
			t.Errorf("fsNetPattern parity: needle %q not present in template (tried %q and %q)", p.Needle, dq, sq)
		}
	}
}

// TestM94D_Template_CapabilityIDRegexParity pins the byte-for-byte
// mirror between reviewer.go's `capabilityIDFormat` regex and the
// inlined `capability_declaration` `capPattern` regex.
func TestM94D_Template_CapabilityIDRegexParity(t *testing.T) {
	body := string(readTemplateBytes(t))
	// reviewer.go declares: capabilityIDFormat = regexp.MustCompile(`^[a-z][a-z0-9_:.-]*$`)
	// The template inlines it as: const capPattern = /^[a-z][a-z0-9_:.-]*$/;
	want := "/^[a-z][a-z0-9_:.-]*$/"
	if !strings.Contains(body, want) {
		t.Errorf("capability-id regex parity: template missing %q", want)
	}
	// Sanity-check the Go side compiles to the same string.
	if got := capabilityIDFormat.String(); got != "^[a-z][a-z0-9_:.-]*$" {
		t.Errorf("reviewer.capabilityIDFormat string drifted: %q", got)
	}
}

// TestM94D_Template_DryRunModeParity pins the byte-for-byte mirror
// between toolregistry.DryRunMode's closed set and the inlined
// `capability_declaration` `dryRunModes` Set.
func TestM94D_Template_DryRunModeParity(t *testing.T) {
	body := string(readTemplateBytes(t))
	want := "new Set(['ghost', 'scoped', 'none'])"
	if !strings.Contains(body, want) {
		t.Errorf("dry_run_mode closed-set parity: template missing %q", want)
	}
	// Sanity-check the Go side carries the same three values.
	goModes := []toolregistry.DryRunMode{
		toolregistry.DryRunModeGhost,
		toolregistry.DryRunModeScoped,
		toolregistry.DryRunModeNone,
	}
	for _, m := range goModes {
		if err := m.Validate(); err != nil {
			t.Errorf("DryRunMode %q: Validate returned %v", m, err)
		}
	}
}

// extractToolCIStepNames flattens jobs.tool-ci.steps[*].name in
// declaration order, scoped to the `tool-ci` job specifically.
// The previous `extractStepNames` flattened across `jobs.*` with
// non-deterministic map iteration order (iter-1 M6); scoping to the
// single job pins ordering AND prevents a future second job (e.g.,
// `release`) from silently satisfying the gate-name tests against
// itself rather than against `tool-ci`.
func extractToolCIStepNames(t *testing.T) []string {
	t.Helper()
	doc := readToolCITemplate(t)
	jobs := castMap(t, doc["jobs"], "jobs")
	job := castMap(t, jobs["tool-ci"], "jobs.tool-ci")
	steps, ok := job["steps"].([]any)
	if !ok {
		t.Fatalf("`jobs.tool-ci.steps` is not a list: %T", job["steps"])
	}
	out := make([]string, 0, len(steps))
	for _, sraw := range steps {
		s := castMap(t, sraw, "step")
		if name, ok := s["name"].(string); ok {
			out = append(out, name)
		}
	}
	return out
}

// castMap accepts either a map[string]any or a map[any]any (yaml.v3
// surfaces both shapes depending on whether the top-level decode
// target was generic) and normalises to map[string]any. A non-map
// value fails the test with a descriptive message.
func castMap(t *testing.T, raw any, label string) map[string]any {
	t.Helper()
	switch m := raw.(type) {
	case map[string]any:
		return m
	case map[any]any:
		out := make(map[string]any, len(m))
		for k, v := range m {
			ks, ok := k.(string)
			if !ok {
				t.Fatalf("non-string key in %s: %T %v", label, k, k)
			}
			out[ks] = v
		}
		return out
	case nil:
		t.Fatalf("missing %s", label)
		return nil
	default:
		t.Fatalf("%s is not a map: %T %v", label, raw, raw)
		return nil
	}
}
