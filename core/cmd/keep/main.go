// Command keep is the Keep service entrypoint.
//
// Boot sequence (AC2/AC3/AC4):
//  1. Load config from env — KEEP_DATABASE_URL is required, everything else
//     has a documented default.
//  2. Create a pgxpool.Pool and Ping it — exit non-zero on either failure.
//  3. Run the HTTP server until SIGINT/SIGTERM, then call http.Server.Shutdown
//     with the configured timeout and close the pool.
//
// M10.1 adds the observability surface: a process-wide
// [*wkmetrics.Metrics] is constructed at boot, mounted at GET /metrics
// on the HTTP router (outside the auth wall), and threaded into the
// outbox worker so publish outcomes are recorded. Structured JSON
// logging is via [wklog] — env vars WK_LOG_LEVEL[_KEEP_*] tune levels
// per subsystem.
package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vadimtrunov/watchkeepers/core/internal/keep/auth"
	"github.com/vadimtrunov/watchkeepers/core/internal/keep/config"
	"github.com/vadimtrunov/watchkeepers/core/internal/keep/publish"
	"github.com/vadimtrunov/watchkeepers/core/internal/keep/server"
	"github.com/vadimtrunov/watchkeepers/core/pkg/wklog"
	"github.com/vadimtrunov/watchkeepers/core/pkg/wkmetrics"
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
	logger := wklog.NewWithWriter("keep", stderr, wklog.Options{})

	cfg, err := config.Load()
	if err != nil {
		logger.Error("config load failed", slog.String("error", err.Error()))
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pingCtx, pingCancel := context.WithTimeout(ctx, pingTimeout)
	defer pingCancel()

	pool, err := pgxpool.New(pingCtx, cfg.DatabaseURL)
	if err != nil {
		logger.Error("pgxpool init failed", slog.String("error", err.Error()))
		return 1
	}
	defer pool.Close()

	if err := pool.Ping(pingCtx); err != nil {
		logger.Error("pgxpool ping failed", slog.String("error", err.Error()))
		return 1
	}

	verifier, err := auth.NewHMACVerifier(cfg.TokenSigningKey, cfg.TokenIssuer, time.Now)
	if err != nil {
		logger.Error("auth verifier init failed", slog.String("error", err.Error()))
		return 1
	}

	// Process-wide metrics instance. Owned by main; threaded into both
	// the HTTP router (mounts GET /metrics) and the outbox worker (so
	// publish outcomes accumulate). Constructed BEFORE the registry so
	// outbox handlers can refer to it on dispatch.
	metrics := wkmetrics.New()

	// Registry is process-owned and closed by Server.Run during shutdown;
	// do not call reg.Close() here or the second close would be a no-op
	// but the invariant is clearer if the lifecycle stays with Server.
	reg := publish.NewRegistry(cfg.SubscribeBuffer, cfg.SubscribeHeartbeat)

	// Outbox worker polls watchkeeper.outbox for unpublished rows and fans
	// them out via the Registry. Server.Run enforces the shutdown order:
	//   cancel(workerCtx) → workerDone → reg.Close() → httpSrv.Shutdown
	workerCfg := publish.WorkerConfig{PollInterval: cfg.OutboxPollInterval}
	worker := publish.NewWorker(pool, reg, workerCfg)
	worker.WithObservability(wklog.NewWithWriter("keep.outbox", stderr, wklog.Options{}), metrics)

	srv := server.NewWithMetrics(cfg, pool, verifier, reg, metrics)
	srv.WithWorker(worker)
	if err := srv.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("server exited", slog.String("error", err.Error()))
		return 1
	}
	return 0
}
