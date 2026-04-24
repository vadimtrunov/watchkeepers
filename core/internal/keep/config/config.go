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
	"time"
)

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
	cfg := Config{
		DatabaseURL:        os.Getenv("KEEP_DATABASE_URL"),
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

	if raw := os.Getenv("KEEP_SHUTDOWN_TIMEOUT"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid KEEP_SHUTDOWN_TIMEOUT %q: %w", raw, err)
		}
		cfg.ShutdownTimeout = d
	}

	if raw := os.Getenv("KEEP_SUBSCRIBE_BUFFER"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("invalid KEEP_SUBSCRIBE_BUFFER %q: must be a positive integer", raw)
		}
		cfg.SubscribeBuffer = n
	}

	if raw := os.Getenv("KEEP_SUBSCRIBE_HEARTBEAT"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid KEEP_SUBSCRIBE_HEARTBEAT %q: %w", raw, err)
		}
		if d <= 0 {
			return Config{}, fmt.Errorf("invalid KEEP_SUBSCRIBE_HEARTBEAT %q: must be positive", raw)
		}
		cfg.SubscribeHeartbeat = d
	}

	if raw := os.Getenv("KEEP_OUTBOX_POLL_INTERVAL"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid KEEP_OUTBOX_POLL_INTERVAL %q: %w", raw, err)
		}
		if d < minOutboxPollInterval {
			return Config{}, fmt.Errorf("invalid KEEP_OUTBOX_POLL_INTERVAL %q: must be >= %s", raw, minOutboxPollInterval)
		}
		if d > maxOutboxPollInterval {
			return Config{}, fmt.Errorf("invalid KEEP_OUTBOX_POLL_INTERVAL %q: must be <= %s", raw, maxOutboxPollInterval)
		}
		cfg.OutboxPollInterval = d
	}

	rawKey := os.Getenv("KEEP_TOKEN_SIGNING_KEY")
	if rawKey == "" {
		return Config{}, ErrMissingTokenSigningKey
	}
	key, err := base64.StdEncoding.DecodeString(rawKey)
	if err != nil {
		return Config{}, fmt.Errorf("invalid KEEP_TOKEN_SIGNING_KEY: base64 decode: %w", err)
	}
	if len(key) < MinTokenSigningKeyBytes {
		return Config{}, fmt.Errorf("%w (got %d)", ErrTokenSigningKeyTooShort, len(key))
	}
	cfg.TokenSigningKey = key

	if cfg.TokenIssuer == "" {
		return Config{}, ErrMissingTokenIssuer
	}

	return cfg, nil
}
