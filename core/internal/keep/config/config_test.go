package config_test

import (
	"encoding/base64"
	"errors"
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
