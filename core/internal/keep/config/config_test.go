package config_test

import (
	"errors"
	"testing"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/internal/keep/config"
)

// fakeDSN is a synthetic Postgres DSN used only by tests. It never touches a
// real database; the credentials are placeholders to satisfy pgx URL parsing.
// #nosec G101 -- test fixture, not a real credential.
const fakeDSN = "postgres://u:p@localhost:5432/db?sslmode=disable"

func TestLoad_AllEnvSet(t *testing.T) {
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
}

func TestLoad_Defaults(t *testing.T) {
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
	t.Setenv("KEEP_DATABASE_URL", fakeDSN)
	t.Setenv("KEEP_SHUTDOWN_TIMEOUT", "not-a-duration")

	_, err := config.Load()
	if err == nil {
		t.Fatal("Load() returned nil error for invalid KEEP_SHUTDOWN_TIMEOUT")
	}
}
