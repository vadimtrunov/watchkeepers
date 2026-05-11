package coordinator

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/notebook"
)

type fakePendingLessonForgetter struct {
	mu       sync.Mutex
	errOut   error
	gotID    string
	gotNow   time.Time
	calls    int
	observed []string
}

func (f *fakePendingLessonForgetter) ForgetPendingLesson(ctx context.Context, id string, now time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.gotID = id
	f.gotNow = now
	f.observed = append(f.observed, id)
	if err := ctx.Err(); err != nil {
		return err
	}
	return f.errOut
}

func newTestForgetDMHandler(t *testing.T, f PendingLessonForgetter) *ForgetDMHandler {
	t.Helper()
	return newForgetDMHandlerWithClock(f, func() time.Time {
		return time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	})
}

func TestForgetDMHandler_ConstructorPanicsOnNil(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("NewForgetDMHandler(nil): expected panic")
		}
	}()
	NewForgetDMHandler(nil)
}

func TestForgetDMHandler_ConstructorWithClockPanicsOnNilClock(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("newForgetDMHandlerWithClock(f, nil): expected panic")
		}
	}()
	newForgetDMHandlerWithClock(&fakePendingLessonForgetter{}, nil)
}

func TestForgetDMHandler_NonMatchingBody(t *testing.T) {
	t.Parallel()
	f := &fakePendingLessonForgetter{}
	h := newTestForgetDMHandler(t, f)

	for _, body := range []string{
		"",
		"hello",
		"forgetfully ignore this",
		"please forget about it",
		"do not forget the meeting",
	} {
		res, err := h.Handle(context.Background(), body)
		if err != nil {
			t.Fatalf("body %q: err = %v", body, err)
		}
		if res.Matched {
			t.Errorf("body %q: Matched=true; want false", body)
		}
	}
	if f.calls != 0 {
		t.Errorf("forgetter.calls = %d; want 0 (no match should never dispatch)", f.calls)
	}
}

func TestForgetDMHandler_MatchedHappyPath(t *testing.T) {
	t.Parallel()
	f := &fakePendingLessonForgetter{}
	h := newTestForgetDMHandler(t, f)

	const validID = "01900000-0000-7000-8000-000000000123"
	res, err := h.Handle(context.Background(), "forget "+validID)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.Matched {
		t.Errorf("Matched=false; want true")
	}
	if !res.Forgotten {
		t.Errorf("Forgotten=false; want true")
	}
	if res.EntryID != validID {
		t.Errorf("EntryID = %q, want %q", res.EntryID, validID)
	}
	if res.Refusal != "" {
		t.Errorf("Refusal = %q, want empty", res.Refusal)
	}
	if f.gotID != validID {
		t.Errorf("forgetter received %q, want %q", f.gotID, validID)
	}
	// The handler MUST forward its clock-stamped `now` so the
	// underlying notebook layer can pin the cooling-off boundary.
	wantNow := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	if !f.gotNow.Equal(wantNow) {
		t.Errorf("forgetter.gotNow = %v, want %v", f.gotNow, wantNow)
	}
}

func TestForgetDMHandler_CaseInsensitivePrefix(t *testing.T) {
	t.Parallel()
	f := &fakePendingLessonForgetter{}
	h := newTestForgetDMHandler(t, f)

	const validID = "01900000-0000-7000-8000-000000000124"
	for _, body := range []string{
		"forget " + validID,
		"FORGET " + validID,
		"Forget " + validID,
		"fOrGeT " + validID,
	} {
		res, err := h.Handle(context.Background(), body)
		if err != nil {
			t.Fatalf("body %q: err: %v", body, err)
		}
		if !res.Matched || !res.Forgotten {
			t.Errorf("body %q: Matched=%v Forgotten=%v; want both true", body, res.Matched, res.Forgotten)
		}
	}
}

func TestForgetDMHandler_NormalisesUUIDCase(t *testing.T) {
	t.Parallel()
	f := &fakePendingLessonForgetter{}
	h := newTestForgetDMHandler(t, f)

	const uppercaseID = "01900000-0000-7000-8000-000000000ABC"
	res, _ := h.Handle(context.Background(), "forget "+uppercaseID)
	if res.EntryID != strings.ToLower(uppercaseID) {
		t.Errorf("EntryID = %q, want lower-case %q", res.EntryID, strings.ToLower(uppercaseID))
	}
}

func TestForgetDMHandler_LeadingTrailingWhitespace(t *testing.T) {
	t.Parallel()
	f := &fakePendingLessonForgetter{}
	h := newTestForgetDMHandler(t, f)

	const validID = "01900000-0000-7000-8000-000000000125"
	res, _ := h.Handle(context.Background(), "  \tforget  "+validID+"  \n")
	if !res.Forgotten {
		t.Errorf("Forgotten=false; want true (whitespace tolerance)")
	}
}

func TestForgetDMHandler_BareForget_MissingArg(t *testing.T) {
	t.Parallel()
	f := &fakePendingLessonForgetter{}
	h := newTestForgetDMHandler(t, f)

	res, err := h.Handle(context.Background(), "forget")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !res.Matched {
		t.Errorf("Matched=false; want true (prefix did match)")
	}
	if !strings.Contains(res.Refusal, "missing entry id") {
		t.Errorf("Refusal = %q, want contains `missing entry id`", res.Refusal)
	}
	if f.calls != 0 {
		t.Errorf("forgetter.calls = %d; want 0 (refusal before dispatch)", f.calls)
	}
}

func TestForgetDMHandler_InvalidUUID(t *testing.T) {
	t.Parallel()
	f := &fakePendingLessonForgetter{}
	h := newTestForgetDMHandler(t, f)

	const canary = "GARBAGE_NOT_A_UUID_DO_NOT_LEAK"
	for _, body := range []string{
		"forget not-a-uuid",
		"forget " + canary,
		"forget 0190",
		"forget 01900000-0000-7000-8000-XXXXXXXXXXXX",
	} {
		res, err := h.Handle(context.Background(), body)
		if err != nil {
			t.Fatalf("body %q: err: %v", body, err)
		}
		if !res.Matched {
			t.Errorf("body %q: Matched=false; want true", body)
		}
		if !strings.Contains(res.Refusal, "must be a canonical UUID") {
			t.Errorf("body %q: Refusal = %q, want canonical-uuid notice", body, res.Refusal)
		}
		if strings.Contains(res.Refusal, canary) {
			t.Errorf("body %q: Refusal LEAKED canary %q", body, canary)
		}
		if res.EntryID != "" {
			t.Errorf("body %q: EntryID = %q, want empty", body, res.EntryID)
		}
	}
	if f.calls != 0 {
		t.Errorf("forgetter.calls = %d; want 0", f.calls)
	}
}

func TestForgetDMHandler_NotFoundClassifiedAsRefusal(t *testing.T) {
	t.Parallel()
	f := &fakePendingLessonForgetter{errOut: notebook.ErrNotFound}
	h := newTestForgetDMHandler(t, f)

	const validID = "01900000-0000-7000-8000-000000000126"
	res, err := h.Handle(context.Background(), "forget "+validID)
	if err != nil {
		t.Fatalf("err = %v, want nil (NotFound classified as refusal)", err)
	}
	if !res.Matched || res.Forgotten || !strings.Contains(res.Refusal, "no notebook entry") {
		t.Errorf("res = %+v, want refusal with `no notebook entry` text", res)
	}
}

func TestForgetDMHandler_NotPendingLessonClassifiedAsRefusal(t *testing.T) {
	t.Parallel()
	// Iter-1 codex Major #1: the notebook layer rejects rows that
	// exist but are not cooling-off lessons. The handler MUST surface
	// a canonical refusal that does NOT disclose which predicate
	// failed (category vs supersession vs cooling-off boundary).
	f := &fakePendingLessonForgetter{errOut: notebook.ErrNotPendingLesson}
	h := newTestForgetDMHandler(t, f)

	const validID = "01900000-0000-7000-8000-000000000130"
	res, err := h.Handle(context.Background(), "forget "+validID)
	if err != nil {
		t.Fatalf("err = %v, want nil (ErrNotPendingLesson classified)", err)
	}
	if res.Forgotten {
		t.Errorf("Forgotten=true; want false")
	}
	if !strings.Contains(res.Refusal, "does not match a pending lesson") {
		t.Errorf("Refusal = %q, want `does not match a pending lesson` text", res.Refusal)
	}
	// PII discipline: must NOT disclose the predicate that failed.
	for _, leak := range []string{"category", "lesson category", "active_after", "superseded", "already active"} {
		if strings.Contains(strings.ToLower(res.Refusal), strings.ToLower(leak)) && leak != "already active" {
			t.Errorf("Refusal LEAKED predicate %q; refusal = %q", leak, res.Refusal)
		}
	}
}

func TestForgetDMHandler_InvalidEntryClassifiedAsRefusal(t *testing.T) {
	t.Parallel()
	f := &fakePendingLessonForgetter{errOut: notebook.ErrInvalidEntry}
	h := newTestForgetDMHandler(t, f)

	const validID = "01900000-0000-7000-8000-000000000127"
	res, err := h.Handle(context.Background(), "forget "+validID)
	if err != nil {
		t.Fatalf("err = %v, want nil (ErrInvalidEntry classified)", err)
	}
	if !strings.Contains(res.Refusal, "non-canonical") {
		t.Errorf("Refusal = %q, want contains `non-canonical`", res.Refusal)
	}
}

func TestForgetDMHandler_UnclassifiedErrorBubbles(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("notebook: pipe full")
	f := &fakePendingLessonForgetter{errOut: sentinel}
	h := newTestForgetDMHandler(t, f)

	const validID = "01900000-0000-7000-8000-000000000128"
	res, err := h.Handle(context.Background(), "forget "+validID)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want wraps sentinel", err)
	}
	if !strings.HasPrefix(err.Error(), "coordinator: forget_dm_handler:") {
		t.Errorf("err = %q, want prefix `coordinator: forget_dm_handler:`", err.Error())
	}
	if res.EntryID != validID {
		t.Errorf("EntryID = %q, want %q (caller still gets the parsed id)", res.EntryID, validID)
	}
}

func TestForgetDMHandler_ContextCancel(t *testing.T) {
	t.Parallel()
	f := &fakePendingLessonForgetter{}
	h := newTestForgetDMHandler(t, f)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	const validID = "01900000-0000-7000-8000-000000000129"
	res, err := h.Handle(ctx, "forget "+validID)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	// Even on cancellation, the caller learns the prefix matched.
	if !res.Matched {
		t.Errorf("Matched=false on cancel; want true (prefix did match)")
	}
	if f.calls != 0 {
		t.Errorf("forgetter.calls = %d; want 0 (cancel short-circuits before dispatch)", f.calls)
	}
}

func TestForgetDMHandler_NoPrefixTokenBoundary(t *testing.T) {
	t.Parallel()
	f := &fakePendingLessonForgetter{}
	h := newTestForgetDMHandler(t, f)

	// `forgetabc` must NOT match — the boundary discipline. Mirrors
	// M8.2.c lesson #4 "discriminant patterns".
	res, _ := h.Handle(context.Background(), "forgetabc 01900000-0000-7000-8000-000000000130")
	if res.Matched {
		t.Errorf("body `forgetabc <id>`: Matched=true; want false (no word boundary)")
	}
}

func TestForgetDMHandler_Concurrent16Goroutines(t *testing.T) {
	t.Parallel()
	f := &fakePendingLessonForgetter{}
	h := newTestForgetDMHandler(t, f)

	const n = 16
	const validID = "01900000-0000-7000-8000-000000000131"
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, _ = h.Handle(context.Background(), "forget "+validID)
		}()
	}
	wg.Wait()
	if f.calls != n {
		t.Fatalf("calls = %d; want %d", f.calls, n)
	}
}
