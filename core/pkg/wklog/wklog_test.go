package wklog_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/wklog"
)

// staticLookup is a deterministic LevelLookup for tests. It is map-backed
// rather than env-backed so concurrent t.Setenv calls cannot interfere
// with parallel sub-tests.
type staticLookup map[string]string

func (s staticLookup) lookup(key string) (string, bool) {
	v, ok := s[key]
	return v, ok
}

// decodeRecord parses one JSON line emitted by the slog handler. It
// returns the raw map so individual fields can be asserted.
func decodeRecord(t *testing.T, line []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatalf("decode %q: %v", line, err)
	}
	return m
}

func TestNew_DefaultsToInfoAndJSON(t *testing.T) {
	var buf bytes.Buffer
	logger := wklog.NewWithWriter("test.subsys", &buf, wklog.Options{
		LevelLookup: staticLookup{}.lookup,
	})

	logger.Debug("hidden") // default level is INFO so DEBUG is dropped.
	logger.Info("seen", "k", "v")

	lines := bytes.Split(bytes.TrimRight(buf.Bytes(), "\n"), []byte("\n"))
	if len(lines) != 1 {
		t.Fatalf("want 1 line (INFO only), got %d: %q", len(lines), buf.String())
	}
	rec := decodeRecord(t, lines[0])
	if rec["level"] != "INFO" {
		t.Errorf("level = %v, want INFO", rec["level"])
	}
	if rec["msg"] != "seen" {
		t.Errorf("msg = %v, want seen", rec["msg"])
	}
	if rec[wklog.AttrSubsystem] != "test.subsys" {
		t.Errorf("%s = %v, want test.subsys", wklog.AttrSubsystem, rec[wklog.AttrSubsystem])
	}
	if rec["k"] != "v" {
		t.Errorf("user attr k = %v, want v", rec["k"])
	}
}

func TestNew_PerSubsystemEnvOverridesGlobal(t *testing.T) {
	var buf bytes.Buffer
	env := staticLookup{
		wklog.EnvLevelDefault:                 "warn",
		wklog.EnvLevelPrefix + "TEST_SUBSYS":  "debug",
		wklog.EnvLevelPrefix + "OTHER_SUBSYS": "error",
	}
	logger := wklog.NewWithWriter("test.subsys", &buf, wklog.Options{
		LevelLookup: env.lookup,
	})

	logger.Debug("hello")

	if buf.Len() == 0 {
		t.Fatal("expected DEBUG record, got nothing — per-subsystem override not applied")
	}
	rec := decodeRecord(t, bytes.TrimRight(buf.Bytes(), "\n"))
	if rec["level"] != "DEBUG" {
		t.Errorf("level = %v, want DEBUG", rec["level"])
	}
}

func TestNew_GlobalEnvFallback(t *testing.T) {
	var buf bytes.Buffer
	env := staticLookup{wklog.EnvLevelDefault: "error"}
	logger := wklog.NewWithWriter("test.subsys", &buf, wklog.Options{
		LevelLookup: env.lookup,
	})

	logger.Warn("hidden")
	logger.Error("seen")

	lines := bytes.Split(bytes.TrimRight(buf.Bytes(), "\n"), []byte("\n"))
	if len(lines) != 1 {
		t.Fatalf("want 1 ERROR line, got %d", len(lines))
	}
	rec := decodeRecord(t, lines[0])
	if rec["level"] != "ERROR" {
		t.Errorf("level = %v, want ERROR", rec["level"])
	}
}

func TestNew_UnrecognisedLevelEmitsWarning(t *testing.T) {
	var (
		out  bytes.Buffer
		warn bytes.Buffer
	)
	env := staticLookup{wklog.EnvLevelDefault: "lolwat"}
	logger := wklog.NewWithWriter("test.subsys", &out, wklog.Options{
		LevelLookup: env.lookup,
		WarnSink:    &warn,
	})

	if !strings.Contains(warn.String(), wklog.EnvLevelDefault) {
		t.Errorf("warning text missing env name: %q", warn.String())
	}
	if !strings.Contains(warn.String(), "lolwat") {
		t.Errorf("warning text missing offending value: %q", warn.String())
	}

	logger.Info("ok")
	rec := decodeRecord(t, bytes.TrimRight(out.Bytes(), "\n"))
	if rec["level"] != "INFO" {
		t.Errorf("fallback level = %v, want INFO", rec["level"])
	}
}

func TestNew_UnrecognisedPerSubsystemLevelFallsThrough(t *testing.T) {
	var (
		out  bytes.Buffer
		warn bytes.Buffer
	)
	env := staticLookup{
		wklog.EnvLevelPrefix + "TEST_SUBSYS": "huh",
		wklog.EnvLevelDefault:                "error",
	}
	logger := wklog.NewWithWriter("test.subsys", &out, wklog.Options{
		LevelLookup: env.lookup,
		WarnSink:    &warn,
	})

	if !strings.Contains(warn.String(), "TEST_SUBSYS") {
		t.Errorf("warning text missing offending subsystem env: %q", warn.String())
	}

	// Per-subsystem fell through to global "error"; INFO should be dropped.
	logger.Info("hidden")
	logger.Error("seen")
	if got := bytes.Count(out.Bytes(), []byte("\n")); got != 1 {
		t.Fatalf("want exactly 1 ERROR line, got %d: %q", got, out.String())
	}
	rec := decodeRecord(t, bytes.TrimRight(out.Bytes(), "\n"))
	if rec["level"] != "ERROR" {
		t.Errorf("level = %v, want ERROR", rec["level"])
	}
}

func TestNew_ExplicitLevelOptionWins(t *testing.T) {
	var buf bytes.Buffer
	debug := slog.LevelDebug
	logger := wklog.NewWithWriter("test.subsys", &buf, wklog.Options{
		Level: &debug,
		LevelLookup: staticLookup{
			wklog.EnvLevelDefault: "error",
		}.lookup,
	})

	logger.Debug("seen")

	if buf.Len() == 0 {
		t.Fatal("expected DEBUG record, got nothing — Options.Level should win")
	}
}

func TestWithCorrelationID_AppearsInRecord(t *testing.T) {
	var buf bytes.Buffer
	logger := wklog.NewWithWriter("test.subsys", &buf, wklog.Options{
		LevelLookup: staticLookup{}.lookup,
	})

	ctx := wklog.WithCorrelationID(context.Background(), "req-abc")
	logger.InfoContext(ctx, "msg")

	rec := decodeRecord(t, bytes.TrimRight(buf.Bytes(), "\n"))
	if rec[wklog.AttrCorrelationID] != "req-abc" {
		t.Errorf("%s = %v, want req-abc", wklog.AttrCorrelationID, rec[wklog.AttrCorrelationID])
	}
}

func TestWithCorrelationID_EmptyIDIsNotStored(t *testing.T) {
	ctx := wklog.WithCorrelationID(context.Background(), "")
	if _, ok := wklog.CorrelationIDFromContext(ctx); ok {
		t.Error("empty id should not be stored")
	}
}

func TestCorrelationIDFromContext_NilContextIsSafe(t *testing.T) {
	//nolint:staticcheck // SA1012: passing nil is intentional — the API
	// must tolerate nil so callers do not have to nil-guard the lookup.
	if _, ok := wklog.CorrelationIDFromContext(nil); ok {
		t.Error("nil context should report no id")
	}
}

func TestCorrelationID_AbsentInRecordWhenNotSet(t *testing.T) {
	var buf bytes.Buffer
	logger := wklog.NewWithWriter("test.subsys", &buf, wklog.Options{
		LevelLookup: staticLookup{}.lookup,
	})

	logger.Info("plain")

	rec := decodeRecord(t, bytes.TrimRight(buf.Bytes(), "\n"))
	if _, ok := rec[wklog.AttrCorrelationID]; ok {
		t.Errorf("%s should be absent when no id is set", wklog.AttrCorrelationID)
	}
}

func TestWithAttrs_PreservesCorrelationAtRoot(t *testing.T) {
	var buf bytes.Buffer
	logger := wklog.NewWithWriter("test.subsys", &buf, wklog.Options{
		LevelLookup: staticLookup{}.lookup,
	}).With("static", "x")

	ctx := wklog.WithCorrelationID(context.Background(), "req-z")
	logger.InfoContext(ctx, "msg")

	rec := decodeRecord(t, bytes.TrimRight(buf.Bytes(), "\n"))
	if rec[wklog.AttrCorrelationID] != "req-z" {
		t.Errorf("correlation id lost after With(): %v", rec)
	}
	if rec["static"] != "x" {
		t.Errorf("static attr lost: %v", rec)
	}
}

// TestWithGroup_CorrelationIDNestsUnderGroup pins the M10.1 iter-1
// finding M2: when a caller chains WithGroup onto a wklog logger,
// slog's group semantics force EVERY record-level attribute (including
// the correlation_id wklog injects at Handle time) to nest under the
// group. The pre-iter-1 doc claimed correlation_id was always at the
// JSON root; this test pins the actual behaviour so a future "fix"
// that flattens it loudly fails the regression.
func TestWithGroup_CorrelationIDNestsUnderGroup(t *testing.T) {
	var buf bytes.Buffer
	logger := wklog.NewWithWriter("test.subsys", &buf, wklog.Options{
		LevelLookup: staticLookup{}.lookup,
	}).WithGroup("audit")

	ctx := wklog.WithCorrelationID(context.Background(), "req-grouped")
	logger.InfoContext(ctx, "msg", "k", "v")

	rec := decodeRecord(t, bytes.TrimRight(buf.Bytes(), "\n"))
	// At JSON root: time, level, msg, subsystem, audit (group).
	// correlation_id is INSIDE audit under the iter-1 contract.
	if _, ok := rec[wklog.AttrCorrelationID]; ok {
		t.Fatalf("contract regression: correlation_id appeared at root despite WithGroup: %v", rec)
	}
	group, ok := rec["audit"].(map[string]any)
	if !ok {
		t.Fatalf("audit group missing: %v", rec)
	}
	if group[wklog.AttrCorrelationID] != "req-grouped" {
		t.Errorf("correlation_id missing from audit group: %v", group)
	}
	if group["k"] != "v" {
		t.Errorf("user attr missing from audit group: %v", group)
	}
}

func TestLogger_ConcurrentWritesAreSafe(t *testing.T) {
	var buf safeBuf
	logger := wklog.NewWithWriter("test.subsys", &buf, wklog.Options{
		LevelLookup: staticLookup{}.lookup,
	})

	const goroutines = 16
	const perG = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			ctx := wklog.WithCorrelationID(context.Background(), "g")
			for j := 0; j < perG; j++ {
				logger.InfoContext(ctx, "tick", "g", i, "j", j)
			}
		}(i)
	}
	wg.Wait()

	lines := bytes.Count(buf.Bytes(), []byte("\n"))
	if lines != goroutines*perG {
		t.Fatalf("want %d records, got %d", goroutines*perG, lines)
	}
}

// safeBuf is a minimal goroutine-safe wrapper for the concurrency test.
// bytes.Buffer is NOT goroutine-safe — without this lock the race
// detector would flag the test's writers, masking actual logger bugs.
type safeBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *safeBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *safeBuf) Bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]byte, s.buf.Len())
	copy(out, s.buf.Bytes())
	return out
}

func TestSetAsDefault_RoundTrips(t *testing.T) {
	var buf bytes.Buffer
	logger := wklog.NewWithWriter("test.subsys", &buf, wklog.Options{
		LevelLookup: staticLookup{}.lookup,
	})

	restore := wklog.SetAsDefault(logger)
	defer restore()

	slog.Info("via-default")
	if !strings.Contains(buf.String(), "via-default") {
		t.Fatalf("SetAsDefault did not redirect slog.Default(): %q", buf.String())
	}
}
