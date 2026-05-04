package lifecycle

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
)

// Compile-time assertion that the concrete keepclient.Client satisfies
// the LocalKeepClient interface this package exposes. Test-only import
// of keepclient: production code in this package never depends on the
// concrete type — only on the interface — which is the whole point of
// the M2b.6 cross-package compile-time-check pattern (see
// docs/LESSONS.md). If a future keepclient method-rename breaks this
// line, the lifecycle production wiring breaks at the same compile
// step, no later.
var _ LocalKeepClient = (*keepclient.Client)(nil)

// strPtr returns a pointer to the supplied string. Helper kept local
// to the test file because no production code path needs a string-
// pointer constructor today.
func strPtr(s string) *string { return &s }

// TestSpawn_InsertThenActivate_ReturnsID — happy path: Spawn returns
// the inserted id and the fake records both calls in Insert→Update
// order with the second carrying status="active".
func TestSpawn_InsertThenActivate_ReturnsID(t *testing.T) {
	t.Parallel()
	fake := &fakeKeepClient{
		insertResp: &keepclient.InsertWatchkeeperResponse{ID: "abc"},
	}
	m := New(fake)

	id, err := m.Spawn(context.Background(), SpawnParams{
		ManifestID:  "mfst-1",
		LeadHumanID: "human-1",
	})
	if err != nil {
		t.Fatalf("Spawn returned error: %v", err)
	}
	if id != "abc" {
		t.Fatalf("Spawn id = %q, want %q", id, "abc")
	}

	calls := fake.recordedCalls()
	if len(calls) != 2 {
		t.Fatalf("recorded %d calls, want 2: %#v", len(calls), calls)
	}
	if calls[0].Kind != fakeCallInsert {
		t.Fatalf("call[0].Kind = %q, want %q", calls[0].Kind, fakeCallInsert)
	}
	if calls[0].InsertReq.ManifestID != "mfst-1" || calls[0].InsertReq.LeadHumanID != "human-1" {
		t.Fatalf("Insert request = %+v, want ManifestID=mfst-1 LeadHumanID=human-1", calls[0].InsertReq)
	}
	if calls[1].Kind != fakeCallUpdate {
		t.Fatalf("call[1].Kind = %q, want %q", calls[1].Kind, fakeCallUpdate)
	}
	if calls[1].UpdateID != "abc" || calls[1].UpdateStatus != "active" {
		t.Fatalf("Update call = %+v, want id=abc status=active", calls[1])
	}
}

// TestRetire_DelegatesToUpdateStatusRetired — happy path: Retire
// triggers exactly one Update with status="retired".
func TestRetire_DelegatesToUpdateStatusRetired(t *testing.T) {
	t.Parallel()
	fake := &fakeKeepClient{}
	m := New(fake)

	if err := m.Retire(context.Background(), "abc"); err != nil {
		t.Fatalf("Retire returned error: %v", err)
	}

	calls := fake.recordedCalls()
	if len(calls) != 1 {
		t.Fatalf("recorded %d calls, want 1", len(calls))
	}
	if calls[0].Kind != fakeCallUpdate {
		t.Fatalf("call[0].Kind = %q, want %q", calls[0].Kind, fakeCallUpdate)
	}
	if calls[0].UpdateID != "abc" || calls[0].UpdateStatus != "retired" {
		t.Fatalf("Update call = %+v, want id=abc status=retired", calls[0])
	}
}

// TestHealth_ProjectsRow — happy path: Health returns a *Status whose
// 7 fields equal the keepclient.Watchkeeper row's same-named fields,
// dropping ActiveManifestVersionID per AC6.
func TestHealth_ProjectsRow(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	spawned := created.Add(time.Minute)
	retired := created.Add(time.Hour)
	row := &keepclient.Watchkeeper{
		ID:                      "abc",
		ManifestID:              "mfst-1",
		LeadHumanID:             "human-1",
		ActiveManifestVersionID: strPtr("ver-1"),
		Status:                  "retired",
		SpawnedAt:               &spawned,
		RetiredAt:               &retired,
		CreatedAt:               created,
	}
	fake := &fakeKeepClient{getResp: row}
	m := New(fake)

	got, err := m.Health(context.Background(), "abc")
	if err != nil {
		t.Fatalf("Health returned error: %v", err)
	}
	want := &Status{
		ID:          "abc",
		ManifestID:  "mfst-1",
		LeadHumanID: "human-1",
		Status:      "retired",
		SpawnedAt:   &spawned,
		RetiredAt:   &retired,
		CreatedAt:   created,
	}
	if got.ID != want.ID || got.ManifestID != want.ManifestID ||
		got.LeadHumanID != want.LeadHumanID || got.Status != want.Status ||
		got.SpawnedAt == nil || !got.SpawnedAt.Equal(*want.SpawnedAt) ||
		got.RetiredAt == nil || !got.RetiredAt.Equal(*want.RetiredAt) ||
		!got.CreatedAt.Equal(want.CreatedAt) {
		t.Fatalf("Health projection = %+v, want %+v", got, want)
	}
}

// TestList_PassthroughToKeepClient — happy path: List returns the same
// rows the fake's listResp carries; the slice length and per-row ids
// match.
func TestList_PassthroughToKeepClient(t *testing.T) {
	t.Parallel()
	created := time.Now().UTC()
	rows := []keepclient.Watchkeeper{
		{ID: "a", ManifestID: "m", LeadHumanID: "h", Status: "active", CreatedAt: created},
		{ID: "b", ManifestID: "m", LeadHumanID: "h", Status: "active", CreatedAt: created},
		{ID: "c", ManifestID: "m", LeadHumanID: "h", Status: "active", CreatedAt: created},
	}
	fake := &fakeKeepClient{listResp: &keepclient.ListWatchkeepersResponse{Items: rows}}
	m := New(fake)

	got, err := m.List(context.Background(), ListFilter{})
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(got) != len(rows) {
		t.Fatalf("List returned %d items, want %d", len(got), len(rows))
	}
	for i, wk := range got {
		if wk == nil {
			t.Fatalf("List[%d] is nil", i)
		}
		if wk.ID != rows[i].ID {
			t.Fatalf("List[%d].ID = %q, want %q", i, wk.ID, rows[i].ID)
		}
	}
}

// TestList_FilterStatus — happy path: List forwards filter.Status to
// keepclient via the recorded ListReq.
func TestList_FilterStatus(t *testing.T) {
	t.Parallel()
	fake := &fakeKeepClient{listResp: &keepclient.ListWatchkeepersResponse{}}
	m := New(fake)

	if _, err := m.List(context.Background(), ListFilter{Status: "active"}); err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	calls := fake.recordedCalls()
	if len(calls) != 1 {
		t.Fatalf("recorded %d calls, want 1", len(calls))
	}
	if calls[0].Kind != fakeCallList {
		t.Fatalf("call[0].Kind = %q, want %q", calls[0].Kind, fakeCallList)
	}
	if calls[0].ListReq.Status != "active" {
		t.Fatalf("List request Status = %q, want %q", calls[0].ListReq.Status, "active")
	}
}

// TestSpawn_InsertFails_ReturnsEmptyIDAndWrappedErr — Insert error
// surfaces as ("", wrapped) with the documented "lifecycle: spawn:
// insert:" prefix and an errors.Is-matchable chain.
func TestSpawn_InsertFails_ReturnsEmptyIDAndWrappedErr(t *testing.T) {
	t.Parallel()
	errInsertBoom := errors.New("insert kaboom")
	fake := &fakeKeepClient{insertErr: errInsertBoom}
	m := New(fake)

	id, err := m.Spawn(context.Background(), SpawnParams{
		ManifestID:  "mfst-1",
		LeadHumanID: "human-1",
	})
	if id != "" {
		t.Fatalf("Spawn id on Insert failure = %q, want empty", id)
	}
	if err == nil {
		t.Fatalf("Spawn returned nil error on Insert failure")
	}
	if !errors.Is(err, errInsertBoom) {
		t.Fatalf("Spawn err = %v, want errors.Is errInsertBoom", err)
	}
	if !strings.Contains(err.Error(), "lifecycle: spawn: insert:") {
		t.Fatalf("Spawn err message = %q, want prefix %q", err.Error(), "lifecycle: spawn: insert:")
	}
	calls := fake.recordedCalls()
	if len(calls) != 1 || calls[0].Kind != fakeCallInsert {
		t.Fatalf("calls = %#v, want one Insert call", calls)
	}
}

// TestSpawn_ActivateFails_ReturnsIDAndWrappedErr — Update error
// returns (id, wrapped) so the caller can retry just the Update
// against the inserted `pending` row. Error message carries
// "lifecycle: spawn: activate:" and chains errActivateBoom.
func TestSpawn_ActivateFails_ReturnsIDAndWrappedErr(t *testing.T) {
	t.Parallel()
	errActivateBoom := errors.New("activate kaboom")
	fake := &fakeKeepClient{
		insertResp: &keepclient.InsertWatchkeeperResponse{ID: "abc"},
		updateErr:  errActivateBoom,
	}
	m := New(fake)

	id, err := m.Spawn(context.Background(), SpawnParams{
		ManifestID:  "mfst-1",
		LeadHumanID: "human-1",
	})
	if id != "abc" {
		t.Fatalf("Spawn id on Update failure = %q, want %q (row exists in DB pending)", id, "abc")
	}
	if err == nil {
		t.Fatalf("Spawn returned nil error on Update failure")
	}
	if !errors.Is(err, errActivateBoom) {
		t.Fatalf("Spawn err = %v, want errors.Is errActivateBoom", err)
	}
	if !strings.Contains(err.Error(), "lifecycle: spawn: activate:") {
		t.Fatalf("Spawn err message = %q, want prefix %q", err.Error(), "lifecycle: spawn: activate:")
	}
	calls := fake.recordedCalls()
	if len(calls) != 2 {
		t.Fatalf("recorded %d calls, want 2 (Insert + failed Update)", len(calls))
	}
}

// TestSpawn_EmptyManifestID_ErrInvalidParams — empty ManifestID is a
// synchronous ErrInvalidParams; the fake records ZERO calls.
func TestSpawn_EmptyManifestID_ErrInvalidParams(t *testing.T) {
	t.Parallel()
	fake := &fakeKeepClient{}
	m := New(fake)

	id, err := m.Spawn(context.Background(), SpawnParams{
		LeadHumanID: "human-1",
	})
	if id != "" {
		t.Fatalf("Spawn id on validation failure = %q, want empty", id)
	}
	if !errors.Is(err, ErrInvalidParams) {
		t.Fatalf("Spawn err = %v, want errors.Is ErrInvalidParams", err)
	}
	if got := fake.callCount(); got != 0 {
		t.Fatalf("recorded %d calls, want 0 (no network round-trip on validation)", got)
	}
}

// TestSpawn_EmptyLeadHumanID_ErrInvalidParams — analogous to the empty
// ManifestID case.
func TestSpawn_EmptyLeadHumanID_ErrInvalidParams(t *testing.T) {
	t.Parallel()
	fake := &fakeKeepClient{}
	m := New(fake)

	id, err := m.Spawn(context.Background(), SpawnParams{
		ManifestID: "mfst-1",
	})
	if id != "" {
		t.Fatalf("Spawn id on validation failure = %q, want empty", id)
	}
	if !errors.Is(err, ErrInvalidParams) {
		t.Fatalf("Spawn err = %v, want errors.Is ErrInvalidParams", err)
	}
	if got := fake.callCount(); got != 0 {
		t.Fatalf("recorded %d calls, want 0", got)
	}
}

// TestRetire_EmptyID_ErrInvalidParams — empty id is a synchronous
// ErrInvalidParams; no Update call.
func TestRetire_EmptyID_ErrInvalidParams(t *testing.T) {
	t.Parallel()
	fake := &fakeKeepClient{}
	m := New(fake)

	err := m.Retire(context.Background(), "")
	if !errors.Is(err, ErrInvalidParams) {
		t.Fatalf("Retire err = %v, want errors.Is ErrInvalidParams", err)
	}
	if got := fake.callCount(); got != 0 {
		t.Fatalf("recorded %d calls, want 0", got)
	}
}

// TestRetire_KeepClientFails_WrapsError — keepclient's
// ErrInvalidStatusTransition flows through Retire's wrap chain so the
// caller's errors.Is still matches.
func TestRetire_KeepClientFails_WrapsError(t *testing.T) {
	t.Parallel()
	fake := &fakeKeepClient{updateErr: keepclient.ErrInvalidStatusTransition}
	m := New(fake)

	err := m.Retire(context.Background(), "abc")
	if err == nil {
		t.Fatalf("Retire returned nil error on keepclient failure")
	}
	if !errors.Is(err, keepclient.ErrInvalidStatusTransition) {
		t.Fatalf("Retire err = %v, want errors.Is keepclient.ErrInvalidStatusTransition", err)
	}
	if !strings.Contains(err.Error(), "lifecycle: retire:") {
		t.Fatalf("Retire err message = %q, want prefix %q", err.Error(), "lifecycle: retire:")
	}
}

// TestHealth_EmptyID_ErrInvalidParams — empty id is a synchronous
// ErrInvalidParams; no Get call.
func TestHealth_EmptyID_ErrInvalidParams(t *testing.T) {
	t.Parallel()
	fake := &fakeKeepClient{}
	m := New(fake)

	got, err := m.Health(context.Background(), "")
	if got != nil {
		t.Fatalf("Health returned non-nil status on validation failure: %+v", got)
	}
	if !errors.Is(err, ErrInvalidParams) {
		t.Fatalf("Health err = %v, want errors.Is ErrInvalidParams", err)
	}
	if got := fake.callCount(); got != 0 {
		t.Fatalf("recorded %d calls, want 0", got)
	}
}

// TestHealth_NotFound_WrapsErrNotFound — keepclient.ErrNotFound flows
// through Health's wrap chain; callers' errors.Is still matches.
func TestHealth_NotFound_WrapsErrNotFound(t *testing.T) {
	t.Parallel()
	fake := &fakeKeepClient{getErr: keepclient.ErrNotFound}
	m := New(fake)

	got, err := m.Health(context.Background(), "abc")
	if got != nil {
		t.Fatalf("Health returned non-nil status on Get failure: %+v", got)
	}
	if !errors.Is(err, keepclient.ErrNotFound) {
		t.Fatalf("Health err = %v, want errors.Is keepclient.ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "lifecycle: health:") {
		t.Fatalf("Health err message = %q, want prefix %q", err.Error(), "lifecycle: health:")
	}
}

// TestList_LimitOutOfRange_ErrInvalidParams — Limit=300 exceeds the
// 200 cap and surfaces as a synchronous ErrInvalidParams; no List call.
func TestList_LimitOutOfRange_ErrInvalidParams(t *testing.T) {
	t.Parallel()
	fake := &fakeKeepClient{}
	m := New(fake)

	got, err := m.List(context.Background(), ListFilter{Limit: 300})
	if got != nil {
		t.Fatalf("List returned non-nil items on validation failure: %+v", got)
	}
	if !errors.Is(err, ErrInvalidParams) {
		t.Fatalf("List err = %v, want errors.Is ErrInvalidParams", err)
	}
	if got := fake.callCount(); got != 0 {
		t.Fatalf("recorded %d calls, want 0", got)
	}
}

// TestList_NegativeLimit_ErrInvalidParams — Limit=-1 surfaces as
// synchronous ErrInvalidParams.
func TestList_NegativeLimit_ErrInvalidParams(t *testing.T) {
	t.Parallel()
	fake := &fakeKeepClient{}
	m := New(fake)

	got, err := m.List(context.Background(), ListFilter{Limit: -1})
	if got != nil {
		t.Fatalf("List returned non-nil items on validation failure: %+v", got)
	}
	if !errors.Is(err, ErrInvalidParams) {
		t.Fatalf("List err = %v, want errors.Is ErrInvalidParams", err)
	}
	if got := fake.callCount(); got != 0 {
		t.Fatalf("recorded %d calls, want 0", got)
	}
}

// TestManager_ConcurrentSpawnsAreIndependent — 50 goroutines each call
// Spawn; the fake's monotonic-id allocator hands every caller a unique
// non-empty id. Pins the invariant that the Manager itself holds no
// shared mutable state beyond the immutable client + logger + clock.
func TestManager_ConcurrentSpawnsAreIndependent(t *testing.T) {
	t.Parallel()
	fake := &fakeKeepClient{}
	m := New(fake)

	const n = 50
	ids := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			id, err := m.Spawn(context.Background(), SpawnParams{
				ManifestID:  "mfst-1",
				LeadHumanID: "human-1",
			})
			ids[i] = id
			errs[i] = err
		}(i)
	}
	wg.Wait()

	seen := make(map[string]int, n)
	for i, id := range ids {
		if errs[i] != nil {
			t.Fatalf("Spawn[%d] returned error: %v", i, errs[i])
		}
		if id == "" {
			t.Fatalf("Spawn[%d] returned empty id", i)
		}
		if prev, dup := seen[id]; dup {
			t.Fatalf("Spawn[%d] returned duplicate id %q (also seen at %d)", i, id, prev)
		}
		seen[id] = i
	}
}

// TestManager_NewPanicsOnNilClient — passing a nil LocalKeepClient is
// a programmer error and panics with a clear message; mirrors
// keepclient.WithBaseURL's panic discipline.
func TestManager_NewPanicsOnNilClient(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("New(nil) did not panic")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("New(nil) panic value type = %T, want string", r)
		}
		if !strings.Contains(msg, "lifecycle") || !strings.Contains(msg, "nil") {
			t.Fatalf("New(nil) panic message = %q, want a clear lifecycle/nil message", msg)
		}
	}()
	_ = New(nil)
}
