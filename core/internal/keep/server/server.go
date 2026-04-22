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
// A nil v is permitted only when no /v1 routes will be exercised
// (e.g. test doubles that only hit /health); a nil pool is permitted in
// tests that exit before the DB round-trip (the router never dereferences
// the pool directly — it is captured by the handlers that need it).
func NewRouter(v auth.Verifier, pool *pgxpool.Pool) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /health", HealthHandler())

	if v != nil {
		authed := AuthMiddleware(v)
		runner := poolRunner{pool: pool}
		mux.Handle("POST /v1/search", authed(handleSearch(runner)))
		mux.Handle("GET /v1/manifests/{manifest_id}", authed(handleGetManifest(runner)))
		mux.Handle("GET /v1/keepers-log", authed(handleLogTail(runner)))
	}
	return mux
}

// Server is the Keep HTTP server plus its owned lifecycle. It does not own
// the pgxpool.Pool — main.go owns and closes the pool after Run returns.
type Server struct {
	httpSrv         *http.Server
	shutdownTimeout time.Duration
}

// New builds a Server bound to cfg.HTTPAddr using the default Keep router.
// The pool is captured by the scoped read handlers; the verifier is
// captured by AuthMiddleware. Both are required by M2.7.b+c endpoints.
//
// For backward compatibility with the earlier M2.7.a call-site shape
// (where no verifier was threaded through), callers that need only
// /health can pass a nil verifier — the router will then skip the /v1/*
// wiring. Production main.go always supplies both.
func New(cfg config.Config, pool *pgxpool.Pool, v auth.Verifier) *Server {
	return NewWithHandler(cfg, pool, NewRouter(v, pool))
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

// Run serves HTTP requests until ctx is canceled, then calls Shutdown with
// the configured timeout. It returns nil on a clean shutdown and an error
// otherwise. http.ErrServerClosed is treated as the success sentinel.
func (s *Server) Run(ctx context.Context) error {
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
		return err
	case <-ctx.Done():
		// Derive the shutdown deadline from a detached copy of ctx so the
		// already-canceled parent does not abort Shutdown immediately while
		// still letting contextcheck see an inherited lineage.
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
