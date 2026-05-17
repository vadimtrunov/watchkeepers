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
	"sync/atomic"
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
// methods (`Open`, `Close`, `IncTokens`, `AppendMessage`,
// `BindSlackChannel`) take the write lock for the duration of the
// call — `IncTokens`' read-modify-write runs under a single
// write-lock acquisition so concurrent increments compose correctly
// (mirrors the M9.4.b `MarkDecided` discipline).
//
// Per-conversation reply notifications use a [sync.Cond] keyed off the
// conversation id; the cond signals every [WaitForReply] waiter on a
// matching reply append. Mirrors the M9.4.b "single cond-var per
// proposal" discipline.
type MemoryRepository struct {
	mu       sync.RWMutex
	rows     map[uuid.UUID]Conversation
	messages map[uuid.UUID][]Message // by conversation id, append-ordered
	replyCV  *sync.Cond              // signals on every reply append
	now      func() time.Time
	newID    func() uuid.UUID
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
	r := &MemoryRepository{
		rows:     make(map[uuid.UUID]Conversation),
		messages: make(map[uuid.UUID][]Message),
		now:      now,
		newID:    newID,
	}
	// The cond-var shares the same mutex as the rows / messages maps
	// so a waiter that observes "no matching reply yet" and a writer
	// that appends a new reply serialise correctly: the writer holds
	// the write lock while it appends + signals, and the waiter's
	// `cv.Wait` releases the lock during the block. Mirrors the
	// stdlib `sync.Cond` discipline.
	r.replyCV = sync.NewCond(&r.mu)
	return r
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

// BindSlackChannel implements [Repository.BindSlackChannel]. The
// validator chain (ctx → trimmed id non-empty) runs BEFORE the mutex
// acquire so a malformed input aborts without blocking other
// goroutines on the write-lock. The full read-modify-write runs under
// the single write-lock so a concurrent duplicate Bind on the same id
// either observes the prior bind (and surfaces
// [ErrSlackChannelAlreadyBound]) or wins the transition itself.
func (r *MemoryRepository) BindSlackChannel(ctx context.Context, id uuid.UUID, slackChannelID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(slackChannelID) == "" {
		return ErrEmptySlackChannelID
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	row, ok := r.rows[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrConversationNotFound, id)
	}
	if row.Status != StatusOpen {
		return fmt.Errorf("%w: %s", ErrAlreadyArchived, id)
	}
	if row.SlackChannelID != "" {
		return fmt.Errorf("%w: %s", ErrSlackChannelAlreadyBound, id)
	}

	row.SlackChannelID = slackChannelID
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

// AppendMessage implements [Repository.AppendMessage]. The validator
// chain runs BEFORE the mutex acquire so a malformed input aborts
// without blocking other goroutines on the write-lock. The append +
// signal happen under a single write-lock so a concurrent
// [WaitForReply] that observes no matching reply at scan time will
// either see the new row on its next iteration or be re-woken by the
// cond-var Broadcast.
func (r *MemoryRepository) AppendMessage(ctx context.Context, params AppendMessageParams) (Message, error) {
	if err := ctx.Err(); err != nil {
		return Message{}, err
	}
	if params.ConversationID == uuid.Nil {
		return Message{}, ErrEmptyConversationID
	}
	if params.OrganizationID == uuid.Nil {
		return Message{}, ErrEmptyOrganization
	}
	if strings.TrimSpace(params.SenderWatchkeeperID) == "" {
		return Message{}, ErrEmptySenderWatchkeeperID
	}
	if len(params.Body) == 0 {
		return Message{}, ErrEmptyMessageBody
	}
	if err := params.Direction.Validate(); err != nil {
		return Message{}, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	conv, ok := r.rows[params.ConversationID]
	if !ok {
		return Message{}, fmt.Errorf("%w: %s", ErrConversationNotFound, params.ConversationID)
	}
	if conv.Status != StatusOpen {
		return Message{}, fmt.Errorf("%w: %s", ErrAlreadyArchived, params.ConversationID)
	}

	// Defensive deep-copy of the caller-supplied body so a subsequent
	// caller-side mutation does not bleed into the held row. Mirrors
	// the participants defensive-copy discipline in [Open].
	body := make([]byte, len(params.Body))
	copy(body, params.Body)

	msg := Message{
		ID:                  r.newID(),
		ConversationID:      params.ConversationID,
		OrganizationID:      params.OrganizationID,
		SenderWatchkeeperID: params.SenderWatchkeeperID,
		Body:                body,
		Direction:           params.Direction,
		CreatedAt:           r.now().UTC(),
	}
	r.messages[params.ConversationID] = append(r.messages[params.ConversationID], msg)

	// Broadcast on every append so any waiter blocked on its cond-var
	// re-checks the message slice for a matching reply. Broadcasting
	// (rather than Signal) handles the multi-waiter case correctly —
	// a future [WaitForReply] caller racing against a slower one is
	// not silently dropped. Mirrors the M9.4.b cond-var discipline.
	if msg.Direction == MessageDirectionReply {
		r.replyCV.Broadcast()
	}

	return cloneMessage(msg), nil
}

// WaitForReply implements [Repository.WaitForReply]. The synchronous
// scan covers the "reply already present" case (a `peer.Reply` raced
// the `peer.Ask` and won); the cond-var block covers the "still
// waiting" case. ctx.Done() and timeout are handled by a per-waiter
// helper goroutine that broadcasts on the same cond-var so the waiter
// wakes up promptly.
func (r *MemoryRepository) WaitForReply(ctx context.Context, conversationID uuid.UUID, since time.Time, timeout time.Duration) (Message, error) {
	if err := ctx.Err(); err != nil {
		return Message{}, err
	}
	if conversationID == uuid.Nil {
		return Message{}, ErrEmptyConversationID
	}
	if timeout <= 0 {
		return Message{}, fmt.Errorf("%w: %s", ErrInvalidWaitTimeout, timeout)
	}

	// The waker goroutine broadcasts on the cond-var once ctx is
	// cancelled OR the per-call timer fires, then sets `timedOut` so
	// the main goroutine can disambiguate "timeout expiry" from
	// "spurious wakeup / reply arrived". The main goroutine signals
	// back via `done` when the wait completes successfully so the
	// waker never fires after the function returns. Broadcasting
	// (rather than Signal) handles the multi-waiter case correctly.
	// Driving expiry through `time.NewTimer` (rather than comparing
	// `r.now()` against a deadline) lets the in-memory adapter run
	// under a fixed-clock test harness — the per-call timer always
	// uses the real wall-clock, decoupled from the repository's
	// stamping clock. Mirrors the M9.4.b "context-aware cond-var
	// wakeup" discipline.
	var (
		timedOut atomic.Bool
		done     = make(chan struct{})
	)
	defer close(done)
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	go func() {
		select {
		case <-done:
			return
		case <-timer.C:
			timedOut.Store(true)
		case <-ctx.Done():
		}
		r.mu.Lock()
		r.replyCV.Broadcast()
		r.mu.Unlock()
	}()

	r.mu.Lock()
	defer r.mu.Unlock()

	for {
		msg, found, err := r.waitForReplyOnce(ctx, conversationID, since, &timedOut)
		if err != nil {
			return Message{}, err
		}
		if found {
			return msg, nil
		}
	}
}

// waitForReplyOnce runs one scan + cond-var park cycle for
// [MemoryRepository.WaitForReply]. It MUST be called with r.mu held.
// Returns (msg, true, nil) when a matching reply is observed,
// (zero, false, err) on terminal ctx / timeout, or
// (zero, false, nil) on a spurious wakeup (caller loops back).
func (r *MemoryRepository) waitForReplyOnce(ctx context.Context, conversationID uuid.UUID, since time.Time, timedOut *atomic.Bool) (Message, bool, error) {
	// Synchronous scan: a reply already present satisfies the wait
	// immediately without blocking. The scan walks the append-
	// ordered slice so the FIRST matching reply wins — a future
	// reply appended after this one is not returned by THIS Wait
	// (its `since` cursor moves forward on the caller side).
	if msgs, ok := r.messages[conversationID]; ok {
		for _, m := range msgs {
			if m.Direction != MessageDirectionReply {
				continue
			}
			if !m.CreatedAt.After(since) {
				continue
			}
			return cloneMessage(m), true, nil
		}
	}

	// Re-check edges BEFORE blocking so a pre-cancelled ctx or an
	// already-expired timer does not pin a waiter on the cond-var.
	if err := r.checkWaitSentinels(ctx, conversationID, timedOut); err != nil {
		return Message{}, false, err
	}

	// Park on the cond-var. The waker goroutine broadcasts on ctx
	// cancellation OR timer expiry; an AppendMessage of a reply on
	// the same conversation also broadcasts. We re-scan + re-check
	// on every wakeup so a spurious wake is harmless.
	r.replyCV.Wait()

	// Re-check sentinels post-wakeup before looping back to scan.
	if err := r.checkWaitSentinels(ctx, conversationID, timedOut); err != nil {
		return Message{}, false, err
	}
	return Message{}, false, nil
}

// checkWaitSentinels reports a terminal error if ctx is cancelled or
// the per-call timeout has fired. Returns nil to mean "keep waiting".
func (r *MemoryRepository) checkWaitSentinels(ctx context.Context, conversationID uuid.UUID, timedOut *atomic.Bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if timedOut.Load() {
		return fmt.Errorf("%w: %s", ErrWaitForReplyTimeout, conversationID)
	}
	return nil
}
