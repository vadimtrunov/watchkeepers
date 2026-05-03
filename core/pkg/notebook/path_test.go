package notebook

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const (
	testAgentID  = "11111111-2222-3333-4444-555555555555"
	otherAgentID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
)

func TestAgentDBPath_RespectsEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(envDataDir, dir)

	got, err := agentDBPath(testAgentID)
	if err != nil {
		t.Fatalf("agentDBPath: %v", err)
	}
	want := filepath.Join(dir, "notebook", testAgentID+".sqlite")
	if got != want {
		t.Fatalf("path mismatch:\n got %s\nwant %s", got, want)
	}
}

func TestAgentDBPath_HomeDirFallback(t *testing.T) {
	dir := t.TempDir()
	// Force the home-dir branch by clearing WATCHKEEPER_DATA and pinning HOME.
	t.Setenv(envDataDir, "")
	t.Setenv("HOME", dir)
	if runtime.GOOS == "windows" {
		// os.UserHomeDir reads USERPROFILE on Windows; harness expects
		// Linux/macOS per AC5.
		t.Skip("home-dir layout assertion is Linux/macOS only")
	}

	got, err := agentDBPath(testAgentID)
	if err != nil {
		t.Fatalf("agentDBPath: %v", err)
	}
	want := filepath.Join(dir, ".local", "share", "watchkeepers", "notebook", testAgentID+".sqlite")
	if got != want {
		t.Fatalf("path mismatch:\n got %s\nwant %s", got, want)
	}
}

func TestAgentDBPath_RejectsBadUUID(t *testing.T) {
	cases := []string{
		"",
		"not-a-uuid",
		"11111111-2222-3333-4444-55555555555", // 11 hex tail
		"11111111-2222-3333-4444-5555555555555",
		"11111111x2222-3333-4444-555555555555",
		"../../../etc/passwd",
	}
	t.Setenv(envDataDir, t.TempDir())
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			_, err := agentDBPath(in)
			if !errors.Is(err, ErrInvalidAgentID) {
				t.Fatalf("agentDBPath(%q): err=%v, want ErrInvalidAgentID", in, err)
			}
		})
	}
}

func TestAgentDBPath_DirIsolation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(envDataDir, dir)

	pathA, err := agentDBPath(testAgentID)
	if err != nil {
		t.Fatalf("agentDBPath A: %v", err)
	}
	pathB, err := agentDBPath(otherAgentID)
	if err != nil {
		t.Fatalf("agentDBPath B: %v", err)
	}
	if pathA == pathB {
		t.Fatalf("two distinct agents resolved to the same path: %s", pathA)
	}
	if got := filepath.Dir(pathA); got != filepath.Dir(pathB) {
		t.Fatalf("agents should share the parent dir, got %s vs %s", filepath.Dir(pathA), filepath.Dir(pathB))
	}
	if !strings.HasSuffix(pathA, testAgentID+".sqlite") {
		t.Fatalf("unexpected suffix on %s", pathA)
	}
	if !strings.HasSuffix(pathB, otherAgentID+".sqlite") {
		t.Fatalf("unexpected suffix on %s", pathB)
	}
}

func TestAgentDBPath_DirMode0o700(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permissions are not meaningful on Windows")
	}
	dir := t.TempDir()
	t.Setenv(envDataDir, dir)

	if _, err := agentDBPath(testAgentID); err != nil {
		t.Fatalf("agentDBPath: %v", err)
	}
	notebookDir := filepath.Join(dir, "notebook")
	info, err := os.Stat(notebookDir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != dirMode {
		t.Fatalf("dir mode = %#o, want %#o", got, dirMode)
	}
}
