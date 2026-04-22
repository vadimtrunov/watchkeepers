// Package config loads the Keep service configuration from the environment.
//
// All values come from environment variables (stdlib os.Getenv, no
// viper/koanf) so the binary stays minimal and 12-factor. KEEP_DATABASE_URL is
// required; KEEP_HTTP_ADDR and KEEP_SHUTDOWN_TIMEOUT fall back to documented
// defaults when unset. Diagnostic error text uses stable, locale-independent
// phrases (see LESSON M2.1.b) so CI assertions never depend on lc_messages.
package config

import (
	"errors"
	"fmt"
	"os"
	"time"
)

// ErrMissingDatabaseURL is returned by Load when KEEP_DATABASE_URL is unset or
// empty. Callers can match it with errors.Is; the wrapped error's text is
// stable across locales.
var ErrMissingDatabaseURL = errors.New("KEEP_DATABASE_URL is required")

// Default values for optional environment variables (AC2).
const (
	DefaultHTTPAddr        = ":8080"
	DefaultShutdownTimeout = 10 * time.Second
)

// Config holds the Keep service runtime configuration loaded from env.
type Config struct {
	// DatabaseURL is a pgx-compatible Postgres DSN (required).
	DatabaseURL string
	// HTTPAddr is the listen address for the Keep HTTP server.
	HTTPAddr string
	// ShutdownTimeout bounds http.Server.Shutdown on SIGINT/SIGTERM.
	ShutdownTimeout time.Duration
}

// Load reads configuration from environment variables, applies defaults for
// optional values, and returns a populated Config. It returns
// ErrMissingDatabaseURL when KEEP_DATABASE_URL is absent or empty, and a
// wrapped error when KEEP_SHUTDOWN_TIMEOUT fails to parse as a Go duration.
func Load() (Config, error) {
	cfg := Config{
		DatabaseURL:     os.Getenv("KEEP_DATABASE_URL"),
		HTTPAddr:        os.Getenv("KEEP_HTTP_ADDR"),
		ShutdownTimeout: DefaultShutdownTimeout,
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

	return cfg, nil
}
