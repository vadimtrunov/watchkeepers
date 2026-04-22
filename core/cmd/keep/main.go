// Command keep is the Keep service entrypoint.
//
// Boot sequence (AC2/AC3/AC4):
//  1. Load config from env — KEEP_DATABASE_URL is required, everything else
//     has a documented default.
//  2. Create a pgxpool.Pool and Ping it — exit non-zero on either failure.
//  3. Run the HTTP server until SIGINT/SIGTERM, then call http.Server.Shutdown
//     with the configured timeout and close the pool.
//
// Business endpoints land in M2.7.c-e; this binary currently serves GET
// /health only.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vadimtrunov/watchkeepers/core/internal/keep/auth"
	"github.com/vadimtrunov/watchkeepers/core/internal/keep/config"
	"github.com/vadimtrunov/watchkeepers/core/internal/keep/server"
)

// pingTimeout bounds the initial pgxpool.Pool.Ping on boot so an unreachable
// DSN fails fast (AC2 negative test "unreachable DB" must exit within 5s).
const pingTimeout = 5 * time.Second

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is the testable entrypoint. It intentionally takes writers and an args
// slice so the main function stays a thin os.Exit wrapper. M2.7.a has no
// flags; args is reserved for future subcommands (e.g. `keep migrate`).
func run(_ []string, _ io.Writer, stderr io.Writer) int {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(stderr, "keep: config: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pingCtx, pingCancel := context.WithTimeout(ctx, pingTimeout)
	defer pingCancel()

	pool, err := pgxpool.New(pingCtx, cfg.DatabaseURL)
	if err != nil {
		fmt.Fprintf(stderr, "keep: pgxpool: %v\n", err)
		return 1
	}
	defer pool.Close()

	if err := pool.Ping(pingCtx); err != nil {
		fmt.Fprintf(stderr, "keep: pgxpool ping: %v\n", err)
		return 1
	}

	verifier, err := auth.NewHMACVerifier(cfg.TokenSigningKey, cfg.TokenIssuer, time.Now)
	if err != nil {
		fmt.Fprintf(stderr, "keep: auth: %v\n", err)
		return 1
	}

	srv := server.New(cfg, pool, verifier)
	if err := srv.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(stderr, "keep: server: %v\n", err)
		return 1
	}
	return 0
}
