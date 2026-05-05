package capability

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// tokenPrefixLen is the number of leading characters of a token that
// MAY appear in log payloads. The remaining bytes are NEVER logged. See
// the redaction-discipline contract in the package godoc.
const tokenPrefixLen = 8

// tokenRandomBytes is the size of the cryptographic-random buffer used
// for token generation. 32 bytes = 256 bits of entropy, encoded as
// URL-safe base64 with no padding (`base64.RawURLEncoding`) yields a
// 43-character token. The 44-character form quoted in some test
// comments would correspond to padded base64; we use the unpadded form
// because URL-safe-no-padding is the project standard for opaque
// identifiers and avoids the `=` character entirely.
const tokenRandomBytes = 32

// Logger is the diagnostic sink wired in via [WithLogger]. The shape
// mirrors the secrets/cron/notebook Logger interfaces: a single
// `Log(ctx, msg, kv...)` method so callers can substitute structured
// loggers (e.g. an slog wrapper) without losing type compatibility.
//
// The variadic `kv` slice carries flat key,value pairs
// (`"scope", scope, "token_prefix", prefix`). A nil logger silently
// drops the message — the package never panics on a nil logger.
//
// IMPORTANT: implementations must never log full token values. Only the
// first [tokenPrefixLen] characters (carried under the `token_prefix`
// key) are acceptable. See the redaction-discipline contract in the
// package godoc.
type Logger interface {
	Log(ctx context.Context, msg string, kv ...any)
}

// Option configures a [Broker] at construction time. Pass options to
// [New]; later options override earlier ones for the same field.
type Option func(*config)

// config is the internal mutable bag the [Option] callbacks populate.
// Held in a separate type so [Broker] itself stays immutable after
// [New] returns.
type config struct {
	clock          func() time.Time
	logger         Logger
	reaperInterval time.Duration
}

// WithClock overrides the wall-clock function used to compute token
// expiry and to evaluate the expiry boundary on [Broker.Validate].
// Defaults to [time.Now]. A nil function is a no-op so callers can
// always pass through whatever clock they have.
//
// Test code wires a deterministic [func() time.Time] backed by an
// advanceable counter so expiry-edge cases avoid `time.Sleep`.
func WithClock(c func() time.Time) Option {
	return func(cfg *config) {
		if c != nil {
			cfg.clock = c
		}
	}
}

// WithLogger wires a diagnostic sink onto the returned [*Broker]. When
// set, the broker calls `Log(ctx, msg, kv...)` on issue / validate /
// revoke / expiry / close events. A nil logger is a no-op so callers
// can always pass through whatever they have.
//
// IMPORTANT: log entries NEVER carry full token values; only the first
// [tokenPrefixLen]-character prefix (under `token_prefix`). See the
// redaction-discipline contract in the package godoc.
func WithLogger(l Logger) Option {
	return func(cfg *config) {
		if l != nil {
			cfg.logger = l
		}
	}
}

// WithReaperInterval enables the optional background reaper. When `d`
// is positive the broker spawns a single goroutine on [New] that wakes
// every `d`, sweeps the internal map, and removes any entries whose
// `expiry <= now()`. A non-positive `d` (zero or negative) disables
// the reaper — only the lazy cleanup path on [Broker.Validate] runs.
//
// The reaper is OFF by default. Enable it for issue-heavy /
// validate-light workloads where lazy cleanup alone would let expired
// entries accumulate.
func WithReaperInterval(d time.Duration) Option {
	return func(cfg *config) {
		cfg.reaperInterval = d
	}
}

// entry is the per-token record stored in the broker map. Held by
// pointer so the reaper can update fields under the broker's write
// lock without copying.
//
// `organizationID` is populated by [Broker.IssueForOrg] (M3.5.a) and
// left empty by the legacy [Broker.Issue] path. The validate side
// reads it via [Broker.ValidateForOrg] to enforce per-tenant pinning;
// the legacy [Broker.Validate] does not look at it, so a token issued
// without an org still passes scope-only validation. Storing the
// field as a plain string (not a sentinel) keeps the legacy serialised
// shape of the entry struct unchanged for callers that snapshot it.
type entry struct {
	scope          string
	organizationID string
	expiry         time.Time
}

// Broker mints, validates, and revokes opaque scoped capability
// tokens. Construct via [New]; the zero value is not usable. Methods
// are safe for concurrent use across goroutines once constructed and
// remain usable until [Broker.Close] returns; after Close every
// Issue / Validate / Revoke call returns [ErrClosed].
//
// See the package godoc and `core/pkg/capability/README.md` for the
// scope convention, expiry boundary semantics, and redaction
// discipline.
type Broker struct {
	clock          func() time.Time
	logger         Logger
	reaperInterval time.Duration

	mu     sync.RWMutex
	tokens map[string]*entry

	closed    atomic.Bool
	closeOnce sync.Once
	done      chan struct{}
	wg        sync.WaitGroup
}

// New constructs a [Broker] with the supplied options applied. The
// constructor never panics — the broker has no required dependency.
// This is an intentional divergence from the M3.1 / M3.2.b / M3.3
// panic-on-nil pattern: a Broker with no clock, logger, or reaper is
// fully functional (it uses [time.Now], drops diagnostics, and runs
// only the lazy-cleanup path).
//
// When [WithReaperInterval] is supplied with a positive duration the
// broker spawns the reaper goroutine here and tracks it via the
// internal [sync.WaitGroup] so [Broker.Close] can drain it.
func New(opts ...Option) *Broker {
	cfg := config{
		clock: time.Now,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	b := &Broker{
		clock:          cfg.clock,
		logger:         cfg.logger,
		reaperInterval: cfg.reaperInterval,
		tokens:         make(map[string]*entry),
		done:           make(chan struct{}),
	}
	if b.reaperInterval > 0 {
		b.wg.Add(1)
		go b.runReaper()
	}
	return b
}

// Issue mints a fresh capability token bound to `scope` with an
// expiry of `clock() + ttl`. Returns the token bytes and a nil error
// on success.
//
// Validation order (synchronous, no map mutation on failure):
//
//  1. `scope == ""` → [ErrInvalidScope].
//  2. `ttl <= 0` → [ErrInvalidTTL].
//  3. broker [Broker.Close]d → [ErrClosed].
//  4. CSPRNG read failure → wrapped error (extremely rare).
//
// On success the broker logs `"capability: issued"` with `scope`,
// `token_prefix` (first 8 chars), and `expiry` (RFC3339). The full
// token NEVER appears in any log entry — see the redaction-discipline
// contract in the package godoc.
func (b *Broker) Issue(scope string, ttl time.Duration) (string, error) {
	if scope == "" {
		return "", ErrInvalidScope
	}
	if ttl <= 0 {
		return "", ErrInvalidTTL
	}
	if b.closed.Load() {
		return "", ErrClosed
	}

	token, err := newToken()
	if err != nil {
		return "", fmt.Errorf("capability: generate token: %w", err)
	}
	expiry := b.clock().Add(ttl)

	b.mu.Lock()
	if b.closed.Load() {
		b.mu.Unlock()
		return "", ErrClosed
	}
	b.tokens[token] = &entry{scope: scope, expiry: expiry}
	b.mu.Unlock()

	b.log(
		context.Background(), "capability: issued",
		"scope", scope,
		"token_prefix", tokenPrefix(token),
		"expiry", expiry.UTC().Format(time.RFC3339Nano),
	)
	return token, nil
}

// Validate looks up `token` and decides admit / deny. Returns nil on
// match; otherwise one of [ErrInvalidToken] / [ErrTokenExpired] /
// [ErrScopeMismatch] / [ErrClosed] / `ctx.Err()`.
//
// Validation order:
//
//  1. `ctx.Err() != nil` → ctx.Err() (pre-check; no map read).
//  2. broker [Broker.Close]d → [ErrClosed].
//  3. token not in map → [ErrInvalidToken].
//  4. `clock() >= entry.expiry` (boundary inclusive) → delete entry,
//     return [ErrTokenExpired].
//  5. `entry.scope != scope` → [ErrScopeMismatch].
//  6. otherwise → nil.
//
// The fast path uses an RLock; the expiry-delete path upgrades to a
// Lock briefly. Steps 3, 4, 5 NEVER include the input token bytes in
// the returned error — only the bare sentinel — so callers' err.Error()
// is safe to log.
func (b *Broker) Validate(ctx context.Context, token, scope string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if b.closed.Load() {
		return ErrClosed
	}

	b.mu.RLock()
	e, ok := b.tokens[token]
	if !ok {
		b.mu.RUnlock()
		return ErrInvalidToken
	}
	expired := !b.clock().Before(e.expiry)
	entryScope := e.scope
	b.mu.RUnlock()

	if expired {
		b.mu.Lock()
		// Re-check the entry under the write lock — a concurrent
		// Revoke or reaper sweep may have removed it already; this is
		// fine, just don't double-delete.
		if cur, stillThere := b.tokens[token]; stillThere && !b.clock().Before(cur.expiry) {
			delete(b.tokens, token)
		}
		b.mu.Unlock()
		b.log(
			ctx, "capability: expired",
			"scope", entryScope,
			"token_prefix", tokenPrefix(token),
		)
		return ErrTokenExpired
	}

	if entryScope != scope {
		b.log(
			ctx, "capability: scope_mismatch",
			"expected_scope", scope,
			"token_prefix", tokenPrefix(token),
		)
		return ErrScopeMismatch
	}

	b.log(
		ctx, "capability: validated",
		"scope", scope,
		"token_prefix", tokenPrefix(token),
	)
	return nil
}

// IssueForOrg mints a fresh capability token bound to `scope`,
// `organizationID`, and an expiry of `clock() + ttl`. It is the
// per-tenant sibling of [Broker.Issue] introduced for M3.5.a so keep
// handlers can pin authorization to the verified tenant carried on
// `auth.Claim.OrganizationID` rather than to a request-body input.
//
// Validation order (synchronous, no map mutation on failure):
//
//  1. `scope == ""` → [ErrInvalidScope].
//  2. `organizationID == ""` → [ErrInvalidOrganization].
//  3. `ttl <= 0` → [ErrInvalidTTL].
//  4. broker [Broker.Close]d → [ErrClosed].
//  5. CSPRNG read failure → wrapped error (extremely rare).
//
// On success the broker logs `"capability: issued"` with `scope`,
// `organization_id`, `token_prefix` (first 8 chars), and `expiry`
// (RFC3339). The full token NEVER appears in any log entry — see the
// redaction-discipline contract in the package godoc. The
// `organization_id` is logged in full (it is a tenant identifier, not
// a secret).
//
// Tokens minted here are validated via [Broker.ValidateForOrg]; calling
// the legacy [Broker.Validate] on them succeeds when the scope matches
// (the legacy validator simply does not inspect the org dimension).
func (b *Broker) IssueForOrg(scope, organizationID string, ttl time.Duration) (string, error) {
	if scope == "" {
		return "", ErrInvalidScope
	}
	if organizationID == "" {
		return "", ErrInvalidOrganization
	}
	if ttl <= 0 {
		return "", ErrInvalidTTL
	}
	if b.closed.Load() {
		return "", ErrClosed
	}

	token, err := newToken()
	if err != nil {
		return "", fmt.Errorf("capability: generate token: %w", err)
	}
	expiry := b.clock().Add(ttl)

	b.mu.Lock()
	if b.closed.Load() {
		b.mu.Unlock()
		return "", ErrClosed
	}
	b.tokens[token] = &entry{scope: scope, organizationID: organizationID, expiry: expiry}
	b.mu.Unlock()

	b.log(
		context.Background(), "capability: issued",
		"scope", scope,
		"organization_id", organizationID,
		"token_prefix", tokenPrefix(token),
		"expiry", expiry.UTC().Format(time.RFC3339Nano),
	)
	return token, nil
}

// ValidateForOrg looks up `token` and decides admit / deny under the
// per-tenant pinning contract. It is the M3.5.a sibling of
// [Broker.Validate]: a token minted for tenant A must NEVER validate
// for tenant B even when the scope matches. Returns nil on full match;
// otherwise one of [ErrInvalidToken] / [ErrTokenExpired] /
// [ErrScopeMismatch] / [ErrOrganizationMismatch] / [ErrClosed] /
// `ctx.Err()`.
//
// Validation order:
//
//  1. `ctx.Err() != nil` → ctx.Err() (pre-check; no map read).
//  2. broker [Broker.Close]d → [ErrClosed].
//  3. token not in map → [ErrInvalidToken].
//  4. `clock() >= entry.expiry` (boundary inclusive) → delete entry,
//     return [ErrTokenExpired].
//  5. `entry.scope != scope` → [ErrScopeMismatch].
//  6. `entry.organizationID != organizationID` →
//     [ErrOrganizationMismatch]. The token is left in the map so a
//     follow-up validate for the right tenant still succeeds.
//  7. otherwise → nil.
//
// A legacy token minted via [Broker.Issue] (no organizationID) will
// fail step 6 with [ErrOrganizationMismatch] because its stored
// organizationID is empty and an empty argument is rejected up-front
// by [Broker.IssueForOrg]. The empty-vs-empty case is impossible to
// reach legitimately, so callers reaching ValidateForOrg with an empty
// `organizationID` argument get [ErrInvalidOrganization] before any
// map work runs.
func (b *Broker) ValidateForOrg(ctx context.Context, token, scope, organizationID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if organizationID == "" {
		return ErrInvalidOrganization
	}
	if b.closed.Load() {
		return ErrClosed
	}

	b.mu.RLock()
	e, ok := b.tokens[token]
	if !ok {
		b.mu.RUnlock()
		return ErrInvalidToken
	}
	expired := !b.clock().Before(e.expiry)
	entryScope := e.scope
	entryOrg := e.organizationID
	b.mu.RUnlock()

	if expired {
		b.mu.Lock()
		// Re-check under the write lock; concurrent Revoke / reaper may
		// have removed the entry already, in which case skip the delete.
		if cur, stillThere := b.tokens[token]; stillThere && !b.clock().Before(cur.expiry) {
			delete(b.tokens, token)
		}
		b.mu.Unlock()
		b.log(
			ctx, "capability: expired",
			"scope", entryScope,
			"organization_id", entryOrg,
			"token_prefix", tokenPrefix(token),
		)
		return ErrTokenExpired
	}

	if entryScope != scope {
		b.log(
			ctx, "capability: scope_mismatch",
			"expected_scope", scope,
			"organization_id", entryOrg,
			"token_prefix", tokenPrefix(token),
		)
		return ErrScopeMismatch
	}

	if entryOrg != organizationID {
		b.log(
			ctx, "capability: organization_mismatch",
			"scope", scope,
			"expected_organization_id", organizationID,
			"token_prefix", tokenPrefix(token),
		)
		return ErrOrganizationMismatch
	}

	b.log(
		ctx, "capability: validated",
		"scope", scope,
		"organization_id", organizationID,
		"token_prefix", tokenPrefix(token),
	)
	return nil
}

// Revoke removes the entry for `token` from the broker. Idempotent:
// revoking a token that was never issued, already expired-and-pruned,
// or already revoked is a no-op and returns nil. Returns [ErrClosed]
// after [Broker.Close].
//
// On success Revoke logs `"capability: revoked"` with the
// `token_prefix` only — never the full token. See the
// redaction-discipline contract in the package godoc.
func (b *Broker) Revoke(token string) error {
	if b.closed.Load() {
		return ErrClosed
	}
	b.mu.Lock()
	delete(b.tokens, token)
	b.mu.Unlock()
	b.log(
		context.Background(), "capability: revoked",
		"token_prefix", tokenPrefix(token),
	)
	return nil
}

// Close marks the broker as closed, stops the reaper goroutine if
// started, drains it, and clears the token map. Subsequent
// [Broker.Issue] / [Broker.Validate] / [Broker.Revoke] calls return
// [ErrClosed]. Idempotent: subsequent Close calls observe the
// closeOnce guard and short-circuit, returning nil.
//
// Close is intentionally synchronous — it returns only after the
// reaper goroutine (if started) has exited. Tests asserting "no
// goroutine leak" rely on this guarantee.
func (b *Broker) Close() error {
	b.closeOnce.Do(func() {
		b.closed.Store(true)
		close(b.done)
		b.wg.Wait()
		b.mu.Lock()
		// Wipe the map outright; freeing the underlying buckets makes
		// the post-Close memory profile match a fresh broker.
		b.tokens = make(map[string]*entry)
		b.mu.Unlock()
		b.log(context.Background(), "capability: closed")
	})
	return nil
}

// runReaper is the optional background sweep loop spawned by [New]
// when [WithReaperInterval] supplies a positive duration. The loop
// exits when the broker's `done` channel closes (during [Broker.Close]).
func (b *Broker) runReaper() {
	defer b.wg.Done()
	ticker := time.NewTicker(b.reaperInterval)
	defer ticker.Stop()
	for {
		select {
		case <-b.done:
			return
		case <-ticker.C:
			b.pruneExpired()
		}
	}
}

// pruneExpired walks the token map under the write lock and deletes
// every entry whose `expiry <= now()`. Called by the reaper goroutine
// (and could be called by Close, but Close clears the map outright so
// no need). Entry deletion under range is safe per the Go spec.
func (b *Broker) pruneExpired() {
	now := b.clock()
	b.mu.Lock()
	defer b.mu.Unlock()
	for tok, e := range b.tokens {
		if !now.Before(e.expiry) {
			delete(b.tokens, tok)
			b.logLocked(
				context.Background(), "capability: reaper_pruned",
				"scope", e.scope,
				"token_prefix", tokenPrefix(tok),
			)
		}
	}
}

// log forwards a diagnostic message to the optional [Logger]. Nil-logger
// safe: a Broker constructed without [WithLogger] silently drops.
func (b *Broker) log(ctx context.Context, msg string, kv ...any) {
	if b.logger == nil {
		return
	}
	b.logger.Log(ctx, msg, kv...)
}

// logLocked is the variant called from contexts that already hold
// [Broker.mu]. It is structurally identical to [Broker.log]; the
// distinct name is purely a documentation aid for callers reasoning
// about lock ordering.
func (b *Broker) logLocked(ctx context.Context, msg string, kv ...any) {
	if b.logger == nil {
		return
	}
	b.logger.Log(ctx, msg, kv...)
}

// tokenPrefix returns the first [tokenPrefixLen] characters of `token`,
// or the whole string if shorter. Used as the `token_prefix` log field
// — the only token-derived value that may appear in any log payload.
func tokenPrefix(token string) string {
	if len(token) <= tokenPrefixLen {
		return token
	}
	return token[:tokenPrefixLen]
}

// newToken generates a fresh 256-bit capability token encoded as
// URL-safe base64 with no padding. Returns a 43-character string on
// success.
func newToken() (string, error) {
	buf := make([]byte, tokenRandomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
