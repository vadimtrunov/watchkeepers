package toolregistry

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
)

func TestDecodeManifest_HappyPath(t *testing.T) {
	t.Parallel()
	raw := []byte(`{
		"name": "find_overdue_tickets",
		"version": "1.0.0",
		"capabilities": ["jira:read"],
		"schema": {"type": "object"}
	}`)
	m, err := DecodeManifest(raw)
	if err != nil {
		t.Fatalf("DecodeManifest: unexpected err: %v", err)
	}
	if m.Name != "find_overdue_tickets" {
		t.Errorf("Name: got %q, want %q", m.Name, "find_overdue_tickets")
	}
	if m.Version != "1.0.0" {
		t.Errorf("Version: got %q, want %q", m.Version, "1.0.0")
	}
	if len(m.Capabilities) != 1 || m.Capabilities[0] != "jira:read" {
		t.Errorf("Capabilities: got %v, want [jira:read]", m.Capabilities)
	}
	if len(m.Schema) == 0 {
		t.Error("Schema: empty")
	}
	if m.Source != "" {
		t.Errorf("Source: got %q, want empty (auto-fill is LoadManifestFromFile's job)", m.Source)
	}
	if m.Signature != "" {
		t.Errorf("Signature: got %q, want empty", m.Signature)
	}
}

func TestDecodeManifest_WithOptionalFields(t *testing.T) {
	t.Parallel()
	raw := []byte(`{
		"name": "count_open_prs",
		"version": "0.4.1",
		"capabilities": ["github:read", "github:list"],
		"schema": {"type": "object", "properties": {}},
		"source": "platform",
		"signature": "deadbeef"
	}`)
	m, err := DecodeManifest(raw)
	if err != nil {
		t.Fatalf("DecodeManifest: unexpected err: %v", err)
	}
	if m.Source != "platform" {
		t.Errorf("Source: got %q, want %q", m.Source, "platform")
	}
	if m.Signature != "deadbeef" {
		t.Errorf("Signature: got %q, want %q", m.Signature, "deadbeef")
	}
}

func TestDecodeManifest_EmptyInput(t *testing.T) {
	t.Parallel()
	_, err := DecodeManifest(nil)
	if !errors.Is(err, ErrManifestParse) {
		t.Fatalf("expected ErrManifestParse, got %v", err)
	}
}

func TestDecodeManifest_MalformedJSON(t *testing.T) {
	t.Parallel()
	cases := []string{
		`{`,
		`}`,
		`{"name": "x"`, // truncated
		`not even json`,
	}
	for _, raw := range cases {
		_, err := DecodeManifest([]byte(raw))
		if !errors.Is(err, ErrManifestParse) {
			t.Errorf("%q: expected ErrManifestParse, got %v", raw, err)
		}
	}
}

func TestDecodeManifest_UnknownField(t *testing.T) {
	t.Parallel()
	raw := []byte(`{
		"name": "x", "version": "1", "capabilities": ["c"],
		"schema": {}, "frobnicate": true
	}`)
	_, err := DecodeManifest(raw)
	if !errors.Is(err, ErrManifestUnknownField) {
		t.Fatalf("expected ErrManifestUnknownField, got %v", err)
	}
	// Also chained through ErrManifestParse so callers matching
	// either sentinel succeed.
	if !errors.Is(err, ErrManifestParse) {
		t.Errorf("expected chain through ErrManifestParse, got %v", err)
	}
}

func TestDecodeManifest_MissingRequired(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"missing-name": `{"version":"1","capabilities":["c"],"schema":{}}`,
		"empty-name":   `{"name":"","version":"1","capabilities":["c"],"schema":{}}`,
		"blank-name":   `{"name":"   ","version":"1","capabilities":["c"],"schema":{}}`,
		"missing-ver":  `{"name":"x","capabilities":["c"],"schema":{}}`,
		"missing-caps": `{"name":"x","version":"1","schema":{}}`,
		"empty-caps":   `{"name":"x","version":"1","capabilities":[],"schema":{}}`,
		"blank-cap":    `{"name":"x","version":"1","capabilities":[" "],"schema":{}}`,
		"missing-sch":  `{"name":"x","version":"1","capabilities":["c"]}`,
		"null-sch":     `{"name":"x","version":"1","capabilities":["c"],"schema":null}`,
	}
	for label, raw := range cases {
		_, err := DecodeManifest([]byte(raw))
		if !errors.Is(err, ErrManifestMissingRequired) {
			t.Errorf("%s: expected ErrManifestMissingRequired, got %v", label, err)
		}
	}
}

func TestDecodeManifest_TrailingGarbage(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"name":"x","version":"1","capabilities":["c"],"schema":{}}{"another":true}`)
	_, err := DecodeManifest(raw)
	if !errors.Is(err, ErrManifestParse) {
		t.Fatalf("expected ErrManifestParse for trailing garbage, got %v", err)
	}
}

func TestDecodeManifest_DefensiveCopy(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"name":"x","version":"1","capabilities":["a","b"],"schema":{"type":"object"}}`)
	m, err := DecodeManifest(raw)
	if err != nil {
		t.Fatalf("DecodeManifest: %v", err)
	}
	// Mutating raw post-decode MUST NOT affect the returned manifest.
	for i := range raw {
		raw[i] = 0
	}
	if m.Name != "x" || m.Version != "1" {
		t.Errorf("post-mutation: scalars corrupted (Name=%q, Version=%q)", m.Name, m.Version)
	}
	if len(m.Capabilities) != 2 || m.Capabilities[0] != "a" || m.Capabilities[1] != "b" {
		t.Errorf("post-mutation: Capabilities corrupted: %v", m.Capabilities)
	}
	if !strings.Contains(string(m.Schema), "object") {
		t.Errorf("post-mutation: Schema corrupted: %q", string(m.Schema))
	}
}

func TestLoadManifestFromFile_HappyPath(t *testing.T) {
	t.Parallel()
	fakeFs := newFakeFS()
	fakeFs.files["tools/platform/count_open_prs/manifest.json"] = []byte(
		`{"name":"count_open_prs","version":"1.0.0","capabilities":["github:read"],"schema":{}}`,
	)
	m, err := LoadManifestFromFile(fakeFs, "tools/platform/count_open_prs", "platform")
	if err != nil {
		t.Fatalf("LoadManifestFromFile: %v", err)
	}
	if m.Source != "platform" {
		t.Errorf("Source: got %q, want %q (auto-fill)", m.Source, "platform")
	}
}

func TestLoadManifestFromFile_SourceMatch(t *testing.T) {
	t.Parallel()
	fakeFs := newFakeFS()
	fakeFs.files["tools/platform/x/manifest.json"] = []byte(
		`{"name":"x","version":"1","capabilities":["c"],"schema":{},"source":"platform"}`,
	)
	m, err := LoadManifestFromFile(fakeFs, "tools/platform/x", "platform")
	if err != nil {
		t.Fatalf("LoadManifestFromFile: %v", err)
	}
	if m.Source != "platform" {
		t.Errorf("Source: got %q, want %q", m.Source, "platform")
	}
}

func TestLoadManifestFromFile_SourceCollision(t *testing.T) {
	t.Parallel()
	fakeFs := newFakeFS()
	fakeFs.files["tools/platform/x/manifest.json"] = []byte(
		`{"name":"x","version":"1","capabilities":["c"],"schema":{},"source":"private"}`,
	)
	_, err := LoadManifestFromFile(fakeFs, "tools/platform/x", "platform")
	if !errors.Is(err, ErrManifestSourceCollision) {
		t.Fatalf("expected ErrManifestSourceCollision, got %v", err)
	}
}

func TestLoadManifestFromFile_EmptyStampingArg(t *testing.T) {
	t.Parallel()
	fakeFs := newFakeFS()
	_, err := LoadManifestFromFile(fakeFs, "tools/x", "")
	if !errors.Is(err, ErrInvalidSourceName) {
		t.Fatalf("expected ErrInvalidSourceName, got %v", err)
	}
	if fakeFs.readCalls != 0 {
		t.Errorf("FS.ReadFile called %d times — should be 0 (validation runs first)", fakeFs.readCalls)
	}
}

func TestLoadManifestFromFile_NilFSPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on nil fs")
		}
		if !strings.Contains(fmt.Sprint(r), "fs must not be nil") {
			t.Errorf("panic message: got %q", r)
		}
	}()
	_, _ = LoadManifestFromFile(nil, "tools/x", "platform")
}

func TestLoadManifestFromFile_ReadError(t *testing.T) {
	t.Parallel()
	fakeFs := newFakeFS()
	_, err := LoadManifestFromFile(fakeFs, "tools/missing", "platform")
	if !errors.Is(err, ErrManifestParse) {
		t.Fatalf("expected wrapped ErrManifestParse, got %v", err)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("expected chain through fs.ErrNotExist, got %v", err)
	}
}

func TestLoadManifestFromFile_DecodeError(t *testing.T) {
	t.Parallel()
	fakeFs := newFakeFS()
	fakeFs.files["tools/x/manifest.json"] = []byte("not json")
	_, err := LoadManifestFromFile(fakeFs, "tools/x", "platform")
	if !errors.Is(err, ErrManifestParse) {
		t.Fatalf("expected ErrManifestParse, got %v", err)
	}
}

// Source-grep AC: the toolregistry production sources never reach
// for `keeperslog.` or `.Append(` — audit emission belongs to M9.7,
// not to M9.1.a. A drift here would mean an audit dependency snuck
// into the data-layer package.
func TestSourceGrepAC_NoAuditCallsInProductionSources(t *testing.T) {
	t.Parallel()
	productionFiles := []string{
		"doc.go",
		"errors.go",
		"manifest.go",
		"config.go",
		"events.go",
		"scheduler.go",
		"osfs.go",
		"yaml.go",
	}
	for _, name := range productionFiles {
		raw, err := osReadProductionFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		body := string(raw)
		for _, banned := range []string{"keeperslog.", ".Append("} {
			if containsOutsideComments(body, banned) {
				t.Errorf("%s: contains banned token %q outside comments (audit emission belongs to M9.7)", name, banned)
			}
		}
	}
}

// osReadProductionFile reads a sibling file in the toolregistry
// package via os.ReadFile so the source-grep AC consults the
// actual on-disk bytes (not an in-memory snapshot).
func osReadProductionFile(name string) ([]byte, error) {
	// Resolve relative to the test working directory which `go test`
	// sets to the package directory.
	return readFile(name)
}
