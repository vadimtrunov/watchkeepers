package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun_WritesBanner(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if code := run(nil, &buf); code != 0 {
		t.Fatalf("run() exit code = %d, want 0", code)
	}
	if !strings.Contains(buf.String(), "watchkeeper-core") {
		t.Fatalf("run() output = %q, want substring %q", buf.String(), "watchkeeper-core")
	}
}

type errWriter struct{}

func (errWriter) Write(_ []byte) (int, error) { return 0, errTest }

var errTest = &simpleErr{msg: "boom"}

type simpleErr struct{ msg string }

func (e *simpleErr) Error() string { return e.msg }

func TestRun_ReturnsOneOnWriteError(t *testing.T) {
	t.Parallel()
	if code := run(nil, errWriter{}); code != 1 {
		t.Fatalf("run() exit code = %d, want 1", code)
	}
}
