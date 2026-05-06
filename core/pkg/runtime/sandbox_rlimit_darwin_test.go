//go:build darwin

package runtime

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestSandboxRun_RlimitDarwinStub — on Darwin the applyRlimits shim
// is a documented no-op: setting CPUTimeSeconds does NOT trip a
// kernel kill, but the M5.4.a wall-clock fence remains active and
// kills the process at the configured wall budget. The test
// configures a tight wall (200 ms) and a loose CPU rlimit (1 s) on
// a long-running `sleep 5`; expectation is TermReasonWallClock and
// no error path involving the rlimit shim.
func TestSandboxRun_RlimitDarwinStub(t *testing.T) {
	t.Parallel()

	cfg := SandboxConfig{
		CPUTimeSeconds:   1,
		WallClockTimeout: 200 * time.Millisecond,
	}
	start := time.Now()
	res, err := NewSandboxRunner([]string{"sleep", "5"}, cfg).Run(context.Background())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("Run returned nil error; the wall-clock fence should still kill on Darwin")
	}
	if !errors.Is(err, ErrSandboxKilled) {
		t.Fatalf("errors.Is(err, ErrSandboxKilled) = false, err = %v", err)
	}
	if res.TermReason != TermReasonWallClock {
		t.Fatalf("TermReason = %q, want %q (Darwin stub must not surface rlimit reasons)", res.TermReason, TermReasonWallClock)
	}
	if elapsed > 1*time.Second {
		t.Fatalf("Run took %v, want < 1s", elapsed)
	}
}
