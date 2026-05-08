// Package server wires the Keep service HTTP surface.
//
// For M2.7.a the server exposes exactly one route: GET /health. Business
// endpoints (search/store/subscribe/log_*, *_manifest) land in M2.7.c-e. The
// package is intentionally small so that main.go wiring stays linear and
// testable.
package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vadimtrunov/watchkeepers/core/internal/keep/auth"
	"github.com/vadimtrunov/watchkeepers/core/internal/keep/config"
	"github.com/vadimtrunov/watchkeepers/core/internal/keep/publish"
)

// healthBody is the exact, byte-stable payload returned by GET /health.
// Declared as a package constant so both the handler and its test reference
// the same literal.
const healthBody = `{"status":"ok"}`

// HealthHandler returns an http.Handler that answers GET /health with a
// 200 OK, Content-Type application/json, and body `{"status":"ok"}`.
func HealthHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, healthBody)
	})
}

// NewRouter returns the HTTP router with every Keep route wired. /health is
// mounted outside the auth wall; every /v1/* route runs through
// AuthMiddleware(v) and is backed by handlers that open a per-request
// scoped transaction against pool via db.WithScope.
//
// reg is the in-process publish Registry that drives GET /v1/subscribe. A
// nil reg is permitted for tests that do not exercise the streaming
// route (M2.7.a-d call sites); NewRouter will simply omit the route in
// that case and any request to it will 404 through the default mux.
//
// A nil v is permitted only when no /v1 routes will be exercised
// (e.g. test doubles that only hit /health); a nil pool is permitted in
// tests that exit before the DB round-trip (the router never dereferences
// the pool directly — it is captured by the handlers that need it).
func NewRouter(v auth.Verifier, pool *pgxpool.Pool, reg *publish.Registry, heartbeat time.Duration) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /health", HealthHandler())

	if v != nil {
		authed := AuthMiddleware(v)
		runner := poolRunner{pool: pool}
		mux.Handle("POST /v1/search", authed(handleSearch(runner)))
		mux.Handle("GET /v1/manifests/{manifest_id}", authed(handleGetManifest(runner)))
		mux.Handle("GET /v1/keepers-log", authed(handleLogTail(runner)))
		mux.Handle("GET /v1/cost-rollups", authed(handleCostRollups(runner)))
		mux.Handle("POST /v1/knowledge-chunks", authed(handleStore(runner)))
		mux.Handle("POST /v1/keepers-log", authed(handleLogAppend(runner)))
		mux.Handle("PUT /v1/manifests/{manifest_id}/versions", authed(handlePutManifestVersion(runner)))
		mux.Handle("POST /v1/watchkeepers", authed(handleInsertWatchkeeper(runner)))
		mux.Handle("PATCH /v1/watchkeepers/{id}/status", authed(handleUpdateWatchkeeperStatus(runner)))
		mux.Handle("PATCH /v1/watchkeepers/{id}/lead", authed(handleSetWatchkeeperLead(runner)))
		mux.Handle("GET /v1/watchkeepers/{id}", authed(handleGetWatchkeeper(runner)))
		mux.Handle("GET /v1/watchkeepers", authed(handleListWatchkeepers(runner)))
		mux.Handle("POST /v1/humans", authed(handleInsertHuman(runner)))
		mux.Handle("GET /v1/humans/by-slack/{slack_user_id}", authed(handleLookupHumanBySlackID(runner)))
		if reg != nil {
			mux.Handle("GET /v1/subscribe", authed(handleSubscribe(reg, heartbeat)))
		}
	}
	return mux
}

// WorkerRunner is the narrow interface Server.Run uses to start and stop
// the outbox worker. The concrete type is *publish.Worker; the interface
// keeps the server package free of a direct import cycle with publish while
// also making it trivial to swap in a no-op for tests.
type WorkerRunner interface {
	Run(ctx context.Context) error
}

// Server is the Keep HTTP server plus its owned lifecycle. It does not own
// the pgxpool.Pool — main.go owns and closes the pool after Run returns.
//
// The server DOES own the publish.Registry for its lifetime: Run closes
// the registry on shutdown so every in-flight GET /v1/subscribe stream
// returns promptly before httpSrv.Shutdown begins its grace period
// (AC6). Callers must not call reg.Close themselves after handing it
// here.
//
// If a worker is supplied via WithWorker, Run starts it in a goroutine and
// cancels it before reg.Close() on shutdown, preserving the invariant:
//
//	cancel(workerCtx) → workerDone wait → reg.Close() → httpSrv.Shutdown
type Server struct {
	httpSrv         *http.Server
	shutdownTimeout time.Duration
	reg             *publish.Registry
	worker          WorkerRunner
}

// New builds a Server bound to cfg.HTTPAddr using the default Keep router.
// The pool is captured by the scoped read/write handlers; the verifier is
// captured by AuthMiddleware; reg drives GET /v1/subscribe.
//
// For backward compatibility with the earlier M2.7.a call-site shape
// (where no verifier was threaded through), callers that need only
// /health can pass nil for v and reg — the router will then skip the
// /v1/* wiring. Production main.go always supplies all three.
func New(cfg config.Config, pool *pgxpool.Pool, v auth.Verifier, reg *publish.Registry) *Server {
	srv := NewWithHandler(cfg, pool, NewRouter(v, pool, reg, cfg.SubscribeHeartbeat))
	srv.reg = reg
	return srv
}

// WithWorker attaches an outbox worker to the server. Run will start it in a
// goroutine and cancel its context before reg.Close() during shutdown,
// preserving the invariant: cancel(workerCtx) → workerDone → reg.Close() →
// httpSrv.Shutdown. Calling WithWorker more than once replaces the previous
// worker; only the last one is used.
func (s *Server) WithWorker(w WorkerRunner) {
	s.worker = w
}

// NewWithHandler builds a Server with a caller-supplied http.Handler. The
// default main.go path uses New (which delegates here with NewRouter); tests
// and future composition can pass a custom mux — useful for exercising
// graceful-shutdown edge cases against a slow handler without making the
// production router aware of them.
func NewWithHandler(cfg config.Config, _ *pgxpool.Pool, h http.Handler) *Server {
	return &Server{
		httpSrv: &http.Server{
			Addr:              cfg.HTTPAddr,
			Handler:           h,
			ReadHeaderTimeout: 5 * time.Second,
		},
		shutdownTimeout: cfg.ShutdownTimeout,
	}
}

// Run serves HTTP requests until ctx is canceled, then shuts down in order:
//  1. Cancel the outbox worker context and wait for it to exit (if wired).
//  2. Close the publish Registry so in-flight SSE streams return.
//  3. Call httpSrv.Shutdown with the configured timeout.
//
// It returns nil on a clean shutdown and an error otherwise.
// http.ErrServerClosed is treated as the success sentinel.
func (s *Server) Run(ctx context.Context) error {
	// Start the outbox worker if one is wired. The worker runs under its
	// own cancellable child context so we can stop it before closing the
	// registry (step 1 of the shutdown invariant).
	var (
		workerCancel context.CancelFunc
		workerDone   chan error
	)
	if s.worker != nil {
		workerCtx, cancel := context.WithCancel(ctx)
		workerCancel = cancel
		workerDone = make(chan error, 1)
		go func() {
			workerDone <- s.worker.Run(workerCtx)
		}()
	}

	errCh := make(chan error, 1)
	go func() {
		if err := s.httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		if workerCancel != nil {
			workerCancel()
			<-workerDone
		}
		return err
	case <-ctx.Done():
		// Step 1: stop the worker first so it does not publish to a
		// closing registry.
		if workerCancel != nil {
			workerCancel()
			<-workerDone
		}
		// Step 2: close the registry so every active SSE handler sees its
		// channel close and returns. Doing this before httpSrv.Shutdown
		// means the grace period is not spent waiting for streams that
		// by contract never end on their own.
		if s.reg != nil {
			s.reg.Close()
		}
		// Step 3: derive the shutdown deadline from a detached copy of ctx
		// so the already-canceled parent does not abort Shutdown immediately
		// while still letting contextcheck see an inherited lineage.
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.shutdownTimeout)
		defer cancel()
		if err := s.httpSrv.Shutdown(shutdownCtx); err != nil {
			return err
		}
		// Drain the goroutine so ListenAndServe's return value is observed.
		if err := <-errCh; err != nil {
			return err
		}
		return nil
	}
}
