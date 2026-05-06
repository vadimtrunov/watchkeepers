//go:build !linux && !darwin

package runtime

import "os/exec"

// applyRlimits is the catch-all shim for platforms that have no
// rlimit implementation in this package (Windows, Plan 9, the
// BSDs, etc.). The contract is asymmetric: when the caller did NOT
// request any rlimit (both fields zero), the call is a no-op and
// returns nil — non-enforcement is the package-wide default for
// zeroed configs. When the caller DID request a non-zero limit, the
// shim refuses with [ErrUnsupportedPlatform] so the runner can fail
// fast and the operator gets a clear signal that the policy will not
// be honoured on this platform.
//
// Build-tagged off Linux and Darwin so the matching pair of real /
// silent-stub implementations always wins on the supported targets.
func applyRlimits(_ *exec.Cmd, _ bool, cfg SandboxConfig) error {
	if cfg.CPUTimeSeconds == 0 && cfg.MemoryCeilingBytes == 0 {
		return nil
	}
	return ErrUnsupportedPlatform
}
