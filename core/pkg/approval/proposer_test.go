package approval

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestNew_PanicsOnNilPublisher(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic")
		}
		if !strings.Contains(fmt.Sprint(r), "deps.Publisher") {
			t.Errorf("panic msg: got %q", r)
		}
	}()
	_ = New(ProposerDeps{
		Clock:            newFakeClock(time.Now()),
		IDGenerator:      &fakeIDGenerator{},
		IdentityResolver: constResolver("agent", nil),
	})
}

func TestNew_PanicsOnNilClock(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic")
		}
		if !strings.Contains(fmt.Sprint(r), "deps.Clock") {
			t.Errorf("panic msg: got %q", r)
		}
	}()
	_ = New(ProposerDeps{
		Publisher:        &fakePublisher{},
		IDGenerator:      &fakeIDGenerator{},
		IdentityResolver: constResolver("agent", nil),
	})
}

func TestNew_PanicsOnNilIDGenerator(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic")
		}
		if !strings.Contains(fmt.Sprint(r), "deps.IDGenerator") {
			t.Errorf("panic msg: got %q", r)
		}
	}()
	_ = New(ProposerDeps{
		Publisher:        &fakePublisher{},
		Clock:            newFakeClock(time.Now()),
		IdentityResolver: constResolver("agent", nil),
	})
}

func TestNew_PanicsOnNilIdentityResolver(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic")
		}
		if !strings.Contains(fmt.Sprint(r), "deps.IdentityResolver") {
			t.Errorf("panic msg: got %q", r)
		}
	}()
	_ = New(ProposerDeps{
		Publisher:   &fakePublisher{},
		Clock:       newFakeClock(time.Now()),
		IDGenerator: &fakeIDGenerator{},
	})
}

func TestNew_AcceptsNilLogger(t *testing.T) {
	t.Parallel()
	p := New(ProposerDeps{
		Publisher:        &fakePublisher{},
		Clock:            newFakeClock(time.Now()),
		IDGenerator:      &fakeIDGenerator{},
		IdentityResolver: constResolver("agent", nil),
	})
	if p == nil {
		t.Fatal("New returned nil")
	}
	if _, err := p.Submit(context.Background(), validInput()); err != nil {
		t.Errorf("Submit with nil Logger: %v", err)
	}
}

func TestSubmit_HappyPath(t *testing.T) {
	t.Parallel()
	p, pub, clk, _, _ := newTestProposer()
	prop, err := p.Submit(context.Background(), validInput())
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if prop.ID == uuid.Nil {
		t.Error("ID: got uuid.Nil")
	}
	if prop.ProposerID != "agent-1" {
		t.Errorf("ProposerID: got %q", prop.ProposerID)
	}
	if !prop.ProposedAt.Equal(clk.Now()) {
		t.Errorf("ProposedAt: got %v, want %v", prop.ProposedAt, clk.Now())
	}
	if prop.CorrelationID == "" {
		t.Error("CorrelationID: empty")
	}
	events := pub.eventsForTopic(TopicToolProposed)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	ev, ok := events[0].event.(ToolProposed)
	if !ok {
		t.Fatalf("event type: got %T", events[0].event)
	}
	if ev.ProposalID != prop.ID {
		t.Errorf("event.ProposalID: got %v, want %v", ev.ProposalID, prop.ID)
	}
	if ev.CorrelationID != prop.CorrelationID {
		t.Errorf("event.CorrelationID: got %q, want %q", ev.CorrelationID, prop.CorrelationID)
	}
}

func TestSubmit_ValidationFailureNoSideEffects(t *testing.T) {
	t.Parallel()
	p, pub, _, _, _ := newTestProposer()
	in := validInput()
	in.PlainLanguageDescription = "" // mandatory
	resolverCount := 0
	p.deps.IdentityResolver = countingResolver(p.deps.IdentityResolver, &resolverCount)

	_, err := p.Submit(context.Background(), in)
	if !errors.Is(err, ErrMissingPlainLanguageDescription) {
		t.Fatalf("expected ErrMissingPlainLanguageDescription, got %v", err)
	}
	if resolverCount != 0 {
		t.Errorf("IdentityResolver called %d times — must be 0 when validation fails", resolverCount)
	}
	if len(pub.snapshot()) != 0 {
		t.Errorf("publisher invoked despite validation failure: %v", pub.snapshot())
	}
}

func TestSubmit_CtxPreCancelled(t *testing.T) {
	t.Parallel()
	p, pub, _, _, _ := newTestProposer()
	resolverCount := 0
	p.deps.IdentityResolver = countingResolver(p.deps.IdentityResolver, &resolverCount)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := p.Submit(ctx, validInput())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if resolverCount != 0 {
		t.Errorf("IdentityResolver called %d times after ctx cancel — must be 0", resolverCount)
	}
	if len(pub.snapshot()) != 0 {
		t.Errorf("publisher invoked despite ctx cancel: %v", pub.snapshot())
	}
}

func TestSubmit_IdentityResolverErrorWraps(t *testing.T) {
	t.Parallel()
	p, pub, _, _, _ := newTestProposer()
	cause := errors.New("resolver: token expired")
	p.deps.IdentityResolver = constResolver("", cause)
	_, err := p.Submit(context.Background(), validInput())
	if !errors.Is(err, ErrIdentityResolution) {
		t.Errorf("expected ErrIdentityResolution, got %v", err)
	}
	if !errors.Is(err, cause) {
		t.Errorf("expected chain to %v, got %v", cause, err)
	}
	if len(pub.snapshot()) != 0 {
		t.Errorf("publisher invoked despite resolver error: %v", pub.snapshot())
	}
}

func TestSubmit_IdentityResolverEmptySuccess(t *testing.T) {
	t.Parallel()
	p, pub, _, _, _ := newTestProposer()
	p.deps.IdentityResolver = constResolver("", nil)
	_, err := p.Submit(context.Background(), validInput())
	if !errors.Is(err, ErrEmptyResolvedIdentity) {
		t.Fatalf("expected ErrEmptyResolvedIdentity, got %v", err)
	}
	if len(pub.snapshot()) != 0 {
		t.Errorf("publisher invoked despite empty-resolved-identity: %v", pub.snapshot())
	}
}

func TestSubmit_IDGeneratorErrorSurfaces(t *testing.T) {
	t.Parallel()
	p, pub, _, idGen, _ := newTestProposer()
	idGen.err = errors.New("uuid: out of entropy")
	_, err := p.Submit(context.Background(), validInput())
	if err == nil {
		t.Fatal("expected error from id generator")
	}
	if !strings.Contains(err.Error(), "out of entropy") {
		t.Errorf("error chain missing underlying cause: %v", err)
	}
	if len(pub.snapshot()) != 0 {
		t.Errorf("publisher invoked despite id-gen failure: %v", pub.snapshot())
	}
}

func TestSubmit_PublisherErrorWraps(t *testing.T) {
	t.Parallel()
	p, pub, _, _, logger := newTestProposer()
	cause := errors.New("bus: backpressure")
	pub.err = cause

	_, err := p.Submit(context.Background(), validInput())
	if !errors.Is(err, ErrPublishToolProposed) {
		t.Fatalf("expected ErrPublishToolProposed, got %v", err)
	}
	if !errors.Is(err, cause) {
		t.Errorf("expected chain to %v, got %v", cause, err)
	}
	// Logger should have captured the failure.
	if len(logger.snapshot()) == 0 {
		t.Error("expected logger entry on publish failure")
	}
}

func TestSubmit_PublisherErrorRedactsBodies(t *testing.T) {
	t.Parallel()
	p, pub, _, _, logger := newTestProposer()
	pub.err = errors.New("bus: backpressure")
	in := validInput()
	in.CodeDraft = "CANARY_CODE_DRAFT_PII"
	in.Purpose = "CANARY_PURPOSE_PII"
	in.PlainLanguageDescription = "CANARY_PLD_PII"

	_, _ = p.Submit(context.Background(), in)

	for _, e := range logger.snapshot() {
		if strings.Contains(e.msg, "CANARY_") {
			t.Errorf("logger msg leaked canary: %q", e.msg)
		}
		for _, v := range e.kv {
			if strings.Contains(fmt.Sprint(v), "CANARY_") {
				t.Errorf("logger kv leaked canary: %v", v)
			}
		}
	}
}

func TestSubmit_PerCallIdentityResolverInvocation(t *testing.T) {
	t.Parallel()
	p, _, _, _, _ := newTestProposer()
	count := 0
	p.deps.IdentityResolver = countingResolver(constResolver("agent-1", nil), &count)
	for i := 0; i < 5; i++ {
		if _, err := p.Submit(context.Background(), validInput()); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}
	if count != 5 {
		t.Errorf("expected 5 resolver calls, got %d", count)
	}
}

func TestSubmit_DefensiveCopyOfCapabilities(t *testing.T) {
	t.Parallel()
	p, pub, _, _, _ := newTestProposer()
	in := validInput()
	in.Capabilities = []string{"github:read", "github:list"}

	prop, err := p.Submit(context.Background(), in)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	// Mutate caller-side slice.
	in.Capabilities[0] = "MUTATED"
	in.Capabilities = append(in.Capabilities, "extra")

	if prop.Input.Capabilities[0] != "github:read" {
		t.Errorf("Proposal.Input.Capabilities aliased caller: %v", prop.Input.Capabilities)
	}
	if len(prop.Input.Capabilities) != 2 {
		t.Errorf("Proposal len changed: %d", len(prop.Input.Capabilities))
	}
	// Event payload independence.
	ev := pub.eventsForTopic(TopicToolProposed)[0].event.(ToolProposed)
	if ev.CapabilityIDs[0] != "github:read" {
		t.Errorf("event CapabilityIDs aliased caller: %v", ev.CapabilityIDs)
	}
}

// TestSubmit_EventPayloadOmitsBodies — PII canary harness mirroring
// the M9.1.b `TestPIIRedactionCanary_EffectiveToolsetUpdatedPayload`
// and M9.2 `TestPIIRedactionCanary_ToolShadowedPayload`. Synthetic
// canary substrings are stuffed into CodeDraft / Purpose /
// PlainLanguageDescription; the verbatim `%+v` dump of the event
// payload MUST NOT contain any of them.
const (
	//nolint:gosec // G101: synthetic redaction-harness canaries, not real credentials.
	canaryCodeDraft = "CANARY_CODE_DRAFT_PII_a7c2f9_DO_NOT_LEAK"
	//nolint:gosec // G101: synthetic redaction-harness canaries, not real credentials.
	canaryPurpose = "CANARY_PURPOSE_PII_b3e8d1_DO_NOT_LEAK"
	//nolint:gosec // G101: synthetic redaction-harness canaries, not real credentials.
	canaryPlainLanguageDescription = "CANARY_PLD_PII_c1f4a6_DO_NOT_LEAK"
)

func TestSubmit_EventPayloadOmitsBodies(t *testing.T) {
	t.Parallel()
	p, pub, _, _, _ := newTestProposer()
	in := validInput()
	in.CodeDraft = canaryCodeDraft + "; export const run = ...;"
	in.Purpose = canaryPurpose + " — daily digest"
	in.PlainLanguageDescription = "Description: " + canaryPlainLanguageDescription

	if _, err := p.Submit(context.Background(), in); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	ev := pub.eventsForTopic(TopicToolProposed)[0].event
	dump := fmt.Sprintf("%+v", ev)
	for _, canary := range []string{canaryCodeDraft, canaryPurpose, canaryPlainLanguageDescription} {
		if strings.Contains(dump, canary) {
			t.Errorf("event payload leaked %q in %q", canary, dump)
		}
	}
}

// TestSubmit_LoggerPayloadOmitsBodies — even on the failure path
// (publisher error), the logger MUST NOT see CodeDraft / Purpose /
// PlainLanguageDescription bodies.
func TestSubmit_LoggerPayloadOmitsBodies(t *testing.T) {
	t.Parallel()
	p, pub, _, _, logger := newTestProposer()
	pub.err = errors.New("bus: down")
	in := validInput()
	in.CodeDraft = canaryCodeDraft
	in.Purpose = canaryPurpose
	in.PlainLanguageDescription = canaryPlainLanguageDescription

	_, _ = p.Submit(context.Background(), in)

	for _, e := range logger.snapshot() {
		if strings.Contains(e.msg, "CANARY_") {
			t.Errorf("logger msg leaked canary: %q", e.msg)
		}
		for _, v := range e.kv {
			if strings.Contains(fmt.Sprint(v), "CANARY_") {
				t.Errorf("logger kv leaked canary: %v", v)
			}
		}
	}
}

// TestSubmit_ConcurrencyNoDataRace runs 16 goroutines each issuing
// 4 concurrent Submits. Race-detector clean + every event observed
// + every proposal id unique.
func TestSubmit_ConcurrencyNoDataRace(t *testing.T) {
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
					t.Errorf("concurrent Submit: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	events := pub.eventsForTopic(TopicToolProposed)
	if len(events) != goroutines*perGoroutine {
		t.Errorf("expected %d events, got %d", goroutines*perGoroutine, len(events))
	}
	seen := map[uuid.UUID]bool{}
	for _, ev := range events {
		id := ev.event.(ToolProposed).ProposalID
		if id == uuid.Nil {
			t.Errorf("event has uuid.Nil ID")
		}
		if seen[id] {
			t.Errorf("duplicate ProposalID %v", id)
		}
		seen[id] = true
	}
}

// TestSourceGrepAC_NoAuditCallsInProductionSources — the approval
// package must remain audit-free at M9.4.a (audit emission belongs
// to M9.7 via the subscriber that joins TopicToolProposed). A
// drift here would mean an audit dependency snuck into the
// authoring boundary.
//
// File paths are relative; `go test` sets the working directory to
// the package directory at test time, so `os.ReadFile("doc.go")`
// resolves against this source dir. Same CWD invariant as the
// toolregistry source-grep AC (M9.1.a).
func TestSourceGrepAC_NoAuditCallsInProductionSources(t *testing.T) {
	t.Parallel()
	productionFiles := []string{
		"doc.go",
		"errors.go",
		"target_source.go",
		"approval_mode.go",
		"proposal.go",
		"events.go",
		"proposer.go",
		"proposalstore.go",
		"webhook.go",
		"reviewer.go",
		"card.go",
		"callbacks.go",
	}
	for _, name := range productionFiles {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		body := string(raw)
		for _, banned := range []string{"keeperslog.", ".Append("} {
			if containsOutsideComments(body, banned) {
				t.Errorf("%s: contains banned token %q outside comments", name, banned)
			}
		}
	}
}

// containsOutsideComments reports whether `body` contains `needle`
// outside any `//` line comment. Block comments are out of scope —
// the toolregistry package does not use them either and the source-
// grep AC mirrors that discipline.
func containsOutsideComments(body, needle string) bool {
	for _, line := range strings.Split(body, "\n") {
		stripped := stripLineComment(line)
		if strings.Contains(stripped, needle) {
			return true
		}
	}
	return false
}

func stripLineComment(line string) string {
	if i := strings.Index(line, "//"); i >= 0 {
		return line[:i]
	}
	return line
}
