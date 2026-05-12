package approval

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func TestInMemoryProposalStore_StoreAndLookup_Happy(t *testing.T) {
	s := NewInMemoryProposalStore()
	p := newTestProposal()
	s.Record(p)

	got, err := s.Lookup(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.ID != p.ID {
		t.Errorf("ID mismatch: want %s got %s", p.ID, got.ID)
	}
	if got.ProposerID != p.ProposerID {
		t.Errorf("ProposerID mismatch")
	}
	if got.Input.Name != p.Input.Name {
		t.Errorf("Input.Name mismatch")
	}
}

func TestInMemoryProposalStore_Lookup_NotFound(t *testing.T) {
	s := NewInMemoryProposalStore()
	id := mustNewUUIDv7()
	_, err := s.Lookup(context.Background(), id)
	if !errors.Is(err, ErrProposalNotFound) {
		t.Errorf("want ErrProposalNotFound, got %v", err)
	}
	if !strings.Contains(err.Error(), id.String()) {
		t.Errorf("error must mention requested id: %v", err)
	}
}

func TestInMemoryProposalStore_Lookup_RefusesCancelledCtx(t *testing.T) {
	s := NewInMemoryProposalStore()
	p := newTestProposal()
	s.Record(p)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.Lookup(ctx, p.ID)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("want context.Canceled, got %v", err)
	}
}

func TestInMemoryProposalStore_Record_ZeroIDIsNoOp(t *testing.T) {
	s := NewInMemoryProposalStore()
	p := newTestProposal()
	p.ID = uuid.Nil
	// Should not panic.
	s.Record(p)
	_, err := s.Lookup(context.Background(), uuid.Nil)
	if !errors.Is(err, ErrProposalNotFound) {
		t.Errorf("zero-id record must not land in store, got %v", err)
	}
}

func TestInMemoryProposalStore_DefensiveCopy_OnStore(t *testing.T) {
	s := NewInMemoryProposalStore()
	p := newTestProposal()
	p.Input.Capabilities = []string{"github:read"}
	s.Record(p)

	// Mutate the source slice; the stored proposal must NOT pick up
	// the mutation.
	p.Input.Capabilities[0] = "MUTATED"
	p.Input.Capabilities = append(p.Input.Capabilities, "EXTRA")

	got, err := s.Lookup(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if len(got.Input.Capabilities) != 1 || got.Input.Capabilities[0] != "github:read" {
		t.Errorf("defensive copy on Store leaked caller mutation: %v", got.Input.Capabilities)
	}
}

func TestInMemoryProposalStore_DefensiveCopy_OnLookup(t *testing.T) {
	s := NewInMemoryProposalStore()
	p := newTestProposal()
	p.Input.Capabilities = []string{"github:read"}
	s.Record(p)

	got, err := s.Lookup(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	got.Input.Capabilities[0] = "MUTATED"
	got.Input.Capabilities = append(got.Input.Capabilities, "EXTRA")

	again, err := s.Lookup(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("Lookup again: %v", err)
	}
	if len(again.Input.Capabilities) != 1 || again.Input.Capabilities[0] != "github:read" {
		t.Errorf("defensive copy on Lookup leaked caller mutation: %v", again.Input.Capabilities)
	}
}

func TestInMemoryProposalStore_MarkDecided_FirstTimeAndReplay(t *testing.T) {
	s := NewInMemoryProposalStore()
	id := mustNewUUIDv7()

	firstTime, err := s.MarkDecided(context.Background(), id, DecisionApproved)
	if err != nil {
		t.Fatalf("first MarkDecided: %v", err)
	}
	if !firstTime {
		t.Errorf("first MarkDecided: want firstTime=true")
	}

	// Same-kind replay — idempotent (false, nil).
	firstTime, err = s.MarkDecided(context.Background(), id, DecisionApproved)
	if err != nil {
		t.Errorf("replay MarkDecided: want nil err, got %v", err)
	}
	if firstTime {
		t.Errorf("replay MarkDecided: want firstTime=false")
	}
}

func TestInMemoryProposalStore_MarkDecided_DifferentKindConflict(t *testing.T) {
	s := NewInMemoryProposalStore()
	id := mustNewUUIDv7()

	if _, err := s.MarkDecided(context.Background(), id, DecisionApproved); err != nil {
		t.Fatalf("first MarkDecided: %v", err)
	}
	firstTime, err := s.MarkDecided(context.Background(), id, DecisionRejected)
	if !errors.Is(err, ErrDecisionConflict) {
		t.Errorf("different-kind MarkDecided: want ErrDecisionConflict, got %v", err)
	}
	if firstTime {
		t.Errorf("different-kind MarkDecided: firstTime must be false on conflict")
	}
}

func TestInMemoryProposalStore_MarkDecided_RejectsBadInput(t *testing.T) {
	s := NewInMemoryProposalStore()
	id := mustNewUUIDv7()

	if _, err := s.MarkDecided(context.Background(), uuid.Nil, DecisionApproved); err == nil {
		t.Errorf("zero id must fail")
	}
	if _, err := s.MarkDecided(context.Background(), id, DecisionKind("invalid")); !errors.Is(err, ErrInvalidDecisionKind) {
		t.Errorf("unknown kind must fail with ErrInvalidDecisionKind, got %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.MarkDecided(ctx, id, DecisionApproved); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled ctx must propagate, got %v", err)
	}
}

func TestInMemoryProposalStore_UnmarkDecided_AllowsRetry(t *testing.T) {
	s := NewInMemoryProposalStore()
	id := mustNewUUIDv7()

	if _, err := s.MarkDecided(context.Background(), id, DecisionApproved); err != nil {
		t.Fatalf("MarkDecided: %v", err)
	}
	if err := s.UnmarkDecided(context.Background(), id, DecisionApproved); err != nil {
		t.Fatalf("UnmarkDecided: %v", err)
	}
	firstTime, err := s.MarkDecided(context.Background(), id, DecisionApproved)
	if err != nil {
		t.Fatalf("post-unmark MarkDecided: %v", err)
	}
	if !firstTime {
		t.Errorf("post-unmark MarkDecided: want firstTime=true (claim was rolled back)")
	}
}

func TestInMemoryProposalStore_UnmarkDecided_DifferentKindIsNoOp(t *testing.T) {
	s := NewInMemoryProposalStore()
	id := mustNewUUIDv7()

	if _, err := s.MarkDecided(context.Background(), id, DecisionApproved); err != nil {
		t.Fatalf("MarkDecided: %v", err)
	}
	// A losing caller attempting to unmark the WINNING claim with a
	// different kind must NOT erase the winner's claim.
	if err := s.UnmarkDecided(context.Background(), id, DecisionRejected); err != nil {
		t.Fatalf("UnmarkDecided (different kind): %v", err)
	}
	firstTime, err := s.MarkDecided(context.Background(), id, DecisionApproved)
	if err != nil {
		t.Fatalf("MarkDecided: %v", err)
	}
	if firstTime {
		t.Errorf("winner's claim must survive a different-kind Unmark; got firstTime=true")
	}
}

func TestInMemoryProposalStore_MarkDecided_ConcurrencyOneWinner(t *testing.T) {
	s := NewInMemoryProposalStore()
	id := mustNewUUIDv7()

	const n = 16
	wins := 0
	var winMu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			firstTime, err := s.MarkDecided(context.Background(), id, DecisionApproved)
			if err != nil {
				t.Errorf("MarkDecided: %v", err)
				return
			}
			if firstTime {
				winMu.Lock()
				wins++
				winMu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Errorf("concurrent same-kind MarkDecided must elect exactly one winner; got %d", wins)
	}
}

func TestDecisionKind_Validate(t *testing.T) {
	for _, ok := range []DecisionKind{DecisionApproved, DecisionRejected} {
		if err := ok.Validate(); err != nil {
			t.Errorf("%q should be valid: %v", ok, err)
		}
	}
	for _, bad := range []DecisionKind{"", "test_in_my_dm", "ask_questions", "Approved"} {
		if err := bad.Validate(); !errors.Is(err, ErrInvalidDecisionKind) {
			t.Errorf("%q should fail: %v", bad, err)
		}
	}
}

func TestInMemoryProposalStore_Concurrency_16Goroutines(t *testing.T) {
	s := NewInMemoryProposalStore()
	const n = 16
	proposals := make([]Proposal, n)
	for i := 0; i < n; i++ {
		proposals[i] = newTestProposal()
	}
	var wg sync.WaitGroup
	wg.Add(n * 2)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			s.Record(proposals[i])
		}()
		go func() {
			defer wg.Done()
			// Lookups may race against Records; the goal is to assert
			// no data race surfaces under `go test -race`.
			_, _ = s.Lookup(context.Background(), proposals[i].ID)
		}()
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		got, err := s.Lookup(context.Background(), proposals[i].ID)
		if err != nil {
			t.Fatalf("Lookup[%d]: %v", i, err)
		}
		if got.ID != proposals[i].ID {
			t.Errorf("Lookup[%d]: id mismatch", i)
		}
	}
}
