package k2k_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/k2k"
)

// fixedClock returns a deterministic time.Time so test assertions on
// `OpenedAt` / `ClosedAt` are byte-for-byte stable. Mirrors the
// `saga.fixedClock` helper in `core/pkg/spawn/saga/dao_memory_test.go`.
func fixedClock() func() time.Time {
	t := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return t }
}

func newRepo(t *testing.T) *k2k.MemoryRepository {
	t.Helper()
	return k2k.NewMemoryRepository(fixedClock(), nil)
}

func validParams() k2k.OpenParams {
	return k2k.OpenParams{
		OrganizationID: uuid.New(),
		Participants:   []string{"bot-a", "bot-b"},
		Subject:        "review #42",
		TokenBudget:    1000,
		CorrelationID:  uuid.New(),
	}
}

func TestMemoryRepository_Open_HappyPath(t *testing.T) {
	t.Parallel()

	r := newRepo(t)
	params := validParams()

	got, err := r.Open(context.Background(), params)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got.ID == uuid.Nil {
		t.Errorf("ID = uuid.Nil, want non-zero (minted by repository)")
	}
	if got.OrganizationID != params.OrganizationID {
		t.Errorf("OrganizationID = %v, want %v", got.OrganizationID, params.OrganizationID)
	}
	if got.Subject != params.Subject {
		t.Errorf("Subject = %q, want %q", got.Subject, params.Subject)
	}
	if got.Status != k2k.StatusOpen {
		t.Errorf("Status = %q, want %q", got.Status, k2k.StatusOpen)
	}
	if got.TokenBudget != params.TokenBudget {
		t.Errorf("TokenBudget = %d, want %d", got.TokenBudget, params.TokenBudget)
	}
	if got.TokensUsed != 0 {
		t.Errorf("TokensUsed = %d, want 0 on fresh open", got.TokensUsed)
	}
	if got.OpenedAt.IsZero() {
		t.Errorf("OpenedAt = zero, want stamped")
	}
	if !got.ClosedAt.IsZero() {
		t.Errorf("ClosedAt = %v, want zero on fresh open", got.ClosedAt)
	}
	if got.CorrelationID != params.CorrelationID {
		t.Errorf("CorrelationID = %v, want %v", got.CorrelationID, params.CorrelationID)
	}
	if len(got.Participants) != 2 {
		t.Errorf("Participants len = %d, want 2", len(got.Participants))
	}
}

func TestMemoryRepository_Open_RefusesCancelledCtx(t *testing.T) {
	t.Parallel()

	r := newRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := r.Open(ctx, validParams())
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Open with cancelled ctx: err = %v, want context.Canceled", err)
	}
}

func TestMemoryRepository_Open_RejectsEmptyOrganization(t *testing.T) {
	t.Parallel()

	r := newRepo(t)
	p := validParams()
	p.OrganizationID = uuid.Nil

	_, err := r.Open(context.Background(), p)
	if !errors.Is(err, k2k.ErrEmptyOrganization) {
		t.Errorf("Open zero-org: err = %v, want ErrEmptyOrganization", err)
	}
}

func TestMemoryRepository_Open_RejectsEmptySubject(t *testing.T) {
	t.Parallel()

	for _, subject := range []string{"", "   ", "\t\n"} {
		subject := subject
		t.Run(subject, func(t *testing.T) {
			t.Parallel()
			r := newRepo(t)
			p := validParams()
			p.Subject = subject

			_, err := r.Open(context.Background(), p)
			if !errors.Is(err, k2k.ErrEmptySubject) {
				t.Errorf("Open empty/whitespace subject %q: err = %v, want ErrEmptySubject", subject, err)
			}
		})
	}
}

func TestMemoryRepository_Open_RejectsEmptyParticipants(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		participants []string
	}{
		{"nil", nil},
		{"empty", []string{}},
		{"contains empty", []string{"bot-a", ""}},
		{"contains whitespace", []string{"bot-a", "  "}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRepo(t)
			p := validParams()
			p.Participants = tc.participants

			_, err := r.Open(context.Background(), p)
			if !errors.Is(err, k2k.ErrEmptyParticipants) {
				t.Errorf("Open with %s: err = %v, want ErrEmptyParticipants", tc.name, err)
			}
		})
	}
}

func TestMemoryRepository_Open_RejectsNegativeBudget(t *testing.T) {
	t.Parallel()

	r := newRepo(t)
	p := validParams()
	p.TokenBudget = -1

	_, err := r.Open(context.Background(), p)
	if !errors.Is(err, k2k.ErrInvalidTokenBudget) {
		t.Errorf("Open negative budget: err = %v, want ErrInvalidTokenBudget", err)
	}
}

func TestMemoryRepository_Open_AcceptsZeroBudget(t *testing.T) {
	t.Parallel()

	r := newRepo(t)
	p := validParams()
	p.TokenBudget = 0

	got, err := r.Open(context.Background(), p)
	if err != nil {
		t.Fatalf("Open zero budget: %v", err)
	}
	if got.TokenBudget != 0 {
		t.Errorf("TokenBudget = %d, want 0", got.TokenBudget)
	}
}

func TestMemoryRepository_Open_DefensiveCopyOnInput(t *testing.T) {
	t.Parallel()

	r := newRepo(t)
	p := validParams()
	p.Participants = []string{"bot-a", "bot-b"}

	got, err := r.Open(context.Background(), p)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Mutate the source slice; the stored row must NOT pick up
	// the mutation.
	p.Participants[0] = "MUTATED"
	p.Participants = append(p.Participants, "EXTRA")

	again, err := r.Get(context.Background(), got.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(again.Participants) != 2 || again.Participants[0] != "bot-a" {
		t.Errorf("defensive copy on Open leaked caller mutation: %v", again.Participants)
	}
}

func TestMemoryRepository_Get_DefensiveCopyOnRead(t *testing.T) {
	t.Parallel()

	r := newRepo(t)
	got, err := r.Open(context.Background(), validParams())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Mutate the returned slice; a second Get must observe the
	// original participants.
	got.Participants[0] = "MUTATED"

	again, err := r.Get(context.Background(), got.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if again.Participants[0] == "MUTATED" {
		t.Errorf("defensive copy on Get leaked caller mutation: %v", again.Participants)
	}
}

func TestMemoryRepository_Get_UnknownID_ReturnsTypedSentinel(t *testing.T) {
	t.Parallel()

	r := newRepo(t)
	id := uuid.New()

	_, err := r.Get(context.Background(), id)
	if !errors.Is(err, k2k.ErrConversationNotFound) {
		t.Errorf("Get unknown id: err = %v, want ErrConversationNotFound", err)
	}
	if !strings.Contains(err.Error(), id.String()) {
		t.Errorf("Get unknown id: error must mention requested id: %v", err)
	}
}

func TestMemoryRepository_Get_RefusesCancelledCtx(t *testing.T) {
	t.Parallel()

	r := newRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := r.Get(ctx, uuid.New())
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Get with cancelled ctx: err = %v, want context.Canceled", err)
	}
}

func TestMemoryRepository_List_FilterByOrg(t *testing.T) {
	t.Parallel()

	r := newRepo(t)
	orgA := uuid.New()
	orgB := uuid.New()

	for i := 0; i < 3; i++ {
		p := validParams()
		p.OrganizationID = orgA
		if _, err := r.Open(context.Background(), p); err != nil {
			t.Fatalf("Open orgA: %v", err)
		}
	}
	for i := 0; i < 2; i++ {
		p := validParams()
		p.OrganizationID = orgB
		if _, err := r.Open(context.Background(), p); err != nil {
			t.Fatalf("Open orgB: %v", err)
		}
	}

	gotA, err := r.List(context.Background(), k2k.ListFilter{OrganizationID: orgA})
	if err != nil {
		t.Fatalf("List orgA: %v", err)
	}
	if len(gotA) != 3 {
		t.Errorf("List orgA len = %d, want 3", len(gotA))
	}
	for _, row := range gotA {
		if row.OrganizationID != orgA {
			t.Errorf("List orgA leaked row from %v", row.OrganizationID)
		}
	}

	gotB, err := r.List(context.Background(), k2k.ListFilter{OrganizationID: orgB})
	if err != nil {
		t.Fatalf("List orgB: %v", err)
	}
	if len(gotB) != 2 {
		t.Errorf("List orgB len = %d, want 2", len(gotB))
	}
}

func TestMemoryRepository_List_FilterByStatus(t *testing.T) {
	t.Parallel()

	r := newRepo(t)
	org := uuid.New()

	openIDs := make([]uuid.UUID, 0, 2)
	for i := 0; i < 2; i++ {
		p := validParams()
		p.OrganizationID = org
		row, err := r.Open(context.Background(), p)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		openIDs = append(openIDs, row.ID)
	}
	// Archive one of the two.
	if err := r.Close(context.Background(), openIDs[0], "done"); err != nil {
		t.Fatalf("Close: %v", err)
	}

	open, err := r.List(context.Background(), k2k.ListFilter{OrganizationID: org, Status: k2k.StatusOpen})
	if err != nil {
		t.Fatalf("List open: %v", err)
	}
	if len(open) != 1 || open[0].ID != openIDs[1] {
		t.Errorf("List open: got %d rows, want 1 with id %s; got = %+v", len(open), openIDs[1], open)
	}

	archived, err := r.List(context.Background(), k2k.ListFilter{OrganizationID: org, Status: k2k.StatusArchived})
	if err != nil {
		t.Fatalf("List archived: %v", err)
	}
	if len(archived) != 1 || archived[0].ID != openIDs[0] {
		t.Errorf("List archived: got %d rows, want 1 with id %s", len(archived), openIDs[0])
	}
}

func TestMemoryRepository_List_RejectsEmptyOrg(t *testing.T) {
	t.Parallel()

	r := newRepo(t)
	_, err := r.List(context.Background(), k2k.ListFilter{})
	if !errors.Is(err, k2k.ErrEmptyOrganization) {
		t.Errorf("List zero-org: err = %v, want ErrEmptyOrganization", err)
	}
}

func TestMemoryRepository_List_RejectsInvalidStatus(t *testing.T) {
	t.Parallel()

	r := newRepo(t)
	_, err := r.List(context.Background(), k2k.ListFilter{
		OrganizationID: uuid.New(),
		Status:         k2k.Status("bogus"),
	})
	if !errors.Is(err, k2k.ErrInvalidStatus) {
		t.Errorf("List bogus status: err = %v, want ErrInvalidStatus", err)
	}
}

func TestMemoryRepository_Close_HappyPath(t *testing.T) {
	t.Parallel()

	r := newRepo(t)
	row, err := r.Open(context.Background(), validParams())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if err := r.Close(context.Background(), row.ID, "summary"); err != nil {
		t.Fatalf("Close: %v", err)
	}

	again, err := r.Get(context.Background(), row.ID)
	if err != nil {
		t.Fatalf("Get post-close: %v", err)
	}
	if again.Status != k2k.StatusArchived {
		t.Errorf("Status post-close = %q, want %q", again.Status, k2k.StatusArchived)
	}
	if again.ClosedAt.IsZero() {
		t.Errorf("ClosedAt post-close = zero, want stamped")
	}
	if again.CloseReason != "summary" {
		t.Errorf("CloseReason = %q, want %q", again.CloseReason, "summary")
	}
}

func TestMemoryRepository_Close_UnknownID(t *testing.T) {
	t.Parallel()

	r := newRepo(t)
	err := r.Close(context.Background(), uuid.New(), "")
	if !errors.Is(err, k2k.ErrConversationNotFound) {
		t.Errorf("Close unknown id: err = %v, want ErrConversationNotFound", err)
	}
}

func TestMemoryRepository_Close_DoubleArchiveRejected(t *testing.T) {
	t.Parallel()

	r := newRepo(t)
	row, err := r.Open(context.Background(), validParams())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := r.Close(context.Background(), row.ID, "first"); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	err = r.Close(context.Background(), row.ID, "second")
	if !errors.Is(err, k2k.ErrAlreadyArchived) {
		t.Errorf("second Close: err = %v, want ErrAlreadyArchived", err)
	}
}

func TestMemoryRepository_IncTokens_HappyPath(t *testing.T) {
	t.Parallel()

	r := newRepo(t)
	row, err := r.Open(context.Background(), validParams())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	got, err := r.IncTokens(context.Background(), row.ID, 100)
	if err != nil {
		t.Fatalf("IncTokens: %v", err)
	}
	if got != 100 {
		t.Errorf("IncTokens returned = %d, want 100", got)
	}

	got, err = r.IncTokens(context.Background(), row.ID, 250)
	if err != nil {
		t.Fatalf("IncTokens 2: %v", err)
	}
	if got != 350 {
		t.Errorf("IncTokens returned = %d, want 350", got)
	}

	final, err := r.Get(context.Background(), row.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.TokensUsed != 350 {
		t.Errorf("TokensUsed = %d, want 350", final.TokensUsed)
	}
}

func TestMemoryRepository_IncTokens_RejectsNonPositiveDelta(t *testing.T) {
	t.Parallel()

	for _, delta := range []int64{0, -1, -1000} {
		delta := delta
		t.Run(fmt.Sprintf("delta=%d", delta), func(t *testing.T) {
			t.Parallel()
			r := newRepo(t)
			row, err := r.Open(context.Background(), validParams())
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			_, err = r.IncTokens(context.Background(), row.ID, delta)
			if !errors.Is(err, k2k.ErrInvalidTokenDelta) {
				t.Errorf("IncTokens delta=%d: err = %v, want ErrInvalidTokenDelta", delta, err)
			}
		})
	}
}

func TestMemoryRepository_IncTokens_UnknownID(t *testing.T) {
	t.Parallel()

	r := newRepo(t)
	_, err := r.IncTokens(context.Background(), uuid.New(), 10)
	if !errors.Is(err, k2k.ErrConversationNotFound) {
		t.Errorf("IncTokens unknown id: err = %v, want ErrConversationNotFound", err)
	}
}

func TestMemoryRepository_IncTokens_RefusesArchivedRow(t *testing.T) {
	t.Parallel()

	r := newRepo(t)
	row, err := r.Open(context.Background(), validParams())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := r.Close(context.Background(), row.ID, "done"); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, err = r.IncTokens(context.Background(), row.ID, 10)
	if !errors.Is(err, k2k.ErrAlreadyArchived) {
		t.Errorf("IncTokens on archived row: err = %v, want ErrAlreadyArchived", err)
	}
}

// TestMemoryRepository_IncTokens_ConcurrentIncrements pins the
// read-modify-write atomicity contract: 16 goroutines each increment
// 100 times with delta=1 must leave TokensUsed exactly 1600. A naive
// RLock-read + Lock-write split would produce a lost update and the
// final count would be lower.
func TestMemoryRepository_IncTokens_ConcurrentIncrements(t *testing.T) {
	t.Parallel()

	r := newRepo(t)
	row, err := r.Open(context.Background(), validParams())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	const (
		goroutines       = 16
		incrementsPerGor = 100
		delta            = int64(1)
	)

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < incrementsPerGor; i++ {
				if _, err := r.IncTokens(context.Background(), row.ID, delta); err != nil {
					t.Errorf("IncTokens: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	final, err := r.Get(context.Background(), row.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	want := int64(goroutines * incrementsPerGor)
	if final.TokensUsed != want {
		t.Errorf("TokensUsed after concurrent inc = %d, want %d (lost-update bug?)", final.TokensUsed, want)
	}
}

// TestMemoryRepository_PerOrgIsolation pins the per-tenant filter
// discipline that mirrors the Postgres RLS policy on the
// `k2k_conversations` table. The in-memory store enforces it
// programmatically via the [ListFilter.OrganizationID] field;
// production Postgres enforces it via the per-org RLS policy from
// migration 029 keyed off `watchkeeper.org`. This test pins the
// in-memory behaviour so a regression at the consumer layer
// surfaces before it reaches the integration test against Postgres.
func TestMemoryRepository_PerOrgIsolation(t *testing.T) {
	t.Parallel()

	r := newRepo(t)
	orgA := uuid.New()
	orgB := uuid.New()

	pa := validParams()
	pa.OrganizationID = orgA
	rowA, err := r.Open(context.Background(), pa)
	if err != nil {
		t.Fatalf("Open orgA: %v", err)
	}

	pb := validParams()
	pb.OrganizationID = orgB
	if _, err := r.Open(context.Background(), pb); err != nil {
		t.Fatalf("Open orgB: %v", err)
	}

	// List under orgB must NOT see the orgA row.
	gotB, err := r.List(context.Background(), k2k.ListFilter{OrganizationID: orgB})
	if err != nil {
		t.Fatalf("List orgB: %v", err)
	}
	for _, row := range gotB {
		if row.ID == rowA.ID {
			t.Errorf("List orgB leaked orgA row %s", rowA.ID)
		}
		if row.OrganizationID != orgB {
			t.Errorf("List orgB returned cross-tenant row from %v", row.OrganizationID)
		}
	}
}

func TestStatus_Validate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		s       k2k.Status
		wantErr bool
	}{
		{k2k.StatusOpen, false},
		{k2k.StatusArchived, false},
		{k2k.Status(""), true},
		{k2k.Status("bogus"), true},
		{k2k.Status("OPEN"), true}, // case-sensitive: lowercase only
	}
	for _, tc := range cases {
		tc := tc
		t.Run(string(tc.s), func(t *testing.T) {
			t.Parallel()
			err := tc.s.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate(%q): err = %v, wantErr = %v", tc.s, err, tc.wantErr)
			}
			if tc.wantErr && !errors.Is(err, k2k.ErrInvalidStatus) {
				t.Errorf("Validate(%q): err = %v, want ErrInvalidStatus", tc.s, err)
			}
		})
	}
}

// TestNewPostgresRepository_PanicsOnNilQuerier pins the
// panic-on-nil-deps discipline established by the saga step
// constructors (see `core/pkg/spawn/*_step.go`). A nil querier is a
// programmer bug at wiring time, not a runtime error to thread through
// error returns.
func TestNewPostgresRepository_PanicsOnNilQuerier(t *testing.T) {
	t.Parallel()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("NewPostgresRepository(nil): no panic")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("panic value = %T, want string", r)
		}
		if !strings.Contains(msg, "querier must not be nil") {
			t.Errorf("panic message = %q, want substring 'querier must not be nil'", msg)
		}
	}()
	k2k.NewPostgresRepository(nil, nil)
}
