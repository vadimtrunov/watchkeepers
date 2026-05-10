package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/jira"
	agentruntime "github.com/vadimtrunov/watchkeepers/core/pkg/runtime"
)

// fakeJiraSearcher is the hand-rolled stub the M8.2.b tests use to
// inject a synchronous Search target without standing up a real HTTP
// client. Pattern mirrors [fakeJiraUpdater] from M8.2.a — no mocking
// lib. `pages` is a queue: each Search call pops the head and returns
// it; callers prime the queue per test. `gotJQL` / `gotOpts` /
// `keyHistory` are guarded by `mu` so the concurrent invocation test
// does not race; `calls` is an atomic counter for cheap assertion.
type fakeJiraSearcher struct {
	calls      int32
	mu         sync.Mutex
	gotJQL     []string
	gotOpts    []jira.SearchOptions
	pages      []jira.SearchResult
	returnErr  error
	keyHistory []jira.IssueKey
	// afterCall, when non-nil, is invoked AFTER recording the call
	// but BEFORE returning the page. Tests use it to inject side
	// effects (e.g. cancel the parent context after page N) — see
	// `TestFindOverdueTicketsHandler_PaginatorChecksContextBetweenPages`.
	afterCall func(callIdx int)
}

func (f *fakeJiraSearcher) Search(_ context.Context, jql string, opts jira.SearchOptions) (jira.SearchResult, error) {
	idx := atomic.AddInt32(&f.calls, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotJQL = append(f.gotJQL, jql)
	f.gotOpts = append(f.gotOpts, opts)
	if f.afterCall != nil {
		f.afterCall(int(idx))
	}
	if f.returnErr != nil {
		return jira.SearchResult{}, f.returnErr
	}
	if len(f.pages) == 0 {
		// Default: terminal empty page.
		return jira.SearchResult{IsLast: true}, nil
	}
	page := f.pages[0]
	f.pages = f.pages[1:]
	for _, iss := range page.Issues {
		f.keyHistory = append(f.keyHistory, iss.Key)
	}
	return page, nil
}

// validArgs builds the minimal valid arg map for the handler. Tests
// that mutate one field shadow it with `args[K] = ...` after copy.
func validArgs() map[string]any {
	return map[string]any{
		ToolArgProjectKey:        "WK",
		ToolArgAssigneeAccountID: "557058:abc-uuid",
		ToolArgStatus:            []any{"In Progress", "To Do"},
		ToolArgAgeThresholdDays:  float64(7),
	}
}

func TestNewFindOverdueTicketsHandler_PanicsOnNilSearcher(t *testing.T) {
	t.Parallel()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on nil searcher; got none")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("panic value not string: %T %v", r, r)
		}
		if !strings.Contains(msg, "searcher must not be nil") {
			t.Errorf("panic message %q does not mention nil-searcher discipline", msg)
		}
	}()

	NewFindOverdueTicketsHandler(nil)
}

//nolint:staticcheck // ST1023: explicit type asserts the factory return shape; without the annotation the assignment is a tautology.
func TestNewFindOverdueTicketsHandler_SatisfiesToolHandlerSignature(t *testing.T) {
	t.Parallel()

	var h agentruntime.ToolHandler = NewFindOverdueTicketsHandler(&fakeJiraSearcher{})
	if h == nil {
		t.Fatal("factory returned nil handler")
	}
}

func TestFindOverdueTicketsHandler_HappyPath(t *testing.T) {
	t.Parallel()

	searcher := &fakeJiraSearcher{
		pages: []jira.SearchResult{
			{
				IsLast: true,
				Issues: []jira.Issue{
					{Key: "WK-1", Summary: "stale", Status: "In Progress", AssigneeID: "557058:abc-uuid", Updated: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
					{Key: "WK-2", Summary: "old", Status: "To Do", AssigneeID: "557058:abc-uuid", Updated: time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)},
				},
			},
		},
	}
	h := newHandlerWithFixedNow(t, searcher, time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC))

	res, err := h(context.Background(), agentruntime.ToolCall{Arguments: validArgs()})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.Error != "" {
		t.Errorf("ToolResult.Error = %q, want empty on happy path", res.Error)
	}
	if res.Output["total_returned"] != 2 {
		t.Errorf("total_returned = %v, want 2", res.Output["total_returned"])
	}
	if res.Output["truncated"] != false {
		t.Errorf("truncated = %v, want false", res.Output["truncated"])
	}
	scope, _ := res.Output["scope"].(map[string]any)
	if scope == nil {
		t.Fatalf("Output[scope] missing or wrong shape: %#v", res.Output["scope"])
	}
	if scope["project_key"] != "WK" {
		t.Errorf("scope.project_key = %v, want WK", scope["project_key"])
	}
	if scope["age_threshold_days"] != 7 {
		t.Errorf("scope.age_threshold_days = %v, want 7", scope["age_threshold_days"])
	}
	if _, present := res.Output["jql"]; present {
		t.Errorf("Output[jql] must be absent (PII discipline — iter-1 critic Major M2)")
	}
	if _, present := scope["assignee_account_id"]; present {
		t.Errorf("scope.assignee_account_id must be absent — Atlassian accountId is GDPR-PII; should not echo back")
	}
	issues, ok := res.Output["issues"].([]map[string]any)
	if !ok || len(issues) != 2 {
		t.Fatalf("Output[issues] shape unexpected: %#v", res.Output["issues"])
	}
	if issues[0]["key"] != "WK-1" {
		t.Errorf("issues[0].key = %v, want WK-1", issues[0]["key"])
	}
	if issues[0]["age_days"] != 14 {
		t.Errorf("issues[0].age_days = %v, want 14 (2026-01-15 - 2026-01-01)", issues[0]["age_days"])
	}
	if issues[1]["age_days"] != 10 {
		t.Errorf("issues[1].age_days = %v, want 10", issues[1]["age_days"])
	}
}

func TestFindOverdueTicketsHandler_JQLComposition(t *testing.T) {
	t.Parallel()

	searcher := &fakeJiraSearcher{}
	h := NewFindOverdueTicketsHandler(searcher)

	args := validArgs()
	args[ToolArgStatus] = []any{"In Progress", "To Do"}
	args[ToolArgAgeThresholdDays] = float64(14)

	if _, err := h(context.Background(), agentruntime.ToolCall{Arguments: args}); err != nil {
		t.Fatalf("handler: %v", err)
	}

	searcher.mu.Lock()
	jql := searcher.gotJQL[0]
	opts := searcher.gotOpts[0]
	searcher.mu.Unlock()

	want := `project = "WK" AND assignee = "557058:abc-uuid" AND status in ("In Progress", "To Do") AND updated < -14d ORDER BY updated ASC`
	if jql != want {
		t.Errorf("composed JQL mismatch:\n got: %s\nwant: %s", jql, want)
	}
	if opts.MaxResults != searchPageSize {
		t.Errorf("MaxResults = %d, want %d", opts.MaxResults, searchPageSize)
	}
	wantFields := map[string]bool{"summary": true, "status": true, "assignee": true, "updated": true}
	if len(opts.Fields) != len(wantFields) {
		t.Errorf("Fields = %v, want exactly %v", opts.Fields, wantFields)
	}
	for _, f := range opts.Fields {
		if !wantFields[f] {
			t.Errorf("Fields contains unexpected entry %q", f)
		}
	}
	if opts.PageToken != "" {
		t.Errorf("first call PageToken = %q, want empty", opts.PageToken)
	}
}

func TestFindOverdueTicketsHandler_PreCheckContextCancelled(t *testing.T) {
	t.Parallel()

	searcher := &fakeJiraSearcher{}
	h := NewFindOverdueTicketsHandler(searcher)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := h(ctx, agentruntime.ToolCall{Arguments: validArgs()})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("handler returned %v on cancelled ctx, want context.Canceled", err)
	}
	if atomic.LoadInt32(&searcher.calls) != 0 {
		t.Errorf("searcher called despite cancelled ctx; calls=%d", searcher.calls)
	}
}

func TestFindOverdueTicketsHandler_ArgValidationRefusals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		mutate    func(args map[string]any)
		wantErrIn string
	}{
		{
			name:      "missing project_key",
			mutate:    func(a map[string]any) { delete(a, ToolArgProjectKey) },
			wantErrIn: "project_key",
		},
		{
			name:      "non-string project_key",
			mutate:    func(a map[string]any) { a[ToolArgProjectKey] = 42 },
			wantErrIn: "project_key must be a string",
		},
		{
			name:      "empty project_key",
			mutate:    func(a map[string]any) { a[ToolArgProjectKey] = "" },
			wantErrIn: "project_key must be non-empty",
		},
		{
			name:      "missing assignee_account_id",
			mutate:    func(a map[string]any) { delete(a, ToolArgAssigneeAccountID) },
			wantErrIn: "assignee_account_id",
		},
		{
			name:      "non-string assignee",
			mutate:    func(a map[string]any) { a[ToolArgAssigneeAccountID] = 42 },
			wantErrIn: "assignee_account_id must be a string",
		},
		{
			name:      "empty assignee",
			mutate:    func(a map[string]any) { a[ToolArgAssigneeAccountID] = "" },
			wantErrIn: "assignee_account_id must be non-empty",
		},
		{
			name:      "missing status",
			mutate:    func(a map[string]any) { delete(a, ToolArgStatus) },
			wantErrIn: "status",
		},
		{
			name:      "non-array status",
			mutate:    func(a map[string]any) { a[ToolArgStatus] = "In Progress" },
			wantErrIn: "status must be an array",
		},
		{
			name:      "empty status array",
			mutate:    func(a map[string]any) { a[ToolArgStatus] = []any{} },
			wantErrIn: "status must contain at least one entry",
		},
		{
			name:      "non-string status entry",
			mutate:    func(a map[string]any) { a[ToolArgStatus] = []any{"In Progress", 42} },
			wantErrIn: "entries must be strings",
		},
		{
			name:      "empty-string status entry",
			mutate:    func(a map[string]any) { a[ToolArgStatus] = []any{""} },
			wantErrIn: "entries must be non-empty",
		},
		{
			name:      "missing age_threshold_days",
			mutate:    func(a map[string]any) { delete(a, ToolArgAgeThresholdDays) },
			wantErrIn: "age_threshold_days",
		},
		{
			name:      "non-number age",
			mutate:    func(a map[string]any) { a[ToolArgAgeThresholdDays] = "7" },
			wantErrIn: "age_threshold_days must be a number",
		},
		{
			name:      "non-integer float age",
			mutate:    func(a map[string]any) { a[ToolArgAgeThresholdDays] = 7.5 },
			wantErrIn: "age_threshold_days must be an integer",
		},
		{
			name:      "zero age",
			mutate:    func(a map[string]any) { a[ToolArgAgeThresholdDays] = float64(0) },
			wantErrIn: "age_threshold_days must be ≥ 1",
		},
		{
			name:      "negative age",
			mutate:    func(a map[string]any) { a[ToolArgAgeThresholdDays] = float64(-1) },
			wantErrIn: "age_threshold_days must be ≥ 1",
		},
		{
			name:      "age over cap",
			mutate:    func(a map[string]any) { a[ToolArgAgeThresholdDays] = float64(maxAgeThresholdDays + 1) },
			wantErrIn: "age_threshold_days must be ≤ 365",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			searcher := &fakeJiraSearcher{}
			h := NewFindOverdueTicketsHandler(searcher)

			args := validArgs()
			tc.mutate(args)
			res, err := h(context.Background(), agentruntime.ToolCall{Arguments: args})
			if err != nil {
				t.Fatalf("handler returned Go err = %v; want nil err with ToolResult.Error", err)
			}
			if !strings.Contains(res.Error, tc.wantErrIn) {
				t.Errorf("ToolResult.Error = %q; want substring %q", res.Error, tc.wantErrIn)
			}
			if atomic.LoadInt32(&searcher.calls) != 0 {
				t.Errorf("searcher called despite validation refusal; calls=%d", searcher.calls)
			}
		})
	}
}

func TestFindOverdueTicketsHandler_RefusesMalformedProjectKey_NoEcho(t *testing.T) {
	t.Parallel()

	// Canary must be SHAPE-INVALID against [projectKeyPattern]
	// (`^[A-Z][A-Z0-9_]+$`) so the gate rejects it. An ALL-UPPERCASE
	// token would slip past the pattern and silently reach the M8.1
	// layer. Lowercase + dashes guarantees rejection while keeping the
	// canary substring distinctive enough to grep for in refusal text.
	const tokenShapedKey = "redaction-probe-token-m82b-project-do-not-log" //nolint:gosec // G101: synthetic redaction-harness canary, not a real credential.

	searcher := &fakeJiraSearcher{}
	h := NewFindOverdueTicketsHandler(searcher)

	args := validArgs()
	args[ToolArgProjectKey] = tokenShapedKey

	res, err := h(context.Background(), agentruntime.ToolCall{Arguments: args})
	if err != nil {
		t.Fatalf("handler returned Go err on malformed project_key; PII could leak via wrap: %v", err)
	}
	if !strings.Contains(res.Error, "project-key shape") {
		t.Errorf("ToolResult.Error = %q; want substring 'project-key shape'", res.Error)
	}
	if strings.Contains(res.Error, tokenShapedKey) {
		t.Errorf("ToolResult.Error echoed the raw project_key value (PII leak): %q", res.Error)
	}
	if atomic.LoadInt32(&searcher.calls) != 0 {
		t.Errorf("searcher called despite malformed project_key; PII gate broken (calls=%d)", searcher.calls)
	}
}

func TestFindOverdueTicketsHandler_RefusesMalformedProjectKey_TableDriven(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		key  string
	}{
		{"lowercase", "wk"},
		{"single uppercase", "W"}, // regex requires ≥2 chars
		{"embedded space", "WK PROJ"},
		{"embedded slash", "WK/sub"},
		{"embedded quote", `WK"`},
		{"trailing whitespace", "WK "},
		{"jql injection attempt", `WK" OR 1=1 --`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			searcher := &fakeJiraSearcher{}
			h := NewFindOverdueTicketsHandler(searcher)

			args := validArgs()
			args[ToolArgProjectKey] = tc.key
			res, err := h(context.Background(), agentruntime.ToolCall{Arguments: args})
			if err != nil {
				t.Fatalf("project_key %q: handler returned Go err: %v", tc.key, err)
			}
			if !strings.Contains(res.Error, "project-key shape") {
				t.Errorf("project_key %q: ToolResult.Error = %q; want shape refusal", tc.key, res.Error)
			}
			if strings.Contains(res.Error, tc.key) {
				t.Errorf("project_key %q: refusal text echoed the raw value (PII leak): %q", tc.key, res.Error)
			}
			if atomic.LoadInt32(&searcher.calls) != 0 {
				t.Errorf("project_key %q: searcher called despite malformed key", tc.key)
			}
		})
	}
}

func TestFindOverdueTicketsHandler_RefusesMalformedAssigneeID_NoEcho(t *testing.T) {
	t.Parallel()

	const tokenShapedID = "REDACTION_PROBE_TOKEN_M82B_ASSIGNEE_DO_NOT_LOG!@#$" //nolint:gosec // G101: synthetic redaction-harness canary, not a real credential.

	searcher := &fakeJiraSearcher{}
	h := NewFindOverdueTicketsHandler(searcher)

	args := validArgs()
	args[ToolArgAssigneeAccountID] = tokenShapedID

	res, _ := h(context.Background(), agentruntime.ToolCall{Arguments: args})
	if !strings.Contains(res.Error, "accountId shape") {
		t.Errorf("ToolResult.Error = %q; want substring 'accountId shape'", res.Error)
	}
	if strings.Contains(res.Error, tokenShapedID) {
		t.Errorf("ToolResult.Error echoed the raw accountId (PII leak): %q", res.Error)
	}
	if atomic.LoadInt32(&searcher.calls) != 0 {
		t.Errorf("searcher called despite malformed assignee; PII gate broken")
	}
}

func TestFindOverdueTicketsHandler_RefusesMalformedStatusEntry_NoEcho(t *testing.T) {
	t.Parallel()

	const injectionAttempt = `") OR 1=1 -- REDACTION_PROBE_M82B_STATUS_DO_NOT_LOG` //nolint:gosec // G101: synthetic redaction-harness canary, not a real credential.

	searcher := &fakeJiraSearcher{}
	h := NewFindOverdueTicketsHandler(searcher)

	args := validArgs()
	args[ToolArgStatus] = []any{"In Progress", injectionAttempt}

	res, _ := h(context.Background(), agentruntime.ToolCall{Arguments: args})
	if !strings.Contains(res.Error, "status-name shape") {
		t.Errorf("ToolResult.Error = %q; want substring 'status-name shape'", res.Error)
	}
	if strings.Contains(res.Error, injectionAttempt) {
		t.Errorf("ToolResult.Error echoed the raw status entry (PII leak / JQL leak): %q", res.Error)
	}
	if atomic.LoadInt32(&searcher.calls) != 0 {
		t.Errorf("searcher called despite malformed status entry")
	}
}

func TestFindOverdueTicketsHandler_AcceptsAgeAsInt(t *testing.T) {
	t.Parallel()

	searcher := &fakeJiraSearcher{}
	h := NewFindOverdueTicketsHandler(searcher)

	args := validArgs()
	args[ToolArgAgeThresholdDays] = int(7) // not float64 — exercise the int branch.

	res, err := h(context.Background(), agentruntime.ToolCall{Arguments: args})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.Error != "" {
		t.Fatalf("expected acceptance of int age; got refusal %q", res.Error)
	}
}

func TestFindOverdueTicketsHandler_AcceptsAgeFromJSONDecode(t *testing.T) {
	t.Parallel()

	// Round-trip a typical args payload through json.Unmarshal so the
	// runtime's actual decode shape (numbers → float64, arrays →
	// []any, objects → map[string]any) is what reaches the handler.
	const payload = `{
		"project_key": "WK",
		"assignee_account_id": "557058:abc-uuid",
		"status": ["In Progress"],
		"age_threshold_days": 7
	}`
	var args map[string]any
	if err := json.Unmarshal([]byte(payload), &args); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	searcher := &fakeJiraSearcher{}
	h := NewFindOverdueTicketsHandler(searcher)

	res, err := h(context.Background(), agentruntime.ToolCall{Arguments: args})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.Error != "" {
		t.Errorf("expected acceptance of JSON-decoded args; got refusal %q", res.Error)
	}
}

func TestFindOverdueTicketsHandler_PaginationCollectsMultiplePages(t *testing.T) {
	t.Parallel()

	page1 := jira.SearchResult{
		IsLast:        false,
		NextPageToken: "cursor-2",
		Issues: []jira.Issue{
			{Key: "WK-1", Updated: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
			{Key: "WK-2", Updated: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)},
		},
	}
	page2 := jira.SearchResult{
		IsLast: true,
		Issues: []jira.Issue{
			{Key: "WK-3", Updated: time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)},
		},
	}
	searcher := &fakeJiraSearcher{pages: []jira.SearchResult{page1, page2}}
	h := NewFindOverdueTicketsHandler(searcher)

	res, err := h(context.Background(), agentruntime.ToolCall{Arguments: validArgs()})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.Output["total_returned"] != 3 {
		t.Errorf("total_returned = %v, want 3", res.Output["total_returned"])
	}
	if res.Output["truncated"] != false {
		t.Errorf("truncated = %v, want false (IsLast reached)", res.Output["truncated"])
	}
	if got := atomic.LoadInt32(&searcher.calls); got != 2 {
		t.Errorf("Search called %d times; want 2", got)
	}

	searcher.mu.Lock()
	defer searcher.mu.Unlock()
	if len(searcher.gotOpts) != 2 {
		t.Fatalf("captured opts len = %d; want 2", len(searcher.gotOpts))
	}
	if searcher.gotOpts[0].PageToken != "" {
		t.Errorf("first call PageToken = %q; want empty", searcher.gotOpts[0].PageToken)
	}
	if searcher.gotOpts[1].PageToken != "cursor-2" {
		t.Errorf("second call PageToken = %q; want cursor-2", searcher.gotOpts[1].PageToken)
	}
}

func TestFindOverdueTicketsHandler_TruncatesAtMaxIssues(t *testing.T) {
	t.Parallel()

	// Build a single page that EXCEEDS maxOverdueIssues to force the
	// per-issue cap branch (without requiring [maxOverdueIssues / page]
	// pages of fixture data).
	bigPage := jira.SearchResult{
		IsLast:        false,
		NextPageToken: "cursor-x",
	}
	for i := 0; i < maxOverdueIssues+5; i++ {
		bigPage.Issues = append(bigPage.Issues, jira.Issue{
			Key: jira.IssueKey(fmt.Sprintf("WK-%d", i+1)),
		})
	}
	searcher := &fakeJiraSearcher{pages: []jira.SearchResult{bigPage}}
	h := NewFindOverdueTicketsHandler(searcher)

	res, err := h(context.Background(), agentruntime.ToolCall{Arguments: validArgs()})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.Output["total_returned"] != maxOverdueIssues {
		t.Errorf("total_returned = %v, want %d (capped)", res.Output["total_returned"], maxOverdueIssues)
	}
	if res.Output["truncated"] != true {
		t.Errorf("truncated = %v, want true (issue cap fired before IsLast)", res.Output["truncated"])
	}
}

func TestFindOverdueTicketsHandler_TruncatesAtMaxPages(t *testing.T) {
	t.Parallel()

	// Each page returns ONE issue and a non-empty cursor → IsLast=false
	// indefinitely. The page cap [maxOverduePages] fires before
	// [maxOverdueIssues] does.
	pages := make([]jira.SearchResult, maxOverduePages+2)
	for i := range pages {
		pages[i] = jira.SearchResult{
			IsLast:        false,
			NextPageToken: fmt.Sprintf("cursor-%d", i+1),
			Issues: []jira.Issue{
				{Key: jira.IssueKey(fmt.Sprintf("WK-%d", i+1))},
			},
		}
	}
	searcher := &fakeJiraSearcher{pages: pages}
	h := NewFindOverdueTicketsHandler(searcher)

	res, err := h(context.Background(), agentruntime.ToolCall{Arguments: validArgs()})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if got := atomic.LoadInt32(&searcher.calls); got != maxOverduePages {
		t.Errorf("Search called %d times; want %d (page cap)", got, maxOverduePages)
	}
	if res.Output["total_returned"] != maxOverduePages {
		t.Errorf("total_returned = %v, want %d", res.Output["total_returned"], maxOverduePages)
	}
	if res.Output["truncated"] != true {
		t.Errorf("truncated = %v, want true (page cap fired)", res.Output["truncated"])
	}
}

func TestFindOverdueTicketsHandler_SearchErrorWrapped(t *testing.T) {
	t.Parallel()

	wantInner := errors.New("jira: Search: 503 service unavailable")
	searcher := &fakeJiraSearcher{returnErr: wantInner}
	h := NewFindOverdueTicketsHandler(searcher)

	_, err := h(context.Background(), agentruntime.ToolCall{Arguments: validArgs()})
	if !errors.Is(err, wantInner) {
		t.Fatalf("handler did not wrap inner error; got %v, want %v in chain", err, wantInner)
	}
	if !strings.HasPrefix(err.Error(), "coordinator: find_overdue_tickets:") {
		t.Errorf("handler err = %q; want prefix coordinator: find_overdue_tickets:", err.Error())
	}
}

func TestFindOverdueTicketsHandler_ConcurrentInvocationSafety(t *testing.T) {
	t.Parallel()

	// 16 goroutines invoking the same handler against a shared searcher.
	// Asserts the handler is goroutine-safe and that every call reaches
	// the searcher (none short-circuit incorrectly under concurrent load).
	searcher := &fakeJiraSearcher{}
	h := NewFindOverdueTicketsHandler(searcher)

	const goroutines = 16
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			if _, err := h(context.Background(), agentruntime.ToolCall{Arguments: validArgs()}); err != nil {
				t.Errorf("goroutine: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&searcher.calls); got != goroutines {
		t.Errorf("searcher called %d times; want %d", got, goroutines)
	}
}

// TestFindOverdueTicketsHandler_ValidationPrecedesSearchCall_SourceGrep
// pins the gate ordering at the source-file level. A refactor that
// moved [searcher.Search] BEFORE the per-arg readers would silently
// pass every runtime test (the M8.1 layer would still reject malformed
// JQL, but the iter-1 PII discipline requires that no user-supplied
// raw value reaches the network at all). The source-grep pattern
// mirrors the M8.2.a [TestUpdateTicketFieldHandler_AssigneeRefusalPrecedesUpdaterCall_SourceGrep]
// (the M8.1 lesson #7 source-grep AC family). Anchored to the helper
// CALL pattern, NOT the bare symbol — the consts at the top of the
// file would otherwise satisfy a symbol-only search.
func TestFindOverdueTicketsHandler_ValidationPrecedesSearchCall_SourceGrep(t *testing.T) {
	t.Parallel()

	srcPath := repoRelative(t, "core/pkg/coordinator/find_overdue_tickets.go")
	bytes, err := os.ReadFile(srcPath) //nolint:gosec // test reads a fixed repo-relative path.
	if err != nil {
		t.Fatalf("ReadFile %s: %v", srcPath, err)
	}
	body := stripGoComments(string(bytes))

	searchIdx := strings.Index(body, "searcher.Search(")
	if searchIdx < 0 {
		t.Fatal("source missing searcher.Search( call site")
	}
	// Each per-arg reader CALL must precede the network call so a
	// malformed input never reaches the M8.1 layer.
	guards := []string{
		"readProjectKeyArg(",
		"readAssigneeAccountIDArg(",
		"readStatusArg(",
		"readAgeThresholdDaysArg(",
	}
	for _, guard := range guards {
		idx := strings.Index(body, guard)
		if idx < 0 {
			t.Errorf("source missing validation call %q; PII gate removed?", guard)
			continue
		}
		if idx >= searchIdx {
			t.Errorf("validation call %q must precede searcher.Search; got call@%d search@%d",
				guard, idx, searchIdx)
		}
	}
	// Defence-in-depth: also assert each shape regex is referenced
	// somewhere in the file.
	for _, sym := range []string{
		"projectKeyPattern.MatchString",
		"accountIDPattern.MatchString",
		"statusNamePattern.MatchString",
	} {
		if !strings.Contains(body, sym) {
			t.Errorf("source missing %q; shape regex check removed?", sym)
		}
	}
}

// TestFindOverdueTicketsHandler_NoAuditOrLogAppend_SourceGrep pins the
// audit discipline: the handler MUST NOT call into keeperslog (the
// audit boundary lives at the runtime's tool-result reflection layer,
// M5.6.b). Mirrors the M*.c.* source-grep AC family.
func TestFindOverdueTicketsHandler_NoAuditOrLogAppend_SourceGrep(t *testing.T) {
	t.Parallel()

	srcPath := repoRelative(t, "core/pkg/coordinator/find_overdue_tickets.go")
	bytes, err := os.ReadFile(srcPath) //nolint:gosec // test reads a fixed repo-relative path.
	if err != nil {
		t.Fatalf("ReadFile %s: %v", srcPath, err)
	}
	body := stripGoComments(string(bytes))

	for _, forbidden := range []string{"keeperslog.", ".Append("} {
		if strings.Contains(body, forbidden) {
			t.Errorf("source contains forbidden audit shape %q outside comments; runtime owns audit, not handlers", forbidden)
		}
	}
}

func TestComposeOverdueJQL_QuoteEscaping(t *testing.T) {
	t.Parallel()

	// Defence-in-depth: even though shape pre-validation rejects raw
	// `"` and `\` in user inputs, the JQL composer escapes them so a
	// future regression that admits one still emits well-formed JQL.
	got := composeOverdueJQL(
		"WK",
		"557058:abc-uuid",
		[]string{`needs "review"`, `back\slash`},
		7,
	)
	want := `project = "WK" AND assignee = "557058:abc-uuid" AND status in ("needs \"review\"", "back\\slash") AND updated < -7d ORDER BY updated ASC`
	if got != want {
		t.Errorf("composed JQL mismatch:\n got: %s\nwant: %s", got, want)
	}
}

func TestJqlQuote_BackslashEscapedBeforeDoubleQuote(t *testing.T) {
	t.Parallel()

	// Critical ordering: backslash MUST escape FIRST so a `\"` input
	// becomes `\\\"` (literal backslash + escaped quote), not `\\"`
	// (escaped backslash + raw quote that closes the literal).
	got := jqlQuote(`a\"b`)
	want := `"a\\\"b"`
	if got != want {
		t.Errorf("jqlQuote(`a\\\"b`) = %s; want %s", got, want)
	}
}

func TestFindOverdueTicketsHandler_AgeDaysHandlesZeroUpdated(t *testing.T) {
	t.Parallel()

	// Issue with a zero `Updated` (parse failure / field-not-requested)
	// must not panic and must surface age_days=0 + empty updated.
	searcher := &fakeJiraSearcher{
		pages: []jira.SearchResult{
			{
				IsLast: true,
				Issues: []jira.Issue{
					{Key: "WK-1", Summary: "no updated"},
				},
			},
		},
	}
	h := NewFindOverdueTicketsHandler(searcher)

	res, err := h(context.Background(), agentruntime.ToolCall{Arguments: validArgs()})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	issues, _ := res.Output["issues"].([]map[string]any)
	if len(issues) != 1 {
		t.Fatalf("issues len = %d; want 1", len(issues))
	}
	if issues[0]["age_days"] != 0 {
		t.Errorf("age_days for zero-Updated issue = %v; want 0", issues[0]["age_days"])
	}
	if issues[0]["updated"] != "" {
		t.Errorf("updated for zero-Updated issue = %q; want empty", issues[0]["updated"])
	}
}

// TestFindOverdueTicketsHandler_RefusesTokenShapedAssigneeID_NoEcho
// pins iter-1 codex Major #2: an all-uppercase token-shaped string
// (e.g. an env-var leak like `THE_API_KEY_VALUE`) satisfies
// [accountIDPattern]'s character class but lacks the digit-or-colon
// discriminant. Without [accountIDDiscriminantPattern] the token would
// reach the M8.1 layer and (on the prior Output["jql"] surface) echo
// back to the agent. After the iter-1 fix it MUST be refused
// pre-network with NO echo.
func TestFindOverdueTicketsHandler_RefusesTokenShapedAssigneeID_NoEcho(t *testing.T) {
	t.Parallel()

	// Shape-VALID against character class (only A-Z + _), shape-
	// INVALID against the digit-or-colon discriminant. Token-key
	// canary; iter-1 codex Major.
	const tokenShapedID = "THE_API_KEY_VALUE_DO_NOT_LOG" //nolint:gosec // G101: synthetic redaction-harness canary.

	searcher := &fakeJiraSearcher{}
	h := NewFindOverdueTicketsHandler(searcher)

	args := validArgs()
	args[ToolArgAssigneeAccountID] = tokenShapedID

	res, err := h(context.Background(), agentruntime.ToolCall{Arguments: args})
	if err != nil {
		t.Fatalf("handler returned Go err on token-shaped accountId: %v", err)
	}
	if !strings.Contains(res.Error, "accountId shape") {
		t.Errorf("ToolResult.Error = %q; want substring 'accountId shape'", res.Error)
	}
	if strings.Contains(res.Error, tokenShapedID) {
		t.Errorf("ToolResult.Error echoed the raw token (PII leak): %q", res.Error)
	}
	if atomic.LoadInt32(&searcher.calls) != 0 {
		t.Errorf("searcher called despite token-shaped accountId; discriminant gate broken")
	}
}

// TestFindOverdueTicketsHandler_TruncatesAtMaxIssues_OnLastPageWithUnread
// pins iter-1 codex+critic Major #1: when the issue cap fires INSIDE a
// page that ALSO has IsLast=true with unread issues remaining, the
// handler must surface `truncated=true` (not silently drop the unread
// tail and report `truncated=false`). Atlassian's documented contract
// allows server-side over-shoot; without this branch the agent
// confidently mis-reports coverage.
func TestFindOverdueTicketsHandler_TruncatesAtMaxIssues_OnLastPageWithUnread(t *testing.T) {
	t.Parallel()

	// Single page, IsLast=true, NextPageToken="", but Issues has
	// MORE than maxOverdueIssues entries. The cap fires mid-page; the
	// remaining tail is unread. Truncated MUST be true.
	bigPage := jira.SearchResult{IsLast: true, NextPageToken: ""}
	for i := 0; i < maxOverdueIssues+5; i++ {
		bigPage.Issues = append(bigPage.Issues, jira.Issue{
			Key: jira.IssueKey(fmt.Sprintf("WK-%d", i+1)),
		})
	}
	searcher := &fakeJiraSearcher{pages: []jira.SearchResult{bigPage}}
	h := NewFindOverdueTicketsHandler(searcher)

	res, err := h(context.Background(), agentruntime.ToolCall{Arguments: validArgs()})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.Output["total_returned"] != maxOverdueIssues {
		t.Errorf("total_returned = %v, want %d", res.Output["total_returned"], maxOverdueIssues)
	}
	if res.Output["truncated"] != true {
		t.Errorf("truncated = %v, want true (issue cap fired with unread tail on the last page; "+
			"silently dropping the tail without surfacing truncation is a data-loss bug)",
			res.Output["truncated"])
	}
}

// TestFindOverdueTicketsHandler_TruncatesOnIsLastWithNonEmptyCursor
// pins iter-1 codex Major #1 (server-contract violation symmetric to
// the M8.1 inverse guard): when Atlassian returns IsLast=true with a
// non-empty NextPageToken, the handler must treat the cursor as
// authoritative and surface truncated=true rather than silently
// dropping the next page's worth of data.
func TestFindOverdueTicketsHandler_TruncatesOnIsLastWithNonEmptyCursor(t *testing.T) {
	t.Parallel()

	const staleCursor = "stale-cursor" //nolint:gosec // G101: synthetic test cursor (Atlassian PageToken is an opaque pagination handle, not a credential — mirrors core/pkg/jira/search_test.go convention).
	page := jira.SearchResult{
		IsLast:        true,
		NextPageToken: staleCursor,
		Issues: []jira.Issue{
			{Key: "WK-1"},
		},
	}
	searcher := &fakeJiraSearcher{pages: []jira.SearchResult{page}}
	h := NewFindOverdueTicketsHandler(searcher)

	res, err := h(context.Background(), agentruntime.ToolCall{Arguments: validArgs()})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.Output["truncated"] != true {
		t.Errorf("truncated = %v, want true on IsLast=true + non-empty cursor (M8.1 contract violation, must fail-safe)",
			res.Output["truncated"])
	}
}

// TestFindOverdueTicketsHandler_PaginatorChecksContextBetweenPages
// pins iter-1 critic gap: paginateOverdue checks ctx.Err() at the top
// of every page, but no test exercised it. Cancel ctx after the first
// page; the second page must NOT be requested.
func TestFindOverdueTicketsHandler_PaginatorChecksContextBetweenPages(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())

	page1 := jira.SearchResult{IsLast: false, NextPageToken: "cursor-2", Issues: []jira.Issue{{Key: "WK-1"}}}
	page2 := jira.SearchResult{IsLast: true, Issues: []jira.Issue{{Key: "WK-2"}}}
	searcher := &fakeJiraSearcher{
		pages: []jira.SearchResult{page1, page2},
		// Cancel ctx as a side effect of returning page1; page2 must
		// then never be requested because the paginator's per-page
		// ctx.Err() pre-check fails.
		afterCall: func(callIdx int) {
			if callIdx == 1 {
				cancel()
			}
		},
	}
	h := NewFindOverdueTicketsHandler(searcher)

	_, err := h(ctx, agentruntime.ToolCall{Arguments: validArgs()})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("handler err = %v; want context.Canceled wrapped", err)
	}
	if got := atomic.LoadInt32(&searcher.calls); got != 1 {
		t.Errorf("Search called %d times; want exactly 1 (ctx cancel between pages must short-circuit)", got)
	}
}

// TestNewFindOverdueTicketsHandler_PanicsOnNilClock pins iter-1 critic
// gap: the unexported [newFindOverdueTicketsHandlerWithClock] panics
// on a nil clock too (mirroring the nil-searcher discipline) — but no
// test exercised the branch.
func TestNewFindOverdueTicketsHandler_PanicsOnNilClock(t *testing.T) {
	t.Parallel()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on nil clock; got none")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "clock must not be nil") {
			t.Errorf("panic msg %v does not mention nil-clock discipline", r)
		}
	}()

	newFindOverdueTicketsHandlerWithClock(&fakeJiraSearcher{}, nil)
}

// fixedClock returns a `func() time.Time` always returning `t`. Used
// by tests that need a deterministic `now` for `age_days` assertions.
// No package-level mutation — each handler instance carries its own
// clock closure, so parallel tests cannot race on it.
func fixedClock(at time.Time) func() time.Time {
	return func() time.Time { return at }
}

// newHandlerWithFixedNow is a one-line shortcut for tests that want
// the production handler shape but with a pinned clock. Hides the
// internal factory name so the test reads as "build me a handler at
// time X".
func newHandlerWithFixedNow(t *testing.T, searcher JiraSearcher, at time.Time) agentruntime.ToolHandler {
	t.Helper()
	return newFindOverdueTicketsHandlerWithClock(searcher, fixedClock(at))
}
