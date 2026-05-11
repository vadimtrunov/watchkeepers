package toolregistry

import (
	"errors"
	"testing"
)

func TestSourceConfig_Validate_HappyPath(t *testing.T) {
	t.Parallel()
	cases := []SourceConfig{
		{Name: "platform", Kind: SourceKindGit, URL: "https://x/y", Branch: "main", PullPolicy: PullPolicyOnBoot},
		{Name: "private", Kind: SourceKindGit, URL: "https://x/y", PullPolicy: PullPolicyCron, CronSpec: "0 */15 * * * *"},
		{Name: "ops", Kind: SourceKindGit, URL: "https://x/y", PullPolicy: PullPolicyOnDemand},
		{Name: "local", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
		{Name: "hosted", Kind: SourceKindHosted, PullPolicy: PullPolicyOnDemand},
	}
	for _, sc := range cases {
		if err := sc.Validate(); err != nil {
			t.Errorf("Validate(%+v): unexpected err: %v", sc, err)
		}
	}
}

func TestSourceConfig_Validate_FailurePaths(t *testing.T) {
	t.Parallel()
	cases := []struct {
		label  string
		sc     SourceConfig
		sentin error
	}{
		{
			label:  "empty name",
			sc:     SourceConfig{Kind: SourceKindGit, URL: "https://x", PullPolicy: PullPolicyOnBoot},
			sentin: ErrInvalidSourceName,
		},
		{
			label:  "blank name",
			sc:     SourceConfig{Name: "   ", Kind: SourceKindGit, URL: "https://x", PullPolicy: PullPolicyOnBoot},
			sentin: ErrInvalidSourceName,
		},
		{
			label:  "invalid kind",
			sc:     SourceConfig{Name: "x", Kind: "garbage", PullPolicy: PullPolicyOnBoot},
			sentin: ErrInvalidSourceKind,
		},
		{
			label:  "git missing url",
			sc:     SourceConfig{Name: "x", Kind: SourceKindGit, PullPolicy: PullPolicyOnBoot},
			sentin: ErrMissingSourceURL,
		},
		{
			label:  "local with url",
			sc:     SourceConfig{Name: "x", Kind: SourceKindLocal, URL: "https://x", PullPolicy: PullPolicyOnBoot},
			sentin: ErrSourceURLNotAllowed,
		},
		{
			label:  "hosted with url",
			sc:     SourceConfig{Name: "x", Kind: SourceKindHosted, URL: "https://x", PullPolicy: PullPolicyOnBoot},
			sentin: ErrSourceURLNotAllowed,
		},
		{
			label:  "invalid pull policy",
			sc:     SourceConfig{Name: "x", Kind: SourceKindGit, URL: "https://x", PullPolicy: "weekly"},
			sentin: ErrInvalidPullPolicy,
		},
		{
			label:  "cron missing spec",
			sc:     SourceConfig{Name: "x", Kind: SourceKindGit, URL: "https://x", PullPolicy: PullPolicyCron},
			sentin: ErrInvalidCronSpec,
		},
		{
			label:  "cron blank spec",
			sc:     SourceConfig{Name: "x", Kind: SourceKindGit, URL: "https://x", PullPolicy: PullPolicyCron, CronSpec: "   "},
			sentin: ErrInvalidCronSpec,
		},
	}
	for _, tc := range cases {
		err := tc.sc.Validate()
		if !errors.Is(err, tc.sentin) {
			t.Errorf("%s: expected %v, got %v", tc.label, tc.sentin, err)
		}
	}
}

func TestSourceConfig_EffectiveBranch(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"":         "main",
		"   ":      "main",
		"main":     "main",
		"develop":  "develop",
		"feature1": "feature1",
	}
	for in, want := range cases {
		got := SourceConfig{Branch: in}.EffectiveBranch()
		if got != want {
			t.Errorf("EffectiveBranch(%q): got %q, want %q", in, got, want)
		}
	}
}

func TestValidateSources_DuplicateNames(t *testing.T) {
	t.Parallel()
	sources := []SourceConfig{
		{Name: "x", Kind: SourceKindGit, URL: "https://x", PullPolicy: PullPolicyOnBoot},
		{Name: "x", Kind: SourceKindGit, URL: "https://y", PullPolicy: PullPolicyOnBoot},
	}
	err := ValidateSources(sources)
	if !errors.Is(err, ErrDuplicateSourceName) {
		t.Fatalf("expected ErrDuplicateSourceName, got %v", err)
	}
}

func TestValidateSources_FirstFailureWins(t *testing.T) {
	t.Parallel()
	sources := []SourceConfig{
		{Name: "ok", Kind: SourceKindGit, URL: "https://x", PullPolicy: PullPolicyOnBoot},
		{Name: "", Kind: SourceKindGit, URL: "https://x", PullPolicy: PullPolicyOnBoot},
		{Name: "would-be-dup", Kind: SourceKindGit, URL: "https://x", PullPolicy: PullPolicyOnBoot},
	}
	err := ValidateSources(sources)
	if !errors.Is(err, ErrInvalidSourceName) {
		t.Fatalf("expected ErrInvalidSourceName (first failure), got %v", err)
	}
}

func TestValidateSources_Empty(t *testing.T) {
	t.Parallel()
	if err := ValidateSources(nil); err != nil {
		t.Errorf("nil sources: unexpected err: %v", err)
	}
	if err := ValidateSources([]SourceConfig{}); err != nil {
		t.Errorf("empty sources: unexpected err: %v", err)
	}
}

func TestCloneSources_Defensive(t *testing.T) {
	t.Parallel()
	orig := []SourceConfig{{Name: "x", Kind: SourceKindGit, URL: "https://x", PullPolicy: PullPolicyOnBoot}}
	cp := CloneSources(orig)
	if len(cp) != 1 {
		t.Fatalf("CloneSources: len got %d, want 1", len(cp))
	}
	orig[0].Name = "MUTATED"
	if cp[0].Name != "x" {
		t.Errorf("CloneSources: copy bled (got %q)", cp[0].Name)
	}
}

func TestDecodeSourcesYAML_HappyPath(t *testing.T) {
	t.Parallel()
	raw := []byte(`
tool_sources:
  - name: platform
    kind: git
    url: https://github.com/example/platform
    branch: main
    pull_policy: on-boot
  - name: private
    kind: git
    url: https://github.com/example/private
    pull_policy: cron
    cron_spec: "0 */15 * * * *"
    auth_secret: PRIVATE_TOOLS_TOKEN
  - name: ops
    kind: local
    pull_policy: on-demand
`)
	sources, err := DecodeSourcesYAML(raw)
	if err != nil {
		t.Fatalf("DecodeSourcesYAML: %v", err)
	}
	if len(sources) != 3 {
		t.Fatalf("len(sources): got %d, want 3", len(sources))
	}
	if sources[0].Name != "platform" || sources[0].Kind != SourceKindGit {
		t.Errorf("sources[0]: got %+v", sources[0])
	}
	if sources[1].AuthSecret != "PRIVATE_TOOLS_TOKEN" {
		t.Errorf("sources[1].AuthSecret: got %q", sources[1].AuthSecret)
	}
	if sources[2].Kind != SourceKindLocal || sources[2].PullPolicy != PullPolicyOnDemand {
		t.Errorf("sources[2]: got %+v", sources[2])
	}
}

func TestDecodeSourcesYAML_EmptyDocument(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"", "   ", "\n\n"} {
		sources, err := DecodeSourcesYAML([]byte(raw))
		if err != nil {
			t.Errorf("DecodeSourcesYAML(%q): unexpected err: %v", raw, err)
		}
		if len(sources) != 0 {
			t.Errorf("DecodeSourcesYAML(%q): expected 0 sources, got %d", raw, len(sources))
		}
	}
}

func TestDecodeSourcesYAML_AbsentKey(t *testing.T) {
	t.Parallel()
	// A valid YAML document without the `tool_sources:` key — the
	// decoder treats it as "no sources configured", not an error.
	raw := []byte(`other_key: value`)
	// other_key is not in the struct so strict mode rejects it —
	// this is the expected behaviour.
	_, err := DecodeSourcesYAML(raw)
	if err == nil {
		t.Fatal("expected error for unknown top-level key, got nil")
	}
}

func TestDecodeSourcesYAML_UnknownEntryField(t *testing.T) {
	t.Parallel()
	raw := []byte(`
tool_sources:
  - name: x
    kind: git
    url: https://x
    pull_policy: on-boot
    nonsense: 42
`)
	_, err := DecodeSourcesYAML(raw)
	if err == nil {
		t.Fatal("expected error for unknown entry field, got nil")
	}
}

func TestDecodeSourcesYAML_ValidationFailurePropagates(t *testing.T) {
	t.Parallel()
	raw := []byte(`
tool_sources:
  - name: ""
    kind: git
    url: https://x
    pull_policy: on-boot
`)
	_, err := DecodeSourcesYAML(raw)
	if !errors.Is(err, ErrInvalidSourceName) {
		t.Fatalf("expected ErrInvalidSourceName, got %v", err)
	}
}

func TestDecodeSourcesYAML_DuplicateNames(t *testing.T) {
	t.Parallel()
	raw := []byte(`
tool_sources:
  - name: x
    kind: git
    url: https://x
    pull_policy: on-boot
  - name: x
    kind: git
    url: https://y
    pull_policy: on-boot
`)
	_, err := DecodeSourcesYAML(raw)
	if !errors.Is(err, ErrDuplicateSourceName) {
		t.Fatalf("expected ErrDuplicateSourceName, got %v", err)
	}
}

func TestDecodeSourcesYAML_MultiDocument(t *testing.T) {
	t.Parallel()
	raw := []byte(`
tool_sources:
  - name: x
    kind: git
    url: https://x
    pull_policy: on-boot
---
tool_sources:
  - name: y
    kind: git
    url: https://y
    pull_policy: on-boot
`)
	_, err := DecodeSourcesYAML(raw)
	if err == nil {
		t.Fatal("expected error for multi-document YAML, got nil")
	}
}

func TestLoadSourcesYAMLFromFile_HappyPath(t *testing.T) {
	t.Parallel()
	fakeFs := newFakeFS()
	fakeFs.files["/etc/wk/sources.yaml"] = []byte(`
tool_sources:
  - name: platform
    kind: git
    url: https://x
    pull_policy: on-boot
`)
	sources, err := LoadSourcesYAMLFromFile(fakeFs, "/etc/wk/sources.yaml")
	if err != nil {
		t.Fatalf("LoadSourcesYAMLFromFile: %v", err)
	}
	if len(sources) != 1 || sources[0].Name != "platform" {
		t.Errorf("sources: got %+v", sources)
	}
}

func TestLoadSourcesYAMLFromFile_EmptyPath(t *testing.T) {
	t.Parallel()
	sources, err := LoadSourcesYAMLFromFile(newFakeFS(), "")
	if err != nil {
		t.Fatalf("LoadSourcesYAMLFromFile(''): %v", err)
	}
	if sources != nil {
		t.Errorf("expected nil sources for empty path, got %+v", sources)
	}
}

func TestLoadSourcesYAMLFromFile_NilFS(t *testing.T) {
	t.Parallel()
	_, err := LoadSourcesYAMLFromFile(nil, "/etc/wk/sources.yaml")
	if err == nil {
		t.Fatal("expected error for nil fs, got nil")
	}
}

func TestLoadSourcesYAMLFromFile_ReadError(t *testing.T) {
	t.Parallel()
	fakeFs := newFakeFS()
	// File not populated → fakeFS returns fs.ErrNotExist.
	_, err := LoadSourcesYAMLFromFile(fakeFs, "/etc/wk/missing.yaml")
	if err == nil {
		t.Fatal("expected wrapped read error, got nil")
	}
}
