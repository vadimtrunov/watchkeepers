package lifecycle

import (
	"context"
	"fmt"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
)

// LocalKeepClient is the minimal subset of the keepclient surface that
// [Manager] consumes. Defined as an interface in this package so tests
// can substitute a hand-rolled fake without standing up an HTTP server,
// and so production code never has to import the concrete
// `*keepclient.Client` type at all — only the four methods this package
// actually calls. `*keepclient.Client` satisfies the interface as-is;
// the compile-time assertion lives in `lifecycle_test.go`, mirroring
// the notebook+archivestore one-way import-cycle-break pattern
// documented in `docs/LESSONS.md` (M2b.6).
type LocalKeepClient interface {
	InsertWatchkeeper(ctx context.Context, req keepclient.InsertWatchkeeperRequest) (*keepclient.InsertWatchkeeperResponse, error)
	UpdateWatchkeeperStatus(ctx context.Context, id, status string) error
	GetWatchkeeper(ctx context.Context, id string) (*keepclient.Watchkeeper, error)
	ListWatchkeepers(ctx context.Context, req keepclient.ListWatchkeepersRequest) (*keepclient.ListWatchkeepersResponse, error)
}

// Logger is the audit-emit sink wired in via [WithLogger]. The shape is
// the same single-method subset of `*keepclient.Client` that
// `notebook.Logger` exposes — defined locally so this package does not
// take a hard dependency on notebook, and so callers can substitute a
// retry/backoff wrapper without losing type compatibility. The Logger
// is reserved for a future audit-emit hook ("watchkeeper_spawned" /
// "watchkeeper_retired" — see README "Future extensions"); the M3.2.b
// methods do not call it yet.
type Logger interface {
	LogAppend(ctx context.Context, req keepclient.LogAppendRequest) (*keepclient.LogAppendResponse, error)
}

// Manager is the lifecycle bookkeeping facade. Construct via [New];
// the zero value is not usable. Every state-changing method is a
// passthrough (List, Retire), a row read with projection (Health), or
// a two-step Insert+Update sequence (Spawn) — the receiver itself
// holds no shared mutable state, so concurrent calls to Manager
// methods are independent at the lifecycle layer (the underlying
// LocalKeepClient governs request-level concurrency).
type Manager struct {
	keepClient LocalKeepClient
	logger     Logger
	clock      func() time.Time
}

// Option configures a [Manager] at construction time. Pass options to
// [New]; later options override earlier ones for the same field.
type Option func(*Manager)

// WithLogger wires an audit-emit sink onto the returned [*Manager].
// Reserved for a future "watchkeeper_spawned" / "watchkeeper_retired"
// audit hook; M3.2.b methods do not yet call it. A nil logger argument
// is a no-op so callers can always pass through whatever they have.
//
// `*keepclient.Client` satisfies [Logger] structurally; tests may
// substitute a hand-rolled fake.
func WithLogger(l Logger) Option {
	return func(m *Manager) {
		if l != nil {
			m.logger = l
		}
	}
}

// WithClock overrides the wall-clock source used by future audit-emit
// timestamps. Defaults to [time.Now]. Tests use it to produce
// deterministic timestamps; production callers should rarely need it.
// A nil clock argument is a no-op so callers can always pass through
// whatever they have.
func WithClock(c func() time.Time) Option {
	return func(m *Manager) {
		if c != nil {
			m.clock = c
		}
	}
}

// New constructs a [Manager] backed by the supplied [LocalKeepClient].
// `client` is required; passing a nil client is a programmer error and
// panics with a clear message — matches `keepclient.WithBaseURL`'s
// panic discipline (a Manager with no client cannot do anything
// useful, and silently no-oping every call would mask the bug).
//
// The defaults `clock = time.Now` and `logger = nil` (audit emit
// disabled) are applied first; supplied options override them.
func New(client LocalKeepClient, opts ...Option) *Manager {
	if client == nil {
		panic("lifecycle: New: client must not be nil")
	}
	m := &Manager{
		keepClient: client,
		clock:      time.Now,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// SpawnParams is the input shape for [Manager.Spawn]. Mirrors the
// required + optional fields of [keepclient.InsertWatchkeeperRequest],
// renamed for readability at the lifecycle layer.
type SpawnParams struct {
	// ManifestID is the parent manifest UUID. Required; empty values
	// return [ErrInvalidParams] synchronously.
	ManifestID string
	// LeadHumanID is the lead-operator human UUID. Required; empty
	// values return [ErrInvalidParams] synchronously.
	LeadHumanID string
	// ActiveManifestVersionID is the optional pinned manifest version
	// UUID. Empty means "let the server store SQL NULL" — passed
	// through as `omitempty` by keepclient.
	ActiveManifestVersionID string
}

// Status is the slim health view returned by [Manager.Health]. It
// projects [keepclient.Watchkeeper] minus the
// `ActiveManifestVersionID` field (callers needing the full row can
// reach for the keepclient directly). The pointer-typed timestamps
// preserve the SQL-NULL distinction from the underlying row:
// `SpawnedAt` is nil while pending, `RetiredAt` is nil before retire.
type Status struct {
	// ID is the watchkeeper row UUID.
	ID string
	// ManifestID is the parent manifest UUID.
	ManifestID string
	// LeadHumanID is the lead-operator human UUID.
	LeadHumanID string
	// Status is one of "pending" | "active" | "retired".
	Status string
	// SpawnedAt is set on the pending→active transition; nil while
	// pending.
	SpawnedAt *time.Time
	// RetiredAt is set on the active→retired transition; nil before
	// retire.
	RetiredAt *time.Time
	// CreatedAt is the row's created_at timestamp.
	CreatedAt time.Time
}

// ListFilter is the input shape for [Manager.List]. Mirrors the
// relevant subset of [keepclient.ListWatchkeepersRequest] (`Status`,
// `Limit`); the keepclient response envelope is returned to the
// caller as-is so seek-pagination follow-ups (NextCursor) flow
// through without an additional translation.
type ListFilter struct {
	// Status filters by lifecycle state when non-empty. Allowed values
	// are "pending" | "active" | "retired"; any other non-empty value
	// is forwarded to keepclient which rejects it with
	// [keepclient.ErrInvalidRequest].
	Status string
	// Limit caps the number of rows returned. 0 means "let the server
	// apply its default". Negative or > 200 returns [ErrInvalidParams]
	// synchronously without a network round-trip — kept in lockstep
	// with keepclient's matching bound check.
	Limit int
}

// maxListLimit mirrors [keepclient]'s `maxWatchkeeperListLimit` so
// the lifecycle bound check fails fast on the same threshold the
// keepclient layer enforces. Kept in lockstep documentation-wise:
// raising this without raising keepclient's bound (or vice versa)
// would split the failure surface.
const maxListLimit = 200

// Spawn registers a new logical watchkeeper and marks it active.
// Performs two keepclient round-trips in sequence:
//
//  1. InsertWatchkeeper(ctx, {ManifestID, LeadHumanID,
//     ActiveManifestVersionID}) creates a row with status='pending'.
//  2. UpdateWatchkeeperStatus(ctx, id, "active") transitions it to
//     active.
//
// # Partial-failure shape
//
// Empty ManifestID or LeadHumanID returns ("", ErrInvalidParams)
// synchronously without any network round-trip. On Insert failure
// returns ("", fmt.Errorf("lifecycle: spawn: insert: %w", err)) — no
// row exists yet, retry the whole call.  On Update failure returns
// (id, fmt.Errorf("lifecycle: spawn: activate: %w", err)) — the row IS
// in the database in `pending` state, so the caller can retry just
// the Update against the populated id (mirrors M2b.4 / M2b.7
// partial-failure shape).
func (m *Manager) Spawn(ctx context.Context, params SpawnParams) (string, error) {
	if params.ManifestID == "" || params.LeadHumanID == "" {
		return "", ErrInvalidParams
	}

	resp, err := m.keepClient.InsertWatchkeeper(ctx, keepclient.InsertWatchkeeperRequest{
		ManifestID:              params.ManifestID,
		LeadHumanID:             params.LeadHumanID,
		ActiveManifestVersionID: params.ActiveManifestVersionID,
	})
	if err != nil {
		return "", fmt.Errorf("lifecycle: spawn: insert: %w", err)
	}

	if err := m.keepClient.UpdateWatchkeeperStatus(ctx, resp.ID, "active"); err != nil {
		return resp.ID, fmt.Errorf("lifecycle: spawn: activate: %w", err)
	}
	return resp.ID, nil
}

// Retire transitions a watchkeeper row to status='retired'. Empty id
// returns [ErrInvalidParams] synchronously without a network round-
// trip. keepclient errors are wrapped as
// `fmt.Errorf("lifecycle: retire: %w", err)`; callers can `errors.Is`
// against [keepclient.ErrInvalidStatusTransition] etc. through the
// wrap chain.
func (m *Manager) Retire(ctx context.Context, id string) error {
	if id == "" {
		return ErrInvalidParams
	}
	if err := m.keepClient.UpdateWatchkeeperStatus(ctx, id, "retired"); err != nil {
		return fmt.Errorf("lifecycle: retire: %w", err)
	}
	return nil
}

// Health reads the watchkeeper row and projects it into a [Status].
// Empty id returns [ErrInvalidParams] synchronously. keepclient
// errors are wrapped as `fmt.Errorf("lifecycle: health: %w", err)`;
// callers can `errors.Is(err, keepclient.ErrNotFound)` through the
// wrap chain to distinguish a missing watchkeeper from a transport
// error.
//
// `ActiveManifestVersionID` is intentionally dropped from the
// projection — it is a manifest concern, not a health concern.
// Callers that need the full row can call
// [keepclient.Client.GetWatchkeeper] directly.
func (m *Manager) Health(ctx context.Context, id string) (*Status, error) {
	if id == "" {
		return nil, ErrInvalidParams
	}
	wk, err := m.keepClient.GetWatchkeeper(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: health: %w", err)
	}
	return &Status{
		ID:          wk.ID,
		ManifestID:  wk.ManifestID,
		LeadHumanID: wk.LeadHumanID,
		Status:      wk.Status,
		SpawnedAt:   wk.SpawnedAt,
		RetiredAt:   wk.RetiredAt,
		CreatedAt:   wk.CreatedAt,
	}, nil
}

// List passes through to [keepclient.Client.ListWatchkeepers] after a
// pre-validation bound check on `filter.Limit`. The bound check is
// kept in lockstep with keepclient's matching check so callers see
// [ErrInvalidParams] (a lifecycle sentinel) for the same out-of-range
// values keepclient would reject — without burning a network round-
// trip on the obvious bug.
//
// keepclient errors are wrapped as
// `fmt.Errorf("lifecycle: list: %w", err)`; callers can `errors.Is`
// against [keepclient.ErrInvalidRequest] etc. through the wrap chain.
//
// The result is a slice of pointers into a fresh backing array so
// callers can hold individual rows by reference (e.g. for fan-out
// dispatching) without re-copying the underlying struct.
func (m *Manager) List(ctx context.Context, filter ListFilter) ([]*keepclient.Watchkeeper, error) {
	if filter.Limit < 0 || filter.Limit > maxListLimit {
		return nil, ErrInvalidParams
	}
	resp, err := m.keepClient.ListWatchkeepers(ctx, keepclient.ListWatchkeepersRequest{
		Status: filter.Status,
		Limit:  filter.Limit,
	})
	if err != nil {
		return nil, fmt.Errorf("lifecycle: list: %w", err)
	}
	out := make([]*keepclient.Watchkeeper, len(resp.Items))
	for i := range resp.Items {
		out[i] = &resp.Items[i]
	}
	return out, nil
}
