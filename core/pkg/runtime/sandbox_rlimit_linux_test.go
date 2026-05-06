//go:build linux

package runtime

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// sandboxTestHelperEnv is the env-var the parent test process sets
// when it self-spawns the test binary as a CPU-bound or memory-hungry
// child. The child branch detects the value, runs the appropriate
// workload to its conclusion (or to a kernel kill), and never returns
// to the testing harness.
const sandboxTestHelperEnv = "SANDBOX_TEST_HELPER"

// TestMain is the self-spawn dispatcher for the CPU-time and
// memory-ceiling tests. When SANDBOX_TEST_HELPER is set to a known
// helper name we abandon the testing harness entirely and run the
// workload; the child process never reaches m.Run(). Any other value
// falls through to m.Run() so the standard test discovery happens.
func TestMain(m *testing.M) {
	switch os.Getenv(sandboxTestHelperEnv) {
	case "memhog":
		runMemHog()
		os.Exit(0)
	case "cpuhog":
		runCPUHog()
		os.Exit(0) // unreachable: SIGXCPU fires before the loop ends
	}
	os.Exit(m.Run())
}

// runMemHog allocates a 256 MiB byte buffer and touches every page so
// the kernel commits it (without page-touching the OS may lazily
// over-commit and never trip RLIMIT_AS). When the runner caps the
// process at 64 MiB this allocation reliably triggers the memory
// ceiling and the kernel kills the process before this function
// returns. Using 256 MiB gives a generous overrun above the 64 MiB
// cap so the kill fires well before the loop completes.
func runMemHog() {
	const sz = 256 << 20 // 256 MiB — generous overrun above 64 MiB cap
	buf := make([]byte, sz)
	// Touch every page to force physical commitment so RLIMIT_AS
	// enforcement fires before the loop completes.
	for i := 0; i < len(buf); i += 4096 {
		buf[i] = byte(i)
	}
}

// runCPUHog spins in a tight integer-arithmetic loop that consumes
// CPU time without producing any output. No I/O means no
// OutputByteCap interference. The loop is infinite; SIGXCPU fires
// once the configured RLIMIT_CPU seconds are exhausted.
func runCPUHog() {
	x := uint64(1)
	for {
		x = x*2654435761 + 1
		_ = x
	}
}

// TestSandboxRun_RlimitZeroConfig — no rlimit fields set, applyRlimits
// is a no-op and the process exits naturally. Sanity check that the
// new wiring does not break the M5.4.a happy path on Linux.
func TestSandboxRun_RlimitZeroConfig(t *testing.T) {
	t.Parallel()

	res, err := newShRunner("exit 0", SandboxConfig{}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.TermReason != TermReasonNatural {
		t.Fatalf("TermReason = %q, want %q", res.TermReason, TermReasonNatural)
	}
}

// TestSandboxRun_CPUTimeKill — self-spawns the test binary with
// SANDBOX_TEST_HELPER=cpuhog (handled in TestMain). The child spins
// in a tight integer loop that produces no output, so OutputByteCap
// never fires. The runner caps the child at 1 CPU-second; the kernel
// delivers SIGXCPU and the runner attributes the death to
// TermReasonCPUTime. Wall-clock cap of 10 s is the safety net.
func TestSandboxRun_CPUTimeKill(t *testing.T) {
	// No t.Parallel(): t.Setenv panics from a test that has already
	// called t.Parallel() or whose ancestor has.

	cfg := SandboxConfig{
		CPUTimeSeconds:   1,
		WallClockTimeout: 10 * time.Second,
	}
	t.Setenv(sandboxTestHelperEnv, "cpuhog")
	start := time.Now()
	// -test.run=^$ ensures the child runs zero tests if the env-var
	// dispatcher in TestMain somehow does not fire (defensive).
	res, err := NewSandboxRunner([]string{os.Args[0], "-test.run=^$"}, cfg).Run(context.Background())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("Run returned nil error on CPU-time kill")
	}
	if !errors.Is(err, ErrSandboxKilled) {
		t.Fatalf("errors.Is(err, ErrSandboxKilled) = false, err = %v", err)
	}
	if res.TermReason != TermReasonCPUTime {
		t.Fatalf("TermReason = %q, want %q (elapsed=%v)", res.TermReason, TermReasonCPUTime, elapsed)
	}
	if elapsed > 8*time.Second {
		t.Fatalf("Run took %v, want < 8s", elapsed)
	}
}

// TestSandboxRun_MemoryCeilingKill — self-spawns the test binary
// with SANDBOX_TEST_HELPER=memhog (handled in TestMain). The child
// allocates 256 MiB and touches every page while the runner caps the
// process at 64 MiB, so the kernel kills it on the RLIMIT_AS
// boundary; the runner attributes the death to
// TermReasonMemoryCeiling.
func TestSandboxRun_MemoryCeilingKill(t *testing.T) {
	// No t.Parallel() here: t.Setenv panics when called from a test (or
	// any of its ancestors) that has already called t.Parallel().
	cfg := SandboxConfig{
		MemoryCeilingBytes: 64 << 20, // 64 MiB
		WallClockTimeout:   15 * time.Second,
	}
	t.Setenv(sandboxTestHelperEnv, "memhog")
	// -test.run=^$ ensures the child runs zero tests if the env-var
	// dispatcher in TestMain somehow does not fire (defensive).
	cmd := []string{os.Args[0], "-test.run=^$"}
	runner := NewSandboxRunner(cmd, cfg)

	start := time.Now()
	res, err := runner.Run(context.Background())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("Run returned nil error on memory-ceiling kill (TermReason=%q, exit=%d)", res.TermReason, res.ExitCode)
	}
	if !errors.Is(err, ErrSandboxKilled) {
		t.Fatalf("errors.Is(err, ErrSandboxKilled) = false, err = %v", err)
	}
	if res.TermReason != TermReasonMemoryCeiling {
		t.Fatalf("TermReason = %q, want %q (elapsed=%v, exit=%d)", res.TermReason, TermReasonMemoryCeiling, elapsed, res.ExitCode)
	}
	if elapsed > 12*time.Second {
		t.Fatalf("Run took %v, want < 12s", elapsed)
	}
}

// TestSandboxRun_CPUTimeBeatsWallClock — combined CPU (1 s) and wall
// (5 s) budgets; the CPU limit is the tighter fence on a busy loop,
// so SIGXCPU fires first and the runner reports TermReasonCPUTime,
// not TermReasonWallClock. Uses the self-spawn cpuhog pattern so
// OutputByteCap cannot interfere.
func TestSandboxRun_CPUTimeBeatsWallClock(t *testing.T) {
	// No t.Parallel(): t.Setenv panics from a test that has already
	// called t.Parallel() or whose ancestor has.

	cfg := SandboxConfig{
		CPUTimeSeconds:   1,
		WallClockTimeout: 5 * time.Second,
	}
	t.Setenv(sandboxTestHelperEnv, "cpuhog")
	// -test.run=^$ ensures the child runs zero tests if the env-var
	// dispatcher in TestMain somehow does not fire (defensive).
	res, err := NewSandboxRunner([]string{os.Args[0], "-test.run=^$"}, cfg).Run(context.Background())
	if err == nil {
		t.Fatalf("Run returned nil error")
	}
	if !errors.Is(err, ErrSandboxKilled) {
		t.Fatalf("errors.Is(err, ErrSandboxKilled) = false, err = %v", err)
	}
	if res.TermReason != TermReasonCPUTime {
		t.Fatalf("TermReason = %q, want %q", res.TermReason, TermReasonCPUTime)
	}
}

// TestSandboxRun_WallClockBeatsCPUTime — combined wall (100 ms) and
// CPU (100 s) budgets; the wall-clock fence trips first because the
// CPU limit is unreachable in 100 ms. The runner reports
// TermReasonWallClock and the existing M5.4.a guard remains
// authoritative when the rlimit is loose.
func TestSandboxRun_WallClockBeatsCPUTime(t *testing.T) {
	t.Parallel()

	cfg := SandboxConfig{
		CPUTimeSeconds:   100,
		WallClockTimeout: 100 * time.Millisecond,
	}
	start := time.Now()
	res, err := NewSandboxRunner([]string{"sleep", "5"}, cfg).Run(context.Background())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("Run returned nil error")
	}
	if !errors.Is(err, ErrSandboxKilled) {
		t.Fatalf("errors.Is(err, ErrSandboxKilled) = false, err = %v", err)
	}
	if res.TermReason != TermReasonWallClock {
		t.Fatalf("TermReason = %q, want %q", res.TermReason, TermReasonWallClock)
	}
	if elapsed > 1*time.Second {
		t.Fatalf("Run took %v, want < 1s", elapsed)
	}
}
