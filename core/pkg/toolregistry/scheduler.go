package toolregistry

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// FS is the file-system seam consumed by [Scheduler] and the M9.1.b
// [Registry] scanner. The real production wiring satisfies it via a
// thin shim around the `os` package; tests substitute hand-rolled
// fakes that drive each method to a specific failure path. Method
// names mirror the stdlib so a production shim is mechanical.
//
// The interface is INTERNAL to the watchkeepers tree (the package
// is `toolregistry`, not `toolregistryext`). Adding a method —
// [FS.ReadDir] was reintroduced in M9.1.b after the M9.1.a iter-1
// fix removed it as unused — is a source-incompatible change for
// any out-of-tree implementer; downstream callers in this repo all
// inherit `OSFS{}` so the additive method does not affect them.
// External implementers who need a stable subset should embed the
// stdlib `io/fs` interfaces directly.
//
// Method contract:
//
//   - MkdirAll behaves like [os.MkdirAll]: idempotent, ok-if-exists.
//   - Stat returns the stdlib [os.ErrNotExist] sentinel chain when the
//     path is absent; the scheduler `errors.Is` against that sentinel
//     to distinguish "clone needed" from "stat failed".
//   - ReadFile / ReadDir mirror the stdlib semantics. ReadDir on a
//     missing path MUST return an error chain that satisfies
//     `errors.Is(err, fs.ErrNotExist)` so the M9.1.b scanner can
//     distinguish "source directory absent" from "I/O failure".
type FS interface {
	MkdirAll(path string, perm os.FileMode) error
	Stat(path string) (os.FileInfo, error)
	ReadFile(path string) ([]byte, error)
	ReadDir(path string) ([]fs.DirEntry, error)
}

// GitClient is the git seam consumed by [Scheduler]. Both methods
// take the per-call ctx so cancellation propagates; the `Auth` field
// on [CloneOpts] / [PullOpts] is the RESOLVED plaintext credential —
// never the secret-reference name. The resolver layer
// ([AuthSecretResolver]) is responsible for turning a reference into
// a token; this interface only sees the token. Implementations MUST
// NOT log `Auth` (even truncated) on any path.
type GitClient interface {
	Clone(ctx context.Context, opts CloneOpts) error
	Pull(ctx context.Context, opts PullOpts) error
}

// CloneOpts groups the parameters of a [GitClient.Clone] call. All
// fields are required.
type CloneOpts struct {
	URL    string
	Branch string
	Dir    string
	Auth   string
}

// PullOpts groups the parameters of a [GitClient.Pull] call.
type PullOpts struct {
	Dir  string
	Auth string
}

// Clock is the time seam. Production wiring uses a function-typed
// [ClockFunc] wrapping [time.Now]; tests pin a deterministic value
// so event timestamps and correlation ids are reproducible.
type Clock interface {
	Now() time.Time
}

// ClockFunc adapts a plain `func() time.Time` to [Clock]. The
// `time.Now` wrapper is the production-default; tests pass in a
// closure capturing a `*time.Time` they advance manually.
type ClockFunc func() time.Time

// Now implements [Clock].
func (f ClockFunc) Now() time.Time { return f() }

// SignatureVerifier is the signature-verification seam. M9.1.a wires
// the default [NoopSignatureVerifier], which accepts every directory
// unconditionally. M9.3 will plug in a cosign / minisign-backed
// implementation that consults per-source public keys pinned in the
// operator config. The seam is invoked AFTER clone / pull but BEFORE
// emitting [SourceSynced]; a non-nil return aborts the sync and
// surfaces [SourceFailed] with phase `signature`.
type SignatureVerifier interface {
	Verify(ctx context.Context, sourceName, dir string) error
}

// NoopSignatureVerifier is the default [SignatureVerifier]: it
// returns nil for every input. M9.1.a leaves signature verification
// off because the cosign / minisign integration lands in M9.3; the
// seam is in place so M9.3 can swap in the real verifier without
// touching the scheduler.
type NoopSignatureVerifier struct{}

// Verify implements [SignatureVerifier].
func (NoopSignatureVerifier) Verify(_ context.Context, _, _ string) error { return nil }

// AuthSecretResolver is the per-call seam for converting a
// [SourceConfig.AuthSecret] reference name into a plaintext
// credential. Called on EVERY [Scheduler.SyncOnce] invocation
// (never cached) so a per-tenant rotation takes effect on the next
// sync without restarting the process.
//
// PII discipline: the resolved credential flows through the
// scheduler to [GitClient]. The scheduler never logs it, never
// embeds it in [SourceFailed], never persists it.
type AuthSecretResolver func(ctx context.Context, sourceName, ref string) (string, error)

// Publisher is the [eventbus.Bus] subset the scheduler consumes —
// only the [Publisher.Publish] method. Defined here so production
// code never has to import the concrete `*eventbus.Bus` and tests
// can substitute a hand-rolled fake (mirrors
// `cron.LocalPublisher`).
type Publisher interface {
	Publish(ctx context.Context, topic string, event any) error
}

// Logger is the diagnostic sink wired in via [WithLogger]. Same
// shape as `cron.Logger` / `secrets.Logger` — flat-`kv` variadic.
// Nil-logger safe. PII discipline: implementations MUST NEVER log
// the resolved [SourceConfig.AuthSecret] value or any URL that may
// contain embedded credentials. The scheduler itself only invokes
// Log with the source NAME, the error TYPE, and the phase.
type Logger interface {
	Log(ctx context.Context, msg string, kv ...any)
}

// Deps groups the required dependencies of a [Scheduler]. Construct
// via [New]; nil values for required deps panic.
type Deps struct {
	FS        FS
	Git       GitClient
	Clock     Clock
	Publisher Publisher
	DataDir   string
}

// Option configures a [Scheduler] at construction time. Pass options
// to [New]; later options override earlier ones for the same field.
type Option func(*Scheduler)

// WithSignatureVerifier overrides the default [NoopSignatureVerifier].
// Passing nil is a no-op (the default verifier stays in place).
func WithSignatureVerifier(v SignatureVerifier) Option {
	return func(s *Scheduler) {
		if v != nil {
			s.verifier = v
		}
	}
}

// WithAuthSecretResolver wires a per-call resolver for source
// auth-secret refs. A nil resolver is a no-op; callers that have no
// resolver MUST NOT configure any source with a non-empty
// [SourceConfig.AuthSecret] (the scheduler would otherwise fail every
// sync of that source with [ErrAuthResolution]).
func WithAuthSecretResolver(r AuthSecretResolver) Option {
	return func(s *Scheduler) {
		if r != nil {
			s.resolver = r
		}
	}
}

// WithLogger wires a diagnostic sink onto the scheduler. Nil-logger
// safe (a Scheduler constructed without WithLogger silently drops
// diagnostic messages).
func WithLogger(l Logger) Option {
	return func(s *Scheduler) {
		if l != nil {
			s.logger = l
		}
	}
}

// Scheduler is the M9.1.a tool-source sync orchestrator. Constructed
// via [New]; the zero value is not usable. Safe for concurrent use
// across goroutines — per-source mutexes serialise concurrent
// [Scheduler.SyncOnce] calls for the SAME source so two callers
// cannot race a clone against a pull.
type Scheduler struct {
	deps     Deps
	sources  []SourceConfig
	bySource map[string]SourceConfig

	verifier SignatureVerifier
	resolver AuthSecretResolver
	logger   Logger

	// perSourceMu hands out one *sync.Mutex per configured source so
	// concurrent SyncOnce calls for the SAME source serialise while
	// SyncOnce calls for DIFFERENT sources run in parallel.
	perSourceMu map[string]*sync.Mutex
}

// New constructs a [Scheduler] with `deps` and `sources` validated.
// Required deps (`FS`, `Git`, `Clock`, `Publisher`, non-empty
// `DataDir`) MUST be non-nil / non-empty; passing nil panics with a
// clear message — matches `lifecycle.New` + `cron.New` discipline.
//
// `sources` is validated via [ValidateSources]; the first failure is
// returned. The slice is defensively deep-copied so caller mutation
// post-construction cannot bleed into the scheduler.
func New(deps Deps, sources []SourceConfig, opts ...Option) (*Scheduler, error) {
	if deps.FS == nil {
		panic("toolregistry: New: deps.FS must not be nil")
	}
	if deps.Git == nil {
		panic("toolregistry: New: deps.Git must not be nil")
	}
	if deps.Clock == nil {
		panic("toolregistry: New: deps.Clock must not be nil")
	}
	if deps.Publisher == nil {
		panic("toolregistry: New: deps.Publisher must not be nil")
	}
	if strings.TrimSpace(deps.DataDir) == "" {
		return nil, ErrInvalidDataDir
	}

	if err := ValidateSources(sources); err != nil {
		return nil, err
	}

	cloned := CloneSources(sources)
	bySource := make(map[string]SourceConfig, len(cloned))
	perSourceMu := make(map[string]*sync.Mutex, len(cloned))
	for _, s := range cloned {
		bySource[s.Name] = s
		perSourceMu[s.Name] = &sync.Mutex{}
	}

	s := &Scheduler{
		deps:        deps,
		sources:     cloned,
		bySource:    bySource,
		perSourceMu: perSourceMu,
		verifier:    NoopSignatureVerifier{},
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// Sources returns a defensive copy of the configured source list.
// Callers can inspect the configured set without holding a reference
// to the scheduler's internal slice.
func (s *Scheduler) Sources() []SourceConfig {
	return CloneSources(s.sources)
}

// SyncOnce clones or pulls the configured source `sourceName` into
// `<DataDir>/tools/<sourceName>/` and emits the appropriate event on
// success or failure. For `kind: local` and `kind: hosted` the
// scheduler verifies the directory exists on disk (it does NOT
// create it — population is the responsibility of out-of-band
// tooling: M9.5 `make tools-local-install` for local, M9.4's
// hosted-storage pipeline for hosted) and emits [SourceSynced]
// without git work.
//
// Returns:
//
//   - nil on success ([SourceSynced] emitted AND the publisher
//     accepted it).
//   - [ErrUnknownSource] if `sourceName` is not configured.
//   - A wrapped sentinel from this package on any sync-phase failure.
//     The caller's `errors.Is` on the wrapped sentinel identifies the
//     failure kind; the corresponding [SourceFailed] event names the
//     phase via [SourceFailed.Phase].
//   - A wrapped publisher error if the on-disk sync succeeded but
//     emitting [SourceSynced] failed. Downstream M9.1.b learns about
//     changed source contents EXCLUSIVELY through this event; a
//     dropped publish means a missed hot-reload. The on-disk state
//     is already committed, so the caller's next SyncOnce will pick
//     up where this one left off, but the caller MUST observe the
//     error so an external retry / monitoring path can fire.
//
// Concurrency: per-source mutex serialises concurrent calls for the
// SAME source from `mu.Lock()` through the entire critical section,
// INCLUDING the signature-verification call. This is deliberate:
// release-during-verify would let a concurrent SyncOnce mutate the
// on-disk tree underneath the verifier and produce a false-positive
// success against a mid-pull tree. The trade-off is that a slow
// verifier (M9.3 cosign / minisign network fetch) blocks concurrent
// syncs of the SAME source for the duration of the verification;
// different sources still proceed in parallel. M9.3 will revisit
// if the latency proves prohibitive.
func (s *Scheduler) SyncOnce(ctx context.Context, sourceName string) error {
	src, ok := s.bySource[sourceName]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownSource, sourceName)
	}

	mu := s.perSourceMu[sourceName]
	mu.Lock()
	defer mu.Unlock()

	correlationID := s.newCorrelationID()
	localPath, err := s.safeLocalPath(sourceName)
	if err != nil {
		s.emitFailure(ctx, sourceName, "path", err, correlationID)
		return err
	}

	// Resolve auth secret per-call. Empty AuthSecret skips the
	// resolver entirely (cleanest for public git sources). Local /
	// hosted kinds already reject non-empty AuthSecret at
	// [SourceConfig.Validate], so reaching the resolver here means
	// `src.Kind == SourceKindGit`.
	auth, err := s.resolveAuth(ctx, src)
	if err != nil {
		s.emitFailure(ctx, sourceName, "auth", err, correlationID)
		return fmt.Errorf("%w: %w", ErrAuthResolution, err)
	}

	// `local` / `hosted` kinds skip the git path AND skip MkdirAll
	// — population is the responsibility of out-of-band tooling.
	// A missing directory surfaces [ErrLocalSourceMissing] with
	// phase `stat` so the operator sees the misconfiguration loudly
	// rather than landing a false-positive `source_synced` on a
	// freshly-created empty tree.
	if src.Kind == SourceKindLocal || src.Kind == SourceKindHosted {
		if _, err := s.deps.FS.Stat(localPath); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				wrapped := fmt.Errorf("%w: %q at %q: %w", ErrLocalSourceMissing, sourceName, localPath, err)
				s.emitFailure(ctx, sourceName, "stat", wrapped, correlationID)
				return wrapped
			}
			s.emitFailure(ctx, sourceName, "stat", err, correlationID)
			return fmt.Errorf("%w: %w", ErrFSStat, err)
		}
		if err := s.verifySignature(ctx, src, localPath); err != nil {
			s.emitFailure(ctx, sourceName, "signature", err, correlationID)
			return err
		}
		return s.emitSuccess(ctx, sourceName, localPath, correlationID)
	}

	// Git kind. MkdirAll runs only on this branch — local / hosted
	// already returned above, and a successful clone into a brand-
	// new directory needs the parent path present.
	if err := s.deps.FS.MkdirAll(localPath, 0o755); err != nil {
		s.emitFailure(ctx, sourceName, "mkdir", err, correlationID)
		return fmt.Errorf("%w: %w", ErrFSMkdir, err)
	}

	// The presence of `<localPath>/.git` determines clone-vs-pull;
	// an FS that reports stat failure for any reason other than
	// not-exist aborts the sync.
	dotGit := filepath.Join(localPath, ".git")
	_, statErr := s.deps.FS.Stat(dotGit)
	switch {
	case statErr == nil:
		if err := s.deps.Git.Pull(ctx, PullOpts{Dir: localPath, Auth: auth}); err != nil {
			s.emitFailure(ctx, sourceName, "pull", err, correlationID)
			return fmt.Errorf("%w: %w", ErrSyncPull, err)
		}
	case errors.Is(statErr, fs.ErrNotExist):
		opts := CloneOpts{
			URL:    src.URL,
			Branch: src.EffectiveBranch(),
			Dir:    localPath,
			Auth:   auth,
		}
		if err := s.deps.Git.Clone(ctx, opts); err != nil {
			s.emitFailure(ctx, sourceName, "clone", err, correlationID)
			return fmt.Errorf("%w: %w", ErrSyncClone, err)
		}
	default:
		s.emitFailure(ctx, sourceName, "stat", statErr, correlationID)
		return fmt.Errorf("%w: %w", ErrFSStat, statErr)
	}

	if err := s.verifySignature(ctx, src, localPath); err != nil {
		s.emitFailure(ctx, sourceName, "signature", err, correlationID)
		return err
	}

	return s.emitSuccess(ctx, sourceName, localPath, correlationID)
}

// safeLocalPath returns the on-disk path the scheduler should sync
// `sourceName` into AND asserts the path stays strictly under
// `<DataDir>/tools/`. Belt-and-braces over [SourceConfig.Validate]'s
// character allowlist: if a future bug lets a `..` slip through the
// validator (e.g. via a different config loader), this method
// refuses to operate on the dangerous path. The check uses
// [filepath.Clean] on both the parent and the candidate to be
// robust against trailing slashes.
func (s *Scheduler) safeLocalPath(sourceName string) (string, error) {
	parent := filepath.Clean(filepath.Join(s.deps.DataDir, "tools"))
	cand := filepath.Clean(filepath.Join(parent, sourceName))
	rel, err := filepath.Rel(parent, cand)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: %q", ErrUnsafeLocalPath, sourceName)
	}
	return cand, nil
}

// resolveAuth invokes [AuthSecretResolver] for sources whose
// `AuthSecret` is non-empty. Sources without `AuthSecret` skip the
// resolver and pass an empty auth string to [GitClient]; sources
// with a non-empty `AuthSecret` but no configured resolver land
// [ErrAuthResolution] up-front. A resolver that returns `("", nil)`
// for a non-empty AuthSecret is treated as a failure via
// [ErrEmptyResolvedAuth] — silently demoting a credentialed source
// to anonymous would mask resolver bugs and either surface later as
// an opaque 401 on a private repo or succeed against an
// unintended public mirror.
func (s *Scheduler) resolveAuth(ctx context.Context, src SourceConfig) (string, error) {
	if src.AuthSecret == "" {
		return "", nil
	}
	if s.resolver == nil {
		return "", fmt.Errorf("no resolver configured for source %q", src.Name)
	}
	auth, err := s.resolver(ctx, src.Name, src.AuthSecret)
	if err != nil {
		return "", err
	}
	if auth == "" {
		return "", fmt.Errorf("%w: source %q ref %q", ErrEmptyResolvedAuth, src.Name, src.AuthSecret)
	}
	return auth, nil
}

// verifySignature wraps the optional [SignatureVerifier]. The
// default [NoopSignatureVerifier] returns nil; a real verifier's
// non-nil return is wrapped in [ErrSignatureVerification] so callers
// can `errors.Is` without depending on the underlying verifier type.
func (s *Scheduler) verifySignature(ctx context.Context, src SourceConfig, dir string) error {
	if err := s.verifier.Verify(ctx, src.Name, dir); err != nil {
		return fmt.Errorf("%w: %w", ErrSignatureVerification, err)
	}
	return nil
}

// emitSuccess publishes a [SourceSynced] event AND surfaces any
// publisher error to the SyncOnce caller. The on-disk sync has
// already committed; returning the publisher error gives the caller
// a chance to retry the publish (or page an operator) so M9.1.b's
// effective-toolset recompute is not silently skipped. The
// underlying error is logged via the optional [Logger] for triage
// and wrapped through `fmt.Errorf("toolregistry: publish synced: %w",
// err)` for the return path.
func (s *Scheduler) emitSuccess(ctx context.Context, sourceName, localPath, correlationID string) error {
	ev := SourceSynced{
		SourceName:    sourceName,
		SyncedAt:      s.deps.Clock.Now(),
		LocalPath:     localPath,
		CorrelationID: correlationID,
	}
	if err := s.deps.Publisher.Publish(ctx, TopicSourceSynced, ev); err != nil {
		s.log(
			ctx, "toolregistry: publish source_synced failed",
			"source", sourceName,
			"err_type", leafErrType(err),
		)
		return fmt.Errorf("toolregistry: publish synced: %w", err)
	}
	return nil
}

// emitFailure publishes a [SourceFailed] event. A publisher error
// is logged but NOT returned — the SyncOnce caller already has the
// original sync error in hand and translating a publisher hiccup
// into a different return value would muddle the failure
// classification. Asymmetry vs [emitSuccess] is deliberate: success
// emit failure means M9.1.b loses a recompute signal (data lost,
// so escalate); failure emit failure means M9.1.b loses a failure
// notification (visibility lost, so log).
func (s *Scheduler) emitFailure(ctx context.Context, sourceName, phase string, syncErr error, correlationID string) {
	ev := SourceFailed{
		SourceName:    sourceName,
		FailedAt:      s.deps.Clock.Now(),
		ErrorType:     leafErrType(syncErr),
		Phase:         phase,
		CorrelationID: correlationID,
	}
	if err := s.deps.Publisher.Publish(ctx, TopicSourceFailed, ev); err != nil {
		s.log(
			ctx, "toolregistry: publish source_failed failed",
			"source", sourceName,
			"err_type", leafErrType(err),
		)
	}
	s.log(
		ctx, "toolregistry: sync failed",
		"source", sourceName,
		"phase", phase,
		"err_type", leafErrType(syncErr),
	)
}

// leafErrType returns the Go type name of the deepest non-nil
// error in `err`'s [errors.Unwrap] chain. The naive
// `fmt.Sprintf("%T", err)` returns the OUTER wrapper type — for a
// `fmt.Errorf("%w: %w", sentinel, underlying)` call that is
// `*fmt.wrapErrors`, an internal stdlib type that tells subscribers
// nothing about the actual failure surface. Walking to the leaf
// gives the original `*fs.PathError` / `*url.Error` / custom-type
// triagers can act on.
//
// A nil input returns the literal `"<nil>"` to avoid a panic on
// `reflect.TypeOf(nil)` downstream; callers never pass nil today
// (both emit paths gate on a non-nil error first) but the helper
// is robust against future drift.
func leafErrType(err error) string {
	if err == nil {
		return "<nil>"
	}
	for {
		next := errors.Unwrap(err)
		if next == nil {
			return fmt.Sprintf("%T", err)
		}
		err = next
	}
}

// newCorrelationID mints a process-monotonic identifier for one
// sync cycle. Format is intentionally opaque (the nanosecond clock
// value formatted as base-10 int64); subscribers MUST NOT parse it.
func (s *Scheduler) newCorrelationID() string {
	return strconv.FormatInt(s.deps.Clock.Now().UnixNano(), 10)
}

// log forwards a diagnostic message to the optional [Logger].
func (s *Scheduler) log(ctx context.Context, msg string, kv ...any) {
	if s.logger == nil {
		return
	}
	s.logger.Log(ctx, msg, kv...)
}
