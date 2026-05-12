package approval

import (
	"errors"
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/toolregistry"
)

func TestMode_ValidateHappyPath(t *testing.T) {
	t.Parallel()
	cases := []Mode{ModeGitPR, ModeSlackNative, ModeBoth}
	for _, m := range cases {
		if err := m.Validate(); err != nil {
			t.Errorf("Validate(%q): unexpected err %v", m, err)
		}
	}
}

func TestMode_ValidateRejectsEmpty(t *testing.T) {
	t.Parallel()
	if err := Mode("").Validate(); !errors.Is(err, ErrInvalidMode) {
		t.Errorf("expected ErrInvalidMode, got %v", err)
	}
}

func TestMode_ValidateRejectsUnknown(t *testing.T) {
	t.Parallel()
	cases := []Mode{"slack", "git-pr ", "BOTH", "ai-only"}
	for _, m := range cases {
		if err := m.Validate(); !errors.Is(err, ErrInvalidMode) {
			t.Errorf("Validate(%q): expected ErrInvalidMode, got %v", m, err)
		}
	}
}

func TestMode_Includes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		mode  Mode
		slack bool
		gitPR bool
	}{
		{ModeGitPR, false, true},
		{ModeSlackNative, true, false},
		{ModeBoth, true, true},
	}
	for _, c := range cases {
		if c.mode.IncludesSlackNative() != c.slack {
			t.Errorf("IncludesSlackNative(%q): got %v, want %v", c.mode, c.mode.IncludesSlackNative(), c.slack)
		}
		if c.mode.IncludesGitPR() != c.gitPR {
			t.Errorf("IncludesGitPR(%q): got %v, want %v", c.mode, c.mode.IncludesGitPR(), c.gitPR)
		}
	}
}

func TestValidateModeForSources_GitPROnlyRejectsHosted(t *testing.T) {
	t.Parallel()
	sources := []toolregistry.SourceConfig{
		{Name: "git-src", Kind: toolregistry.SourceKindGit, PullPolicy: toolregistry.PullPolicyOnBoot, URL: "https://example/x.git"},
		{Name: "hosted-src", Kind: toolregistry.SourceKindHosted, PullPolicy: toolregistry.PullPolicyOnBoot},
	}
	err := ValidateModeForSources(ModeGitPR, sources)
	if !errors.Is(err, ErrModeMismatchHosted) {
		t.Fatalf("expected ErrModeMismatchHosted, got %v", err)
	}
}

func TestValidateModeForSources_SlackNativeAllowsHosted(t *testing.T) {
	t.Parallel()
	sources := []toolregistry.SourceConfig{
		{Name: "hosted-src", Kind: toolregistry.SourceKindHosted, PullPolicy: toolregistry.PullPolicyOnBoot},
	}
	if err := ValidateModeForSources(ModeSlackNative, sources); err != nil {
		t.Errorf("expected nil for slack-native + hosted, got %v", err)
	}
}

func TestValidateModeForSources_BothAllowsHosted(t *testing.T) {
	t.Parallel()
	sources := []toolregistry.SourceConfig{
		{Name: "hosted-src", Kind: toolregistry.SourceKindHosted, PullPolicy: toolregistry.PullPolicyOnBoot},
	}
	if err := ValidateModeForSources(ModeBoth, sources); err != nil {
		t.Errorf("expected nil for both + hosted, got %v", err)
	}
}

func TestValidateModeForSources_GitPROnlyOKWithoutHosted(t *testing.T) {
	t.Parallel()
	sources := []toolregistry.SourceConfig{
		{Name: "git-src", Kind: toolregistry.SourceKindGit, PullPolicy: toolregistry.PullPolicyOnBoot, URL: "https://example/x.git"},
		{Name: "local-src", Kind: toolregistry.SourceKindLocal, PullPolicy: toolregistry.PullPolicyOnBoot},
	}
	if err := ValidateModeForSources(ModeGitPR, sources); err != nil {
		t.Errorf("expected nil for git-pr without hosted, got %v", err)
	}
}

func TestValidateModeForSources_EmptySources(t *testing.T) {
	t.Parallel()
	if err := ValidateModeForSources(ModeGitPR, nil); err != nil {
		t.Errorf("empty sources: %v", err)
	}
}

func TestValidateModeForSources_InvalidModePropagates(t *testing.T) {
	t.Parallel()
	err := ValidateModeForSources(Mode("nope"), nil)
	if !errors.Is(err, ErrInvalidMode) {
		t.Errorf("expected ErrInvalidMode, got %v", err)
	}
}

func TestDecodeModeYAML_HappyPath(t *testing.T) {
	t.Parallel()
	cases := map[string]Mode{
		"approval_mode: git-pr":       ModeGitPR,
		"approval_mode: slack-native": ModeSlackNative,
		"approval_mode: both":         ModeBoth,
	}
	for raw, want := range cases {
		got, err := DecodeModeYAML([]byte(raw))
		if err != nil {
			t.Errorf("%q: %v", raw, err)
			continue
		}
		if got != want {
			t.Errorf("%q: got %q, want %q", raw, got, want)
		}
	}
}

func TestDecodeModeYAML_RejectsEmpty(t *testing.T) {
	t.Parallel()
	cases := [][]byte{nil, []byte(""), []byte("   "), []byte("\n\n")}
	for _, raw := range cases {
		_, err := DecodeModeYAML(raw)
		if !errors.Is(err, ErrModeYAMLParse) {
			t.Errorf("%q: expected ErrModeYAMLParse, got %v", string(raw), err)
		}
	}
}

func TestDecodeModeYAML_RejectsUnknownField(t *testing.T) {
	t.Parallel()
	raw := []byte("approval_mode: git-pr\nunexpected_field: 42\n")
	_, err := DecodeModeYAML(raw)
	if !errors.Is(err, ErrModeYAMLParse) {
		t.Errorf("expected ErrModeYAMLParse, got %v", err)
	}
}

func TestDecodeModeYAML_RejectsMultiDoc(t *testing.T) {
	t.Parallel()
	raw := []byte("approval_mode: git-pr\n---\napproval_mode: both\n")
	_, err := DecodeModeYAML(raw)
	if !errors.Is(err, ErrModeYAMLMultiDoc) {
		t.Errorf("expected ErrModeYAMLMultiDoc, got %v", err)
	}
}

func TestDecodeModeYAML_RejectsInvalidValue(t *testing.T) {
	t.Parallel()
	raw := []byte("approval_mode: ai-only\n")
	_, err := DecodeModeYAML(raw)
	if !errors.Is(err, ErrInvalidMode) {
		t.Errorf("expected ErrInvalidMode, got %v", err)
	}
}

// TestDecodeModeYAML_TypoInKey pins the strict-decode
// invariant: a typo in the top-level key (`approvalMode:` instead of
// `approval_mode:`) is refused as an unknown field rather than
// silently producing a zero-valued [Mode]. Same lesson as
// M9.1.a Pattern 8 (strict YAML decoding at the document root).
func TestDecodeModeYAML_TypoInKey(t *testing.T) {
	t.Parallel()
	raw := []byte("approvalMode: git-pr\n")
	_, err := DecodeModeYAML(raw)
	if !errors.Is(err, ErrModeYAMLParse) {
		t.Errorf("expected ErrModeYAMLParse, got %v", err)
	}
}
