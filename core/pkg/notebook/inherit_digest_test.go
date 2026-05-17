// inherit_digest_test.go pins the Phase 2 §M7.1.d acceptance:
//
//   - seeded `notebook_inherited` audit rows produce the expected
//     digest payload for two leads;
//   - tick scheduler test confirms idempotent runs (no duplicate
//     DMs within 24h via the `last_run_at` marker row);
//   - empty 24h window produces no DM.
//
// The tests use hand-rolled fakes (no mock library) per the project
// pattern from M3.6 / M6.3.e / the M7.1.c family.
package notebook

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeInheritScanner is the hand-rolled stand-in for
// [InheritAuditScanner]. The test seeds `rows` and the fake returns
// the subset whose `OccurredAt` falls inside the requested half-open
// window. `calls` records every dispatch so the tests can assert the
// scanner was (or was not) reached.
type fakeInheritScanner struct {
	mu    sync.Mutex
	rows  []InheritEvent
	err   error
	calls []fakeScanCall
}

type fakeScanCall struct {
	organizationID string
	since          time.Time
	until          time.Time
}

func (f *fakeInheritScanner) ScanInherited(_ context.Context, organizationID string, since, until time.Time) ([]InheritEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeScanCall{organizationID: organizationID, since: since, until: until})
	if f.err != nil {
		return nil, f.err
	}
	out := make([]InheritEvent, 0, len(f.rows))
	for _, row := range f.rows {
		// Half-open window [since, until) matches the production
		// scanner's strictly-after-cursor contract.
		if !row.OccurredAt.Before(since) && row.OccurredAt.Before(until) {
			out = append(out, row)
		}
	}
	return out, nil
}

func (f *fakeInheritScanner) callsSnapshot() []fakeScanCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeScanCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// fakeLeadResolver is the hand-rolled stand-in for [LeadResolver].
// The test seeds a per-successor map of lead addresses; an entry
// missing from the map returns [ErrLeadHasNoSlackID] so the test
// can also exercise the "skip-no-slack" branch.
type fakeLeadResolver struct {
	mu      sync.Mutex
	mapping map[string]LeadAddress
	missing map[string]bool
	calls   []string
}

func (f *fakeLeadResolver) ResolveLead(_ context.Context, successorWatchkeeperID string) (LeadAddress, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, successorWatchkeeperID)
	if f.missing[successorWatchkeeperID] {
		return LeadAddress{}, ErrLeadHasNoSlackID
	}
	lead, ok := f.mapping[successorWatchkeeperID]
	if !ok {
		return LeadAddress{}, fmt.Errorf("fake resolver: no lead for %s", successorWatchkeeperID)
	}
	return lead, nil
}

// fakeInheritPoster is the hand-rolled stand-in for
// [InheritDigestPoster]. The test inspects `posts` to assert which
// leads received which bodies. The post call is thread-safe so the
// scheduler test can exercise it from the ticker goroutine.
type fakeInheritPoster struct {
	mu    sync.Mutex
	posts []fakePostCall
	err   error
}

type fakePostCall struct {
	lead LeadAddress
	body string
}

func (f *fakeInheritPoster) PostDigest(_ context.Context, lead LeadAddress, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts = append(f.posts, fakePostCall{lead: lead, body: body})
	return f.err
}

func (f *fakeInheritPoster) postsSnapshot() []fakePostCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakePostCall, len(f.posts))
	copy(out, f.posts)
	return out
}

// fakeRunsStore is the hand-rolled stand-in for
// [InheritDigestRunsStore]. A single in-memory map keyed by
// organization id holds the prior run marker. LoadLastRun returns
// `(zero, false, nil)` on a miss matching the production
// pgx-backed wrapper's contract.
type fakeRunsStore struct {
	mu    sync.Mutex
	rows  map[string]InheritDigestRun
	saves int32
}

func newFakeRunsStore() *fakeRunsStore {
	return &fakeRunsStore{rows: make(map[string]InheritDigestRun)}
}

func (f *fakeRunsStore) LoadLastRun(_ context.Context, organizationID string) (InheritDigestRun, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[organizationID]
	return row, ok, nil
}

func (f *fakeRunsStore) SaveRun(_ context.Context, run InheritDigestRun) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[run.OrganizationID] = run
	atomic.AddInt32(&f.saves, 1)
	return nil
}

func (f *fakeRunsStore) saveCount() int32 {
	return atomic.LoadInt32(&f.saves)
}

// digestTestOrgID is a deterministic test-only organization id used
// across the tests. The string shape is a canonical UUID so the
// production runs-store wrapper's pg uuid cast would succeed.
const digestTestOrgID = "00000000-0000-4000-8000-000000000000"

// digestTestNow returns a fixed reference timestamp anchoring every
// test's `now`. Pinned in UTC so the window-math arithmetic is
// deterministic across the developer + CI timezones.
func digestTestNow() time.Time {
	return time.Date(2026, time.May, 17, 12, 0, 0, 0, time.UTC)
}

// TestRunInheritDigest_TwoLeads — acceptance #1: seeded rows produce
// the expected digest payload for two distinct leads. Each lead gets
// exactly one DM whose body names the predecessor → successor pairs
// and entry counts; the marker advances past the scan window.
func TestRunInheritDigest_TwoLeads(t *testing.T) {
	t.Parallel()

	now := digestTestNow()
	scanner := &fakeInheritScanner{
		rows: []InheritEvent{
			{
				PredecessorWatchkeeperID: "pred-A1",
				SuccessorWatchkeeperID:   "succ-A1",
				EntriesImported:          4,
				OccurredAt:               now.Add(-3 * time.Hour),
			},
			{
				PredecessorWatchkeeperID: "pred-A2",
				SuccessorWatchkeeperID:   "succ-A2",
				EntriesImported:          7,
				OccurredAt:               now.Add(-2 * time.Hour),
			},
			{
				PredecessorWatchkeeperID: "pred-B1",
				SuccessorWatchkeeperID:   "succ-B1",
				EntriesImported:          12,
				OccurredAt:               now.Add(-1 * time.Hour),
			},
		},
	}
	resolver := &fakeLeadResolver{
		mapping: map[string]LeadAddress{
			"succ-A1": {HumanID: "lead-A", SlackUserID: "U-A", DisplayName: "Lead A"},
			"succ-A2": {HumanID: "lead-A", SlackUserID: "U-A", DisplayName: "Lead A"},
			"succ-B1": {HumanID: "lead-B", SlackUserID: "U-B", DisplayName: "Lead B"},
		},
	}
	poster := &fakeInheritPoster{}
	store := newFakeRunsStore()

	deps := InheritDigestDeps{
		Scanner:   scanner,
		Resolver:  resolver,
		Poster:    poster,
		RunsStore: store,
	}

	if err := RunInheritDigest(context.Background(), digestTestOrgID, now, deps, nil); err != nil {
		t.Fatalf("RunInheritDigest: %v", err)
	}

	posts := poster.postsSnapshot()
	if len(posts) != 2 {
		t.Fatalf("post count = %d, want 2 (one per lead); posts=%+v", len(posts), posts)
	}

	// Lead A: 2 entries A1 + A2 → total 11 across 2 successors.
	// Lead B: 1 entry B1 → total 12 across 1 successor.
	leadAFound, leadBFound := false, false
	for _, p := range posts {
		switch p.lead.HumanID {
		case "lead-A":
			leadAFound = true
			assertLeadADigestBody(t, p.body)
		case "lead-B":
			leadBFound = true
			assertLeadBDigestBody(t, p.body)
		default:
			t.Errorf("unexpected lead in post: %+v", p.lead)
		}
	}
	if !leadAFound {
		t.Errorf("Lead A did not receive a DM")
	}
	if !leadBFound {
		t.Errorf("Lead B did not receive a DM")
	}

	// Marker advanced to `now`.
	stored, ok, err := store.LoadLastRun(context.Background(), digestTestOrgID)
	if err != nil {
		t.Fatalf("LoadLastRun: %v", err)
	}
	if !ok {
		t.Fatal("marker row missing after RunInheritDigest")
	}
	if !stored.LastRunAt.Equal(now) {
		t.Errorf("LastRunAt = %v, want %v", stored.LastRunAt, now)
	}
	if !stored.LastWindowEnd.Equal(now) {
		t.Errorf("LastWindowEnd = %v, want %v", stored.LastWindowEnd, now)
	}
	if stored.LastWindowStart.After(now) {
		t.Errorf("LastWindowStart (%v) is in the future relative to now (%v)", stored.LastWindowStart, now)
	}
}

// TestRunInheritDigest_EmptyWindow — acceptance #3: an empty 24h
// window produces no DM. The marker still advances so the next
// tick scans the next half-open window.
func TestRunInheritDigest_EmptyWindow(t *testing.T) {
	t.Parallel()

	now := digestTestNow()
	scanner := &fakeInheritScanner{rows: nil}
	resolver := &fakeLeadResolver{mapping: map[string]LeadAddress{}}
	poster := &fakeInheritPoster{}
	store := newFakeRunsStore()

	deps := InheritDigestDeps{
		Scanner:   scanner,
		Resolver:  resolver,
		Poster:    poster,
		RunsStore: store,
	}

	if err := RunInheritDigest(context.Background(), digestTestOrgID, now, deps, nil); err != nil {
		t.Fatalf("RunInheritDigest: %v", err)
	}

	if got := poster.postsSnapshot(); len(got) != 0 {
		t.Errorf("expected zero DMs on empty window, got %d: %+v", len(got), got)
	}
	if got := store.saveCount(); got != 1 {
		t.Errorf("expected exactly one marker save on empty window, got %d", got)
	}
}

// TestRunInheritDigest_IdempotentWithin24h — acceptance #2 (first
// half): a second [RunInheritDigest] call within the 24h cadence
// is a no-op (NO scan, NO post, NO marker write).
func TestRunInheritDigest_IdempotentWithin24h(t *testing.T) {
	t.Parallel()

	now := digestTestNow()
	scanner := &fakeInheritScanner{
		rows: []InheritEvent{
			{
				PredecessorWatchkeeperID: "pred-1",
				SuccessorWatchkeeperID:   "succ-1",
				EntriesImported:          3,
				OccurredAt:               now.Add(-2 * time.Hour),
			},
		},
	}
	resolver := &fakeLeadResolver{
		mapping: map[string]LeadAddress{
			"succ-1": {HumanID: "lead-1", SlackUserID: "U-1", DisplayName: "Lead 1"},
		},
	}
	poster := &fakeInheritPoster{}
	store := newFakeRunsStore()

	deps := InheritDigestDeps{
		Scanner:   scanner,
		Resolver:  resolver,
		Poster:    poster,
		RunsStore: store,
	}

	// First run posts a DM and advances the marker.
	if err := RunInheritDigest(context.Background(), digestTestOrgID, now, deps, nil); err != nil {
		t.Fatalf("first RunInheritDigest: %v", err)
	}
	if got := len(poster.postsSnapshot()); got != 1 {
		t.Fatalf("first run: post count = %d, want 1", got)
	}
	if got := scanner.callsSnapshot(); len(got) != 1 {
		t.Fatalf("first run: scan count = %d, want 1", len(got))
	}
	if got := store.saveCount(); got != 1 {
		t.Fatalf("first run: save count = %d, want 1", got)
	}

	// Second run 30 minutes later: idempotency guard short-circuits.
	secondNow := now.Add(30 * time.Minute)
	if err := RunInheritDigest(context.Background(), digestTestOrgID, secondNow, deps, nil); err != nil {
		t.Fatalf("second RunInheritDigest: %v", err)
	}
	if got := len(poster.postsSnapshot()); got != 1 {
		t.Errorf("second run: post count = %d, want 1 (no duplicate DM)", got)
	}
	if got := len(scanner.callsSnapshot()); got != 1 {
		t.Errorf("second run: scan count = %d, want 1 (no duplicate scan)", got)
	}
	if got := store.saveCount(); got != 1 {
		t.Errorf("second run: save count = %d, want 1 (no marker bump)", got)
	}
}

// TestRunInheritDigest_AfterCadenceWindow — acceptance #2 (second
// half): a [RunInheritDigest] call AFTER the 24h cadence elapses
// runs normally; the window cursor advances from the prior marker
// rather than rewinding.
func TestRunInheritDigest_AfterCadenceWindow(t *testing.T) {
	t.Parallel()

	now := digestTestNow()
	later := now.Add(25 * time.Hour)

	scanner := &fakeInheritScanner{
		rows: []InheritEvent{
			// Old row — drained on the first run.
			{
				PredecessorWatchkeeperID: "pred-old",
				SuccessorWatchkeeperID:   "succ-old",
				EntriesImported:          1,
				OccurredAt:               now.Add(-2 * time.Hour),
			},
			// Fresh row — only visible on the second run.
			{
				PredecessorWatchkeeperID: "pred-new",
				SuccessorWatchkeeperID:   "succ-new",
				EntriesImported:          2,
				OccurredAt:               now.Add(6 * time.Hour),
			},
		},
	}
	resolver := &fakeLeadResolver{
		mapping: map[string]LeadAddress{
			"succ-old": {HumanID: "lead", SlackUserID: "U", DisplayName: "Lead"},
			"succ-new": {HumanID: "lead", SlackUserID: "U", DisplayName: "Lead"},
		},
	}
	poster := &fakeInheritPoster{}
	store := newFakeRunsStore()

	deps := InheritDigestDeps{
		Scanner:   scanner,
		Resolver:  resolver,
		Poster:    poster,
		RunsStore: store,
	}

	if err := RunInheritDigest(context.Background(), digestTestOrgID, now, deps, nil); err != nil {
		t.Fatalf("first RunInheritDigest: %v", err)
	}
	if err := RunInheritDigest(context.Background(), digestTestOrgID, later, deps, nil); err != nil {
		t.Fatalf("second RunInheritDigest: %v", err)
	}

	posts := poster.postsSnapshot()
	if len(posts) != 2 {
		t.Fatalf("post count = %d, want 2 (one per cadence cycle); posts=%+v", len(posts), posts)
	}
	// First post (older) names pred-old; second names pred-new.
	if !strings.Contains(posts[0].body, "pred-old") {
		t.Errorf("first post body missing pred-old: %s", posts[0].body)
	}
	if !strings.Contains(posts[1].body, "pred-new") {
		t.Errorf("second post body missing pred-new: %s", posts[1].body)
	}
	if strings.Contains(posts[1].body, "pred-old") {
		t.Errorf("second post unexpectedly re-included pred-old: %s", posts[1].body)
	}

	// Window assertions: the second scan's `since` is the first
	// scan's `until` (= the first run's `now`).
	calls := scanner.callsSnapshot()
	if len(calls) != 2 {
		t.Fatalf("scan call count = %d, want 2", len(calls))
	}
	if !calls[1].since.Equal(now) {
		t.Errorf("second scan since = %v, want %v (= first scan's until)", calls[1].since, now)
	}
	if !calls[1].until.Equal(later) {
		t.Errorf("second scan until = %v, want %v", calls[1].until, later)
	}
}

// TestRunInheritDigest_ResolverErrorDoesNotAbortRun — per-lead
// resolver failures surface via onLead but do NOT poison the
// remainder of the run. The lead whose resolution succeeded
// still receives a DM and the marker advances.
func TestRunInheritDigest_ResolverErrorDoesNotAbortRun(t *testing.T) {
	t.Parallel()

	now := digestTestNow()
	scanner := &fakeInheritScanner{
		rows: []InheritEvent{
			{
				PredecessorWatchkeeperID: "pred-bad",
				SuccessorWatchkeeperID:   "succ-bad",
				EntriesImported:          1,
				OccurredAt:               now.Add(-3 * time.Hour),
			},
			{
				PredecessorWatchkeeperID: "pred-good",
				SuccessorWatchkeeperID:   "succ-good",
				EntriesImported:          5,
				OccurredAt:               now.Add(-1 * time.Hour),
			},
		},
	}
	resolver := &fakeLeadResolver{
		mapping: map[string]LeadAddress{
			"succ-good": {HumanID: "lead", SlackUserID: "U", DisplayName: "Lead"},
		},
		// succ-bad is missing → fake returns a wrapped error
	}
	poster := &fakeInheritPoster{}
	store := newFakeRunsStore()

	deps := InheritDigestDeps{
		Scanner:   scanner,
		Resolver:  resolver,
		Poster:    poster,
		RunsStore: store,
	}

	var (
		mu        sync.Mutex
		callbacks []struct {
			lead LeadAddress
			err  error
		}
	)
	onLead := func(lead LeadAddress, _ []InheritEvent, err error) {
		mu.Lock()
		defer mu.Unlock()
		callbacks = append(callbacks, struct {
			lead LeadAddress
			err  error
		}{lead: lead, err: err})
	}

	if err := RunInheritDigest(context.Background(), digestTestOrgID, now, deps, onLead); err != nil {
		t.Fatalf("RunInheritDigest: %v", err)
	}

	posts := poster.postsSnapshot()
	if len(posts) != 1 {
		t.Errorf("post count = %d, want 1 (good lead only); posts=%+v", len(posts), posts)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(callbacks) != 2 {
		t.Errorf("callback count = %d, want 2 (one per lead branch)", len(callbacks))
	}
	sawBadErr := false
	sawGoodOK := false
	for _, cb := range callbacks {
		if cb.err != nil {
			sawBadErr = true
		} else {
			sawGoodOK = true
		}
	}
	if !sawBadErr {
		t.Error("expected at least one error callback from the unresolved successor")
	}
	if !sawGoodOK {
		t.Error("expected at least one success callback from the resolved successor")
	}
	if got := store.saveCount(); got != 1 {
		t.Errorf("save count = %d, want 1 (marker still advances on partial failure)", got)
	}
}

// TestRunInheritDigest_LeadHasNoSlackID — the resolver returns
// [ErrLeadHasNoSlackID] for a lead with no Slack contact; the DM
// is skipped but the marker advances. Mirrors the
// "skip-no-slack" branch documented on the LeadResolver seam.
func TestRunInheritDigest_LeadHasNoSlackID(t *testing.T) {
	t.Parallel()

	now := digestTestNow()
	scanner := &fakeInheritScanner{
		rows: []InheritEvent{
			{
				PredecessorWatchkeeperID: "pred-1",
				SuccessorWatchkeeperID:   "succ-no-slack",
				EntriesImported:          3,
				OccurredAt:               now.Add(-2 * time.Hour),
			},
		},
	}
	resolver := &fakeLeadResolver{
		missing: map[string]bool{"succ-no-slack": true},
	}
	poster := &fakeInheritPoster{}
	store := newFakeRunsStore()

	deps := InheritDigestDeps{
		Scanner:   scanner,
		Resolver:  resolver,
		Poster:    poster,
		RunsStore: store,
	}

	var got error
	onLead := func(_ LeadAddress, _ []InheritEvent, err error) {
		got = err
	}

	if err := RunInheritDigest(context.Background(), digestTestOrgID, now, deps, onLead); err != nil {
		t.Fatalf("RunInheritDigest: %v", err)
	}
	if !errors.Is(got, ErrLeadHasNoSlackID) {
		t.Errorf("onLead err = %v, want ErrLeadHasNoSlackID", got)
	}
	if posts := poster.postsSnapshot(); len(posts) != 0 {
		t.Errorf("expected zero DMs on no-slack lead, got %+v", posts)
	}
	if got := store.saveCount(); got != 1 {
		t.Errorf("save count = %d, want 1 (marker advances even when DM is skipped)", got)
	}
}

// TestRunInheritDigest_PanicsOnNilDep — constructor panics on a
// nil seam. Matches the [NewNotebookInheritStep] /
// [NewBotProfileStep] nil-dep discipline.
func TestRunInheritDigest_PanicsOnNilDep(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		deps InheritDigestDeps
	}{
		{"nil scanner", InheritDigestDeps{Scanner: nil, Resolver: &fakeLeadResolver{}, Poster: &fakeInheritPoster{}, RunsStore: newFakeRunsStore()}},
		{"nil resolver", InheritDigestDeps{Scanner: &fakeInheritScanner{}, Resolver: nil, Poster: &fakeInheritPoster{}, RunsStore: newFakeRunsStore()}},
		{"nil poster", InheritDigestDeps{Scanner: &fakeInheritScanner{}, Resolver: &fakeLeadResolver{}, Poster: nil, RunsStore: newFakeRunsStore()}},
		{"nil runs store", InheritDigestDeps{Scanner: &fakeInheritScanner{}, Resolver: &fakeLeadResolver{}, Poster: &fakeInheritPoster{}, RunsStore: nil}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("expected panic for %s", tc.name)
				}
			}()
			_ = RunInheritDigest(context.Background(), digestTestOrgID, digestTestNow(), tc.deps, nil)
		})
	}
}

// TestRunInheritDigest_RejectsEmptyOrg — empty organization id
// returns [ErrInvalidEntry] synchronously.
func TestRunInheritDigest_RejectsEmptyOrg(t *testing.T) {
	t.Parallel()

	deps := InheritDigestDeps{
		Scanner:   &fakeInheritScanner{},
		Resolver:  &fakeLeadResolver{},
		Poster:    &fakeInheritPoster{},
		RunsStore: newFakeRunsStore(),
	}
	err := RunInheritDigest(context.Background(), "", digestTestNow(), deps, nil)
	if !errors.Is(err, ErrInvalidEntry) {
		t.Errorf("err = %v, want ErrInvalidEntry", err)
	}
}

// TestRunInheritDigest_RejectsFutureMarker — iter-1 codex C2: a
// marker row written with a future `last_run_at` would otherwise
// hit the `now.Sub(last_run_at) < cadence` idempotency fast path
// and silently stall the job indefinitely. The validation guard
// runs BEFORE the idempotency check so the typed sentinel
// surfaces immediately.
func TestRunInheritDigest_RejectsFutureMarker(t *testing.T) {
	t.Parallel()

	now := digestTestNow()
	scanner := &fakeInheritScanner{}
	resolver := &fakeLeadResolver{}
	poster := &fakeInheritPoster{}
	store := newFakeRunsStore()

	// Seed a marker whose `last_run_at` is 12h in the future
	// (within the 24h idempotency window relative to `now`).
	// Without the fix the run would short-circuit silently; with
	// the fix it returns ErrInvalidDigestWindow.
	if err := store.SaveRun(context.Background(), InheritDigestRun{
		OrganizationID:  digestTestOrgID,
		LastRunAt:       now.Add(12 * time.Hour),
		LastWindowStart: now.Add(-12 * time.Hour),
		LastWindowEnd:   now.Add(12 * time.Hour),
	}); err != nil {
		t.Fatalf("seed marker: %v", err)
	}

	deps := InheritDigestDeps{
		Scanner:   scanner,
		Resolver:  resolver,
		Poster:    poster,
		RunsStore: store,
	}
	err := RunInheritDigest(context.Background(), digestTestOrgID, now, deps, nil)
	if !errors.Is(err, ErrInvalidDigestWindow) {
		t.Errorf("err = %v, want ErrInvalidDigestWindow", err)
	}
	// Scanner / poster MUST NOT be called when the marker is
	// invalid — the guard precedes any seam dispatch.
	if got := scanner.callsSnapshot(); len(got) != 0 {
		t.Errorf("scanner called %d times on invalid marker, want 0", len(got))
	}
	if got := poster.postsSnapshot(); len(got) != 0 {
		t.Errorf("poster called %d times on invalid marker, want 0", len(got))
	}
}

// TestRunInheritDigest_RejectsBackwardsWindow — when the prior
// marker's `last_window_end` is later than `now` (clock skew or
// manual marker bump in the wrong direction), the run returns
// [ErrInvalidDigestWindow] without scanning.
func TestRunInheritDigest_RejectsBackwardsWindow(t *testing.T) {
	t.Parallel()

	now := digestTestNow()
	store := newFakeRunsStore()
	// Seed a marker whose `last_window_end` is 1h in the future.
	if err := store.SaveRun(context.Background(), InheritDigestRun{
		OrganizationID:  digestTestOrgID,
		LastRunAt:       now.Add(-48 * time.Hour),
		LastWindowStart: now.Add(-48 * time.Hour),
		LastWindowEnd:   now.Add(1 * time.Hour),
	}); err != nil {
		t.Fatalf("seed marker: %v", err)
	}

	deps := InheritDigestDeps{
		Scanner:   &fakeInheritScanner{},
		Resolver:  &fakeLeadResolver{},
		Poster:    &fakeInheritPoster{},
		RunsStore: store,
	}
	err := RunInheritDigest(context.Background(), digestTestOrgID, now, deps, nil)
	if !errors.Is(err, ErrInvalidDigestWindow) {
		t.Errorf("err = %v, want ErrInvalidDigestWindow", err)
	}
}

// TestPeriodicInheritDigest_DisabledShortCircuits — the `enabled`
// flag false path returns [ErrInheritDigestDisabled] immediately
// without starting a ticker.
func TestPeriodicInheritDigest_DisabledShortCircuits(t *testing.T) {
	t.Parallel()

	scanner := &fakeInheritScanner{}
	deps := InheritDigestDeps{
		Scanner:   scanner,
		Resolver:  &fakeLeadResolver{},
		Poster:    &fakeInheritPoster{},
		RunsStore: newFakeRunsStore(),
	}
	err := PeriodicInheritDigest(context.Background(), digestTestOrgID, false, time.Hour, nil, deps, nil, nil)
	if !errors.Is(err, ErrInheritDigestDisabled) {
		t.Errorf("err = %v, want ErrInheritDigestDisabled", err)
	}
	if got := scanner.callsSnapshot(); len(got) != 0 {
		t.Errorf("scanner called %d times when disabled, want 0", len(got))
	}
}

// TestPeriodicInheritDigest_BadCadence — non-positive cadence
// returns [ErrInvalidCadence] synchronously. Mirrors the
// [PeriodicBackup] cadence-validation discipline.
func TestPeriodicInheritDigest_BadCadence(t *testing.T) {
	t.Parallel()

	deps := InheritDigestDeps{
		Scanner:   &fakeInheritScanner{},
		Resolver:  &fakeLeadResolver{},
		Poster:    &fakeInheritPoster{},
		RunsStore: newFakeRunsStore(),
	}
	for _, cadence := range []time.Duration{0, -1, -time.Hour} {
		err := PeriodicInheritDigest(context.Background(), digestTestOrgID, true, cadence, nil, deps, nil, nil)
		if !errors.Is(err, ErrInvalidCadence) {
			t.Errorf("cadence=%v: err = %v, want ErrInvalidCadence", cadence, err)
		}
	}
}

// TestPeriodicInheritDigest_IdempotentTicks — acceptance #2
// (scheduler half): the periodic loop fires multiple ticks within
// the 24h cadence; the idempotency guard in [RunInheritDigest]
// prevents duplicate DMs. The first tick posts the DM; subsequent
// ticks short-circuit. The loop exits cleanly on ctx cancel.
func TestPeriodicInheritDigest_IdempotentTicks(t *testing.T) {
	t.Parallel()

	now := digestTestNow()
	scanner := &fakeInheritScanner{
		rows: []InheritEvent{
			{
				PredecessorWatchkeeperID: "pred-1",
				SuccessorWatchkeeperID:   "succ-1",
				EntriesImported:          2,
				OccurredAt:               now.Add(-1 * time.Hour),
			},
		},
	}
	resolver := &fakeLeadResolver{
		mapping: map[string]LeadAddress{
			"succ-1": {HumanID: "lead", SlackUserID: "U", DisplayName: "Lead"},
		},
	}
	poster := &fakeInheritPoster{}
	store := newFakeRunsStore()

	// A frozen clock keeps every tick inside the 24h cadence.
	clock := func() time.Time { return now }

	deps := InheritDigestDeps{
		Scanner:   scanner,
		Resolver:  resolver,
		Poster:    poster,
		RunsStore: store,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tickErrs := make(chan error, 8)
	done := make(chan error, 1)
	go func() {
		done <- PeriodicInheritDigest(ctx, digestTestOrgID, true, 5*time.Millisecond, clock, deps, func(err error) {
			tickErrs <- err
		}, nil)
	}()

	// Poll for at least 3 ticks via the onTick callback so we
	// know the scheduler has dispatched multiple cycles.
	collected := 0
	deadline := time.Now().Add(3 * time.Second)
	for collected < 3 && time.Now().Before(deadline) {
		select {
		case err := <-tickErrs:
			if err != nil {
				t.Errorf("unexpected per-tick err: %v", err)
			}
			collected++
		case <-time.After(100 * time.Millisecond):
		}
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("loop exit err = %v, want context.Canceled wrap", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("PeriodicInheritDigest did not exit within 2s of ctx cancel")
	}

	// Exactly one DM despite multiple ticks — the idempotency
	// guard short-circuits every tick after the first.
	if got := len(poster.postsSnapshot()); got != 1 {
		t.Errorf("post count = %d, want 1 (no duplicate DM within 24h cadence)", got)
	}
}

// TestEventTypeNotebookInheritedMirror is the wire-shape
// regression guard: the notebook-side mirror of
// `spawn.EventTypeNotebookInherited` MUST equal the literal
// `notebook_inherited`. Drift would silently break the
// scanner's event_type filter.
func TestEventTypeNotebookInheritedMirror(t *testing.T) {
	t.Parallel()
	if EventTypeNotebookInherited != "notebook_inherited" {
		t.Errorf("EventTypeNotebookInherited = %q, want notebook_inherited", EventTypeNotebookInherited)
	}
}

// assertContains is a tiny test helper used across the digest
// tests to keep the assertion sites compact. Surfaces a clear
// diagnostic when the substring is missing.
func assertContains(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("body missing %q; full body: %s", needle, haystack)
	}
}

// assertLeadADigestBody pins the Lead-A digest body shape.
// Extracted from [TestRunInheritDigest_TwoLeads] to keep that
// test under the gocyclo budget.
func assertLeadADigestBody(t *testing.T, body string) {
	t.Helper()
	assertContains(t, body, "Lead A")
	assertContains(t, body, "11 inherited entries")
	assertContains(t, body, "2 successors")
	assertContains(t, body, "pred-A1 → succ-A1")
	assertContains(t, body, "pred-A2 → succ-A2")
	assertContains(t, body, "(4 entries)")
	assertContains(t, body, "(7 entries)")
	// Stable per-pair order: A1 (older) before A2 (newer).
	idxA1 := strings.Index(body, "pred-A1")
	idxA2 := strings.Index(body, "pred-A2")
	if idxA1 < 0 || idxA2 < 0 || idxA1 > idxA2 {
		t.Errorf("expected pred-A1 before pred-A2 in body; got idxA1=%d idxA2=%d", idxA1, idxA2)
	}
}

// assertLeadBDigestBody pins the Lead-B digest body shape.
// Extracted alongside [assertLeadADigestBody] to keep
// [TestRunInheritDigest_TwoLeads] under the gocyclo budget.
func assertLeadBDigestBody(t *testing.T, body string) {
	t.Helper()
	assertContains(t, body, "Lead B")
	assertContains(t, body, "12 inherited entries")
	assertContains(t, body, "1 successor")
	assertContains(t, body, "pred-B1 → succ-B1")
	assertContains(t, body, "(12 entries)")
}
