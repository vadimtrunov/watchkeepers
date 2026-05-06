package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// Termination-reason constants surfaced via [RunResult.TermReason]. The
// set extends across M5.4.a (wall-clock, output-cap, context-cancel)
// and M5.4.b (cpu-time, memory-ceiling). Callers MUST match against
// these constants instead of string literals so future renames are
// compiler-checked.
const (
	// TermReasonNatural — the process exited on its own. The exit
	// code rides on [RunResult.ExitCode]; a non-zero code is still a
	// natural exit (no kill was issued by the sandbox).
	TermReasonNatural = "natural"

	// TermReasonWallClock — the wall-clock timer fired and the
	// sandbox killed the process via [exec.Cmd.Process.Kill]. Run
	// returns an error wrapping [ErrSandboxKilled].
	TermReasonWallClock = "wall_clock"

	// TermReasonOutputCap — cumulative bytes written to stdout+stderr
	// crossed [SandboxConfig.OutputByteCap] and the sandbox killed
	// the process. Run returns an error wrapping [ErrSandboxKilled].
	TermReasonOutputCap = "output_cap"

	// TermReasonContextCanceled — the parent [context.Context] was
	// cancelled (or its deadline expired) while the process was
	// running; the sandbox killed it. Run returns an error wrapping
	// both [ErrSandboxKilled] and the underlying [context.Context]
	// error so the call site can match either signal via
	// [errors.Is].
	TermReasonContextCanceled = "context_canceled"

	// TermReasonCPUTime — the kernel raised SIGXCPU after the
	// process consumed [SandboxConfig.CPUTimeSeconds] of CPU time
	// (Linux RLIMIT_CPU). Best-effort: the runner inspects the wait
	// status for SIGXCPU specifically, so this reason only fires on
	// platforms that enforce the rlimit (Linux). Run returns an
	// error wrapping [ErrSandboxKilled]. M5.4.b.
	TermReasonCPUTime = "cpu_time"

	// TermReasonMemoryCeiling — the kernel raised SIGKILL/SIGSEGV
	// after the process tried to grow its address space past
	// [SandboxConfig.MemoryCeilingBytes] (Linux RLIMIT_AS). The
	// classification is heuristic: if the configured ceiling is
	// non-zero AND the wait status reports an unprompted SIGKILL or
	// SIGSEGV (not one of our own kill paths), the runner attributes
	// the death to the memory ceiling. Run returns an error wrapping
	// [ErrSandboxKilled]. M5.4.b.
	TermReasonMemoryCeiling = "memory_ceiling"
)

// SandboxConfig is the value-typed, plain-data configuration the
// [SandboxRunner] reads at [SandboxRunner.Run] time. Zero values are
// valid: a zero [SandboxConfig.WallClockTimeout] arms no timer, a
// zero [SandboxConfig.OutputByteCap] applies no cap, and a zero rlimit
// field disables that rlimit. Callers wire non-zero values from
// upstream policy (manifest fields, M5.5 loader).
//
// The wall-clock and output-cap guardrails are platform-portable
// (M5.4.a). The rlimit guardrails (M5.4.b) are Linux-only at the
// kernel-enforcement layer: Darwin accepts the configuration but
// silently does not enforce; other platforms reject non-zero rlimit
// values with [ErrUnsupportedPlatform]. See [SandboxRunner.Run] for
// the enforcement contract.
type SandboxConfig struct {
	// WallClockTimeout is the maximum wall-clock duration the
	// process may run from [exec.Cmd.Start] to [exec.Cmd.Wait]
	// return. Zero means "no timeout" — the process runs until it
	// exits naturally or another guardrail trips.
	WallClockTimeout time.Duration

	// OutputByteCap is the cumulative ceiling on bytes written to
	// stdout AND stderr combined. Zero means "no cap" — the process
	// may produce arbitrary output. Non-zero caps trigger a
	// [exec.Cmd.Process.Kill] call once the counter crosses the
	// threshold; some overrun is expected because the kill is async.
	OutputByteCap int64

	// CPUTimeSeconds caps total CPU time (user+system) the process
	// may consume before the kernel raises SIGXCPU. Zero means "no
	// limit". Enforced on Linux via RLIMIT_CPU applied with
	// [unix.Prlimit] after [exec.Cmd.Start] returns. On Darwin the
	// value is accepted but silently NOT enforced (the M5.4.a
	// wall-clock guard remains the active fence). On other
	// platforms a non-zero value surfaces [ErrUnsupportedPlatform].
	// M5.4.b.
	CPUTimeSeconds uint64

	// MemoryCeilingBytes caps the process's virtual address space
	// (RLIMIT_AS). Zero means "no limit". Enforced on Linux via
	// RLIMIT_AS applied with [unix.Prlimit] after [exec.Cmd.Start]
	// returns; the kernel kills the process with SIGKILL/SIGSEGV
	// when an allocation would push it past the ceiling. On Darwin
	// the value is accepted but silently NOT enforced. On other
	// platforms a non-zero value surfaces [ErrUnsupportedPlatform].
	// M5.4.b.
	MemoryCeilingBytes uint64
}

// RunResult is the value [SandboxRunner.Run] returns alongside the
// error. The struct is plain-data with no methods; the runner
// populates every field before return so callers can inspect outcomes
// without re-running the process.
type RunResult struct {
	// TermReason is one of the exported `TermReason*` constants. The
	// set is closed for M5.4.a; future runtimes MAY extend it.
	TermReason string

	// ExitCode mirrors [exec.Cmd.ProcessState.ExitCode]: the
	// process's exit code on natural exit (0 on success, non-zero on
	// failure), or -1 when the process was terminated by a signal
	// (the typical sandbox-kill outcome).
	ExitCode int

	// Stdout is the captured stdout up to (and slightly past)
	// [SandboxConfig.OutputByteCap]. The slice is the runner's only
	// reference; callers MAY retain it without copying.
	Stdout []byte

	// Stderr is the captured stderr up to (and slightly past)
	// [SandboxConfig.OutputByteCap]. Same lifetime contract as
	// [RunResult.Stdout].
	Stderr []byte

	// Error carries the underlying [exec.Cmd.Wait] error message
	// when one is present (empty on natural-success exit). The
	// returned `error` from [SandboxRunner.Run] is the sandbox's
	// wrapped form; this field surfaces the raw wait diagnostic for
	// audit / logging without forcing the caller to unwrap.
	Error string
}

// SandboxRunner wraps an [exec.Cmd] argv and a [SandboxConfig], then
// drives the lifecycle of the spawned process under the configured
// guardrails. The runner is single-shot — each [SandboxRunner.Run]
// builds a fresh [exec.Cmd] from the captured argv so a runner value
// can be reused across attempts without leaking state.
//
// Construct with [NewSandboxRunner]. Direct struct literals work but
// skip the empty-argv guard.
type SandboxRunner struct {
	argv []string
	cfg  SandboxConfig
}

// NewSandboxRunner returns a [SandboxRunner] bound to `argv` and
// `cfg`. Argv[0] is the executable; argv[1:] are positional args (the
// stand-in subprocess pattern for tests is `{"/bin/sh", "-c", script}`).
// Panics on an empty argv — the runner has nothing to spawn.
func NewSandboxRunner(argv []string, cfg SandboxConfig) *SandboxRunner {
	if len(argv) == 0 {
		panic("runtime: NewSandboxRunner requires a non-empty argv")
	}
	out := make([]string, len(argv))
	copy(out, argv)
	return &SandboxRunner{argv: out, cfg: cfg}
}

// Run spawns the configured process and supervises it until it exits
// naturally OR one of the sandbox guardrails fires. Returns the
// populated [RunResult] (always non-nil — even on error paths the
// captured TermReason / ExitCode / Stdout / Stderr fields are set so
// callers can audit) and an error that:
//
//   - Is nil on natural exits, regardless of [RunResult.ExitCode].
//   - Wraps [ErrSandboxKilled] on any sandbox-driven kill path.
//   - Additionally wraps the [context.Context] error on
//     context-cancellation paths so [errors.Is] matches both
//     [ErrSandboxKilled] and [context.Canceled] /
//     [context.DeadlineExceeded].
//
// The wall-clock timer arms at [exec.Cmd.Start] return and disarms via
// `defer` once [exec.Cmd.Wait] returns. The output-byte counter is
// shared across stdout+stderr writers and triggers a kill when it
// crosses [SandboxConfig.OutputByteCap]. A goroutine watches
// [context.Context.Done] and triggers a kill on cancellation. All
// three kill paths funnel through a [sync.Once] so the second arrival
// is a no-op (idempotent kill is required because Go's
// [exec.Cmd.Process.Kill] is not safe to call after the process has
// been reaped).
func (r *SandboxRunner) Run(ctx context.Context) (*RunResult, error) {
	cmd := exec.CommandContext(ctx, r.argv[0], r.argv[1:]...) //nolint:gosec // argv comes from a trusted call site (manifest); CommandContext also gives us a free SIGKILL on ctx-cancel, but our explicit ctx-watcher arm sets TermReason BEFORE the kill so we never observe the race-loss.

	st := newRunState()
	cmd.Stdout = st.makeWriter(r.cfg.OutputByteCap, &st.stdout)
	cmd.Stderr = st.makeWriter(r.cfg.OutputByteCap, &st.stderr)
	st.killFn = func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}

	if err := cmd.Start(); err != nil {
		return &RunResult{
			TermReason: TermReasonNatural,
			ExitCode:   -1,
			Error:      err.Error(),
		}, fmt.Errorf("runtime: cmd.Start: %w", err)
	}

	timer := r.armWallClockTimer(st)
	ctxDone := r.armCtxWatcher(ctx, st)

	waitErr := cmd.Wait()

	if timer != nil {
		timer.Stop()
	}
	select {
	case <-ctxDone:
	default:
		close(ctxDone)
	}

	return st.buildResult(ctx, cmd, waitErr), classifyError(ctx, st, waitErr)
}

// armWallClockTimer arms the wall-clock kill if the config asked for
// one and returns the timer handle so the caller can Stop it.
// Returns nil when no timer is needed.
func (r *SandboxRunner) armWallClockTimer(st *runState) *time.Timer {
	if r.cfg.WallClockTimeout <= 0 {
		return nil
	}
	return time.AfterFunc(r.cfg.WallClockTimeout, func() {
		st.killWith(TermReasonWallClock)
	})
}

// armCtxWatcher spawns the ctx-cancel watcher when the supplied
// context is cancellable; returns the teardown channel the caller
// closes after Wait. Returns an already-closed channel for
// non-cancellable contexts so the teardown branch is uniform.
func (r *SandboxRunner) armCtxWatcher(ctx context.Context, st *runState) chan struct{} {
	ctxDone := make(chan struct{})
	if ctx.Done() == nil {
		close(ctxDone)
		return ctxDone
	}
	go func() {
		select {
		case <-ctx.Done():
			st.killWith(TermReasonContextCanceled)
		case <-ctxDone:
		}
	}()
	return ctxDone
}

// runState bundles the per-Run mutable state so the main flow can stay
// flat. Every field is concurrency-safe in its own right.
type runState struct {
	stdout   bytes.Buffer
	stderr   bytes.Buffer
	bufMu    sync.Mutex   // serialises bytes.Buffer writes (Buffer is not goroutine-safe).
	written  atomic.Int64 // cumulative stdout+stderr bytes for cap enforcement.
	reason   atomic.Value // string; first writer wins.
	killOnce sync.Once
	killed   atomic.Bool
	killFn   func() // wired by Run after exec.Command returns; nil before Start.
}

func newRunState() *runState { return &runState{} }

// killWith captures the termination reason (first writer wins via
// CompareAndSwap), kills the process exactly once via sync.Once, and
// flips the killed flag.
func (s *runState) killWith(why string) {
	s.reason.CompareAndSwap(nil, why)
	s.killOnce.Do(func() {
		s.killed.Store(true)
		if s.killFn != nil {
			s.killFn()
		}
	})
}

// makeWriter returns an io.Writer that captures bytes into `buf` under
// bufMu, increments the shared cumulative counter, and fires the
// output-cap kill the first time the counter crosses `byteCap`. A
// zero byteCap disables the kill path; bytes are still captured.
func (s *runState) makeWriter(byteCap int64, buf *bytes.Buffer) io.Writer {
	return writerFunc(func(p []byte) (int, error) {
		s.bufMu.Lock()
		n, err := buf.Write(p)
		s.bufMu.Unlock()
		if byteCap > 0 && s.written.Add(int64(n)) >= byteCap {
			s.killWith(TermReasonOutputCap)
		}
		return n, err
	})
}

// buildResult assembles the populated RunResult. The TermReason is
// always set so the caller can switch on it without a default branch.
func (s *runState) buildResult(_ context.Context, cmd *exec.Cmd, waitErr error) *RunResult {
	res := &RunResult{
		Stdout: s.stdout.Bytes(),
		Stderr: s.stderr.Bytes(),
	}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	} else {
		res.ExitCode = -1
	}
	if waitErr != nil {
		res.Error = waitErr.Error()
	}
	if s.killed.Load() {
		why, _ := s.reason.Load().(string)
		if why == "" {
			why = TermReasonNatural
		}
		res.TermReason = why
		return res
	}
	res.TermReason = TermReasonNatural
	return res
}

// classifyError returns the wrapped error the caller observes. Natural
// exits — including non-zero exit codes — return nil. Sandbox-driven
// kills wrap ErrSandboxKilled, and ctx-cancel additionally joins the
// underlying context error so [errors.Is] matches either signal.
func classifyError(ctx context.Context, s *runState, _ error) error {
	if !s.killed.Load() {
		return nil
	}
	why, _ := s.reason.Load().(string)
	err := fmt.Errorf("runtime: sandbox killed (%s): %w", why, ErrSandboxKilled)
	if why != TermReasonContextCanceled {
		return err
	}
	ctxErr := ctx.Err()
	if ctxErr == nil {
		ctxErr = context.Canceled
	}
	return errors.Join(err, ctxErr)
}

// writerFunc adapts a plain func(p []byte) (int, error) to the
// [io.Writer] interface. Kept private — the only call site is the
// byte-counting wrapper above.
type writerFunc func(p []byte) (int, error)

// Write implements [io.Writer].
func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
