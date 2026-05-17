// memory.go ships the in-memory [Repository] implementation used by
// the M1.1.a unit tests AND (intentionally) by dev / smoke runs that
// don't want a Postgres-backed adapter wired up. The pattern mirrors
// `saga.MemorySpawnSagaDAO`: the test surface and the dev-loop wiring
// share one implementation.
//
// resolution order: Open → ctx.Err → param validation → mutex acquire
// → mint id → defensive deep-copy of Participants → rows[id] = clone
// → mutex release → return clone. Get / List / Close / IncTokens follow
// the contract documented on [Repository].
//
// audit discipline: the store never imports `keeperslog` and never
// calls `.Append(`. The audit log entries for the K2K lifecycle
// (`k2k_conversation_opened` / `k2k_conversation_closed` / etc.) live
// in the M1.4 audit subscriber that observes the lifecycle events; the
// store itself is a transient state surface, not an audit sink.
//
// PII discipline: the `Subject` field is operator-supplied free-text;
// the M1.4 audit emitter carries the redaction contract. The store's
// only PII responsibility at this layer is defensive deep-copy of the
// `Participants` slice so caller-side mutation cannot bleed.
package k2k

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// MemoryRepository is a process-local [Repository] implementation
// backed by a `map[uuid.UUID]Conversation`. Constructed via
// [NewMemoryRepository]; the zero value is NOT usable — callers must
// always go through the constructor so the internal map is non-nil.
//
// `now` is overridable so unit tests can drive deterministic
// `OpenedAt` / `ClosedAt` values without a fixture clock threaded
// through the Open / Close call sites. `newID` is overridable for the
// same reason so the minted id is predictable in tests.
//
// Concurrency: read methods (`Get`, `List`) take an RLock so
// concurrent reads across distinct ids never block each other. Write
// methods (`Open`, `Close`, `IncTokens`) take the write lock for the
// duration of the call — `IncTokens`' read-modify-write runs under a
// single write-lock acquisition so concurrent increments compose
// correctly (mirrors the M9.4.b `MarkDecided` discipline).
type MemoryRepository struct {
	mu    sync.RWMutex
	rows  map[uuid.UUID]Conversation
	now   func() time.Time
	newID func() uuid.UUID
}

// Compile-time assertion: [*MemoryRepository] satisfies [Repository].
// Pins the integration shape so a future change to the interface
// surface fails the build here. Mirrors the
// `var _ saga.SpawnSagaDAO = (*saga.MemorySpawnSagaDAO)(nil)`
// discipline.
var _ Repository = (*MemoryRepository)(nil)

// NewMemoryRepository returns an empty in-memory store. The optional
// `now` argument overrides the wall-clock used to stamp `OpenedAt` /
// `ClosedAt`; pass nil to use [time.Now]. The optional `newID` argument
// overrides the id minter; pass nil to use [uuid.New].
func NewMemoryRepository(now func() time.Time, newID func() uuid.UUID) *MemoryRepository {
	if now == nil {
		now = time.Now
	}
	if newID == nil {
		newID = uuid.New
	}
	return &MemoryRepository{
		rows:  make(map[uuid.UUID]Conversation),
		now:   now,
		newID: newID,
	}
}

// Open implements [Repository.Open]. Validation order matches the
// interface godoc: ctx → org → subject → participants → budget. Each
// validator runs BEFORE the mutex acquire so a pre-cancelled ctx or a
// malformed input aborts without blocking other goroutines on the
// write-lock.
func (r *MemoryRepository) Open(ctx context.Context, params OpenParams) (Conversation, error) {
	if err := ctx.Err(); err != nil {
		return Conversation{}, err
	}
	if params.OrganizationID == uuid.Nil {
		return Conversation{}, ErrEmptyOrganization
	}
	if strings.TrimSpace(params.Subject) == "" {
		return Conversation{}, ErrEmptySubject
	}
	if len(params.Participants) == 0 {
		return Conversation{}, ErrEmptyParticipants
	}
	for _, p := range params.Participants {
		if strings.TrimSpace(p) == "" {
			return Conversation{}, ErrEmptyParticipants
		}
	}
	if params.TokenBudget < 0 {
		return Conversation{}, ErrInvalidTokenBudget
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	id := r.newID()
	// Defensive deep-copy of the caller-supplied slice so a subsequent
	// caller-side mutation does not bleed into the held row. Mirrors
	// `cloneProposal` from `core/pkg/approval/proposalstore.go`.
	participants := make([]string, len(params.Participants))
	copy(participants, params.Participants)

	row := Conversation{
		ID:             id,
		OrganizationID: params.OrganizationID,
		Participants:   participants,
		Subject:        params.Subject,
		Status:         StatusOpen,
		TokenBudget:    params.TokenBudget,
		TokensUsed:     0,
		OpenedAt:       r.now().UTC(),
		CorrelationID:  params.CorrelationID,
	}
	r.rows[id] = row

	// Return a value-copy with its own defensive deep-copy so the
	// caller cannot mutate the held row by mutating the returned slice.
	return cloneConversation(row), nil
}

// Get implements [Repository.Get]. The ctx-check before the RLock
// matters under contention: if many callers race against a Close
// holding the write-lock, a pre-cancelled ctx aborts BEFORE waiting on
// the lock. Mirrors `InMemoryProposalStore.Lookup`'s discipline.
func (r *MemoryRepository) Get(ctx context.Context, id uuid.UUID) (Conversation, error) {
	if err := ctx.Err(); err != nil {
		return Conversation{}, err
	}
	r.mu.RLock()
	row, ok := r.rows[id]
	r.mu.RUnlock()
	if !ok {
		return Conversation{}, fmt.Errorf("%w: %s", ErrConversationNotFound, id)
	}
	return cloneConversation(row), nil
}

// List implements [Repository.List]. Per the interface godoc, the
// in-memory adapter requires a non-zero `filter.OrganizationID` —
// without it the store would leak cross-tenant rows that the
// production Postgres adapter would refuse via RLS. Returning an
// empty slice on the zero-org filter is safer than panicking but
// would silently mask a misconfiguration; surface a sentinel error
// instead so the caller fixes the wiring rather than silently seeing
// zero results.
func (r *MemoryRepository) List(ctx context.Context, filter ListFilter) ([]Conversation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if filter.OrganizationID == uuid.Nil {
		return nil, ErrEmptyOrganization
	}
	if filter.Status != "" {
		if err := filter.Status.Validate(); err != nil {
			return nil, err
		}
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]Conversation, 0, len(r.rows))
	for _, row := range r.rows {
		if row.OrganizationID != filter.OrganizationID {
			continue
		}
		if filter.Status != "" && row.Status != filter.Status {
			continue
		}
		out = append(out, cloneConversation(row))
	}
	return out, nil
}

// Close implements [Repository.Close]. The full critical section runs
// under the single write-lock so a concurrent duplicate Close on the
// same id either observes the prior archive (and surfaces
// [ErrAlreadyArchived]) or wins the transition itself — never both.
func (r *MemoryRepository) Close(ctx context.Context, id uuid.UUID, reason string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	row, ok := r.rows[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrConversationNotFound, id)
	}
	if row.Status == StatusArchived {
		return fmt.Errorf("%w: %s", ErrAlreadyArchived, id)
	}

	row.Status = StatusArchived
	row.ClosedAt = r.now().UTC()
	row.CloseReason = reason
	r.rows[id] = row
	return nil
}

// IncTokens implements [Repository.IncTokens]. The whole
// read-modify-write runs under the single write-lock so concurrent
// IncTokens on the same id compose correctly — under a naive
// RLock-read + Lock-write split, two goroutines could both observe the
// pre-increment value and both write `pre + delta` rather than
// `pre + 2*delta`. The 16-goroutine concurrency test pins this.
func (r *MemoryRepository) IncTokens(ctx context.Context, id uuid.UUID, delta int64) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if delta <= 0 {
		return 0, fmt.Errorf("%w: %d", ErrInvalidTokenDelta, delta)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	row, ok := r.rows[id]
	if !ok {
		return 0, fmt.Errorf("%w: %s", ErrConversationNotFound, id)
	}
	if row.Status != StatusOpen {
		return 0, fmt.Errorf("%w: %s", ErrAlreadyArchived, id)
	}

	row.TokensUsed += delta
	r.rows[id] = row
	return row.TokensUsed, nil
}
