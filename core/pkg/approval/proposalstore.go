// Doc-block at file head documenting the seam contract.
//
// resolution order: Store(p) → mutex acquire → defensive deep-copy →
// items[p.ID] = clone → mutex release. Lookup(ctx, id) → ctx.Err →
// mutex acquire (RLock) → items[id] resolution → defensive deep-copy →
// mutex release → return clone.
//
// audit discipline: the store never imports `keeperslog` and never
// calls `.Append(` (see source-grep AC). The audit log entry for the
// "proposal recorded into store" branch lives in the M9.7 audit
// subscriber that observes [TopicToolProposed] events; the store
// itself is a transient in-memory index, not an audit surface.
//
// PII discipline: the store holds the full [Proposal] (including
// `CodeDraft`, `Purpose`, `PlainLanguageDescription`) so the M9.4.b
// webhook receiver and the M9.4.b slack-native AI reviewer can resolve
// the full payload by id. The full record never crosses the eventbus
// boundary — see [ToolProposed]'s allowlist for that. Defensive
// deep-copy on both Store + Lookup prevents caller-side mutation from
// corrupting the held record OR the returned record.

package approval

import (
	"context"
	"fmt"
	"sync"

	"github.com/google/uuid"
)

// ProposalLookup is the seam the M9.4.b webhook receiver and the
// M9.4.b callback dispatcher consume to resolve a stored [Proposal] by
// id. Production wiring satisfies it via [*InMemoryProposalStore];
// tests substitute a hand-rolled fake (see `fakes_test.go`) so each
// dispatcher path can drive lookup-miss / lookup-error branches
// independently of the store implementation.
//
// Contract:
//
//   - Return the resolved [Proposal] on success.
//   - Return [ErrProposalNotFound] (wrapped with the missing id) when
//     no entry matches. Implementers MUST NOT pre-wrap with the
//     sentinel — the [ErrProposalNotFound] sentinel itself is the
//     wrap layer; the cause chain follows.
//   - Return ctx.Err() pass-through on a cancelled context. The
//     in-memory implementation checks ctx BEFORE acquiring the mutex
//     so cancellation is honoured even when the store is contended.
type ProposalLookup interface {
	Lookup(ctx context.Context, id uuid.UUID) (Proposal, error)
}

// ProposalRecorder is the symmetric write-side seam. Production wiring
// composes the recorder + lookup behind the same
// [*InMemoryProposalStore]; the seams are split so a test can stub the
// recorder and the lookup independently (mirrors the
// `toolregistry.Publisher` / `toolregistry.AuthSecretResolver` split
// from M9.1.a). The recorder is also the integration seam for a
// future SQL-backed store (M9.4 follow-up): a downstream impl can
// satisfy [ProposalRecorder] without committing to the in-memory shape.
type ProposalRecorder interface {
	Record(p Proposal)
}

// DecisionKind is the closed-set enum the decision-recorder seam
// consumes. Only the two terminal decisions are tracked — the
// `dry_run_requested` / `question_asked` clicks are intermediate
// interactions that can legitimately repeat (a lead may click
// `[Test in my DM]` more than once during deliberation).
type DecisionKind string

const (
	// DecisionApproved identifies a `tool_approved` decision claim
	// (git-pr webhook OR slack-native `[Approve]` click).
	DecisionApproved DecisionKind = "approved"

	// DecisionRejected identifies a `tool_rejected` decision claim
	// (slack-native `[Reject]` click).
	DecisionRejected DecisionKind = "rejected"
)

// Validate reports whether `k` is in the closed [DecisionKind] set.
// Returns [ErrInvalidDecisionKind] otherwise (including the empty
// string).
func (k DecisionKind) Validate() error {
	switch k {
	case DecisionApproved, DecisionRejected:
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrInvalidDecisionKind, string(k))
	}
}

// DecisionRecorder is the idempotency seam the M9.4.b webhook receiver
// and the callback dispatcher consume to claim the terminal
// `tool_approved` / `tool_rejected` decision exactly once per proposal.
// Production wiring satisfies it via [*InMemoryProposalStore]; tests
// substitute a hand-rolled fake.
//
// Contract:
//
//   - [MarkDecided] returns `(true, nil)` on the FIRST successful
//     claim — caller MUST proceed to publish the corresponding event
//     and treat itself as authoritative for this decision.
//   - [MarkDecided] returns `(false, nil)` on a same-kind replay
//     (idempotent success — caller MUST treat the call as a no-op
//     duplicate delivery and return without re-publishing).
//   - [MarkDecided] returns `(false, ErrDecisionConflict)` when the
//     stored decision is a DIFFERENT kind (e.g. `Approved` then
//     `Rejected`). The first decision is final; the caller surfaces
//     the conflict on the audit row.
//   - [UnmarkDecided] reverses a prior [MarkDecided] when the
//     side-effect publish failed — so a retried delivery can re-claim
//     and re-publish. Idempotent: an Unmark for a never-claimed (or
//     conflicting-kind) decision is silently OK.
type DecisionRecorder interface {
	MarkDecided(ctx context.Context, id uuid.UUID, kind DecisionKind) (firstTime bool, err error)
	UnmarkDecided(ctx context.Context, id uuid.UUID, kind DecisionKind) error
}

// InMemoryProposalStore is the production [ProposalLookup] +
// [ProposalRecorder] for M9.4.b. The map is guarded by a
// [sync.RWMutex] so concurrent Lookup callers (the webhook receiver
// AND the callback dispatcher both go through it) run in parallel
// while Record blocks them only briefly. The zero value is not usable;
// always construct via [NewInMemoryProposalStore].
//
// Memory bounds: the store has NO eviction at this layer. Production
// wiring SHOULD size the deployment for the expected number of
// in-flight proposals; an M9.4.b follow-up (or M9.7) will plug in an
// age-out policy backed by [Proposal.ProposedAt]. The M9.4.b ship
// scope deliberately defers eviction because the typical operator
// flow is a small number of in-flight proposals (≤100) that age out
// when the lead clicks Approve / Reject within minutes.
type InMemoryProposalStore struct {
	mu        sync.RWMutex
	items     map[uuid.UUID]Proposal
	decisions map[uuid.UUID]DecisionKind
}

// Compile-time assertion: the in-memory store satisfies all three seam
// interfaces. Pins the integration shape against future drift in any
// of them.
var (
	_ ProposalLookup   = (*InMemoryProposalStore)(nil)
	_ ProposalRecorder = (*InMemoryProposalStore)(nil)
	_ DecisionRecorder = (*InMemoryProposalStore)(nil)
)

// NewInMemoryProposalStore returns a fresh empty store. The returned
// pointer is safe for concurrent use across goroutines once
// constructed.
func NewInMemoryProposalStore() *InMemoryProposalStore {
	return &InMemoryProposalStore{
		items:     make(map[uuid.UUID]Proposal),
		decisions: make(map[uuid.UUID]DecisionKind),
	}
}

// MarkDecided implements [DecisionRecorder]. The full critical section
// runs under the single write-lock so a concurrent duplicate delivery
// against the same proposal id either observes the claim (and the
// caller silent-200s without re-publishing) or wins the claim itself —
// never both.
func (s *InMemoryProposalStore) MarkDecided(ctx context.Context, id uuid.UUID, kind DecisionKind) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if id == uuid.Nil {
		return false, fmt.Errorf("approval: MarkDecided: zero proposal id")
	}
	if err := kind.Validate(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if prior, ok := s.decisions[id]; ok {
		if prior == kind {
			return false, nil
		}
		return false, fmt.Errorf("%w: prior %q, attempted %q", ErrDecisionConflict, string(prior), string(kind))
	}
	s.decisions[id] = kind
	return true, nil
}

// UnmarkDecided implements [DecisionRecorder]. Reverses a prior
// [MarkDecided] when the side-effect publish failed. Same-kind
// unmark only — an attempt to unmark a different (conflicting) kind is
// silently dropped so a stale rollback from a losing concurrent caller
// cannot erase the winner's claim.
func (s *InMemoryProposalStore) UnmarkDecided(ctx context.Context, id uuid.UUID, kind DecisionKind) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if id == uuid.Nil {
		return fmt.Errorf("approval: UnmarkDecided: zero proposal id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if prior, ok := s.decisions[id]; ok && prior == kind {
		delete(s.decisions, id)
	}
	return nil
}

// Record persists `p` under `p.ID`. A zero-valued `p.ID` is a
// programmer bug and the call is silently dropped — the lookup-miss
// boundary catches a downstream attempt to resolve it. Defensive
// deep-copy keeps the stored record independent of caller-side
// mutation of `p.Input.Capabilities`.
//
// Record is idempotent in the sense that the same `p.ID` can be
// re-recorded with a new payload; the LAST writer wins. The M9.4.a
// [Proposer.Submit] mints UUIDv7 ids so duplicate-record-with-stale-
// payload is not a real scenario.
func (s *InMemoryProposalStore) Record(p Proposal) {
	if p.ID == uuid.Nil {
		return
	}
	clone := cloneProposal(p)
	s.mu.Lock()
	s.items[p.ID] = clone
	s.mu.Unlock()
}

// Lookup returns the stored [Proposal] under `id`. Resolution order:
//
//  1. ctx.Err — refuse a pre-cancelled ctx with the cause sentinel.
//  2. mutex acquire (RLock) — concurrent Lookups parallelise; only
//     Record blocks them.
//  3. map resolution — miss surfaces as [ErrProposalNotFound] wrapped
//     with the requested id for operator debugging.
//  4. defensive deep-copy — the returned record is independent of
//     caller-side mutation of the held record (and vice-versa).
//
// The ctx-check before the mutex acquire matters under contention:
// if many callers race against a Record holding the write-lock, a
// pre-cancelled ctx aborts BEFORE waiting on the lock. Mirrors
// [Proposer.Submit]'s "ctx.Err before any side effects" discipline.
func (s *InMemoryProposalStore) Lookup(ctx context.Context, id uuid.UUID) (Proposal, error) {
	if err := ctx.Err(); err != nil {
		return Proposal{}, err
	}
	s.mu.RLock()
	p, ok := s.items[id]
	s.mu.RUnlock()
	if !ok {
		return Proposal{}, fmt.Errorf("%w: %s", ErrProposalNotFound, id)
	}
	return cloneProposal(p), nil
}

// cloneProposal defensively deep-copies the reference-typed fields on
// a [Proposal] — currently only `Input.Capabilities`. Mirrors
// [cloneProposalInput] from `proposal.go`; redeclared here as a
// package-private helper so the store does not depend on the
// proposer's internal helper.
//
// Strings are immutable in Go (header-plus-pointer values) so
// aliasing the backing bytes is safe; the deep copy targets only the
// capability-slice header.
func cloneProposal(p Proposal) Proposal {
	out := p
	out.Input = cloneProposalInput(p.Input)
	return out
}
