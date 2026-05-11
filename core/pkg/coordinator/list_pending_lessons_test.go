package coordinator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/notebook"
	agentruntime "github.com/vadimtrunov/watchkeepers/core/pkg/runtime"
)

type fakePendingLister struct {
	mu       sync.Mutex
	rows     []notebook.PendingLessonRow
	errOut   error
	gotNow   time.Time
	gotLimit int
	calls    int
}

func (f *fakePendingLister) PendingLessons(_ context.Context, now time.Time, limit int) ([]notebook.PendingLessonRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.gotNow = now
	f.gotLimit = limit
	if f.errOut != nil {
		return nil, f.errOut
	}
	out := make([]notebook.PendingLessonRow, len(f.rows))
	copy(out, f.rows)
	return out, nil
}

func listHandler(t *testing.T, l PendingLessonLister, clock func() time.Time) agentruntime.ToolHandler {
	t.Helper()
	return newListPendingLessonsHandlerWithClock(l, clock)
}

func TestListPendingLessons_Name(t *testing.T) {
	t.Parallel()
	if ListPendingLessonsName != "list_pending_lessons" {
		t.Fatalf("ListPendingLessonsName = %q, want %q", ListPendingLessonsName, "list_pending_lessons")
	}
}

func TestListPendingLessons_ConstructorPanicsOnNilDeps(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("NewListPendingLessonsHandler(nil): expected panic")
		}
	}()
	NewListPendingLessonsHandler(nil)
}

func TestListPendingLessons_HandlerWithClockPanicsOnNilClock(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("newListPendingLessonsHandlerWithClock(l, nil): expected panic")
		}
	}()
	newListPendingLessonsHandlerWithClock(&fakePendingLister{}, nil)
}

func TestListPendingLessons_DefaultLimit(t *testing.T) {
	t.Parallel()
	l := &fakePendingLister{}
	h := listHandler(t, l, time.Now)

	if _, err := h(context.Background(), agentruntime.ToolCall{Arguments: map[string]any{}}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if l.gotLimit != defaultListPendingLessonsLimit {
		t.Errorf("forwarded limit = %d, want default %d", l.gotLimit, defaultListPendingLessonsLimit)
	}
}

func TestListPendingLessons_LimitRefusals(t *testing.T) {
	t.Parallel()
	l := &fakePendingLister{}
	h := listHandler(t, l, time.Now)

	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"non-number", map[string]any{"limit": "ten"}, "limit must be a number"},
		{"non-integer", map[string]any{"limit": 1.5}, "limit must be an integer"},
		{"zero", map[string]any{"limit": 0}, "limit must be ≥ 1"},
		{"negative", map[string]any{"limit": -3}, "limit must be ≥ 1"},
		{"too large", map[string]any{"limit": float64(maxListPendingLessonsLimit + 1)}, "limit must be ≤"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := h(context.Background(), agentruntime.ToolCall{Arguments: c.args})
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if !strings.Contains(res.Error, c.want) {
				t.Errorf("refusal %q does not contain %q", res.Error, c.want)
			}
			if !strings.HasPrefix(res.Error, listPendingRefusalPrefix) {
				t.Errorf("refusal %q missing prefix %q", res.Error, listPendingRefusalPrefix)
			}
		})
	}
}

func TestListPendingLessons_HappyPath(t *testing.T) {
	t.Parallel()
	fixedNow := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	tenHoursLater := fixedNow.Add(10 * time.Hour).UnixMilli()
	rows := []notebook.PendingLessonRow{{
		ID:          "01900000-0000-7000-8000-000000000010",
		Subject:     "find_overdue_tickets: rate_limited",
		CreatedAt:   fixedNow.UnixMilli(),
		ActiveAfter: tenHoursLater,
	}}
	l := &fakePendingLister{rows: rows}
	h := listHandler(t, l, func() time.Time { return fixedNow })

	res, err := h(context.Background(), agentruntime.ToolCall{Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Error != "" {
		t.Fatalf("refusal: %q", res.Error)
	}

	lessons, ok := res.Output["lessons"].([]map[string]any)
	if !ok {
		t.Fatalf("lessons not []map[string]any; type = %T", res.Output["lessons"])
	}
	if len(lessons) != 1 {
		t.Fatalf("got %d lessons; want 1", len(lessons))
	}
	got := lessons[0]
	if got["id"] != rows[0].ID {
		t.Errorf("id = %v, want %v", got["id"], rows[0].ID)
	}
	if got["subject"] != "find_overdue_tickets: rate_limited" {
		t.Errorf("subject = %v", got["subject"])
	}
	if got["cooling_off_hours_left"] != 10 {
		t.Errorf("cooling_off_hours_left = %v, want 10", got["cooling_off_hours_left"])
	}
	if _, present := got["content"]; present {
		t.Errorf("Output projection LEAKED `content` field; want dropped")
	}
}

// TestListPendingLessons_PendingLessonRowDoesNotExposeContent asserts
// the PII discipline AT THE TYPE LAYER (iter-1 codex Minor): the
// PendingLessonRow struct intentionally has no `Content` field, so
// the projection cannot leak the lesson body even via a future
// fmt-dump / debug-log path. Pinned via a compile-time reflection
// scan rather than a runtime canary so a future PR that re-adds the
// field fails CI at build time rather than at a "did we remember to
// drop it?" runtime grep.
func TestListPendingLessons_PendingLessonRowDoesNotExposeContent(t *testing.T) {
	t.Parallel()
	rt := reflect.TypeOf(notebook.PendingLessonRow{})
	for i := 0; i < rt.NumField(); i++ {
		name := rt.Field(i).Name
		if strings.EqualFold(name, "Content") || strings.EqualFold(name, "Body") {
			t.Errorf("PendingLessonRow exposes field %q; M8.3 iter-1 codex Minor says drop it at the type layer", name)
		}
	}
}

func TestListPendingLessons_ForwardsListerError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("notebook: pipe broken")
	l := &fakePendingLister{errOut: sentinel}
	h := listHandler(t, l, time.Now)

	_, err := h(context.Background(), agentruntime.ToolCall{Arguments: map[string]any{}})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want wraps %v", err, sentinel)
	}
	if !strings.HasPrefix(err.Error(), "coordinator: list_pending_lessons:") {
		t.Errorf("err prefix = %q", err.Error())
	}
}

func TestListPendingLessons_ContextCancel(t *testing.T) {
	t.Parallel()
	l := &fakePendingLister{}
	h := listHandler(t, l, time.Now)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := h(ctx, agentruntime.ToolCall{Arguments: map[string]any{}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestListPendingLessons_NoLogAppend_SourceGrep(t *testing.T) {
	t.Parallel()
	_, thisFile, _, _ := runtime.Caller(0)
	srcPath := filepath.Join(filepath.Dir(thisFile), "list_pending_lessons.go")
	body, err := os.ReadFile(srcPath) //nolint:gosec // fixed repo path.
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	src := string(body)
	noLineComments := regexp.MustCompile(`(?m)^\s*//.*$`).ReplaceAllString(src, "")
	noBlockComments := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(noLineComments, "")
	if strings.Contains(noBlockComments, "keeperslog.") {
		t.Errorf("list_pending_lessons.go references keeperslog outside comments")
	}
	if strings.Contains(noBlockComments, ".Append(") {
		t.Errorf("list_pending_lessons.go calls .Append(...) outside comments")
	}
}

func TestListPendingLessons_CoolingOffClampsToZeroWhenPast(t *testing.T) {
	t.Parallel()
	fixedNow := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	l := &fakePendingLister{rows: []notebook.PendingLessonRow{{
		ID: "01900000-0000-7000-8000-000000000020", Subject: "x",
		ActiveAfter: fixedNow.Add(-time.Hour).UnixMilli(),
	}}}
	h := listHandler(t, l, func() time.Time { return fixedNow })

	res, _ := h(context.Background(), agentruntime.ToolCall{Arguments: map[string]any{}})
	lessons, _ := res.Output["lessons"].([]map[string]any)
	if lessons[0]["cooling_off_hours_left"] != 0 {
		t.Errorf("cooling_off_hours_left = %v, want 0 (clamped)", lessons[0]["cooling_off_hours_left"])
	}
}

func TestListPendingLessons_Concurrent16Goroutines(t *testing.T) {
	t.Parallel()
	l := &fakePendingLister{}
	h := listHandler(t, l, time.Now)

	const n = 16
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, _ = h(context.Background(), agentruntime.ToolCall{Arguments: map[string]any{}})
		}()
	}
	wg.Wait()
	if l.calls != n {
		t.Fatalf("calls = %d, want %d", l.calls, n)
	}
}
