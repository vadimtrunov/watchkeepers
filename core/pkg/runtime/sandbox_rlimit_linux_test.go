//go:build linux

package runtime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"
)

// sandboxTestHelperEnv is the env-var the parent test process sets
// when it self-spawns the test binary as a memory-hungry child. The
// child branch detects the env var, allocates a buffer that exceeds
// the runner's MemoryCeilingBytes, and exits if the kernel did not
// kill it first.
const sandboxTestHelperEnv = "SANDBOX_TEST_HELPER"

// TestMain is the self-spawn dispatcher for the memory-ceiling test.
// When SANDBOX_TEST_HELPER == "memhog" we abandon the testing harness
// entirely and run the memory-hungry workload to its conclusion (or
// to a SIGKILL from the rlimit). Any other value falls through to
// `m.Run()` so the standard test discovery still happens.
func TestMain(m *testing.M) {
	if os.Getenv(sandboxTestHelperEnv) == "memhog" {
		runMemHog()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runMemHog allocates a 1 GiB byte buffer and touches every page so
// the kernel commits it (without page-touching the OS may lazily
// over-commit and never trip RLIMIT_AS). When the runner caps the
// process at 64 MiB this allocation reliably triggers the memory
// ceiling and the kernel kills the process before this function
// returns.
func runMemHog() {
	const sz = 1 << 30 // 1 GiB
	buf := make([]byte, sz)
	for i := 0; i < len(buf); i += 4096 {
		buf[i] = 1
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

// TestSandboxRun_CPUTimeKill — a CPU-bound subprocess (`yes`) with a
// 1-second CPU budget is killed by the kernel via SIGXCPU; the
// runner attributes the death to TermReasonCPUTime. Wall-clock cap
// of 10 s is the safety net so a busted classifier cannot hang the
// test.
func TestSandboxRun_CPUTimeKill(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("yes"); err != nil {
		t.Skipf("yes binary not available: %v", err)
	}

	cfg := SandboxConfig{
		CPUTimeSeconds:   1,
		WallClockTimeout: 10 * time.Second,
		OutputByteCap:    1 << 20, // 1 MiB so we don't hit the cap first
	}
	start := time.Now()
	res, err := NewSandboxRunner([]string{"yes"}, cfg).Run(context.Background())
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
// allocates 1 GiB while the runner caps the process at 64 MiB, so
// the kernel kills it via SIGKILL on the OOM/RLIMIT_AS boundary;
// the runner attributes the death to TermReasonMemoryCeiling.
func TestSandboxRun_MemoryCeilingKill(t *testing.T) {
	t.Parallel()

	cfg := SandboxConfig{
		MemoryCeilingBytes: 64 << 20, // 64 MiB
		WallClockTimeout:   15 * time.Second,
	}
	cmd := []string{os.Args[0]}
	runner := NewSandboxRunner(cmd, cfg)
	// Inject the helper marker via a per-runner env var. The current
	// SandboxRunner does not expose env-var configuration, so use
	// os.Setenv for the duration of the test (the runner inherits
	// the parent env via exec.CommandContext).
	t.Setenv(sandboxTestHelperEnv, "memhog")

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
// not TermReasonWallClock.
func TestSandboxRun_CPUTimeBeatsWallClock(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("yes"); err != nil {
		t.Skipf("yes binary not available: %v", err)
	}

	cfg := SandboxConfig{
		CPUTimeSeconds:   1,
		WallClockTimeout: 5 * time.Second,
		OutputByteCap:    1 << 20,
	}
	res, err := NewSandboxRunner([]string{"yes"}, cfg).Run(context.Background())
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
