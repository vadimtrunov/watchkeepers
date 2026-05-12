package approval

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
)

// recordedEvent captures a single publish call for assertion.
type recordedEvent struct {
	topic string
	event any
}

type fakePublisher struct {
	mu       sync.Mutex
	events   []recordedEvent
	err      error
	errAfter int // when > 0 returns err on the err-after'th call onward
}

func (p *fakePublisher) Publish(_ context.Context, topic string, event any) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, recordedEvent{topic: topic, event: event})
	if p.err != nil && (p.errAfter == 0 || len(p.events) >= p.errAfter) {
		return p.err
	}
	return nil
}

func (p *fakePublisher) snapshot() []recordedEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]recordedEvent, len(p.events))
	copy(out, p.events)
	return out
}

func (p *fakePublisher) eventsForTopic(topic string) []recordedEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []recordedEvent
	for _, ev := range p.events {
		if ev.topic == topic {
			out = append(out, ev)
		}
	}
	return out
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{now: t} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// fakeIDGenerator returns a deterministic sequence of UUIDs derived
// from a counter. Tests pre-load `next` or `err` to drive specific
// scenarios.
type fakeIDGenerator struct {
	mu      sync.Mutex
	next    uuid.UUID
	counter uint64
	err     error
}

func (g *fakeIDGenerator) NewUUID() (uuid.UUID, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.err != nil {
		return uuid.Nil, g.err
	}
	g.counter++
	// Stamp the counter into the last 8 bytes so successive calls are
	// distinguishable even when `next` is zero-valued.
	id := g.next
	for i := 8; i < 16; i++ {
		id[i] = byte(g.counter >> (8 * (15 - i)))
	}
	return id, nil
}

// logEntry captures a single Logger.Log call.
type logEntry struct {
	msg string
	kv  []any
}

type fakeLogger struct {
	mu      sync.Mutex
	entries []logEntry
}

func (l *fakeLogger) Log(_ context.Context, msg string, kv ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cp := make([]any, len(kv))
	copy(cp, kv)
	l.entries = append(l.entries, logEntry{msg: msg, kv: cp})
}

func (l *fakeLogger) snapshot() []logEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]logEntry, len(l.entries))
	copy(out, l.entries)
	return out
}

// constResolver returns a fixed value for every call. Tests pin the
// "happy path" value or `err`.
func constResolver(value string, err error) IdentityResolver {
	return func(_ context.Context) (string, error) {
		return value, err
	}
}

// countingResolver wraps an inner resolver and increments a counter
// on each call (used by the per-call-invocation assertion).
func countingResolver(inner IdentityResolver, count *int) IdentityResolver {
	return func(ctx context.Context) (string, error) {
		*count++
		return inner(ctx)
	}
}

// validInput returns a [ProposalInput] that passes every field
// validation — base fixture for tests that want to mutate a single
// field.
func validInput() ProposalInput {
	return ProposalInput{
		Name:                     "count_open_prs",
		Purpose:                  "Surface the open-PR count on the daily digest.",
		PlainLanguageDescription: "Counts how many of your team's pull requests are still waiting for review.",
		CodeDraft:                "export const run = async () => 42;",
		Capabilities:             []string{"github:read"},
		TargetSource:             TargetSourcePlatform,
	}
}

// newTestProposer wires a [Proposer] with all fakes and returns the
// proposer plus the fakes so tests can assert on them. The default
// configuration produces a happy-path Submit.
func newTestProposer() (*Proposer, *fakePublisher, *fakeClock, *fakeIDGenerator, *fakeLogger) {
	pub := &fakePublisher{}
	clk := newFakeClock(time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC))
	idGen := &fakeIDGenerator{}
	logger := &fakeLogger{}
	p := New(ProposerDeps{
		Publisher:        pub,
		Clock:            clk,
		IDGenerator:      idGen,
		IdentityResolver: constResolver("agent-1", nil),
		Logger:           logger,
	})
	return p, pub, clk, idGen, logger
}

// fakeSchedulerSyncer captures SyncOnce calls and optionally returns an
// error. Used by webhook tests.
type fakeSchedulerSyncer struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (s *fakeSchedulerSyncer) SyncOnce(_ context.Context, source string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, source)
	return s.err
}

func (s *fakeSchedulerSyncer) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.calls))
	copy(out, s.calls)
	return out
}

// constSourceResolver returns a fixed source name for every call.
func constSourceResolver(name string, err error) SourceForTarget {
	return func(_ context.Context, _ TargetSource) (string, error) {
		return name, err
	}
}

// constSecretResolver returns a fixed secret-byte slice for every
// call.
func constSecretResolver(secret []byte, err error) WebhookSecretResolver {
	return func(_ context.Context) ([]byte, error) {
		return secret, err
	}
}

// fakeDecisionRecorder captures MarkDecided / UnmarkDecided calls.
// Default behaviour mirrors a fresh [*InMemoryProposalStore]: the
// first MarkDecided per id wins; same-kind replay → (false, nil);
// different-kind replay → (false, ErrDecisionConflict).
type fakeDecisionRecorder struct {
	mu        sync.Mutex
	decisions map[uuid.UUID]DecisionKind
	marks     []struct {
		ID   uuid.UUID
		Kind DecisionKind
	}
	unmarks []struct {
		ID   uuid.UUID
		Kind DecisionKind
	}
	// markErr is returned by MarkDecided when non-nil; otherwise the
	// fake applies the normal claim semantics.
	markErr error
}

func newFakeDecisionRecorder() *fakeDecisionRecorder {
	return &fakeDecisionRecorder{decisions: make(map[uuid.UUID]DecisionKind)}
}

func (r *fakeDecisionRecorder) MarkDecided(_ context.Context, id uuid.UUID, kind DecisionKind) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.marks = append(r.marks, struct {
		ID   uuid.UUID
		Kind DecisionKind
	}{ID: id, Kind: kind})
	if r.markErr != nil {
		return false, r.markErr
	}
	if prior, ok := r.decisions[id]; ok {
		if prior == kind {
			return false, nil
		}
		return false, ErrDecisionConflict
	}
	r.decisions[id] = kind
	return true, nil
}

func (r *fakeDecisionRecorder) UnmarkDecided(_ context.Context, id uuid.UUID, kind DecisionKind) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.unmarks = append(r.unmarks, struct {
		ID   uuid.UUID
		Kind DecisionKind
	}{ID: id, Kind: kind})
	if prior, ok := r.decisions[id]; ok && prior == kind {
		delete(r.decisions, id)
	}
	return nil
}

func (r *fakeDecisionRecorder) markCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.marks)
}

func (r *fakeDecisionRecorder) unmarkCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.unmarks)
}

// fakeDryRunRequester captures RequestDryRun calls.
type fakeDryRunRequester struct {
	mu    sync.Mutex
	calls []struct {
		ProposalID uuid.UUID
		LeadDM     string
	}
	err error
}

func (r *fakeDryRunRequester) RequestDryRun(_ context.Context, id uuid.UUID, leadDM string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, struct {
		ProposalID uuid.UUID
		LeadDM     string
	}{ProposalID: id, LeadDM: leadDM})
	return r.err
}

// fakeProposalLookup is a hand-rolled [ProposalLookup] for the
// callback dispatcher tests. Distinct from [InMemoryProposalStore]
// so tests can drive lookup-error branches independently.
type fakeProposalLookup struct {
	mu    sync.Mutex
	items map[uuid.UUID]Proposal
	err   error
}

func newFakeProposalLookup() *fakeProposalLookup {
	return &fakeProposalLookup{items: make(map[uuid.UUID]Proposal)}
}

func (l *fakeProposalLookup) Lookup(_ context.Context, id uuid.UUID) (Proposal, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return Proposal{}, l.err
	}
	p, ok := l.items[id]
	if !ok {
		return Proposal{}, ErrProposalNotFound
	}
	return p, nil
}

func (l *fakeProposalLookup) put(p Proposal) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.items[p.ID] = p
}

// mustNewUUIDv7 mints a UUIDv7 and t.Fatal()s on error. Used to seed
// test proposals.
func mustNewUUIDv7() uuid.UUID {
	id, err := uuid.NewV7()
	if err != nil {
		panic("test: uuid.NewV7: " + err.Error())
	}
	return id
}

// constScopeResolver returns a fixed [Scope] (or error) for every
// call. The deterministic shape lets tests pin the scoped-mode
// rewrite without spinning up a real lead-DM / Jira-sandbox lookup.
func constScopeResolver(scope Scope, err error) ScopeResolver {
	return func(_ context.Context, _ uuid.UUID) (Scope, error) {
		return scope, err
	}
}

// fakeBrokerForwarder captures every forwarded invocation and
// optionally returns an error on the configured call (1-indexed). The
// fake snapshots a defensive deep-copy of the supplied Args so a
// caller-side mutation post-Forward cannot retroactively corrupt the
// recorded trail.
type fakeBrokerForwarder struct {
	mu       sync.Mutex
	calls    []BrokerInvocation
	err      error
	errAfter int
}

func (f *fakeBrokerForwarder) Forward(_ context.Context, inv BrokerInvocation) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	args := make(map[string]string, len(inv.Args))
	for k, v := range inv.Args {
		args[k] = v
	}
	f.calls = append(f.calls, BrokerInvocation{Kind: inv.Kind, Op: inv.Op, Args: args})
	if f.err != nil && (f.errAfter == 0 || len(f.calls) >= f.errAfter) {
		return f.err
	}
	return nil
}

func (f *fakeBrokerForwarder) snapshot() []BrokerInvocation {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]BrokerInvocation, len(f.calls))
	copy(out, f.calls)
	return out
}

// mutatingBrokerForwarder injects a key into the supplied
// invocation's Args map on Forward. Used by the iter-1 codex M1
// defence-in-depth test to assert the executor's post-Forward
// re-clone protects the stored Outcome.Effective from a buggy
// forwarder mutation.
type mutatingBrokerForwarder struct {
	injectKey string
	injectVal string
}

func (m *mutatingBrokerForwarder) Forward(_ context.Context, inv BrokerInvocation) error {
	if inv.Args != nil {
		inv.Args[m.injectKey] = m.injectVal
	}
	return nil
}

// cancellingBrokerForwarder cancels the supplied [context.CancelFunc]
// on the configured Forward call (1-indexed). Used by the iter-1
// critic M2/M3 fixes to drive the per-iteration ctx-check + the
// publish-despite-cancel-after-side-effects path.
type cancellingBrokerForwarder struct {
	mu        sync.Mutex
	callCount *int32
	cancelOn  int32
	cancel    context.CancelFunc
	// onCancel, when non-nil, is Done()'d after firing cancel — so the
	// publish-despite-cancel test can deterministically order the
	// cancel against the loop's next iteration.
	onCancel *sync.WaitGroup
	calls    []BrokerInvocation
}

func (c *cancellingBrokerForwarder) Forward(_ context.Context, inv BrokerInvocation) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	*c.callCount++
	args := make(map[string]string, len(inv.Args))
	for k, v := range inv.Args {
		args[k] = v
	}
	c.calls = append(c.calls, BrokerInvocation{Kind: inv.Kind, Op: inv.Op, Args: args})
	if *c.callCount == c.cancelOn && c.cancel != nil {
		c.cancel()
		if c.onCancel != nil {
			c.onCancel.Done()
		}
	}
	return nil
}

// newTestProposal builds a fully-populated [Proposal] suitable for the
// store / webhook / callback dispatcher tests. The caller may mutate
// the returned struct freely.
func newTestProposal() Proposal {
	id := mustNewUUIDv7()
	return Proposal{
		ID:            id,
		ProposerID:    "agent-1",
		Input:         validInput(),
		ProposedAt:    time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC),
		CorrelationID: id.String(),
	}
}
