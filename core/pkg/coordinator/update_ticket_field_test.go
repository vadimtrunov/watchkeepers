package coordinator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/jira"
	agentruntime "github.com/vadimtrunov/watchkeepers/core/pkg/runtime"
)

// fakeJiraUpdater is the hand-rolled stub the M8.2.a tests use to
// inject a synchronous UpdateFields target without standing up a real
// HTTP client. Pattern mirrors the M*.c.* prior art (no mocking lib).
// `gotKey`, `gotFields`, and `keyHistory` are guarded by `mu` so the
// concurrent invocation test (`TestUpdateTicketFieldHandler_ConcurrentInvocationSafety`)
// does not race; `calls` is an atomic counter for cheap assertion.
type fakeJiraUpdater struct {
	calls      int32
	gotKey     jira.IssueKey
	gotFields  map[string]any
	returnErr  error
	mu         sync.Mutex
	keyHistory []jira.IssueKey
}

func (f *fakeJiraUpdater) UpdateFields(_ context.Context, key jira.IssueKey, fields map[string]any) error {
	atomic.AddInt32(&f.calls, 1)
	f.mu.Lock()
	f.gotKey = key
	f.gotFields = fields
	f.keyHistory = append(f.keyHistory, key)
	f.mu.Unlock()
	return f.returnErr
}

func TestNewUpdateTicketFieldHandler_PanicsOnNilUpdater(t *testing.T) {
	t.Parallel()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on nil updater; got none")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("panic value not string: %T %v", r, r)
		}
		if !strings.Contains(msg, "updater must not be nil") {
			t.Errorf("panic message %q does not mention nil-updater discipline", msg)
		}
	}()

	NewUpdateTicketFieldHandler(nil)
}

func TestUpdateTicketFieldHandler_HappyPath(t *testing.T) {
	t.Parallel()

	updater := &fakeJiraUpdater{}
	h := NewUpdateTicketFieldHandler(updater)

	call := agentruntime.ToolCall{
		Name: UpdateTicketFieldName,
		Arguments: map[string]any{
			ToolArgIssueKey: "WK-42",
			ToolArgFields: map[string]any{
				"summary": "new title",
				"labels":  []any{"coord-touched"},
			},
		},
	}

	res, err := h(context.Background(), call)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if res.Error != "" {
		t.Errorf("ToolResult.Error = %q, want empty on happy path", res.Error)
	}
	if res.Output["issue_key"] != "WK-42" {
		t.Errorf("Output[issue_key] = %v, want WK-42", res.Output["issue_key"])
	}
	if res.Output["fields_updated"] != 2 {
		t.Errorf("Output[fields_updated] = %v, want 2", res.Output["fields_updated"])
	}
	if atomic.LoadInt32(&updater.calls) != 1 {
		t.Errorf("updater called %d times; want 1", updater.calls)
	}
	if updater.gotKey != "WK-42" {
		t.Errorf("updater got key %q, want WK-42", updater.gotKey)
	}
	if updater.gotFields["summary"] != "new title" {
		t.Errorf("updater got fields %v; missing summary=new title", updater.gotFields)
	}
}

func TestUpdateTicketFieldHandler_PreCheckContextCancelled(t *testing.T) {
	t.Parallel()

	updater := &fakeJiraUpdater{}
	h := NewUpdateTicketFieldHandler(updater)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	call := agentruntime.ToolCall{
		Arguments: map[string]any{
			ToolArgIssueKey: "WK-1",
			ToolArgFields:   map[string]any{"summary": "x"},
		},
	}
	_, err := h(ctx, call)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("handler returned %v on cancelled ctx, want context.Canceled", err)
	}
	if atomic.LoadInt32(&updater.calls) != 0 {
		t.Errorf("updater called despite cancelled ctx; calls=%d", updater.calls)
	}
}

func TestUpdateTicketFieldHandler_RefusesAssignee(t *testing.T) {
	t.Parallel()

	updater := &fakeJiraUpdater{}
	h := NewUpdateTicketFieldHandler(updater)

	call := agentruntime.ToolCall{
		Arguments: map[string]any{
			ToolArgIssueKey: "WK-42",
			ToolArgFields: map[string]any{
				"summary":        "x",
				AssigneeFieldKey: map[string]any{"accountId": "u-1"},
			},
		},
	}

	res, err := h(context.Background(), call)
	if err != nil {
		t.Fatalf("handler returned err on assignee refusal; want nil err + ToolResult.Error: %v", err)
	}
	if !strings.Contains(res.Error, "lead approval") {
		t.Errorf("ToolResult.Error = %q; want it to mention lead approval", res.Error)
	}
	if !strings.Contains(res.Error, "reassignment") {
		t.Errorf("ToolResult.Error = %q; want it to mention reassignment", res.Error)
	}
	if atomic.LoadInt32(&updater.calls) != 0 {
		t.Errorf("updater called despite assignee in args; dual-defense broken (calls=%d)", updater.calls)
	}
}

func TestUpdateTicketFieldHandler_RefusesAssigneeEvenWhenSoleField(t *testing.T) {
	t.Parallel()

	updater := &fakeJiraUpdater{}
	h := NewUpdateTicketFieldHandler(updater)

	call := agentruntime.ToolCall{
		Arguments: map[string]any{
			ToolArgIssueKey: "WK-42",
			ToolArgFields: map[string]any{
				AssigneeFieldKey: map[string]any{"accountId": "u-1"},
			},
		},
	}

	res, _ := h(context.Background(), call)
	if !strings.Contains(res.Error, "reassignment") {
		t.Errorf("sole-assignee call must still be refused; ToolResult.Error = %q", res.Error)
	}
	if atomic.LoadInt32(&updater.calls) != 0 {
		t.Errorf("updater called despite assignee-only; calls=%d", updater.calls)
	}
}

func TestUpdateTicketFieldHandler_ArgValidationRefusals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		args      map[string]any
		wantErrIn string
	}{
		{
			name:      "missing issue_key",
			args:      map[string]any{ToolArgFields: map[string]any{"summary": "x"}},
			wantErrIn: "issue_key",
		},
		{
			name: "non-string issue_key",
			args: map[string]any{
				ToolArgIssueKey: 42,
				ToolArgFields:   map[string]any{"summary": "x"},
			},
			wantErrIn: "issue_key must be a string",
		},
		{
			name: "empty issue_key",
			args: map[string]any{
				ToolArgIssueKey: "",
				ToolArgFields:   map[string]any{"summary": "x"},
			},
			wantErrIn: "issue_key must be non-empty",
		},
		{
			name:      "missing fields",
			args:      map[string]any{ToolArgIssueKey: "WK-1"},
			wantErrIn: "fields",
		},
		{
			name: "non-map fields",
			args: map[string]any{
				ToolArgIssueKey: "WK-1",
				ToolArgFields:   "summary=x",
			},
			wantErrIn: "fields must be an object",
		},
		{
			name: "empty fields map",
			args: map[string]any{
				ToolArgIssueKey: "WK-1",
				ToolArgFields:   map[string]any{},
			},
			wantErrIn: "fields must contain at least one entry",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			updater := &fakeJiraUpdater{}
			h := NewUpdateTicketFieldHandler(updater)

			res, err := h(context.Background(), agentruntime.ToolCall{Arguments: tc.args})
			if err != nil {
				t.Fatalf("handler returned Go err = %v; want nil err with ToolResult.Error", err)
			}
			if !strings.Contains(res.Error, tc.wantErrIn) {
				t.Errorf("ToolResult.Error = %q; want substring %q", res.Error, tc.wantErrIn)
			}
			if atomic.LoadInt32(&updater.calls) != 0 {
				t.Errorf("updater called despite validation refusal; calls=%d", updater.calls)
			}
		})
	}
}

func TestUpdateTicketFieldHandler_JiraErrorWrapped(t *testing.T) {
	t.Parallel()

	wantInner := errors.New("jira: UpdateFields WK-42: 503 service unavailable")
	updater := &fakeJiraUpdater{returnErr: wantInner}
	h := NewUpdateTicketFieldHandler(updater)

	call := agentruntime.ToolCall{
		Arguments: map[string]any{
			ToolArgIssueKey: "WK-42",
			ToolArgFields:   map[string]any{"summary": "x"},
		},
	}

	_, err := h(context.Background(), call)
	if !errors.Is(err, wantInner) {
		t.Fatalf("handler did not wrap inner error; got %v, want %v in chain", err, wantInner)
	}
	if !strings.HasPrefix(err.Error(), "coordinator: update_ticket_field:") {
		t.Errorf("handler err = %q; want prefix coordinator: update_ticket_field:", err.Error())
	}
}

// TestUpdateTicketFieldHandler_AssigneeRefusalPrecedesUpdaterCall_SourceGrep
// pins the gate ordering at the source-file level. A refactor that
// moved the updater call BEFORE the assignee check would silently
// pass every runtime test (the `assignee` arg would still trip the
// M8.1 jira whitelist downstream — but the dual-defense lesson
// requires both layers, and a regression here would erode the
// boundary documented in `doc.go`. The source-grep pattern matches
// the M8.1 lesson #7 (`TestUpdateFields_WhitelistEnforcement_SourceGrep`).
//
// `stripGoComments` removes both line and block comments so allowed
// mentions of `assignee` inside docstrings (the doc.go `dual-defense
// alongside the M8.1 jira whitelist that excludes "assignee"`
// paragraph, plus the AssigneeFieldKey const docblock) do NOT
// false-positive.
//
// Iter-1 review (codex Major #1) flagged that an earlier version of
// this AC matched the FIRST occurrence of `AssigneeFieldKey`, which
// landed on the const declaration NOT the runtime guard — so a
// refactor moving the guard below `updater.UpdateFields` still
// passed. The fix searches for the runtime-guard pattern
// `fields[AssigneeFieldKey]`, which only appears at the actual
// gate site (the const declaration uses bare `AssigneeFieldKey`,
// the docblock uses `"assignee"` literal). Pin the assertion to the
// guard, not to the symbol.
func TestUpdateTicketFieldHandler_AssigneeRefusalPrecedesUpdaterCall_SourceGrep(t *testing.T) {
	t.Parallel()

	srcPath := repoRelative(t, "core/pkg/coordinator/update_ticket_field.go")
	bytes, err := os.ReadFile(srcPath) //nolint:gosec // test reads a fixed repo-relative path.
	if err != nil {
		t.Fatalf("ReadFile %s: %v", srcPath, err)
	}
	body := stripGoComments(string(bytes))

	// Match the runtime guard pattern `fields[AssigneeFieldKey]`,
	// NOT the const symbol — the const declaration would silently
	// satisfy a symbol-only search even if the runtime check was
	// removed (codex iter-1 Major #1).
	const guardPattern = "fields[AssigneeFieldKey]"
	guardIdx := strings.Index(body, guardPattern)
	if guardIdx < 0 {
		t.Fatalf("source missing runtime guard %q; assignee gate removed?", guardPattern)
	}
	updaterIdx := strings.Index(body, "updater.UpdateFields(")
	if updaterIdx < 0 {
		t.Fatal("source missing updater.UpdateFields call site")
	}
	if guardIdx >= updaterIdx {
		t.Errorf(
			"assignee runtime guard must precede updater.UpdateFields; got guard@%d updater@%d",
			guardIdx, updaterIdx,
		)
	}

	// Refusal text const must surface in the source so the agent's
	// re-plan path has a stable substring to react on.
	if !strings.Contains(body, "errReassignmentBlocked") {
		t.Error("source missing errReassignmentBlocked reference; refusal channel removed?")
	}

	// Issue-key shape pre-validation must precede the updater call
	// (iter-1 codex Major #2 / critic M3): a malformed key reaching
	// the M8.1 layer would echo the raw value via `%q` in
	// `validateIssueKey`'s error wrap, leaking through the Go-error
	// chain into the M5.6.b reflection layer.
	//
	// Source-grep target: the CALL SITE of `readIssueKeyArg(`, not
	// the function definition (which lives below the closure body).
	// `readIssueKeyArg` is the helper that runs `issueKeyPattern.
	// MatchString`; pinning its CALL position relative to
	// `updater.UpdateFields(` is the right guarantee.
	const shapeGuardCallPattern = "readIssueKeyArg("
	shapeCallIdx := strings.Index(body, shapeGuardCallPattern)
	if shapeCallIdx < 0 {
		t.Fatalf("source missing issue-key shape guard call %q; PII gate removed?", shapeGuardCallPattern)
	}
	if shapeCallIdx >= updaterIdx {
		t.Errorf(
			"issue-key shape guard call must precede updater.UpdateFields; got call@%d updater@%d",
			shapeCallIdx, updaterIdx,
		)
	}
	// Defence-in-depth: also assert the regex match itself exists
	// somewhere in the file (not just the call helper). Position
	// doesn't matter — only presence — because the helper is the
	// stable boundary.
	if !strings.Contains(body, "issueKeyPattern.MatchString") {
		t.Error("source missing issueKeyPattern.MatchString; shape regex check removed?")
	}
}

// TestUpdateTicketFieldHandler_RefusesMalformedIssueKey_NoEcho pins
// the iter-1 PII finding (codex Major #2 / critic M3): a malformed
// `issue_key` MUST be refused BEFORE the M8.1 layer runs, AND the
// refusal text MUST NOT echo the raw value. A token-shaped key
// passed by mistake would otherwise leak via the M8.1
// `validateIssueKey` error wrap (`%q` of the raw key).
func TestUpdateTicketFieldHandler_RefusesMalformedIssueKey_NoEcho(t *testing.T) {
	t.Parallel()

	const tokenShapedKey = "REDACTION_PROBE_TOKEN_M82A_DO_NOT_LOG"

	updater := &fakeJiraUpdater{}
	h := NewUpdateTicketFieldHandler(updater)

	call := agentruntime.ToolCall{
		Arguments: map[string]any{
			ToolArgIssueKey: tokenShapedKey,
			ToolArgFields:   map[string]any{"summary": "x"},
		},
	}

	res, err := h(context.Background(), call)
	if err != nil {
		t.Fatalf("handler returned Go err on malformed key; PII could leak via wrap: %v", err)
	}
	if res.Error == "" {
		t.Fatal("handler accepted malformed issue_key; pre-network gate failed")
	}
	if !strings.Contains(res.Error, "Atlassian shape") {
		t.Errorf("ToolResult.Error = %q; want substring 'Atlassian shape'", res.Error)
	}
	if strings.Contains(res.Error, tokenShapedKey) {
		t.Errorf("ToolResult.Error echoed the raw key value (PII leak): %q", res.Error)
	}
	if atomic.LoadInt32(&updater.calls) != 0 {
		t.Errorf("updater called despite malformed key; PII gate broken (calls=%d)", updater.calls)
	}
}

// TestUpdateTicketFieldHandler_RefusesMalformedIssueKey_TableDriven
// covers the boundary cases of the regex `[A-Z][A-Z0-9_]+-[1-9][0-9]*`:
// lowercase project, missing dash, leading-zero numeric tail, only
// project, only number, embedded slash (path-traversal shape).
func TestUpdateTicketFieldHandler_RefusesMalformedIssueKey_TableDriven(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		key  string
	}{
		{"lowercase project", "wk-1"},
		{"missing dash", "WK1"},
		{"leading zero numeric tail", "WK-01"},
		{"zero numeric tail", "WK-0"},
		{"only project", "WK-"},
		{"only number", "-42"},
		{"embedded slash", "WK/42"},
		{"trailing whitespace", "WK-42 "},
		{"single-letter project", "W-42"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			updater := &fakeJiraUpdater{}
			h := NewUpdateTicketFieldHandler(updater)

			res, err := h(context.Background(), agentruntime.ToolCall{
				Arguments: map[string]any{
					ToolArgIssueKey: tc.key,
					ToolArgFields:   map[string]any{"summary": "x"},
				},
			})
			if err != nil {
				t.Fatalf("handler returned Go err on malformed key %q: %v", tc.key, err)
			}
			if !strings.Contains(res.Error, "Atlassian shape") {
				t.Errorf("key %q: ToolResult.Error = %q; want shape refusal", tc.key, res.Error)
			}
			if strings.Contains(res.Error, tc.key) {
				t.Errorf("key %q: refusal text echoed the raw key (PII leak): %q", tc.key, res.Error)
			}
			if atomic.LoadInt32(&updater.calls) != 0 {
				t.Errorf("key %q: updater called despite malformed key", tc.key)
			}
		})
	}
}

// TestUpdateTicketFieldHandler_DefensiveCopiesFieldsMap pins the
// iter-1 critic M2 finding: a future updater impl that retains the
// `fields` map MUST NOT observe post-call mutations from the LLM
// runtime that re-uses [agentruntime.ToolCall.Arguments]. The handler
// clones via `maps.Clone` before dispatch; this test asserts the
// updater receives a different map pointer than what was passed in.
func TestUpdateTicketFieldHandler_DefensiveCopiesFieldsMap(t *testing.T) {
	t.Parallel()

	updater := &fakeJiraUpdater{}
	h := NewUpdateTicketFieldHandler(updater)

	originalFields := map[string]any{"summary": "original"}
	call := agentruntime.ToolCall{
		Arguments: map[string]any{
			ToolArgIssueKey: "WK-42",
			ToolArgFields:   originalFields,
		},
	}

	if _, err := h(context.Background(), call); err != nil {
		t.Fatalf("handler: %v", err)
	}

	// Mutate the original map AFTER the handler returns; if the
	// updater retained the same map pointer (no defensive copy), the
	// mutation would bleed into the recorded `gotFields`.
	originalFields["summary"] = "MUTATED"

	updater.mu.Lock()
	got := updater.gotFields
	updater.mu.Unlock()

	if got["summary"] != "original" {
		t.Errorf("updater observed post-call mutation; defensive copy broken (got=%v)", got)
	}
	// Pointer-identity check: the slice headers must differ. (Two
	// distinct map[string]any cannot be compared with ==; compare
	// reflect.ValueOf(...).Pointer() OR write a value to one map and
	// confirm it does not appear in the other.)
	got["sentinel"] = "from-updater"
	if _, leaked := originalFields["sentinel"]; leaked {
		t.Error("updater's map mutation leaked into the original; defensive copy is shallow-of-shared-map")
	}
}

func TestUpdateTicketFieldHandler_ConcurrentInvocationSafety(t *testing.T) {
	t.Parallel()

	// 16 goroutines invoking the same handler against a shared updater.
	// Asserts the handler is goroutine-safe and that every call reaches
	// the updater (none short-circuit incorrectly under concurrent load).
	updater := &fakeJiraUpdater{}
	h := NewUpdateTicketFieldHandler(updater)

	const goroutines = 16
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			// idx+1 keeps the issue-key tail in the regex-valid
			// range (`[1-9][0-9]*`); WK-0 would be refused by the
			// new shape pre-validator.
			call := agentruntime.ToolCall{
				Arguments: map[string]any{
					ToolArgIssueKey: fmt.Sprintf("WK-%d", idx+1),
					ToolArgFields:   map[string]any{"summary": fmt.Sprintf("turn-%d", idx)},
				},
			}
			if _, err := h(context.Background(), call); err != nil {
				t.Errorf("goroutine %d: %v", idx, err)
			}
		}(i)
	}
	wg.Wait()

	if got := atomic.LoadInt32(&updater.calls); got != goroutines {
		t.Errorf("updater called %d times; want %d", got, goroutines)
	}
	updater.mu.Lock()
	defer updater.mu.Unlock()
	if len(updater.keyHistory) != goroutines {
		t.Errorf("key history len %d; want %d", len(updater.keyHistory), goroutines)
	}
}

// TestNewUpdateTicketFieldHandler_SatisfiesToolHandlerSignature is a
// test-bound (NOT package-init) assertion that the factory's return
// type is assignable to [agentruntime.ToolHandler]. A regression that
// drifted the factory signature would fail build at this line. The
// iter-1 review (critic m1) flagged the previous package-level
// `var _ = NewUpdateTicketFieldHandler(&fakeJiraUpdater{})` shape as
// (a) running the constructor at package init, (b) carrying a comment
// that misstated the safety reasoning. The test-bound form runs
// inside `go test` only, where construction overhead is irrelevant.
//
//nolint:staticcheck // ST1023: explicit type asserts the factory return shape; without the annotation the assignment is a tautology.
func TestNewUpdateTicketFieldHandler_SatisfiesToolHandlerSignature(t *testing.T) {
	t.Parallel()

	var h agentruntime.ToolHandler = NewUpdateTicketFieldHandler(&fakeJiraUpdater{})
	if h == nil {
		t.Fatal("factory returned nil handler")
	}
}

// repoRelative resolves a repo-relative path to an absolute path by
// climbing from this test file's directory up to the repo root.
// Mirrors the pattern in
// `core/pkg/manifest/watchmaster_seed_test.go::repoRelative`. The
// climb is THREE `..` (`core/pkg/coordinator/<file>` → `core/pkg/`
// → `core/` → repo root) regardless of test file location depth.
//
// TODO: hoist into `core/internal/testutil` once a third caller
// appears (M8.2.b's `find_overdue_tickets` source-grep AC will need
// it; iter-1 critic m2 flagged the duplication).
func repoRelative(t *testing.T, rel string) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", ".."))
	return filepath.Join(repoRoot, rel)
}

// stripGoComments removes Go line (`//…`) and block (`/*…*/`) comments
// from `src` so source-grep ACs do not trip on allowed mentions of
// the forbidden tokens inside docstrings. Mirrors the
// `core/pkg/jira/client_test.go::stripGoComments` helper introduced
// in the M8.1 iter-1 review.
func stripGoComments(src string) string {
	var b strings.Builder
	b.Grow(len(src))

	i := 0
	n := len(src)
	for i < n {
		// Block comment: /* … */
		if i+1 < n && src[i] == '/' && src[i+1] == '*' {
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				return b.String()
			}
			i = i + 2 + end + 2
			continue
		}
		// Line comment: // … \n
		if i+1 < n && src[i] == '/' && src[i+1] == '/' {
			end := strings.IndexByte(src[i:], '\n')
			if end < 0 {
				return b.String()
			}
			i += end
			continue
		}
		b.WriteByte(src[i])
		i++
	}
	return b.String()
}
