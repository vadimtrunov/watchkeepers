package localpatch

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestContentDigest_Deterministic(t *testing.T) {
	t.Parallel()
	f := newFakeFS()
	f.AddFile("/r/manifest.json", []byte(`{"x":1}`))
	f.AddFile("/r/src/a.ts", []byte(`a`))
	f.AddFile("/r/src/b.ts", []byte(`b`))

	h1, err := ContentDigest(f, "/r")
	if err != nil {
		t.Fatalf("ContentDigest: %v", err)
	}
	h2, err := ContentDigest(f, "/r")
	if err != nil {
		t.Fatalf("ContentDigest 2nd: %v", err)
	}
	if h1 != h2 {
		t.Errorf("digest not deterministic: %q vs %q", h1, h2)
	}
	if len(h1) != 64 {
		t.Errorf("digest length: got %d want 64", len(h1))
	}
}

func TestContentDigest_ChangesOnContentChange(t *testing.T) {
	t.Parallel()
	f1 := newFakeFS()
	f1.AddFile("/r/manifest.json", []byte(`{"x":1}`))
	h1, err := ContentDigest(f1, "/r")
	if err != nil {
		t.Fatalf("d1: %v", err)
	}
	f2 := newFakeFS()
	f2.AddFile("/r/manifest.json", []byte(`{"x":2}`))
	h2, err := ContentDigest(f2, "/r")
	if err != nil {
		t.Fatalf("d2: %v", err)
	}
	if h1 == h2 {
		t.Errorf("digest stable across content change")
	}
}

func TestContentDigest_ChangesOnExecBitFlip(t *testing.T) {
	t.Parallel()
	f1 := newFakeFS()
	f1.AddFileMode("/r/run.sh", []byte(`echo`), 0o644)
	h1, _ := ContentDigest(f1, "/r")
	f2 := newFakeFS()
	f2.AddFileMode("/r/run.sh", []byte(`echo`), 0o755)
	h2, _ := ContentDigest(f2, "/r")
	if h1 == h2 {
		t.Errorf("digest stable across exec-bit flip")
	}
}

func TestContentDigest_PathPrefixCollisionResistant(t *testing.T) {
	t.Parallel()
	// Two trees that, without length-prefixing, would byte-stream
	// identically: ("a", "bc") vs ("ab", "c"). Length-prefixing
	// guarantees distinct digests.
	f1 := newFakeFS()
	f1.AddFile("/r/a", []byte("bc"))
	f2 := newFakeFS()
	f2.AddFile("/r/ab", []byte("c"))
	h1, _ := ContentDigest(f1, "/r")
	h2, _ := ContentDigest(f2, "/r")
	if h1 == h2 {
		t.Errorf("digest collides under path-prefix permutation")
	}
}

func TestContentDigest_EmptyRootRefused(t *testing.T) {
	t.Parallel()
	_, err := ContentDigest(newFakeFS(), "")
	if !errors.Is(err, ErrFolderRead) {
		t.Errorf("err: got %v want ErrFolderRead", err)
	}
}

func TestContentDigest_NilFSPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected panic on nil FS")
		}
	}()
	_, _ = ContentDigest(nil, "/r")
}

func TestContentDigest_ReadDirErrorWrapped(t *testing.T) {
	t.Parallel()
	f := newFakeFS()
	f.AddDir("/r")
	f.readDirErr["/r"] = errors.New("io error")
	_, err := ContentDigest(f, "/r")
	if !errors.Is(err, ErrFolderRead) {
		t.Errorf("err: got %v want ErrFolderRead", err)
	}
}

func TestContentDigest_PassesSnapshotIntegrationCheck(t *testing.T) {
	t.Parallel()
	// End-to-end: install creates a snapshot; the snapshot's digest
	// must equal the original folder's digest (the snapshot/copy
	// pipeline must be content-preserving).
	fx := newInstallerFixture(t)
	fx.fs.AddFile(filepath.Join(testFolderPath, "src/extra.ts"), []byte(`extra`))
	folderDigest, err := ContentDigest(fx.fs, testFolderPath)
	if err != nil {
		t.Fatalf("digest folder: %v", err)
	}
	// Pre-populate live tree so install snapshots before overwrite.
	live := filepath.Join(testDataDir, "tools", testSourceName, testToolName)
	fx.fs.AddFile(filepath.Join(live, "manifest.json"), validManifestJSON(testToolName, "0.9.0"))
	if _, err := fx.inst.Install(context.Background(), validInstallReq()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	// Compute digest over the new live tree — it should equal the
	// original folder's digest (we just copied byte-for-byte).
	liveDigest, err := ContentDigest(fx.fs, live)
	if err != nil {
		t.Fatalf("digest live: %v", err)
	}
	if folderDigest != liveDigest {
		t.Errorf("install copy is not content-preserving: folder=%s live=%s", folderDigest, liveDigest)
	}
}

func TestSnapshotPath_RejectsTraversal(t *testing.T) {
	t.Parallel()
	cases := []struct{ src, tool, ver string }{
		{"../escape", "ok", "1.0.0"},
		{"ok", "..", "1.0.0"},
		{"ok", "ok", "../escape"},
		{"", "ok", "1.0.0"},
		{"ok/sub", "ok", "1.0.0"},
		{"ok", "ok", "1.0.0/sub"},
	}
	for _, tc := range cases {
		_, err := snapshotPath("/data", tc.src, tc.tool, tc.ver)
		if err == nil {
			t.Errorf("snapshotPath(%q,%q,%q) returned nil — expected traversal refusal", tc.src, tc.tool, tc.ver)
		}
		if err != nil && !strings.Contains(err.Error(), "unsafe path") {
			t.Errorf("snapshotPath(%q,%q,%q): err missing 'unsafe path': %v", tc.src, tc.tool, tc.ver, err)
		}
	}
}

func TestLiveToolPath_RejectsTraversal(t *testing.T) {
	t.Parallel()
	// Iter-1 codex m11 fix: include embedded-separator cases the
	// snapshotPath test covers but liveToolPath previously did not.
	cases := []struct{ src, tool string }{
		{"../escape", "ok"},
		{"ok", ".."},
		{"", "ok"},
		{"ok/sub", "ok"},
		{"ok", "ok/sub"},
		{"ok\\sub", "ok"},
	}
	for _, tc := range cases {
		_, err := liveToolPath("/data", tc.src, tc.tool)
		if err == nil {
			t.Errorf("liveToolPath(%q,%q) returned nil — expected refusal", tc.src, tc.tool)
		}
	}
}
