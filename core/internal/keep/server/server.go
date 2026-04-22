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

// NewRouter returns the HTTP router with every Keep route wired. For M2.7.a
// this is only /health; future milestones append to the mux here.
func NewRouter() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /health", HealthHandler())
	return mux
}

// Server is the Keep HTTP server plus its owned lifecycle. It does not own
// the pgxpool.Pool — main.go owns and closes the pool after Run returns.
type Server struct {
	httpSrv         *http.Server
	shutdownTimeout time.Duration
}

// New builds a Server bound to cfg.HTTPAddr using the default Keep router. It
// accepts the *pgxpool.Pool so future routes can capture it via closure, but
// M2.7.a has no DB-backed endpoints yet so the pool parameter is currently
// unused at the handler layer (boot-time Ping happens in main).
func New(cfg config.Config, pool *pgxpool.Pool) *Server {
	return NewWithHandler(cfg, pool, NewRouter())
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
