package runtime

import (
	"context"
	"errors"
	goruntime "runtime"
	"strings"
	"testing"
	"time"
)

// skipIfWindows centralises the cross-platform guard. M5.4.a stand-in
// subprocesses use `/bin/sh`; Windows support is out-of-scope per the
// M5.4 cross-cutting decision.
func skipIfWindows(t *testing.T) {
	t.Helper()
	if goruntime.GOOS == "windows" {
		t.Skip("sandbox tests require POSIX shell; M5.4 is cross-platform-deferred on Windows")
	}
}

// newShRunner returns a [SandboxRunner] wrapping `/bin/sh -c script`
// with the supplied [SandboxConfig]. Callers configure the runner's
// behaviour by mutating the returned value before calling [Run].
func newShRunner(script string, cfg SandboxConfig) *SandboxRunner {
	return NewSandboxRunner([]string{"/bin/sh", "-c", script}, cfg)
}

// TestSandboxRun_NaturalExitZero — happy path: a stand-in subprocess
// that exits cleanly returns TermReason "natural", ExitCode 0, no
// error, and empty captures.
func TestSandboxRun_NaturalExitZero(t *testing.T) {
	skipIfWindows(t)
	t.Parallel()

	res, err := newShRunner("exit 0", SandboxConfig{}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.TermReason != TermReasonNatural {
		t.Fatalf("TermReason = %q, want %q", res.TermReason, TermReasonNatural)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", res.ExitCode)
	}
	if res.Error != "" {
		t.Fatalf("Error = %q, want empty", res.Error)
	}
	if len(res.Stdout) != 0 {
		t.Fatalf("Stdout len = %d, want 0", len(res.Stdout))
	}
	if len(res.Stderr) != 0 {
		t.Fatalf("Stderr len = %d, want 0", len(res.Stderr))
	}
}

// TestSandboxRun_NaturalExitNonZero — non-zero exit codes are NOT
// surfaced as ErrSandboxKilled; the caller inspects RunResult.ExitCode.
func TestSandboxRun_NaturalExitNonZero(t *testing.T) {
	skipIfWindows(t)
	t.Parallel()

	res, err := newShRunner("exit 2", SandboxConfig{}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.TermReason != TermReasonNatural {
		t.Fatalf("TermReason = %q, want %q", res.TermReason, TermReasonNatural)
	}
	if res.ExitCode != 2 {
		t.Fatalf("ExitCode = %d, want 2", res.ExitCode)
	}
	if errors.Is(err, ErrSandboxKilled) {
		t.Fatalf("Run returned ErrSandboxKilled on natural non-zero exit")
	}
}

// TestSandboxRun_StdoutCapturedUnderCap — stdout content is captured
// verbatim when below the cap; TermReason stays "natural".
func TestSandboxRun_StdoutCapturedUnderCap(t *testing.T) {
	skipIfWindows(t)
	t.Parallel()

	res, err := newShRunner("echo hello", SandboxConfig{OutputByteCap: 1024}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.TermReason != TermReasonNatural {
		t.Fatalf("TermReason = %q, want %q", res.TermReason, TermReasonNatural)
	}
	if got := string(res.Stdout); got != "hello\n" {
		t.Fatalf("Stdout = %q, want %q", got, "hello\n")
	}
}

// TestSandboxRun_WallClockKill — a long sleep with a tight wall-clock
// budget produces TermReason "wall_clock" and errors.Is(err, ErrSandboxKilled).
// The 4× safety factor guards against slow CI: 100ms budget should
// resolve in well under 500ms.
func TestSandboxRun_WallClockKill(t *testing.T) {
	skipIfWindows(t)
	t.Parallel()

	start := time.Now()
	res, err := newShRunner("sleep 5", SandboxConfig{WallClockTimeout: 100 * time.Millisecond}).Run(context.Background())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("Run returned nil error on wall-clock kill")
	}
	if !errors.Is(err, ErrSandboxKilled) {
		t.Fatalf("errors.Is(err, ErrSandboxKilled) = false, err = %v", err)
	}
	if res.TermReason != TermReasonWallClock {
		t.Fatalf("TermReason = %q, want %q", res.TermReason, TermReasonWallClock)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("Run took %v, want < 500ms", elapsed)
	}
}

// TestSandboxRun_WallClockTimerDoesNotFireOnFastExit — when the process
// exits naturally before the timer fires, TermReason stays "natural"
// and no kill is recorded.
func TestSandboxRun_WallClockTimerDoesNotFireOnFastExit(t *testing.T) {
	skipIfWindows(t)
	t.Parallel()

	res, err := newShRunner("exit 0", SandboxConfig{WallClockTimeout: 5 * time.Second}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.TermReason != TermReasonNatural {
		t.Fatalf("TermReason = %q, want %q", res.TermReason, TermReasonNatural)
	}
}

// TestSandboxRun_OutputCapKillStdout — a fast-stdout subprocess hits
// the cap and is killed; ErrSandboxKilled wraps the error and
// captured stdout is at least the cap but bounded.
func TestSandboxRun_OutputCapKillStdout(t *testing.T) {
	skipIfWindows(t)
	t.Parallel()

	res, err := newShRunner(
		"while :; do echo line; done",
		SandboxConfig{OutputByteCap: 100, WallClockTimeout: 5 * time.Second},
	).Run(context.Background())
	if err == nil {
		t.Fatalf("Run returned nil error on output-cap kill")
	}
	if !errors.Is(err, ErrSandboxKilled) {
		t.Fatalf("errors.Is(err, ErrSandboxKilled) = false, err = %v", err)
	}
	if res.TermReason != TermReasonOutputCap {
		t.Fatalf("TermReason = %q, want %q", res.TermReason, TermReasonOutputCap)
	}
	if len(res.Stdout) < 100 {
		t.Fatalf("len(Stdout) = %d, want >= 100", len(res.Stdout))
	}
	// Bounded — async kill leaves some overrun, but a few KiB max.
	if len(res.Stdout) > 64*1024 {
		t.Fatalf("len(Stdout) = %d, want < 64KiB", len(res.Stdout))
	}
}

// TestSandboxRun_OutputCapKillStderr — stderr writes count toward the
// shared cap and surface in RunResult.Stderr.
func TestSandboxRun_OutputCapKillStderr(t *testing.T) {
	skipIfWindows(t)
	t.Parallel()

	res, err := newShRunner(
		"while :; do echo err 1>&2; done",
		SandboxConfig{OutputByteCap: 100, WallClockTimeout: 5 * time.Second},
	).Run(context.Background())
	if err == nil {
		t.Fatalf("Run returned nil error on output-cap kill")
	}
	if !errors.Is(err, ErrSandboxKilled) {
		t.Fatalf("errors.Is(err, ErrSandboxKilled) = false, err = %v", err)
	}
	if res.TermReason != TermReasonOutputCap {
		t.Fatalf("TermReason = %q, want %q", res.TermReason, TermReasonOutputCap)
	}
	if len(res.Stderr) < 100 {
		t.Fatalf("len(Stderr) = %d, want >= 100", len(res.Stderr))
	}
	if !strings.Contains(string(res.Stderr), "err") {
		t.Fatalf("Stderr does not contain expected token: %q", string(res.Stderr))
	}
}

// TestSandboxRun_ContextCanceled — a parent-cancelled context kills
// the sandboxed process; TermReason is "context_canceled" and
// errors.Is(err, context.Canceled) holds at the call site.
func TestSandboxRun_ContextCanceled(t *testing.T) {
	skipIfWindows(t)
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	res, err := newShRunner("sleep 5", SandboxConfig{}).Run(ctx)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("Run returned nil error on context cancel")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("errors.Is(err, context.Canceled) = false, err = %v", err)
	}
	if !errors.Is(err, ErrSandboxKilled) {
		t.Fatalf("errors.Is(err, ErrSandboxKilled) = false, err = %v", err)
	}
	if res.TermReason != TermReasonContextCanceled {
		t.Fatalf("TermReason = %q, want %q", res.TermReason, TermReasonContextCanceled)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("Run took %v, want < 500ms", elapsed)
	}
}

// TestSandboxRun_ZeroConfigNoEnforcement — a zero SandboxConfig arms
// no timer and applies no cap; long output completes naturally.
func TestSandboxRun_ZeroConfigNoEnforcement(t *testing.T) {
	skipIfWindows(t)
	t.Parallel()

	res, err := newShRunner("echo ok", SandboxConfig{}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.TermReason != TermReasonNatural {
		t.Fatalf("TermReason = %q, want %q", res.TermReason, TermReasonNatural)
	}
	if string(res.Stdout) != "ok\n" {
		t.Fatalf("Stdout = %q, want %q", string(res.Stdout), "ok\n")
	}
}

// TestSandboxRun_GoroutineHygiene — each iteration's setup-teardown
// returns goroutine count to baseline (±1 for runtime jitter). Failing
// this is a leak.
func TestSandboxRun_GoroutineHygiene(t *testing.T) {
	skipIfWindows(t)
	// not parallel — goroutine snapshots are racy with parallel tests.

	// Warm up: run once so any one-time goroutines (e.g. the os/exec
	// reaper) are already spun.
	if _, err := newShRunner("exit 0", SandboxConfig{}).Run(context.Background()); err != nil {
		t.Fatalf("warmup Run: %v", err)
	}

	cases := []struct {
		name string
		cfg  SandboxConfig
		cmd  string
	}{
		{"natural", SandboxConfig{}, "exit 0"},
		{"wall-clock", SandboxConfig{WallClockTimeout: 50 * time.Millisecond}, "sleep 5"},
		{"output-cap", SandboxConfig{OutputByteCap: 50, WallClockTimeout: 5 * time.Second}, "while :; do echo line; done"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := goruntime.NumGoroutine()
			if _, err := newShRunner(tc.cmd, tc.cfg).Run(context.Background()); err != nil && !errors.Is(err, ErrSandboxKilled) {
				t.Fatalf("Run: %v", err)
			}
			// Allow the os/exec reaper goroutine a brief window to retire.
			deadline := time.Now().Add(500 * time.Millisecond)
			for time.Now().Before(deadline) {
				if delta := goruntime.NumGoroutine() - before; delta <= 1 {
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
			delta := goruntime.NumGoroutine() - before
			if delta > 1 {
				t.Fatalf("goroutine leak: delta = %d (before=%d, after=%d)", delta, before, goruntime.NumGoroutine())
			}
		})
	}
}

// TestSandboxRun_IdempotentKill — a process that exits naturally just
// as the wall-clock timer fires must not panic or surface a spurious
// error. The sync.Once-guarded kill ensures the second path is a no-op.
// Race-prone test by design; a tight timeout and a fast subprocess
// maximise the chance of crossing.
func TestSandboxRun_IdempotentKill(t *testing.T) {
	skipIfWindows(t)
	t.Parallel()

	// 50 iterations to exercise the race window.
	for i := 0; i < 50; i++ {
		_, err := newShRunner(
			"exit 0",
			SandboxConfig{WallClockTimeout: 1 * time.Microsecond},
		).Run(context.Background())
		// Either outcome is acceptable: natural-win (nil err) OR
		// wall-clock-win (ErrSandboxKilled). Anything else is a bug.
		if err != nil && !errors.Is(err, ErrSandboxKilled) {
			t.Fatalf("iter %d: unexpected error: %v", i, err)
		}
	}
}

// TestNewSandboxRunner_EmptyArgvPanics — defensive: a runner with no
// argv has nothing to spawn. Panic at construction beats a confusing
// runtime error.
func TestNewSandboxRunner_EmptyArgvPanics(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("NewSandboxRunner with empty argv did not panic")
		}
	}()
	_ = NewSandboxRunner(nil, SandboxConfig{})
}
