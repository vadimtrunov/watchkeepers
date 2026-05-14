package config_test

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/internal/keep/config"
)

// fakeDSN is a synthetic Postgres DSN used only by tests. It never touches a
// real database; the credentials are placeholders to satisfy pgx URL parsing.
// #nosec G101 -- test fixture, not a real credential.
const fakeDSN = "postgres://u:p@localhost:5432/db?sslmode=disable"

// fakeSigningKey32 is a deterministic 32-byte string used in tests, encoded
// as base64 to mirror the real KEEP_TOKEN_SIGNING_KEY env format.
// #nosec G101 -- test fixture, not a real credential.
var fakeSigningKey32 = base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))

// setTokenEnv sets both token env vars to valid values. Tests that need to
// assert defaults for DatabaseURL/HTTPAddr must still set these or the new
// token-related sentinel errors will fire first.
func setTokenEnv(t *testing.T) {
	t.Helper()
	t.Setenv("KEEP_TOKEN_SIGNING_KEY", fakeSigningKey32)
	t.Setenv("KEEP_TOKEN_ISSUER", "keep-test")
}

func TestLoad_AllEnvSet(t *testing.T) {
	setTokenEnv(t)
	t.Setenv("KEEP_DATABASE_URL", fakeDSN)
	t.Setenv("KEEP_HTTP_ADDR", ":9090")
	t.Setenv("KEEP_SHUTDOWN_TIMEOUT", "25s")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}

	if cfg.DatabaseURL != fakeDSN {
		t.Errorf("DatabaseURL = %q", cfg.DatabaseURL)
	}
	if cfg.HTTPAddr != ":9090" {
		t.Errorf("HTTPAddr = %q, want :9090", cfg.HTTPAddr)
	}
	if cfg.ShutdownTimeout != 25*time.Second {
		t.Errorf("ShutdownTimeout = %v, want 25s", cfg.ShutdownTimeout)
	}
	if len(cfg.TokenSigningKey) < 32 {
		t.Errorf("TokenSigningKey len = %d, want >= 32", len(cfg.TokenSigningKey))
	}
	if cfg.TokenIssuer != "keep-test" {
		t.Errorf("TokenIssuer = %q, want keep-test", cfg.TokenIssuer)
	}
}

func TestLoad_Defaults(t *testing.T) {
	setTokenEnv(t)
	t.Setenv("KEEP_DATABASE_URL", fakeDSN)
	// Explicitly unset optionals so we exercise defaults regardless of caller env.
	t.Setenv("KEEP_HTTP_ADDR", "")
	t.Setenv("KEEP_SHUTDOWN_TIMEOUT", "")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}

	if cfg.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr default = %q, want :8080", cfg.HTTPAddr)
	}
	if cfg.ShutdownTimeout != 10*time.Second {
		t.Errorf("ShutdownTimeout default = %v, want 10s", cfg.ShutdownTimeout)
	}
}

func TestLoad_MissingDatabaseURL(t *testing.T) {
	setTokenEnv(t)
	t.Setenv("KEEP_DATABASE_URL", "")

	_, err := config.Load()
	if err == nil {
		t.Fatal("Load() returned nil error, want error for missing KEEP_DATABASE_URL")
	}
	if !errors.Is(err, config.ErrMissingDatabaseURL) {
		t.Errorf("Load() error = %v, want errors.Is ErrMissingDatabaseURL", err)
	}
}

func TestLoad_InvalidShutdownTimeout(t *testing.T) {
	setTokenEnv(t)
	t.Setenv("KEEP_DATABASE_URL", fakeDSN)
	t.Setenv("KEEP_SHUTDOWN_TIMEOUT", "not-a-duration")

	_, err := config.Load()
	if err == nil {
		t.Fatal("Load() returned nil error for invalid KEEP_SHUTDOWN_TIMEOUT")
	}
}

func TestLoad_MissingTokenSigningKey(t *testing.T) {
	t.Setenv("KEEP_DATABASE_URL", fakeDSN)
	t.Setenv("KEEP_TOKEN_SIGNING_KEY", "")
	t.Setenv("KEEP_TOKEN_ISSUER", "keep-test")

	_, err := config.Load()
	if err == nil {
		t.Fatal("Load() returned nil error for missing KEEP_TOKEN_SIGNING_KEY")
	}
	if !errors.Is(err, config.ErrMissingTokenSigningKey) {
		t.Errorf("Load() error = %v, want errors.Is ErrMissingTokenSigningKey", err)
	}
}

func TestLoad_MissingTokenIssuer(t *testing.T) {
	t.Setenv("KEEP_DATABASE_URL", fakeDSN)
	t.Setenv("KEEP_TOKEN_SIGNING_KEY", fakeSigningKey32)
	t.Setenv("KEEP_TOKEN_ISSUER", "")

	_, err := config.Load()
	if err == nil {
		t.Fatal("Load() returned nil error for missing KEEP_TOKEN_ISSUER")
	}
	if !errors.Is(err, config.ErrMissingTokenIssuer) {
		t.Errorf("Load() error = %v, want errors.Is ErrMissingTokenIssuer", err)
	}
}

func TestLoad_TokenSigningKeyTooShort(t *testing.T) {
	t.Setenv("KEEP_DATABASE_URL", fakeDSN)
	// 16 raw bytes -> below the 32-byte floor.
	t.Setenv("KEEP_TOKEN_SIGNING_KEY", base64.StdEncoding.EncodeToString([]byte("0123456789abcdef")))
	t.Setenv("KEEP_TOKEN_ISSUER", "keep-test")

	_, err := config.Load()
	if err == nil {
		t.Fatal("Load() returned nil error for short KEEP_TOKEN_SIGNING_KEY")
	}
	if !errors.Is(err, config.ErrTokenSigningKeyTooShort) {
		t.Errorf("Load() error = %v, want errors.Is ErrTokenSigningKeyTooShort", err)
	}
}

func TestLoad_TokenSigningKeyNotBase64(t *testing.T) {
	t.Setenv("KEEP_DATABASE_URL", fakeDSN)
	t.Setenv("KEEP_TOKEN_SIGNING_KEY", "@@@not-base64@@@")
	t.Setenv("KEEP_TOKEN_ISSUER", "keep-test")

	_, err := config.Load()
	if err == nil {
		t.Fatal("Load() returned nil error for non-base64 KEEP_TOKEN_SIGNING_KEY")
	}
	if !strings.Contains(err.Error(), "base64") {
		t.Errorf("Load() error = %v, want mention of base64", err)
	}
}

func TestLoad_SubscribeDefaults(t *testing.T) {
	setTokenEnv(t)
	t.Setenv("KEEP_DATABASE_URL", fakeDSN)
	t.Setenv("KEEP_SUBSCRIBE_BUFFER", "")
	t.Setenv("KEEP_SUBSCRIBE_HEARTBEAT", "")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.SubscribeBuffer != config.DefaultSubscribeBuffer {
		t.Errorf("SubscribeBuffer default = %d, want %d", cfg.SubscribeBuffer, config.DefaultSubscribeBuffer)
	}
	if cfg.SubscribeHeartbeat != config.DefaultSubscribeHeartbeat {
		t.Errorf("SubscribeHeartbeat default = %v, want %v", cfg.SubscribeHeartbeat, config.DefaultSubscribeHeartbeat)
	}
}

func TestLoad_SubscribeOverrides(t *testing.T) {
	setTokenEnv(t)
	t.Setenv("KEEP_DATABASE_URL", fakeDSN)
	t.Setenv("KEEP_SUBSCRIBE_BUFFER", "128")
	t.Setenv("KEEP_SUBSCRIBE_HEARTBEAT", "5s")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.SubscribeBuffer != 128 {
		t.Errorf("SubscribeBuffer = %d, want 128", cfg.SubscribeBuffer)
	}
	if cfg.SubscribeHeartbeat != 5*time.Second {
		t.Errorf("SubscribeHeartbeat = %v, want 5s", cfg.SubscribeHeartbeat)
	}
}

func TestLoad_SubscribeBufferInvalid(t *testing.T) {
	setTokenEnv(t)
	t.Setenv("KEEP_DATABASE_URL", fakeDSN)

	cases := []string{"0", "-1", "not-a-number"}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("KEEP_SUBSCRIBE_BUFFER", raw)
			if _, err := config.Load(); err == nil {
				t.Fatalf("Load() returned nil for KEEP_SUBSCRIBE_BUFFER=%q", raw)
			}
		})
	}
}

func TestLoad_SubscribeHeartbeatInvalid(t *testing.T) {
	setTokenEnv(t)
	t.Setenv("KEEP_DATABASE_URL", fakeDSN)

	cases := []string{"nope", "0s", "-1s"}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("KEEP_SUBSCRIBE_HEARTBEAT", raw)
			if _, err := config.Load(); err == nil {
				t.Fatalf("Load() returned nil for KEEP_SUBSCRIBE_HEARTBEAT=%q", raw)
			}
		})
	}
}

func TestLoad_OutboxPollIntervalDefault(t *testing.T) {
	setTokenEnv(t)
	t.Setenv("KEEP_DATABASE_URL", fakeDSN)
	t.Setenv("KEEP_OUTBOX_POLL_INTERVAL", "")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.OutboxPollInterval != config.DefaultOutboxPollInterval {
		t.Errorf("OutboxPollInterval default = %v, want %v", cfg.OutboxPollInterval, config.DefaultOutboxPollInterval)
	}
}

func TestLoad_OutboxPollIntervalOverride(t *testing.T) {
	setTokenEnv(t)
	t.Setenv("KEEP_DATABASE_URL", fakeDSN)
	t.Setenv("KEEP_OUTBOX_POLL_INTERVAL", "5s")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.OutboxPollInterval != 5*time.Second {
		t.Errorf("OutboxPollInterval = %v, want 5s", cfg.OutboxPollInterval)
	}
}

func TestLoad_OutboxPollIntervalInvalid(t *testing.T) {
	setTokenEnv(t)
	t.Setenv("KEEP_DATABASE_URL", fakeDSN)

	cases := []struct {
		name string
		raw  string
	}{
		{"not-a-duration", "nope"},
		{"below-min", "50ms"},
		{"above-max", "2m"},
		{"zero", "0s"},
		{"negative", "-1s"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KEEP_OUTBOX_POLL_INTERVAL", tc.raw)
			if _, err := config.Load(); err == nil {
				t.Fatalf("Load() returned nil for KEEP_OUTBOX_POLL_INTERVAL=%q", tc.raw)
			}
		})
	}
}

// TestIter1_DatabaseURL_FileFallback pins the M10.3 iter-1 fix: when
// KEEP_DATABASE_URL_FILE points at a readable file, Load reads the
// DSN from that file (trimming the trailing newline) instead of from
// the env var. The env var itself need not be set. Addresses the
// "credentials visible to `docker inspect`" review finding by letting
// the compose stack mount secrets via docker `secrets:` files.
func TestIter1_DatabaseURL_FileFallback(t *testing.T) {
	setTokenEnv(t)
	path := filepath.Join(t.TempDir(), "dsn.txt")
	if err := os.WriteFile(path, []byte(fakeDSN+"\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	t.Setenv("KEEP_DATABASE_URL_FILE", path)
	// KEEP_DATABASE_URL intentionally unset.

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.DatabaseURL != fakeDSN {
		t.Errorf("DatabaseURL = %q, want %q (trailing newline must be trimmed)", cfg.DatabaseURL, fakeDSN)
	}
}

// TestIter1_TokenSigningKey_FileFallback pins the same file-fallback
// shape for the token signing key. The fixture writes the same
// base64-encoded value the env-form would carry; Load must decode it
// to the same 32-byte key.
func TestIter1_TokenSigningKey_FileFallback(t *testing.T) {
	t.Setenv("KEEP_TOKEN_ISSUER", "keep-test")
	t.Setenv("KEEP_DATABASE_URL", fakeDSN)
	path := filepath.Join(t.TempDir(), "key.b64")
	if err := os.WriteFile(path, []byte(fakeSigningKey32+"\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	t.Setenv("KEEP_TOKEN_SIGNING_KEY_FILE", path)
	// KEEP_TOKEN_SIGNING_KEY intentionally unset.

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(cfg.TokenSigningKey) != 32 {
		t.Errorf("TokenSigningKey length = %d, want 32", len(cfg.TokenSigningKey))
	}
}

// TestIter1_DatabaseURL_FileMissing surfaces the file-read error
// instead of silently falling back to the empty env var.
func TestIter1_DatabaseURL_FileMissing(t *testing.T) {
	setTokenEnv(t)
	t.Setenv("KEEP_DATABASE_URL_FILE", filepath.Join(t.TempDir(), "does-not-exist"))

	_, err := config.Load()
	if err == nil {
		t.Fatalf("Load() returned nil for missing KEEP_DATABASE_URL_FILE")
	}
	if !strings.Contains(err.Error(), "KEEP_DATABASE_URL_FILE") {
		t.Errorf("error %q must name KEEP_DATABASE_URL_FILE so operator sees the diagnostic path", err)
	}
}
