package server

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/vadimtrunov/watchkeepers/core/internal/keep/auth"
)

// FakeScopedRunner is a scopedRunner implementation for tests that lets
// the caller supply a ctx/tx-consuming fn and observe the claim passed
// in. It never opens a transaction; callers pass nil for tx unless they
// also bring a full fake pgx.Tx. Exported only to the _test files in
// this package via the export_test.go convention.
type FakeScopedRunner struct {
	LastClaim  auth.Claim
	FnInvoked  bool
	FnReturns  error
	Tx         pgx.Tx
	BeforeExec func(ctx context.Context, claim auth.Claim)
}

// WithScope implements scopedRunner for tests. It records the claim then
// invokes fn with the configured (possibly nil) tx. Tests can set
// FnReturns to simulate a DB-level error without touching a real pool.
func (f *FakeScopedRunner) WithScope(ctx context.Context, claim auth.Claim, fn func(context.Context, pgx.Tx) error) error {
	f.LastClaim = claim
	if f.BeforeExec != nil {
		f.BeforeExec(ctx, claim)
	}
	f.FnInvoked = true
	if f.FnReturns != nil {
		return f.FnReturns
	}
	return fn(ctx, f.Tx)
}

// NewRouterWithRunner builds a router whose /v1/* handlers run against
// the supplied scopedRunner instead of the real pool. Used only by
// handler unit tests; production code must keep calling NewRouter.
func NewRouterWithRunner(v auth.Verifier, runner *FakeScopedRunner) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /health", HealthHandler())

	if v != nil {
		authed := AuthMiddleware(v)
		mux.Handle("POST /v1/search", authed(handleSearch(runner)))
		mux.Handle("GET /v1/manifests/{manifest_id}", authed(handleGetManifest(runner)))
		mux.Handle("GET /v1/keepers-log", authed(handleLogTail(runner)))
		mux.Handle("POST /v1/knowledge-chunks", authed(handleStore(runner)))
		mux.Handle("POST /v1/keepers-log", authed(handleLogAppend(runner)))
		mux.Handle("PUT /v1/manifests/{manifest_id}/versions", authed(handlePutManifestVersion(runner)))
	}
	return mux
}
