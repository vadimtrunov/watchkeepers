package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestRun_MissingDatabaseURL covers the AC2 negative contract: when
// KEEP_DATABASE_URL is unset the binary exits non-zero and writes a stable,
// locale-independent diagnostic (LESSON M2.1.b) to stderr.
func TestRun_MissingDatabaseURL(t *testing.T) {
	t.Setenv("KEEP_DATABASE_URL", "")

	var stdout, stderr bytes.Buffer
	code := run(nil, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("run returned 0, want non-zero for missing KEEP_DATABASE_URL")
	}

	msg := stderr.String()
	if !strings.Contains(msg, "KEEP_DATABASE_URL is required") {
		t.Errorf("stderr = %q, want stable phrase %q", msg, "KEEP_DATABASE_URL is required")
	}
}
