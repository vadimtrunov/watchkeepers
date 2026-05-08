// pending_approval_memory.go ships the in-memory [PendingApprovalDAO]
// implementation used by the M6.3.b InteractionDispatcher tests AND
// (intentionally) by callers that want a zero-dep approval store for
// dev / smoke runs without a Postgres-backed adapter wired up.
//
// The store is goroutine-safe: every public method takes a single
// per-instance mutex for the duration of the call. Throughput is
// irrelevant — production callers wire a real adapter; this type
// exists so the test surface and the dev-loop wiring share one
// implementation.
package spawn

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// MemoryPendingApprovalDAO is a process-local [PendingApprovalDAO]
// implementation backed by a `map[token]row`. Constructed via
// [NewMemoryPendingApprovalDAO]; the zero value is NOT usable —
// callers must always go through the constructor so the internal map
// is non-nil.
//
// `Now` is overridable so unit tests can drive deterministic
// `requested_at` / `resolved_at` values without a fixture clock
// threaded through the Insert / Resolve call sites.
type MemoryPendingApprovalDAO struct {
	mu   sync.Mutex
	rows map[string]*PendingApproval
	now  func() time.Time
}

// NewMemoryPendingApprovalDAO returns an empty in-memory store. The
// optional `now` argument overrides the wall-clock used to stamp
// `requested_at` / `resolved_at`; pass nil to use [time.Now].
func NewMemoryPendingApprovalDAO(now func() time.Time) *MemoryPendingApprovalDAO {
	if now == nil {
		now = time.Now
	}
	return &MemoryPendingApprovalDAO{
		rows: make(map[string]*PendingApproval),
		now:  now,
	}
}

// Compile-time assertion: [*MemoryPendingApprovalDAO] satisfies
// [PendingApprovalDAO]. Pins the integration shape.
var _ PendingApprovalDAO = (*MemoryPendingApprovalDAO)(nil)

// Insert satisfies [PendingApprovalDAO.Insert]. Returns a wrapped
// error on duplicate `token` (caller bug — every M6.2.x tool mints a
// fresh token per invocation).
func (m *MemoryPendingApprovalDAO) Insert(_ context.Context, token, tool string, paramsJSON json.RawMessage) error {
	if token == "" {
		return fmt.Errorf("spawn: pending approval insert: empty token")
	}
	if tool == "" {
		return fmt.Errorf("spawn: pending approval insert: empty tool_name")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.rows[token]; exists {
		return fmt.Errorf("spawn: pending approval insert: duplicate token %q", token)
	}
	// Defensive copy of the params bytes so a mutating caller cannot
	// alias the stored row.
	stored := make(json.RawMessage, len(paramsJSON))
	copy(stored, paramsJSON)
	m.rows[token] = &PendingApproval{
		ApprovalToken: token,
		ToolName:      tool,
		ParamsJSON:    stored,
		State:         PendingApprovalStatePending,
		RequestedAt:   m.now().UTC(),
	}
	return nil
}

// Get satisfies [PendingApprovalDAO.Get]. Returns a defensive copy so
// a mutating caller cannot race the in-place row.
func (m *MemoryPendingApprovalDAO) Get(_ context.Context, token string) (*PendingApproval, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.rows[token]
	if !ok {
		return nil, ErrPendingApprovalNotFound
	}
	out := *row
	out.ParamsJSON = make(json.RawMessage, len(row.ParamsJSON))
	copy(out.ParamsJSON, row.ParamsJSON)
	return &out, nil
}

// Resolve satisfies [PendingApprovalDAO.Resolve]. Enforces the
// state-machine: `pending` → terminal only.
func (m *MemoryPendingApprovalDAO) Resolve(_ context.Context, token string, decision PendingApprovalDecision) error {
	if decision != PendingApprovalStateApproved && decision != PendingApprovalStateRejected {
		return fmt.Errorf("%w: %q", ErrPendingApprovalInvalidDecision, decision)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.rows[token]
	if !ok {
		return ErrPendingApprovalNotFound
	}
	if row.State != PendingApprovalStatePending {
		return fmt.Errorf("%w: state=%q", ErrPendingApprovalStaleState, row.State)
	}
	row.State = decision
	row.ResolvedAt = m.now().UTC()
	return nil
}
