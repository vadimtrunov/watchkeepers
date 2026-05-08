package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/vadimtrunov/watchkeepers/core/internal/keep/server"
	"github.com/vadimtrunov/watchkeepers/core/pkg/llm/cost"
)

// agentUUID is the canonical agent_id used by every cost-rollup happy
// test. A fixed UUID lets the SQL fake echo the same value back into the
// `agent_id` column.
const agentUUID = "11111111-1111-4111-8111-111111111111"

// otherAgentUUID is used by the RLS-scoping case to assert that a row
// the runner does NOT surface (because RLS hid it) decodes as zero
// buckets.
const otherAgentUUID = "22222222-2222-4222-8222-222222222222"

// fixedFrom / fixedTo bracket every happy case. The window is
// intentionally wider than any single test's bucket count so the SQL
// fake does not have to reason about boundary conditions.
const (
	fixedFrom = "2026-04-01T00:00:00Z"
	fixedTo   = "2026-05-01T00:00:00Z"
)

// fakeBucket bundles the columns each happy-case fake row scans into
// the handler's costRollupBucket struct.
type fakeBucket struct {
	bucket       string
	model        string
	inputTokens  int64
	outputTokens int64
	nCalls       int64
}

// stageCostRollupRows builds a *FakeScopedRunner whose
// fake-tx.Query returns the supplied buckets verbatim. Used by every
// happy / RLS-empty case so the SQL surface stays unexercised by the
// unit tests; the integration suite covers the real query.
func stageCostRollupRows(rows []fakeBucket) *server.FakeScopedRunner {
	scans := make([]func(dest ...any) error, 0, len(rows))
	for _, row := range rows {
		row := row
		scans = append(scans, func(dest ...any) error {
			*dest[0].(*string) = row.bucket
			*dest[1].(*string) = agentUUID
			*dest[2].(*string) = row.model
			*dest[3].(*int64) = row.inputTokens
			*dest[4].(*int64) = row.outputTokens
			*dest[5].(*int64) = row.nCalls
			return nil
		})
	}
	query := func(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
		return server.NewFakeRows(scans, nil), nil
	}
	tx := server.NewFakeTx(server.FakeTxFns{Query: query})
	return &server.FakeScopedRunner{Tx: tx}
}

// doCostRollups issues an authed GET /v1/cost-rollups request via the
// supplied router. Returned recorder is parsed by the caller.
func doCostRollups(t *testing.T, h http.Handler, tok, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/v1/cost-rollups?"+query, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// happyDailyQuery builds the canonical query string for the daily
// happy path. Reused by every successful case so test bodies stay
// short.
func happyDailyQuery() string {
	return "agent_id=" + agentUUID +
		"&from=" + fixedFrom + "&to=" + fixedTo +
		"&grain=daily"
}

// TestCostRollups_HappyDaily covers test-plan case 1: events spread
// across 3 days produce 3 buckets per (agent, model) tuple, sorted, and
// every metric arithmetic matches.
func TestCostRollups_HappyDaily(t *testing.T) {
	rows := []fakeBucket{
		{bucket: "2026-04-10", model: "claude-sonnet-4", inputTokens: 100, outputTokens: 50, nCalls: 2},
		{bucket: "2026-04-11", model: "claude-sonnet-4", inputTokens: 200, outputTokens: 75, nCalls: 3},
		{bucket: "2026-04-12", model: "claude-sonnet-4", inputTokens: 50, outputTokens: 25, nCalls: 1},
	}
	runner := stageCostRollupRows(rows)
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := doCostRollups(t, h, tok, happyDailyQuery())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Buckets []struct {
			Bucket       string `json:"bucket"`
			AgentID      string `json:"agent_id"`
			Model        string `json:"model"`
			InputTokens  int64  `json:"input_tokens"`
			OutputTokens int64  `json:"output_tokens"`
			NCalls       int64  `json:"n_calls"`
		} `json:"buckets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, want := len(resp.Buckets), 3; got != want {
		t.Fatalf("len(buckets) = %d, want %d; body=%s", got, want, rec.Body.String())
	}
	for i, b := range resp.Buckets {
		if b.AgentID != agentUUID {
			t.Errorf("buckets[%d].agent_id = %q, want %q", i, b.AgentID, agentUUID)
		}
		if b.Model != rows[i].model {
			t.Errorf("buckets[%d].model = %q, want %q", i, b.Model, rows[i].model)
		}
		if b.InputTokens != rows[i].inputTokens || b.OutputTokens != rows[i].outputTokens {
			t.Errorf("buckets[%d] tokens = (%d,%d), want (%d,%d)", i,
				b.InputTokens, b.OutputTokens, rows[i].inputTokens, rows[i].outputTokens)
		}
		if b.NCalls != rows[i].nCalls {
			t.Errorf("buckets[%d].n_calls = %d, want %d", i, b.NCalls, rows[i].nCalls)
		}
	}
}

// TestCostRollups_HappyWeekly covers test-plan case 2: events spread
// across 2 weeks produce 2 buckets per (agent, model). The bucket date
// represents the Monday of the ISO week (Postgres `date_trunc('week',
// ...)` semantics).
func TestCostRollups_HappyWeekly(t *testing.T) {
	rows := []fakeBucket{
		{bucket: "2026-04-06", model: "claude-sonnet-4", inputTokens: 500, outputTokens: 200, nCalls: 7},
		{bucket: "2026-04-13", model: "claude-sonnet-4", inputTokens: 600, outputTokens: 250, nCalls: 8},
	}
	runner := stageCostRollupRows(rows)
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	q := "agent_id=" + agentUUID +
		"&from=" + fixedFrom + "&to=" + fixedTo +
		"&grain=weekly"
	rec := doCostRollups(t, h, tok, q)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Buckets []costBucketJSON `json:"buckets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, want := len(resp.Buckets), 2; got != want {
		t.Fatalf("len(buckets) = %d, want %d", got, want)
	}
	if resp.Buckets[0].Bucket != "2026-04-06" || resp.Buckets[1].Bucket != "2026-04-13" {
		t.Errorf("weekly bucket dates = (%q, %q), want Monday boundaries (2026-04-06, 2026-04-13)",
			resp.Buckets[0].Bucket, resp.Buckets[1].Bucket)
	}
}

// costBucketJSON mirrors the wire shape for decode-side assertions.
type costBucketJSON struct {
	Bucket       string `json:"bucket"`
	AgentID      string `json:"agent_id"`
	Model        string `json:"model"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	NCalls       int64  `json:"n_calls"`
}

// TestCostRollups_MixedModel covers test-plan case 3: events for the
// same agent+day across two models produce two buckets in the same
// day, sorted by model ASC.
func TestCostRollups_MixedModel(t *testing.T) {
	rows := []fakeBucket{
		{bucket: "2026-04-10", model: "claude-haiku-4", inputTokens: 100, outputTokens: 50, nCalls: 2},
		{bucket: "2026-04-10", model: "claude-sonnet-4", inputTokens: 300, outputTokens: 150, nCalls: 4},
	}
	runner := stageCostRollupRows(rows)
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := doCostRollups(t, h, tok, happyDailyQuery())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Buckets []costBucketJSON `json:"buckets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := len(resp.Buckets); got != 2 {
		t.Fatalf("len(buckets) = %d, want 2", got)
	}
	if resp.Buckets[0].Model != "claude-haiku-4" || resp.Buckets[1].Model != "claude-sonnet-4" {
		t.Errorf("models = (%q, %q), want sorted ASC (haiku, sonnet)",
			resp.Buckets[0].Model, resp.Buckets[1].Model)
	}
}

// TestCostRollups_EmptyWindow covers test-plan case 4: zero events
// returns 200 with `{"buckets": []}` — never 404.
func TestCostRollups_EmptyWindow(t *testing.T) {
	runner := stageCostRollupRows(nil)
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := doCostRollups(t, h, tok, happyDailyQuery())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (empty must NOT be 404); body=%s", rec.Code, rec.Body.String())
	}
	body := strings.TrimSpace(rec.Body.String())
	if body != `{"buckets":[]}` {
		t.Errorf("body = %q, want exactly {\"buckets\":[]}", body)
	}
}

// rlsScope is the distinct scope used by TestCostRollups_RLSScoping to
// simulate a request from a different org. Using "user:<uuid>" keeps the
// value valid per auth.ValidScope while being visibly distinct from the
// "org" scope every happy-path test uses.
const rlsScope = "user:" + otherAgentUUID

// TestCostRollups_RLSScoping covers test-plan case 5: a claim whose
// scope hides the agent's rows surfaces as zero buckets, not as a
// 4xx/5xx. The fake runner returns no rows when the staged-query path
// runs, simulating RLS rejecting every row.
//
// The test also pins AC4's RLS contract: the handler MUST forward the
// request's claim scope to WithScope unchanged, not substitute a
// default. This is verified by asserting runner.LastClaim.Scope after
// the handler returns.
func TestCostRollups_RLSScoping(t *testing.T) {
	// No rows staged — the fake runner walks the empty scan list and
	// the handler decodes `{"buckets": []}`. Equivalent to "RLS
	// filtered every row".
	runner := stageCostRollupRows(nil)
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	// Mint with a scope DISTINCT from the happy-path "org" scope so the
	// assertion below actually pins propagation rather than merely
	// observing the default.
	tok := mustMintToken(t, ti, rlsScope)

	q := "agent_id=" + otherAgentUUID +
		"&from=" + fixedFrom + "&to=" + fixedTo +
		"&grain=daily"
	rec := doCostRollups(t, h, tok, q)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Buckets []costBucketJSON `json:"buckets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Buckets) != 0 {
		t.Errorf("buckets = %+v, want empty (RLS-hidden rows)", resp.Buckets)
	}
	// AC4 RLS contract: the handler must propagate the request's claim
	// scope to WithScope verbatim so the DB can apply row-level security
	// for the requesting org. A mismatch here means the handler is
	// substituting a default scope instead of forwarding the token's scope.
	if runner.LastClaim.Scope != rlsScope {
		t.Errorf("runner.LastClaim.Scope = %q, want %q (handler must forward request scope to WithScope)",
			runner.LastClaim.Scope, rlsScope)
	}
}

// TestCostRollups_NegativeParams covers test-plan cases 6–10 in a
// table — every malformed input must produce 400 with a stable error
// code and zero PII / SQL leak.
func TestCostRollups_NegativeParams(t *testing.T) {
	cases := []struct {
		name      string
		query     string
		wantError string
	}{
		{
			name:      "missing_agent_id",
			query:     "from=" + fixedFrom + "&to=" + fixedTo + "&grain=daily",
			wantError: "missing_agent_id",
		},
		{
			name:      "non_uuid_agent_id",
			query:     "agent_id=not-a-uuid&from=" + fixedFrom + "&to=" + fixedTo + "&grain=daily",
			wantError: "invalid_agent_id",
		},
		{
			name:      "bad_grain",
			query:     "agent_id=" + agentUUID + "&from=" + fixedFrom + "&to=" + fixedTo + "&grain=hourly",
			wantError: "invalid_grain",
		},
		{
			name:      "missing_grain",
			query:     "agent_id=" + agentUUID + "&from=" + fixedFrom + "&to=" + fixedTo,
			wantError: "missing_grain",
		},
		{
			name: "from_greater_than_to",
			query: "agent_id=" + agentUUID +
				"&from=2026-05-01T00:00:00Z&to=2026-04-01T00:00:00Z&grain=daily",
			wantError: "invalid_range",
		},
		{
			name: "bad_rfc3339_from",
			query: "agent_id=" + agentUUID +
				"&from=2026-04-01&to=" + fixedTo + "&grain=daily",
			wantError: "invalid_from",
		},
		{
			name: "bad_rfc3339_to",
			query: "agent_id=" + agentUUID +
				"&from=" + fixedFrom + "&to=tomorrow&grain=daily",
			wantError: "invalid_to",
		},
		{
			name:      "missing_from",
			query:     "agent_id=" + agentUUID + "&to=" + fixedTo + "&grain=daily",
			wantError: "missing_from",
		},
		{
			name:      "missing_to",
			query:     "agent_id=" + agentUUID + "&from=" + fixedFrom + "&grain=daily",
			wantError: "missing_to",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Each case uses its own fresh runner; none of them should
			// reach the runner — the parser short-circuits at 400.
			runner := stageCostRollupRows(nil)
			h, ti := writeRouterForTest(t, mustFixedNow(), runner)
			tok := mustMintToken(t, ti, "org")

			rec := doCostRollups(t, h, tok, tc.query)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			ensureJSONEnvelope(t, rec)
			var env struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if env.Error != tc.wantError {
				t.Errorf("error = %q, want %q (body=%s)", env.Error, tc.wantError, rec.Body.String())
			}
			if runner.FnInvoked {
				t.Errorf("runner.WithScope was invoked on a 400 path (must short-circuit before SQL)")
			}
		})
	}
}

// TestCostRollups_DBErrorNoLeak covers test-plan case 11: a runner-level
// error surfaces as 500 with a stable code and zero PII / SQL detail.
func TestCostRollups_DBErrorNoLeak(t *testing.T) {
	runner := &server.FakeScopedRunner{
		FnReturns: errors.New("relation \"watchkeeper.keepers_log\" SELECT failure: secret payload"),
	}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := doCostRollups(t, h, tok, happyDailyQuery())
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// The error envelope must NOT echo the SQL or table name back to
	// the caller. Pin the absence of every string that could leak the
	// underlying schema or driver-level diagnostic.
	for _, banned := range []string{
		"keepers_log", "SELECT", "secret payload", "watchkeeper.",
	} {
		if strings.Contains(body, banned) {
			t.Errorf("body contains leaked detail %q (PII/SQL leak): %s", banned, body)
		}
	}
	var env struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error != "cost_rollups_failed" {
		t.Errorf("error = %q, want cost_rollups_failed", env.Error)
	}
}

// TestCostRollups_PIIClosedSet covers test-plan case 12: the response
// JSON must contain ONLY the documented closed-set keys per bucket.
// A future field rename or accidental JOIN that adds a row property
// surfaces here.
func TestCostRollups_PIIClosedSet(t *testing.T) {
	rows := []fakeBucket{
		{bucket: "2026-04-10", model: "claude-sonnet-4", inputTokens: 100, outputTokens: 50, nCalls: 2},
	}
	runner := stageCostRollupRows(rows)
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := doCostRollups(t, h, tok, happyDailyQuery())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Decode into map[string]any so the test sees every key the wire
	// shape carries — a typed struct would silently absorb a stray
	// new field.
	var raw struct {
		Buckets []map[string]any `json:"buckets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(raw.Buckets) != 1 {
		t.Fatalf("len(buckets) = %d, want 1", len(raw.Buckets))
	}
	allowed := make(map[string]struct{}, len(server.CostRollupAllowedBucketKeys))
	for _, k := range server.CostRollupAllowedBucketKeys {
		allowed[k] = struct{}{}
	}
	for k := range raw.Buckets[0] {
		if _, ok := allowed[k]; !ok {
			t.Errorf("bucket carries undocumented key %q (PII discipline regression)", k)
		}
	}
	for k := range allowed {
		if _, present := raw.Buckets[0][k]; !present {
			t.Errorf("bucket missing required key %q", k)
		}
	}
}

// TestCostRollupsEventTypeFilter_HasReportCostPrefix is the cross-PR
// vocabulary regression pin per AC9 / M6.3.e LESSONS § "Cross-PR
// vocabulary alignment via prefix-test regression". The rollup
// query's event_type prefix MUST stay byte-equal-as-prefix to the
// constant emitted by `core/pkg/llm/cost`. A re-key on either side
// trips this test rather than silently producing empty rollups.
func TestCostRollupsEventTypeFilter_HasReportCostPrefix(t *testing.T) {
	if !strings.HasPrefix(server.CostRollupEventTypePrefix, "llm_turn_cost") {
		t.Fatalf("CostRollupEventTypePrefix = %q does NOT start with %q", server.CostRollupEventTypePrefix, "llm_turn_cost")
	}
	if !strings.HasPrefix(cost.EventTypeLLMCallCompleted, server.CostRollupEventTypePrefix) {
		t.Fatalf("cost.EventTypeLLMCallCompleted = %q is NOT prefixed by rollup filter %q",
			cost.EventTypeLLMCallCompleted, server.CostRollupEventTypePrefix)
	}
	if !server.EventTypePrefixHasCostFamily(cost.EventTypeLLMCallCompleted) {
		t.Fatalf("EventTypePrefixHasCostFamily(%q) = false; rollup filter must include cost family",
			cost.EventTypeLLMCallCompleted)
	}
}

// TestCostRollups_ClaimReachesRunner is the wiring assertion: a valid
// request threads the verified claim into the scopedRunner so the
// auth → scope → handler chain is intact end-to-end.
func TestCostRollups_ClaimReachesRunner(t *testing.T) {
	runner := &server.FakeScopedRunner{FnReturns: errors.New("db offline")}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := doCostRollups(t, h, tok, happyDailyQuery())
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	if !runner.FnInvoked {
		t.Error("runner.WithScope was never invoked")
	}
	if runner.LastClaim.Scope != "org" {
		t.Errorf("claim scope = %q, want %q", runner.LastClaim.Scope, "org")
	}
}

// TestCostRollups_UnauthenticatedRejects mirrors the standard auth-wall
// regression: missing token → 401.
func TestCostRollups_UnauthenticatedRejects(t *testing.T) {
	runner := stageCostRollupRows(nil)
	h, _ := writeRouterForTest(t, mustFixedNow(), runner)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/v1/cost-rollups?"+happyDailyQuery(), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}
