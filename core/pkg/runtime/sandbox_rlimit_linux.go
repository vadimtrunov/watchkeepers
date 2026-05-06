//go:build linux

package runtime

import (
	"fmt"
	"os/exec"

	"golang.org/x/sys/unix"
)

// applyRlimits is the Linux implementation of the rlimit shim. Called
// by [SandboxRunner.Run] AFTER [exec.Cmd.Start] returns successfully
// (so [exec.Cmd.Process] is populated with the child PID) and BEFORE
// the wall-clock timer / context watcher arm. Uses [unix.Prlimit]
// targeted at the child PID rather than Go's [syscall.SysProcAttr]
// (which does not surface RLIMIT_CPU / RLIMIT_AS as first-class
// fields) so the post-Start path stays cgo-free and idiomatic.
//
// `started` is a sentinel for the call site contract: the runner only
// invokes this helper after [exec.Cmd.Start] returned without error
// and the child PID is therefore valid. A `started=false` invocation
// is a no-op on Linux (preserved for future pre-exec extension; not
// currently exercised). Returns nil when both rlimit fields are zero.
//
// Errors from [unix.Prlimit] are wrapped with the offending rlimit
// name so the caller can distinguish CPU vs AS without inspecting the
// errno. The runner is responsible for killing the child on any
// non-nil return — see [SandboxRunner.Run].
func applyRlimits(cmd *exec.Cmd, started bool, cfg SandboxConfig) error {
	if !started {
		return nil
	}
	if cfg.CPUTimeSeconds == 0 && cfg.MemoryCeilingBytes == 0 {
		return nil
	}
	if cmd.Process == nil {
		return fmt.Errorf("runtime: applyRlimits: cmd.Process is nil after Start")
	}
	pid := cmd.Process.Pid

	if cfg.CPUTimeSeconds > 0 {
		// Hard limit pinned to soft so the child cannot escalate via setrlimit.
		lim := &unix.Rlimit{
			Cur: cfg.CPUTimeSeconds,
			Max: cfg.CPUTimeSeconds,
		}
		if err := unix.Prlimit(pid, unix.RLIMIT_CPU, lim, nil); err != nil {
			return fmt.Errorf("runtime: applyRlimits RLIMIT_CPU: %w", err)
		}
	}
	if cfg.MemoryCeilingBytes > 0 {
		// Hard limit pinned to soft so the child cannot escalate via setrlimit.
		lim := &unix.Rlimit{
			Cur: cfg.MemoryCeilingBytes,
			Max: cfg.MemoryCeilingBytes,
		}
		if err := unix.Prlimit(pid, unix.RLIMIT_AS, lim, nil); err != nil {
			return fmt.Errorf("runtime: applyRlimits RLIMIT_AS: %w", err)
		}
	}
	return nil
}
