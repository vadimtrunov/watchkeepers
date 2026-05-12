package approval

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// ----- M1 — Submit refuses to publish when ctx is cancelled between
// IdentityResolver and Publish; the Publish call itself runs with a
// cancel-detached ctx so its durability is not racing the caller. -----

// cancellingResolver returns the configured identity AND cancels the
// supplied cancelFn. Tests use this to simulate "the caller cancelled
// mid-flight, just after the IdentityResolver returned". The
// Proposer.Submit pre-publish ctx.Err check should catch this.
func cancellingResolver(identity string, cancelFn context.CancelFunc) IdentityResolver {
	return func(_ context.Context) (string, error) {
		cancelFn()
		return identity, nil
	}
}

func TestSubmit_CtxCancelledMidFlight_RefusesPublish(t *testing.T) {
	t.Parallel()
	p, pub, _, _, _ := newTestProposer()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p.deps.IdentityResolver = cancellingResolver("agent-1", cancel)

	_, err := p.Submit(ctx, validInput())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if len(pub.eventsForTopic(TopicToolProposed)) != 0 {
		t.Errorf("publisher invoked despite mid-flight cancel: %v", pub.snapshot())
	}
}

// slowPublisher records the ctx received and a "cancellable observed"
// flag. Tests use it to assert Submit detaches cancellation BEFORE
// invoking Publish, so the publish's ctx never satisfies
// `ctx.Done()`.
type slowPublisher struct {
	mu             sync.Mutex
	publishCtxs    []context.Context
	delay          time.Duration
	contextCancels int
}

func (sp *slowPublisher) Publish(ctx context.Context, _ string, _ any) error {
	sp.mu.Lock()
	sp.publishCtxs = append(sp.publishCtxs, ctx)
	delay := sp.delay
	sp.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			sp.mu.Lock()
			sp.contextCancels++
			sp.mu.Unlock()
			return ctx.Err()
		}
	}
	return nil
}

func TestSubmit_PublishCtxDetachedFromCallerCancel(t *testing.T) {
	t.Parallel()
	sp := &slowPublisher{delay: 50 * time.Millisecond}
	clk := newFakeClock(time.Now())
	p := New(ProposerDeps{
		Publisher:        sp,
		Clock:            clk,
		IDGenerator:      &fakeIDGenerator{},
		IdentityResolver: constResolver("agent-1", nil),
	})

	ctx, cancel := context.WithCancel(context.Background())
	// Schedule the caller cancel for AFTER Submit has reached
	// Publish (slowPublisher.delay) but BEFORE delay elapses. With
	// the M1 fix, Submit uses context.WithoutCancel(ctx) for the
	// Publish call so the publish completes despite the caller
	// cancel.
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	prop, err := p.Submit(ctx, validInput())
	if err != nil {
		t.Fatalf("Submit: expected success, got %v", err)
	}
	if prop.ID.String() == "" {
		t.Error("ID: empty after success")
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if len(sp.publishCtxs) != 1 {
		t.Fatalf("expected 1 publish call, got %d", len(sp.publishCtxs))
	}
	// The publish ctx MUST report no error even though the parent
	// ctx was cancelled before delay elapsed.
	if err := sp.publishCtxs[0].Err(); err != nil {
		t.Errorf("publish ctx leaked cancel: got Err()=%v", err)
	}
	if sp.contextCancels != 0 {
		t.Errorf("slowPublisher observed %d cancels on its publish ctx (must be 0)", sp.contextCancels)
	}
}

// ----- M2 — DecodeModeYAML accepts trailing `---` separator -----

func TestDecodeModeYAML_AcceptsTrailingSeparator(t *testing.T) {
	t.Parallel()
	cases := []string{
		"approval_mode: git-pr\n---\n",
		"approval_mode: slack-native\n---",
		"approval_mode: both\n---\n   \n",
	}
	for _, raw := range cases {
		got, err := DecodeModeYAML([]byte(raw))
		if err != nil {
			t.Errorf("%q: unexpected err %v", raw, err)
			continue
		}
		if err := got.Validate(); err != nil {
			t.Errorf("%q: decoded %q is invalid: %v", raw, got, err)
		}
	}
}

// ----- M3 — ProposalInput.Validate detects duplicate capabilities -----

func TestProposalInput_ValidateDuplicateCapability(t *testing.T) {
	t.Parallel()
	in := validInput()
	in.Capabilities = []string{"github:read", "jira:read", "github:read"}
	err := in.Validate()
	if !errors.Is(err, ErrDuplicateProposalCapability) {
		t.Errorf("expected ErrDuplicateProposalCapability, got %v", err)
	}
}

func TestProposalInput_ValidateDuplicateCapability_AdjacentEntries(t *testing.T) {
	t.Parallel()
	in := validInput()
	in.Capabilities = []string{"github:read", "github:read"}
	err := in.Validate()
	if !errors.Is(err, ErrDuplicateProposalCapability) {
		t.Errorf("expected ErrDuplicateProposalCapability, got %v", err)
	}
}

// ----- M4 — CorrelationID is derived from ProposalID.String() -----

func TestSubmit_CorrelationIDEqualsProposalIDString(t *testing.T) {
	t.Parallel()
	p, pub, _, _, _ := newTestProposer()
	prop, err := p.Submit(context.Background(), validInput())
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if prop.CorrelationID != prop.ID.String() {
		t.Errorf("Proposal.CorrelationID: got %q, want ID.String()=%q",
			prop.CorrelationID, prop.ID.String())
	}
	ev := pub.eventsForTopic(TopicToolProposed)[0].event.(ToolProposed)
	if ev.CorrelationID != prop.ID.String() {
		t.Errorf("event.CorrelationID: got %q, want %q",
			ev.CorrelationID, prop.ID.String())
	}
}

// TestSubmit_ConcurrencyUniqueCorrelationID — 16 goroutines each
// submitting 4 proposals. CorrelationID is derived from
// ProposalID.String() so under the fake clock (every Now() returns
// the same instant) the CorrelationIDs are STILL unique. Codex
// iter-1 flagged the prior `time.Now().UnixNano()` derivation as
// collision-prone under exactly this scenario.
func TestSubmit_ConcurrencyUniqueCorrelationID(t *testing.T) {
	t.Parallel()
	p, pub, _, _, _ := newTestProposer()
	const goroutines = 16
	const perGoroutine = 4
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				if _, err := p.Submit(context.Background(), validInput()); err != nil {
					t.Errorf("submit: %v", err)
				}
			}
		}()
	}
	wg.Wait()
	seen := map[string]bool{}
	for _, ev := range pub.eventsForTopic(TopicToolProposed) {
		corr := ev.event.(ToolProposed).CorrelationID
		if corr == "" {
			t.Error("empty CorrelationID")
		}
		if seen[corr] {
			t.Errorf("duplicate CorrelationID %q", corr)
		}
		seen[corr] = true
	}
}

// ----- m2 — MaxProposerIDLength enforcement -----

func TestSubmit_ProposerIDTooLongRejected(t *testing.T) {
	t.Parallel()
	p, pub, _, _, _ := newTestProposer()
	p.deps.IdentityResolver = constResolver(strings.Repeat("a", MaxProposerIDLength+1), nil)
	_, err := p.Submit(context.Background(), validInput())
	if !errors.Is(err, ErrInvalidProposerID) {
		t.Fatalf("expected ErrInvalidProposerID, got %v", err)
	}
	if len(pub.snapshot()) != 0 {
		t.Errorf("publisher invoked despite over-long proposer id: %v", pub.snapshot())
	}
}

func TestSubmit_ProposerIDAtBoundaryAccepted(t *testing.T) {
	t.Parallel()
	p, _, _, _, _ := newTestProposer()
	p.deps.IdentityResolver = constResolver(strings.Repeat("a", MaxProposerIDLength), nil)
	if _, err := p.Submit(context.Background(), validInput()); err != nil {
		t.Errorf("at-boundary identity: %v", err)
	}
}

// ----- m4 — PII canary NUL-byte variant -----

//nolint:gosec // G101: synthetic redaction-harness canary, not a real credential.
const canaryCodeDraftNUL = "CANARY_NUL\x00BYTE_CODE_DRAFT_PII_DO_NOT_LEAK"

func TestSubmit_EventPayloadOmitsBodiesWithNULByteCanary(t *testing.T) {
	t.Parallel()
	p, pub, _, _, _ := newTestProposer()
	in := validInput()
	in.CodeDraft = canaryCodeDraftNUL + "; export const run = ...;"

	if _, err := p.Submit(context.Background(), in); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	ev := pub.eventsForTopic(TopicToolProposed)[0].event
	// `%+v` renders NUL bytes verbatim into the dump; a substring
	// search against the canary (which contains the NUL) succeeds
	// iff the payload leaked the body. Distinct from the ASCII
	// canary tests in proposer_test.go so a future logger that
	// strips NUL bytes does not silently invalidate the substring
	// match.
	dump := fmt.Sprintf("%+v", ev)
	if strings.Contains(dump, canaryCodeDraftNUL) {
		t.Errorf("event payload leaked NUL-bearing canary in %q", dump)
	}
}
