//go:build darwin

package runtime

import "os/exec"

// applyRlimits is the Darwin shim of the rlimit guardrail. It is a
// deliberate no-op: Darwin's POSIX rlimit story (`setrlimit` on
// RLIMIT_CPU and RLIMIT_AS) is comparatively quirky and not worth a
// dedicated implementation for Phase 1, where Linux CI is the canonical
// enforcement gate. Darwin developers run under the M5.4.a wall-clock
// + output-cap fences only; rlimit-driven kills do NOT fire here.
//
// Returning nil unconditionally lets callers configure the rlimit
// fields without a build-tag dance: the configuration travels with
// the [SandboxConfig] value and is silently ignored on Darwin. The
// signature must match the other build-tagged variants (Linux real
// impl, !linux && !darwin error stub) so [SandboxRunner.Run] can
// invoke the helper without any platform-specific glue.
func applyRlimits(_ *exec.Cmd, _ bool, _ SandboxConfig) error {
	return nil
}
