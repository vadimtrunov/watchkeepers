// Package config loads the Keep service configuration from the environment.
//
// All values come from environment variables (stdlib os.Getenv, no
// viper/koanf) so the binary stays minimal and 12-factor. KEEP_DATABASE_URL is
// required; KEEP_HTTP_ADDR and KEEP_SHUTDOWN_TIMEOUT fall back to documented
// defaults when unset. Diagnostic error text uses stable, locale-independent
// phrases (see LESSON M2.1.b) so CI assertions never depend on lc_messages.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// envValue resolves a configuration value with file-fallback support.
//
// If `<envName>_FILE` is set, the file at that path is read and its
// contents (trimmed of leading/trailing whitespace, including the
// trailing newline that text editors add) returned. Otherwise the
// value of `<envName>` itself is returned (possibly empty). The
// `_FILE` variant exists so the compose stack (M10.3) can pass
// secrets through docker `secrets:` mounts without the cleartext
// value appearing in `docker inspect` output or in the container's
// `/proc/<pid>/environ` — addresses iter-1 finding #1/#2.
//
// Both forms cannot be set simultaneously; if both are present the
// `_FILE` variant wins because that is the documented secret-bearing
// path. On read failure the original error is wrapped so the
// operator's diagnostic names the file path.
func envValue(envName string) (string, error) {
	if path := os.Getenv(envName + "_FILE"); path != "" {
		raw, err := os.ReadFile(path) //nolint:gosec // path supplied by trusted operator-provided env var
		if err != nil {
			return "", fmt.Errorf("read %s_FILE (%q): %w", envName, path, err)
		}
		return strings.TrimRight(string(raw), " \t\r\n"), nil
	}
	return os.Getenv(envName), nil
}

// MinTokenSigningKeyBytes is the enforced minimum length of the HS256
// signing key (after base64 decode). Mirrors auth.MinSigningKeyBytes so
// the fail-fast gate fires at Config.Load rather than on first token
// verify.
const MinTokenSigningKeyBytes = 32

// Sentinel errors returned by Load. Callers match with errors.Is; the
// messages are stable across locales (LESSON M2.1.b) so CI assertions
// never depend on lc_messages.
var (
	// ErrMissingDatabaseURL — KEEP_DATABASE_URL is unset or empty.
	ErrMissingDatabaseURL = errors.New("KEEP_DATABASE_URL is required")
	// ErrMissingTokenSigningKey — KEEP_TOKEN_SIGNING_KEY is unset.
	ErrMissingTokenSigningKey = errors.New("KEEP_TOKEN_SIGNING_KEY is required")
	// ErrMissingTokenIssuer — KEEP_TOKEN_ISSUER is unset.
	ErrMissingTokenIssuer = errors.New("KEEP_TOKEN_ISSUER is required")
	// ErrTokenSigningKeyTooShort — decoded signing key < 32 bytes.
	ErrTokenSigningKeyTooShort = errors.New("KEEP_TOKEN_SIGNING_KEY must decode to >= 32 bytes")
)

// Default values for optional environment variables (AC2).
const (
	DefaultHTTPAddr           = ":8080"
	DefaultShutdownTimeout    = 10 * time.Second
	DefaultSubscribeBuffer    = 64
	DefaultSubscribeHeartbeat = 15 * time.Second
	DefaultOutboxPollInterval = 1 * time.Second

	// minOutboxPollInterval and maxOutboxPollInterval bound the
	// KEEP_OUTBOX_POLL_INTERVAL env var to prevent accidental DoS (too
	// short) or pathologically stale events (too long).
	minOutboxPollInterval = 100 * time.Millisecond
	maxOutboxPollInterval = 60 * time.Second
)

// Config holds the Keep service runtime configuration loaded from env.
type Config struct {
	// DatabaseURL is a pgx-compatible Postgres DSN (required).
	DatabaseURL string
	// HTTPAddr is the listen address for the Keep HTTP server.
	HTTPAddr string
	// ShutdownTimeout bounds http.Server.Shutdown on SIGINT/SIGTERM.
	ShutdownTimeout time.Duration
	// TokenSigningKey is the raw HMAC-SHA256 key used to verify
	// capability tokens. Decoded from KEEP_TOKEN_SIGNING_KEY (base64).
	// Never logged.
	TokenSigningKey []byte
	// TokenIssuer is the expected `iss` claim on every verified token.
	TokenIssuer string
	// SubscribeBuffer is the per-subscriber event channel capacity used
	// by the publish Registry that backs GET /v1/subscribe. A full buffer
	// causes the slow subscriber to be dropped and its stream closed —
	// sized to absorb short bursts without penalising fast consumers.
	SubscribeBuffer int
	// SubscribeHeartbeat is the interval between SSE heartbeat comments
	// on an idle /v1/subscribe stream. Keeps proxies and the TCP
	// keepalive engine honest without a constant torrent of traffic.
	SubscribeHeartbeat time.Duration
	// OutboxPollInterval is the duration between successive polls of
	// watchkeeper.outbox for unpublished rows. Corresponds to env var
	// KEEP_OUTBOX_POLL_INTERVAL (default 1s, min 100ms, max 60s).
	OutboxPollInterval time.Duration
}

// Load reads configuration from environment variables, applies defaults for
// optional values, and returns a populated Config. It returns
// ErrMissingDatabaseURL when KEEP_DATABASE_URL is absent or empty, and a
// wrapped error when KEEP_SHUTDOWN_TIMEOUT fails to parse as a Go duration.
// KEEP_TOKEN_SIGNING_KEY and KEEP_TOKEN_ISSUER are required for the M2.7.b
// auth middleware; both are validated fail-fast so an operator cannot boot
// a Keep process that would 401 every request at first traffic.
func Load() (Config, error) {
	dbURL, err := envValue("KEEP_DATABASE_URL")
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		DatabaseURL:        dbURL,
		HTTPAddr:           os.Getenv("KEEP_HTTP_ADDR"),
		ShutdownTimeout:    DefaultShutdownTimeout,
		TokenIssuer:        os.Getenv("KEEP_TOKEN_ISSUER"),
		SubscribeBuffer:    DefaultSubscribeBuffer,
		SubscribeHeartbeat: DefaultSubscribeHeartbeat,
		OutboxPollInterval: DefaultOutboxPollInterval,
	}

	if cfg.DatabaseURL == "" {
		return Config{}, ErrMissingDatabaseURL
	}

	if cfg.HTTPAddr == "" {
		cfg.HTTPAddr = DefaultHTTPAddr
	}

	if err := cfg.applyOptionalDurations(); err != nil {
		return Config{}, err
	}

	if err := cfg.applyOptionalInts(); err != nil {
		return Config{}, err
	}

	key, err := loadTokenSigningKey()
	if err != nil {
		return Config{}, err
	}
	cfg.TokenSigningKey = key

	if cfg.TokenIssuer == "" {
		return Config{}, ErrMissingTokenIssuer
	}

	return cfg, nil
}

// applyOptionalDurations parses the duration env vars that may override
// defaults. Extracted to keep Load's cyclomatic complexity within the linter
// threshold (gocyclo ≤ 15).
func (c *Config) applyOptionalDurations() error {
	if raw := os.Getenv("KEEP_SHUTDOWN_TIMEOUT"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return fmt.Errorf("invalid KEEP_SHUTDOWN_TIMEOUT %q: %w", raw, err)
		}
		c.ShutdownTimeout = d
	}

	if raw := os.Getenv("KEEP_SUBSCRIBE_HEARTBEAT"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return fmt.Errorf("invalid KEEP_SUBSCRIBE_HEARTBEAT %q: %w", raw, err)
		}
		if d <= 0 {
			return fmt.Errorf("invalid KEEP_SUBSCRIBE_HEARTBEAT %q: must be positive", raw)
		}
		c.SubscribeHeartbeat = d
	}

	if raw := os.Getenv("KEEP_OUTBOX_POLL_INTERVAL"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return fmt.Errorf("invalid KEEP_OUTBOX_POLL_INTERVAL %q: %w", raw, err)
		}
		if d < minOutboxPollInterval {
			return fmt.Errorf("invalid KEEP_OUTBOX_POLL_INTERVAL %q: must be >= %s", raw, minOutboxPollInterval)
		}
		if d > maxOutboxPollInterval {
			return fmt.Errorf("invalid KEEP_OUTBOX_POLL_INTERVAL %q: must be <= %s", raw, maxOutboxPollInterval)
		}
		c.OutboxPollInterval = d
	}

	return nil
}

// applyOptionalInts parses the integer env vars that may override defaults.
func (c *Config) applyOptionalInts() error {
	if raw := os.Getenv("KEEP_SUBSCRIBE_BUFFER"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return fmt.Errorf("invalid KEEP_SUBSCRIBE_BUFFER %q: must be a positive integer", raw)
		}
		c.SubscribeBuffer = n
	}
	return nil
}

// loadTokenSigningKey reads KEEP_TOKEN_SIGNING_KEY (or its
// KEEP_TOKEN_SIGNING_KEY_FILE file-fallback variant added in M10.3
// iter-1 for compose `secrets:` mounts) from the environment,
// base64-decodes it, and validates its length.
func loadTokenSigningKey() ([]byte, error) {
	rawKey, err := envValue("KEEP_TOKEN_SIGNING_KEY")
	if err != nil {
		return nil, err
	}
	if rawKey == "" {
		return nil, ErrMissingTokenSigningKey
	}
	key, decodeErr := base64.StdEncoding.DecodeString(rawKey)
	if decodeErr != nil {
		return nil, fmt.Errorf("invalid KEEP_TOKEN_SIGNING_KEY: base64 decode: %w", decodeErr)
	}
	if len(key) < MinTokenSigningKeyBytes {
		return nil, fmt.Errorf("%w (got %d)", ErrTokenSigningKeyTooShort, len(key))
	}
	return key, nil
}
