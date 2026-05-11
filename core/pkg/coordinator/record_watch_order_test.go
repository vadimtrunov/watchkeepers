package coordinator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	agentruntime "github.com/vadimtrunov/watchkeepers/core/pkg/runtime"
)

// fakeWatchOrderRecorder is the hand-rolled stub the
// record_watch_order tests inject in place of the future
// `*WatchOrderStore` production wiring. Mirrors the M8.2 fake style:
// one capture field per Record arg (so tests assert the forwarded
// payload), one error knob, one count for the concurrency case, all
// guarded by a mutex.
type fakeWatchOrderRecorder struct {
	mu sync.Mutex

	idToReturn         string
	recordedAtToReturn time.Time
	errToReturn        error

	gotSummary    string
	gotDueAt      time.Time
	gotSourceRef  string
	callCount     int
	cancelObserve bool
}

func (f *fakeWatchOrderRecorder) Record(ctx context.Context, ord WatchOrder) (WatchOrderRecord, error) {
	if f.cancelObserve {
		if err := ctx.Err(); err != nil {
			return WatchOrderRecord{}, err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callCount++
	f.gotSummary = ord.Summary
	f.gotDueAt = ord.DueAt
	f.gotSourceRef = ord.SourceRef
	if f.errToReturn != nil {
		return WatchOrderRecord{}, f.errToReturn
	}
	return WatchOrderRecord{ID: f.idToReturn, RecordedAt: f.recordedAtToReturn}, nil
}

// recordWatchOrderHandler is the per-test factory binding a clock for
// deterministic `recorded_at` output.
func recordWatchOrderHandler(t *testing.T, rec WatchOrderRecorder, clock func() time.Time) agentruntime.ToolHandler {
	t.Helper()
	return newRecordWatchOrderHandlerWithClock(rec, clock)
}

func TestRecordWatchOrder_Name(t *testing.T) {
	t.Parallel()
	if RecordWatchOrderName != "record_watch_order" {
		t.Fatalf("RecordWatchOrderName = %q, want %q", RecordWatchOrderName, "record_watch_order")
	}
}

func TestRecordWatchOrder_ConstructorPanicsOnNilDeps(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("NewRecordWatchOrderHandler(nil): expected panic")
		}
	}()
	NewRecordWatchOrderHandler(nil)
}

func TestRecordWatchOrder_HandlerWithClockPanicsOnNilClock(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("newRecordWatchOrderHandlerWithClock(rec, nil): expected panic")
		}
	}()
	newRecordWatchOrderHandlerWithClock(&fakeWatchOrderRecorder{idToReturn: "x"}, nil)
}

func TestRecordWatchOrder_HappyPath_EchoesRecorderRecordedAt(t *testing.T) {
	t.Parallel()
	handlerClock := time.Date(2026, 5, 11, 11, 0, 0, 0, time.UTC)
	recorderClock := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	rec := &fakeWatchOrderRecorder{
		idToReturn:         "01900000-0000-7000-8000-000000000000",
		recordedAtToReturn: recorderClock, // iter-1 codex Minor: recorder is authoritative
	}
	h := recordWatchOrderHandler(t, rec, func() time.Time { return handlerClock })

	res, err := h(context.Background(), agentruntime.ToolCall{Arguments: map[string]any{
		"summary":    "Review the spawn saga PR by EOD",
		"due_at":     "2026-05-20T17:00:00Z",
		"source_ref": "1747000000.123456",
	}})
	if err != nil {
		t.Fatalf("handler returned err: %v", err)
	}
	if res.Error != "" {
		t.Fatalf("handler refusal: %q", res.Error)
	}
	if got, want := res.Output["watch_order_id"], rec.idToReturn; got != want {
		t.Errorf("watch_order_id = %v, want %v", got, want)
	}
	// Output `recorded_at` MUST mirror the recorder's RecordedAt
	// (not the handler's local clock) so the DM ack matches storage.
	if got, want := res.Output["recorded_at"], recorderClock.Format(time.RFC3339); got != want {
		t.Errorf("recorded_at = %v, want recorder's %v (NOT handler's %v)",
			got, want, handlerClock.Format(time.RFC3339))
	}
	if got, want := res.Output["due_at_recorded"], "2026-05-20T17:00:00Z"; got != want {
		t.Errorf("due_at_recorded = %v, want %v", got, want)
	}
	if got := res.Output["source_ref_present"]; got != true {
		t.Errorf("source_ref_present = %v, want true", got)
	}
	if got := res.Output["summary_chars"]; got != 31 {
		t.Errorf("summary_chars = %v, want 31", got)
	}
	if rec.gotSummary != "Review the spawn saga PR by EOD" {
		t.Errorf("recorder.Summary = %q, want full summary", rec.gotSummary)
	}
}

func TestRecordWatchOrder_FallsBackToHandlerClockOnZeroRecordedAt(t *testing.T) {
	t.Parallel()
	// Recorder returns a zero RecordedAt (the "not yet upgraded"
	// fallback path documented on dispatchWatchOrderRecord); the
	// handler's local clock fills the gap so the success Output
	// still carries a meaningful timestamp.
	handlerClock := time.Date(2026, 5, 11, 11, 0, 0, 0, time.UTC)
	rec := &fakeWatchOrderRecorder{
		idToReturn: "01900000-0000-7000-8000-000000000001",
		// recordedAtToReturn left zero on purpose
	}
	h := recordWatchOrderHandler(t, rec, func() time.Time { return handlerClock })

	res, err := h(context.Background(), agentruntime.ToolCall{Arguments: map[string]any{"summary": "x"}})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got, want := res.Output["recorded_at"], handlerClock.Format(time.RFC3339); got != want {
		t.Errorf("recorded_at = %v, want handler fallback %v", got, want)
	}
}

func TestRecordWatchOrder_OptionalArgsOmitted(t *testing.T) {
	t.Parallel()
	rec := &fakeWatchOrderRecorder{idToReturn: "01900000-0000-7000-8000-000000000001"}
	h := recordWatchOrderHandler(t, rec, time.Now)

	res, err := h(context.Background(), agentruntime.ToolCall{Arguments: map[string]any{
		"summary": "minimal order",
	}})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Error != "" {
		t.Fatalf("refusal: %q", res.Error)
	}
	if res.Output["due_at_recorded"] != "" {
		t.Errorf("due_at_recorded = %v, want empty", res.Output["due_at_recorded"])
	}
	if res.Output["source_ref_present"] != false {
		t.Errorf("source_ref_present = %v, want false", res.Output["source_ref_present"])
	}
}

func TestRecordWatchOrder_ArgRefusals(t *testing.T) {
	t.Parallel()
	rec := &fakeWatchOrderRecorder{idToReturn: "id"}
	h := recordWatchOrderHandler(t, rec, time.Now)

	cases := []struct {
		name      string
		args      map[string]any
		wantRefus string
	}{
		{
			name:      "missing summary",
			args:      map[string]any{},
			wantRefus: "missing required arg: summary",
		},
		{
			name:      "non-string summary",
			args:      map[string]any{"summary": 42},
			wantRefus: "summary must be a string",
		},
		{
			name:      "empty summary",
			args:      map[string]any{"summary": ""},
			wantRefus: "summary must be non-empty",
		},
		{
			name:      "summary too long",
			args:      map[string]any{"summary": strings.Repeat("a", maxWatchOrderSummaryChars+1)},
			wantRefus: "summary must be ≤",
		},
		{
			name:      "due_at non-string",
			args:      map[string]any{"summary": "x", "due_at": 1747},
			wantRefus: "due_at must be a string",
		},
		{
			name:      "due_at empty",
			args:      map[string]any{"summary": "x", "due_at": ""},
			wantRefus: "due_at must be non-empty when present",
		},
		{
			name:      "due_at malformed",
			args:      map[string]any{"summary": "x", "due_at": "not-a-date"},
			wantRefus: "must be an RFC3339 UTC timestamp",
		},
		{
			name:      "due_at non-UTC offset",
			args:      map[string]any{"summary": "x", "due_at": "2026-05-20T17:00:00+02:00"},
			wantRefus: "must be in UTC",
		},
		{
			name:      "source_ref non-string",
			args:      map[string]any{"summary": "x", "source_ref": 9},
			wantRefus: "source_ref must be a string",
		},
		{
			// Iter-1 codex Minor: empty source_ref must refuse, not
			// collapse to "omitted".
			name:      "source_ref empty when present",
			args:      map[string]any{"summary": "x", "source_ref": ""},
			wantRefus: "source_ref must be non-empty when present",
		},
		{
			name:      "source_ref too long",
			args:      map[string]any{"summary": "x", "source_ref": strings.Repeat("s", maxWatchOrderSourceRefChars+1)},
			wantRefus: "source_ref must be ≤",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := h(context.Background(), agentruntime.ToolCall{Arguments: c.args})
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if !strings.Contains(res.Error, c.wantRefus) {
				t.Errorf("refusal %q does not contain %q", res.Error, c.wantRefus)
			}
			if !strings.HasPrefix(res.Error, recordWatchRefusalPrefix) {
				t.Errorf("refusal %q missing prefix %q", res.Error, recordWatchRefusalPrefix)
			}
		})
	}
}

func TestRecordWatchOrder_DueAtNegativeZeroOffsetPassesAsUTC(t *testing.T) {
	t.Parallel()
	// Iter-1 critic Nit: RFC3339 §4.3 says `-00:00` is "unknown
	// local offset"; `time.Parse` reports off==0 so the handler's
	// UTC-only check accepts it. Pin the chosen behaviour with a
	// test so a future tightening of the discipline (e.g. refuse
	// `-00:00` explicitly per RFC3339 §4.3) lands as an explicit
	// behaviour change with a CI signal.
	rec := &fakeWatchOrderRecorder{idToReturn: "01900000-0000-7000-8000-000000000002"}
	h := recordWatchOrderHandler(t, rec, time.Now)
	res, err := h(context.Background(), agentruntime.ToolCall{Arguments: map[string]any{
		"summary": "x",
		"due_at":  "2026-05-20T17:00:00-00:00",
	}})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Error != "" {
		t.Errorf("refusal on -00:00: %q (current chosen behaviour: accept)", res.Error)
	}
	if got, want := res.Output["due_at_recorded"], "2026-05-20T17:00:00Z"; got != want {
		t.Errorf("due_at_recorded = %v, want %v (UTC-normalised)", got, want)
	}
}

func TestRecordWatchOrder_RefusalNeverEchoesRawArgs(t *testing.T) {
	t.Parallel()
	rec := &fakeWatchOrderRecorder{idToReturn: "x"}
	h := recordWatchOrderHandler(t, rec, time.Now)

	const summaryCanary = "CANARY_SUMMARY_LITERAL_DO_NOT_LEAK"
	const sourceRefCanary = "CANARY_SOURCE_REF_LITERAL_DO_NOT_LEAK"
	const dueAtCanary = "CANARY_DUE_AT_LITERAL_DO_NOT_LEAK"

	// summary too long, source_ref non-string, due_at malformed.
	cases := []map[string]any{
		{"summary": summaryCanary + strings.Repeat("a", maxWatchOrderSummaryChars+1)},
		{"summary": "x", "source_ref": sourceRefCanary + strings.Repeat("s", maxWatchOrderSourceRefChars+1)},
		{"summary": "x", "due_at": dueAtCanary},
	}
	for i, args := range cases {
		res, err := h(context.Background(), agentruntime.ToolCall{Arguments: args})
		if err != nil {
			t.Fatalf("case %d: err: %v", i, err)
		}
		for _, canary := range []string{summaryCanary, sourceRefCanary, dueAtCanary} {
			if strings.Contains(res.Error, canary) {
				t.Errorf("case %d: refusal %q LEAKED canary %q", i, res.Error, canary)
			}
		}
	}
}

func TestRecordWatchOrder_ForwardsRecorderError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("notebook: pipe full")
	rec := &fakeWatchOrderRecorder{errToReturn: sentinel}
	h := recordWatchOrderHandler(t, rec, time.Now)

	_, err := h(context.Background(), agentruntime.ToolCall{Arguments: map[string]any{"summary": "x"}})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want wraps %v", err, sentinel)
	}
	if !strings.HasPrefix(err.Error(), "coordinator: record_watch_order:") {
		t.Errorf("err = %q, want prefix `coordinator: record_watch_order:`", err.Error())
	}
}

func TestRecordWatchOrder_ContextCancelObserved(t *testing.T) {
	t.Parallel()
	rec := &fakeWatchOrderRecorder{idToReturn: "x", cancelObserve: true}
	h := recordWatchOrderHandler(t, rec, time.Now)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := h(ctx, agentruntime.ToolCall{Arguments: map[string]any{"summary": "x"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestRecordWatchOrder_NoLogAppend_SourceGrep(t *testing.T) {
	t.Parallel()
	_, thisFile, _, _ := runtime.Caller(0)
	srcPath := filepath.Join(filepath.Dir(thisFile), "record_watch_order.go")
	body, err := os.ReadFile(srcPath) //nolint:gosec // test reads a fixed repo path.
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	src := string(body)
	// Strip comments so doc-block mentions don't false-match.
	noLineComments := regexp.MustCompile(`(?m)^\s*//.*$`).ReplaceAllString(src, "")
	noBlockComments := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(noLineComments, "")
	if strings.Contains(noBlockComments, "keeperslog.") {
		t.Errorf("record_watch_order.go references keeperslog outside comments — audit boundary belongs to the runtime reflector")
	}
	if strings.Contains(noBlockComments, ".Append(") {
		t.Errorf("record_watch_order.go calls .Append(...) outside comments — same audit-boundary violation")
	}
}

func TestRecordWatchOrder_SuccessOutputDropsPIICanaries(t *testing.T) {
	t.Parallel()
	rec := &fakeWatchOrderRecorder{idToReturn: "01900000-0000-7000-8000-000000000002"}
	h := recordWatchOrderHandler(t, rec, time.Now)

	const summaryCanary = "PII_SUMMARY_ZZZ_DO_NOT_LEAK"
	const sourceRefCanary = "PII_SOURCEREF_ZZZ_DO_NOT_LEAK"

	res, err := h(context.Background(), agentruntime.ToolCall{Arguments: map[string]any{
		"summary":    summaryCanary,
		"source_ref": sourceRefCanary,
		"due_at":     "2026-05-20T17:00:00Z",
	}})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Error != "" {
		t.Fatalf("refusal: %q", res.Error)
	}
	// Walk the whole Output map for either canary — neither must appear.
	flat := fmt.Sprintf("%v", res.Output)
	for _, canary := range []string{summaryCanary, sourceRefCanary} {
		if strings.Contains(flat, canary) {
			t.Errorf("Output LEAKED canary %q; serialised Output = %q", canary, flat)
		}
	}
}

func TestRecordWatchOrder_Concurrent16Goroutines(t *testing.T) {
	t.Parallel()
	rec := &fakeWatchOrderRecorder{idToReturn: "shared-id"}
	h := recordWatchOrderHandler(t, rec, time.Now)

	const n = 16
	var wg sync.WaitGroup
	wg.Add(n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, err := h(context.Background(), agentruntime.ToolCall{Arguments: map[string]any{
				"summary": "concurrent",
			}})
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent err: %v", err)
	}
	if rec.callCount != n {
		t.Fatalf("callCount = %d, want %d", rec.callCount, n)
	}
}
