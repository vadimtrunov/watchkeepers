package toolregistry

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"testing"
	"time"
)

func TestScanSourceDir_HappyPathAlphaOrder(t *testing.T) {
	t.Parallel()
	fakeFs := newFakeFS()
	// Two tool subdirs; entries deliberately given in reverse-alpha
	// order to verify the scanner re-sorts by manifest.Name.
	parent := filepath.Join("/data", "tools", "platform")
	fakeFs.dirEntries[parent] = []fs.DirEntry{
		fakeDirEntry{name: "zebra", isDir: true},
		fakeDirEntry{name: "ant", isDir: true},
	}
	fakeFs.files[filepath.Join(parent, "zebra", "manifest.json")] = []byte(
		`{"name":"zebra","version":"1.0.0","capabilities":["c"],"schema":{},"dry_run_mode":"none"}`,
	)
	fakeFs.files[filepath.Join(parent, "ant", "manifest.json")] = []byte(
		`{"name":"ant","version":"1.0.0","capabilities":["c"],"schema":{},"dry_run_mode":"none"}`,
	)

	got, err := ScanSourceDir(context.Background(), fakeFs, "/data", "platform", nil)
	if err != nil {
		t.Fatalf("ScanSourceDir: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len: got %d, want 2", len(got))
	}
	if got[0].Name != "ant" || got[1].Name != "zebra" {
		t.Errorf("order: got %q, %q (want ant, zebra)", got[0].Name, got[1].Name)
	}
	for _, m := range got {
		if m.Source != "platform" {
			t.Errorf("Source: got %q, want platform", m.Source)
		}
	}
}

func TestScanSourceDir_MissingDirReturnsNilNil(t *testing.T) {
	t.Parallel()
	fakeFs := newFakeFS()
	got, err := ScanSourceDir(context.Background(), fakeFs, "/data", "nonexistent", nil)
	if err != nil {
		t.Fatalf("expected nil err for missing dir, got %v", err)
	}
	if got != nil {
		t.Errorf("expected nil result for missing dir, got %v", got)
	}
}

func TestScanSourceDir_ReadDirErrorSurfaces(t *testing.T) {
	t.Parallel()
	fakeFs := newFakeFS()
	parent := filepath.Join("/data", "tools", "x")
	fakeFs.readDirErr[parent] = errSentinel

	_, err := ScanSourceDir(context.Background(), fakeFs, "/data", "x", nil)
	if !errors.Is(err, ErrScanReadDir) {
		t.Fatalf("expected ErrScanReadDir, got %v", err)
	}
	if !errors.Is(err, errSentinel) {
		t.Errorf("expected chain through errSentinel, got %v", err)
	}
}

func TestScanSourceDir_MalformedManifestSkippedAndLogged(t *testing.T) {
	t.Parallel()
	fakeFs := newFakeFS()
	parent := filepath.Join("/data", "tools", "platform")
	fakeFs.dirEntries[parent] = []fs.DirEntry{
		fakeDirEntry{name: "good", isDir: true},
		fakeDirEntry{name: "bad", isDir: true},
	}
	fakeFs.files[filepath.Join(parent, "good", "manifest.json")] = []byte(
		`{"name":"good","version":"1","capabilities":["c"],"schema":{},"dry_run_mode":"none"}`,
	)
	fakeFs.files[filepath.Join(parent, "bad", "manifest.json")] = []byte("not json")

	logger := &fakeLogger{}
	got, err := ScanSourceDir(context.Background(), fakeFs, "/data", "platform", logger)
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
	if len(got) != 1 || got[0].Name != "good" {
		t.Errorf("scan: got %+v, want only [good]", got)
	}
	entries := logger.snapshot()
	if len(entries) == 0 {
		t.Fatal("expected logger to capture the bad manifest")
	}
	found := false
	for _, e := range entries {
		if e.msg == "toolregistry: manifest scan failed" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected 'manifest scan failed' log msg, got entries: %+v", entries)
	}
}

func TestScanSourceDir_NonDirEntriesIgnored(t *testing.T) {
	t.Parallel()
	fakeFs := newFakeFS()
	parent := filepath.Join("/data", "tools", "x")
	fakeFs.dirEntries[parent] = []fs.DirEntry{
		fakeDirEntry{name: "README.md", isDir: false},
		fakeDirEntry{name: "good", isDir: true},
	}
	fakeFs.files[filepath.Join(parent, "good", "manifest.json")] = []byte(
		`{"name":"good","version":"1","capabilities":["c"],"schema":{},"dry_run_mode":"none"}`,
	)

	got, err := ScanSourceDir(context.Background(), fakeFs, "/data", "x", nil)
	if err != nil {
		t.Fatalf("ScanSourceDir: %v", err)
	}
	if len(got) != 1 || got[0].Name != "good" {
		t.Errorf("expected only [good], got %+v", got)
	}
}

func TestScanSourceDir_CtxCancelMidScan(t *testing.T) {
	t.Parallel()
	fakeFs := newFakeFS()
	parent := filepath.Join("/data", "tools", "x")
	fakeFs.dirEntries[parent] = []fs.DirEntry{
		fakeDirEntry{name: "a", isDir: true},
		fakeDirEntry{name: "b", isDir: true},
	}
	fakeFs.files[filepath.Join(parent, "a", "manifest.json")] = []byte(
		`{"name":"a","version":"1","capabilities":["c"],"schema":{},"dry_run_mode":"none"}`,
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := ScanSourceDir(ctx, fakeFs, "/data", "x", nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestScanSourceDir_NilFSPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on nil fs")
		}
	}()
	_, _ = ScanSourceDir(context.Background(), nil, "/data", "x", nil)
}

func TestBuildEffective_PrecedenceFlatteningEarlierWins(t *testing.T) {
	t.Parallel()
	fakeFs := newFakeFS()
	// `platform` and `private` both ship `count_open_prs`; platform is
	// listed first → platform wins, private becomes a shadow.
	for _, src := range []string{"platform", "private"} {
		parent := filepath.Join("/data", "tools", src)
		fakeFs.dirEntries[parent] = []fs.DirEntry{
			fakeDirEntry{name: "count_open_prs", isDir: true},
		}
		fakeFs.files[filepath.Join(parent, "count_open_prs", "manifest.json")] = []byte(
			`{"name":"count_open_prs","version":"1.0.0","capabilities":["c"],"schema":{},"dry_run_mode":"none"}`,
		)
	}
	sources := []SourceConfig{
		{Name: "platform", Kind: SourceKindGit, URL: "https://x", PullPolicy: PullPolicyOnBoot},
		{Name: "private", Kind: SourceKindGit, URL: "https://y", PullPolicy: PullPolicyOnBoot},
	}
	snap, shadows, err := BuildEffective(context.Background(), fakeFs, "/data", sources, time.Now(), nil)
	if err != nil {
		t.Fatalf("BuildEffective: %v", err)
	}
	if snap.Len() != 1 {
		t.Fatalf("expected 1 tool after flattening, got %d", snap.Len())
	}
	got, _ := snap.Lookup("count_open_prs")
	if got.Source != "platform" {
		t.Errorf("expected platform-wins precedence, got Source=%q", got.Source)
	}
	if len(shadows) != 1 {
		t.Fatalf("expected 1 shadow, got %d (%+v)", len(shadows), shadows)
	}
	sh := shadows[0]
	if sh.ToolName != "count_open_prs" || sh.WinnerSource != "platform" || sh.ShadowedSource != "private" {
		t.Errorf("shadow metadata: got %+v", sh)
	}
}

func TestBuildEffective_PerSourceReadDirErrorIsolated(t *testing.T) {
	t.Parallel()
	fakeFs := newFakeFS()
	good := filepath.Join("/data", "tools", "good")
	bad := filepath.Join("/data", "tools", "bad")
	fakeFs.dirEntries[good] = []fs.DirEntry{fakeDirEntry{name: "g1", isDir: true}}
	fakeFs.files[filepath.Join(good, "g1", "manifest.json")] = []byte(
		`{"name":"g1","version":"1","capabilities":["c"],"schema":{},"dry_run_mode":"none"}`,
	)
	fakeFs.readDirErr[bad] = errSentinel

	sources := []SourceConfig{
		{Name: "bad", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
		{Name: "good", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
	}
	logger := &fakeLogger{}
	snap, shadows, err := BuildEffective(context.Background(), fakeFs, "/data", sources, time.Now(), logger)
	if err != nil {
		t.Fatalf("BuildEffective should isolate per-source errors, got err=%v", err)
	}
	if snap.Len() != 1 || snap.Tools[0].Manifest.Name != "g1" {
		t.Errorf("expected only [g1] to survive, got %+v", snap.Tools)
	}
	if len(shadows) != 0 {
		t.Errorf("ReadDir-isolation does not produce shadows, got %+v", shadows)
	}
	// Source-failure log fired.
	found := false
	for _, e := range logger.snapshot() {
		if e.msg == "toolregistry: source scan failed" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected 'source scan failed' log, got: %+v", logger.snapshot())
	}
}

func TestBuildEffective_CtxCancel(t *testing.T) {
	t.Parallel()
	fakeFs := newFakeFS()
	parent := filepath.Join("/data", "tools", "x")
	fakeFs.dirEntries[parent] = []fs.DirEntry{fakeDirEntry{name: "t1", isDir: true}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	sources := []SourceConfig{{Name: "x", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot}}
	_, _, err := BuildEffective(ctx, fakeFs, "/data", sources, time.Now(), nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestBuildEffective_NilFSPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on nil fs")
		}
	}()
	_, _, _ = BuildEffective(context.Background(), nil, "/data", nil, time.Now(), nil)
}

// TestScanSourceDir_IntraSourceDuplicateDeterministicWinner — when two
// per-tool subdirectories within the same source declare the same
// manifest name, the registry must deterministically pick the
// alphabetically-first toolDir regardless of [FS.ReadDir]'s
// iteration order. Pre-sorting `entries` in [ScanSourceDir] pins
// the tiebreaker.
func TestScanSourceDir_IntraSourceDuplicateDeterministicWinner(t *testing.T) {
	t.Parallel()
	for _, order := range []struct {
		label   string
		entries []fs.DirEntry
	}{
		{
			label: "z-before-a",
			entries: []fs.DirEntry{
				fakeDirEntry{name: "z_winner", isDir: true},
				fakeDirEntry{name: "a_loser", isDir: true},
			},
		},
		{
			label: "a-before-z",
			entries: []fs.DirEntry{
				fakeDirEntry{name: "a_loser", isDir: true},
				fakeDirEntry{name: "z_winner", isDir: true},
			},
		},
	} {
		order := order
		t.Run(order.label, func(t *testing.T) {
			t.Parallel()
			fakeFs := newFakeFS()
			parent := filepath.Join("/data", "tools", "src")
			fakeFs.dirEntries[parent] = order.entries
			// Both directories declare the SAME manifest name.
			fakeFs.files[filepath.Join(parent, "z_winner", "manifest.json")] = []byte(
				`{"name":"dup","version":"1.0","capabilities":["c"],"schema":{},"dry_run_mode":"none"}`,
			)
			fakeFs.files[filepath.Join(parent, "a_loser", "manifest.json")] = []byte(
				`{"name":"dup","version":"2.0","capabilities":["c"],"schema":{},"dry_run_mode":"none"}`,
			)
			logger := &fakeLogger{}
			got, err := ScanSourceDir(context.Background(), fakeFs, "/data", "src", logger)
			if err != nil {
				t.Fatalf("ScanSourceDir: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("expected 1 manifest after intra-source dedupe, got %d", len(got))
			}
			// Alphabetically-first toolDir wins: "a_loser" < "z_winner".
			if got[0].Version != "2.0" {
				t.Errorf("dedupe non-deterministic: got version %q, want %q (a_loser should win alphabetically)", got[0].Version, "2.0")
			}
			// Loser log fired with both dir names.
			found := false
			for _, e := range logger.snapshot() {
				if e.msg == "toolregistry: "+ErrIntraSourceDuplicateManifestName.Error() {
					found = true
				}
			}
			if !found {
				t.Errorf("expected intra-source duplicate log, got: %+v", logger.snapshot())
			}
		})
	}
}

func TestBuildEffective_EmptySourcesReturnsEmptySnapshot(t *testing.T) {
	t.Parallel()
	snap, shadows, err := BuildEffective(context.Background(), newFakeFS(), "/data", nil, time.Now(), nil)
	if err != nil {
		t.Fatalf("BuildEffective: %v", err)
	}
	if snap.Len() != 0 {
		t.Errorf("expected empty snapshot, got %d tools", snap.Len())
	}
	// BuildEffective stamps Revision=0; callers needing a monotonic
	// counter (the Registry) stamp post-construction.
	if snap.Revision != 0 {
		t.Errorf("Revision: got %d, want 0 (caller stamps)", snap.Revision)
	}
	if len(shadows) != 0 {
		t.Errorf("empty sources never produce shadows, got %+v", shadows)
	}
}
