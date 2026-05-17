package server

import (
	"context"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vadimtrunov/watchkeepers/core/internal/keep/auth"
	"github.com/vadimtrunov/watchkeepers/core/internal/keep/publish"
)

// FakeTxFns lets watchkeeper handler tests stage a multi-step pgx.Tx
// (SELECT … FOR UPDATE then UPDATE / Exec, or a Query that walks rows).
// Any nil field uses the underlying fakeTx default behaviour.
type FakeTxFns struct {
	QueryRow func(ctx context.Context, sql string, args ...any) pgx.Row
	Query    func(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec     func(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// NewFakeTx builds a multi-method pgx.Tx fake from caller-supplied fns. Used
// by handlers_watchkeeper_test.go to drive transition + list paths without
// a real database. Exported only via this _test seam.
func NewFakeTx(fns FakeTxFns) pgx.Tx {
	return newFakeTxWithFns(fns.QueryRow, fns.Query, fns.Exec)
}

// NewFakeRow builds a pgx.Row whose Scan is delegated to the supplied closure.
// Lets tests stage multi-column rows for the SELECT … FOR UPDATE step in
// handleUpdateWatchkeeperStatus.
func NewFakeRow(scan func(dest ...any) error) pgx.Row {
	return fakeRow{scanFn: scan}
}

// NewFakeRowErr builds a pgx.Row whose Scan returns the supplied error
// verbatim. Used by tests that exercise the pgx.ErrNoRows path of the
// Update / Get handlers.
func NewFakeRowErr(err error) pgx.Row {
	return fakeRow{err: err}
}

// NewFakeRows builds a pgx.Rows whose Next walks `scans` once each and whose
// Scan delegates to the matching closure. The optional rowsErr surfaces from
// Rows.Err() after iteration completes.
func NewFakeRows(scans []func(dest ...any) error, rowsErr error) pgx.Rows {
	return &fakeRows{scans: scans, rowsErr: rowsErr}
}

// SubscribeHandlerForTest returns the bare /v1/subscribe handler (not
// wrapped in AuthMiddleware) so _test files can exercise the handler's
// own defense-in-depth branches against a context carrying a hand-made
// Claim. Exported only through this export_test.go seam.
func SubscribeHandlerForTest(reg *publish.Registry, heartbeat time.Duration) http.Handler {
	return handleSubscribe(reg, heartbeat)
}

// ContextWithClaimForTest attaches a Claim to ctx using the same key the
// middleware uses. Tests that bypass AuthMiddleware (see
// TestSubscribe_BadScope) need this to hand the handler a Claim without
// minting a token.
func ContextWithClaimForTest(ctx context.Context, c auth.Claim) context.Context {
	return context.WithValue(ctx, claimKey, c)
}

// FakeScopedRunner is a scopedRunner implementation for tests that lets
// the caller supply a ctx/tx-consuming fn and observe the claim passed
// in. It never opens a transaction; callers pass nil for tx unless they
// also bring a full fake pgx.Tx. Exported only to the _test files in
// this package via the export_test.go convention.
//
// FakeID, when non-empty, causes WithScope to supply a fakeTx whose
// QueryRow(...).Scan writes FakeID into the first *string destination.
// This allows happy-path 201+id tests without a real database. FakeID
// is only consulted when FnReturns == nil; a non-nil FnReturns always
// takes priority (sentinel / error-translation tests are unaffected).
type FakeScopedRunner struct {
	LastClaim  auth.Claim
	FnInvoked  bool
	FnReturns  error
	FakeID     string
	Tx         pgx.Tx
	BeforeExec func(ctx context.Context, claim auth.Claim)
}

// WithScope implements scopedRunner for tests. It records the claim then
// invokes fn with the configured (possibly nil) tx. Tests can set
// FnReturns to simulate a DB-level error without touching a real pool.
// When FnReturns is nil and FakeID is non-empty, a fakeTx is supplied
// so the handler's QueryRow(...).Scan path writes FakeID as the row id.
func (f *FakeScopedRunner) WithScope(ctx context.Context, claim auth.Claim, fn func(context.Context, pgx.Tx) error) error {
	f.LastClaim = claim
	if f.BeforeExec != nil {
		f.BeforeExec(ctx, claim)
	}
	f.FnInvoked = true
	if f.FnReturns != nil {
		return f.FnReturns
	}
	tx := f.Tx
	if tx == nil && f.FakeID != "" {
		tx = newFakeTx(f.FakeID)
	}
	return fn(ctx, tx)
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
		mux.Handle("GET /v1/cost-rollups", authed(handleCostRollups(runner)))
		mux.Handle("POST /v1/knowledge-chunks", authed(handleStore(runner)))
		mux.Handle("POST /v1/keepers-log", authed(handleLogAppend(runner)))
		mux.Handle("PUT /v1/manifests/{manifest_id}/versions", authed(handlePutManifestVersion(runner)))
		mux.Handle("POST /v1/watchkeepers", authed(handleInsertWatchkeeper(runner)))
		mux.Handle("PATCH /v1/watchkeepers/{id}/status", authed(handleUpdateWatchkeeperStatus(runner)))
		mux.Handle("PATCH /v1/watchkeepers/{id}/lead", authed(handleSetWatchkeeperLead(runner)))
		mux.Handle("GET /v1/watchkeepers/{id}", authed(handleGetWatchkeeper(runner)))
		mux.Handle("GET /v1/watchkeepers", authed(handleListWatchkeepers(runner)))
		mux.Handle("GET /v1/peers", authed(handleListPeers(runner)))
		mux.Handle("POST /v1/humans", authed(handleInsertHuman(runner)))
		mux.Handle("GET /v1/humans/by-slack/{slack_user_id}", authed(handleLookupHumanBySlackID(runner)))
	}
	return mux
}
