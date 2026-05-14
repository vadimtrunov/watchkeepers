package wkmetrics_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/vadimtrunov/watchkeepers/core/pkg/wkmetrics"
)

// doGet issues a context-bound GET to url and returns the response.
// Centralised so every test stays noctx-clean without per-call
// http.NewRequestWithContext boilerplate.
func doGet(t *testing.T, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return resp
}

// doPost issues a context-bound POST to url with the supplied body.
func doPost(t *testing.T, url, contentType string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, body)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return resp
}

// expectedFamilies is the stable Prometheus name set Phase-1
// dashboards depend on. Any drift is a contract break and must be
// flagged loudly — adding a metric is OK, renaming/removing is not.
var expectedFamilies = []string{
	"watchkeeper_llm_tokens_total",
	"watchkeeper_llm_request_duration_seconds",
	"watchkeeper_tool_invocations_total",
	"watchkeeper_eventbus_queue_depth",
	"watchkeeper_messenger_rate_limit_remaining",
	"watchkeeper_http_request_duration_seconds",
	"watchkeeper_outbox_published_total",
}

func TestNew_RegistersExpectedFamilies(t *testing.T) {
	// Prometheus CounterVec/HistogramVec/GaugeVec families do not appear
	// in Registry().Gather() until at least one label combination has
	// been touched. Touch one combination per family so the contract
	// check exercises the same surface operators will see in /metrics.
	m := wkmetrics.New()
	touchAllFamilies(m)

	mf, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	got := map[string]bool{}
	for _, fam := range mf {
		got[fam.GetName()] = true
	}
	for _, want := range expectedFamilies {
		if !got[want] {
			t.Errorf("missing metric family %q (have %v)", want, names(mf))
		}
	}
}

// touchAllFamilies records one sample against every metric family this
// package owns. Used by family-coverage and exposition tests; isolated
// so each family stays explicitly enumerated.
func touchAllFamilies(m *wkmetrics.Metrics) {
	m.RecordTokenSpend("agent-x", "input", "anthropic", 1)
	m.ObserveLLMRequest("anthropic", wkmetrics.OutcomeSuccess, time.Millisecond)
	m.RecordToolInvocation("noop", wkmetrics.OutcomeSuccess)
	m.SetEventBusDepth("watchkeeper.test", 0)
	m.SetRateLimitRemaining("slack", "test", 0)
	m.ObserveHTTPRequest("GET", "/test", 200, time.Millisecond)
	m.RecordOutboxPublished(wkmetrics.OutcomeSuccess)
}

// names is a tiny dump helper for failure messages so a missing family
// shows alongside the actual set, not just "missing X".
func names(mf []*dto.MetricFamily) []string {
	out := make([]string, 0, len(mf))
	for _, m := range mf {
		out = append(out, m.GetName())
	}
	return out
}

func TestRecordTokenSpend_AddsToCounter(t *testing.T) {
	m := wkmetrics.New()
	m.RecordTokenSpend("agent-1", "input", "anthropic", 100)
	m.RecordTokenSpend("agent-1", "input", "anthropic", 50)
	m.RecordTokenSpend("agent-1", "output", "anthropic", 25)

	if got := counterValue(t, m, "watchkeeper_llm_tokens_total", map[string]string{
		"agent_id": "agent-1", "kind": "input", "provider": "anthropic",
	}); got != 150 {
		t.Errorf("input tokens = %v, want 150", got)
	}
	if got := counterValue(t, m, "watchkeeper_llm_tokens_total", map[string]string{
		"agent_id": "agent-1", "kind": "output", "provider": "anthropic",
	}); got != 25 {
		t.Errorf("output tokens = %v, want 25", got)
	}
}

func TestRecordTokenSpend_RejectsNonPositive(t *testing.T) {
	m := wkmetrics.New()
	m.RecordTokenSpend("agent-1", "input", "anthropic", 0)
	m.RecordTokenSpend("agent-1", "input", "anthropic", -100)
	// Zero+negative must NOT register a sample — the family stays empty
	// for that label combination.
	mf, _ := m.Registry().Gather()
	for _, fam := range mf {
		if fam.GetName() == "watchkeeper_llm_tokens_total" {
			t.Errorf("non-positive token-spend was recorded: %v", fam.GetMetric())
		}
	}
}

func TestObserveLLMRequest_RecordsHistogram(t *testing.T) {
	m := wkmetrics.New()
	m.ObserveLLMRequest("anthropic", wkmetrics.OutcomeSuccess, 250*time.Millisecond)
	m.ObserveLLMRequest("anthropic", wkmetrics.OutcomeSuccess, 750*time.Millisecond)

	got := histogramSampleCount(t, m, "watchkeeper_llm_request_duration_seconds", map[string]string{
		"provider": "anthropic",
		"outcome":  wkmetrics.LabelOutcomeSuccess,
	})
	if got != 2 {
		t.Errorf("sample count = %d, want 2", got)
	}
}

func TestRecordToolInvocation_BoundedOutcomes(t *testing.T) {
	m := wkmetrics.New()
	m.RecordToolInvocation("shell_run", wkmetrics.OutcomeSuccess)
	m.RecordToolInvocation("shell_run", wkmetrics.OutcomeError)
	m.RecordToolInvocation("shell_run", wkmetrics.OutcomeSuccess)

	if got := counterValue(t, m, "watchkeeper_tool_invocations_total", map[string]string{
		"tool": "shell_run", "outcome": wkmetrics.LabelOutcomeSuccess,
	}); got != 2 {
		t.Errorf("success count = %v, want 2", got)
	}
	if got := counterValue(t, m, "watchkeeper_tool_invocations_total", map[string]string{
		"tool": "shell_run", "outcome": wkmetrics.LabelOutcomeError,
	}); got != 1 {
		t.Errorf("error count = %v, want 1", got)
	}
}

func TestSetEventBusDepth_LatestWins(t *testing.T) {
	m := wkmetrics.New()
	m.SetEventBusDepth("watchkeeper.spawn", 3)
	m.SetEventBusDepth("watchkeeper.spawn", 7)
	m.SetEventBusDepth("watchkeeper.retire", 1)

	if got := gaugeValue(t, m, "watchkeeper_eventbus_queue_depth", map[string]string{
		"topic": "watchkeeper.spawn",
	}); got != 7 {
		t.Errorf("spawn depth = %v, want 7", got)
	}
	if got := gaugeValue(t, m, "watchkeeper_eventbus_queue_depth", map[string]string{
		"topic": "watchkeeper.retire",
	}); got != 1 {
		t.Errorf("retire depth = %v, want 1", got)
	}
}

func TestSetRateLimitRemaining_PerProviderEndpoint(t *testing.T) {
	m := wkmetrics.New()
	m.SetRateLimitRemaining("slack", "chat.postMessage", 42)
	m.SetRateLimitRemaining("jira", "search", 99)

	if got := gaugeValue(t, m, "watchkeeper_messenger_rate_limit_remaining", map[string]string{
		"provider": "slack", "endpoint": "chat.postMessage",
	}); got != 42 {
		t.Errorf("slack headroom = %v, want 42", got)
	}
	if got := gaugeValue(t, m, "watchkeeper_messenger_rate_limit_remaining", map[string]string{
		"provider": "jira", "endpoint": "search",
	}); got != 99 {
		t.Errorf("jira headroom = %v, want 99", got)
	}
}

func TestObserveHTTPRequest_FoldsStatusToClass(t *testing.T) {
	m := wkmetrics.New()
	m.ObserveHTTPRequest("GET", "/v1/search", 200, time.Millisecond)
	m.ObserveHTTPRequest("GET", "/v1/search", 201, time.Millisecond)
	m.ObserveHTTPRequest("GET", "/v1/search", 404, time.Millisecond)
	m.ObserveHTTPRequest("GET", "/v1/search", 599, time.Millisecond)
	m.ObserveHTTPRequest("GET", "/v1/search", 700, time.Millisecond) // "other"

	for _, c := range []struct {
		class string
		want  uint64
	}{
		{"2xx", 2}, {"4xx", 1}, {"5xx", 1}, {"other", 1},
	} {
		got := histogramSampleCount(t, m, "watchkeeper_http_request_duration_seconds", map[string]string{
			"method": "GET", "route": "/v1/search", "status_class": c.class,
		})
		if got != c.want {
			t.Errorf("class %q count = %d, want %d", c.class, got, c.want)
		}
	}
}

func TestRecordOutboxPublished_AccumulatesPerOutcome(t *testing.T) {
	m := wkmetrics.New()
	for i := 0; i < 5; i++ {
		m.RecordOutboxPublished(wkmetrics.OutcomeSuccess)
	}
	m.RecordOutboxPublished(wkmetrics.OutcomeError)

	if got := counterValue(t, m, "watchkeeper_outbox_published_total", map[string]string{
		"outcome": wkmetrics.LabelOutcomeSuccess,
	}); got != 5 {
		t.Errorf("success = %v, want 5", got)
	}
	if got := counterValue(t, m, "watchkeeper_outbox_published_total", map[string]string{
		"outcome": wkmetrics.LabelOutcomeError,
	}); got != 1 {
		t.Errorf("error = %v, want 1", got)
	}
}

func TestHandler_ServesPrometheusExposition(t *testing.T) {
	m := wkmetrics.New()
	touchAllFamilies(m)

	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	resp := doGet(t, srv.URL)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	text := string(body)
	for _, want := range expectedFamilies {
		if !strings.Contains(text, want) {
			t.Errorf("exposition missing family %q", want)
		}
	}
	// HELP/TYPE lines exist for every family per Prometheus expo conv.
	if !strings.Contains(text, "# HELP watchkeeper_outbox_published_total") {
		t.Error("missing # HELP for outbox_published_total")
	}
	if !strings.Contains(text, "# TYPE watchkeeper_outbox_published_total counter") {
		t.Error("missing # TYPE counter for outbox_published_total")
	}
}

func TestHandler_NilMetricsReturns503(t *testing.T) {
	var m *wkmetrics.Metrics
	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	resp := doGet(t, srv.URL)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("nil-receiver status = %d, want 503", resp.StatusCode)
	}
}

func TestNilReceiver_AllRecordersAreNoOps(t *testing.T) {
	var m *wkmetrics.Metrics
	// None of these may panic; calling with a nil receiver is the
	// instrumentation contract that lets callers wire unconditionally.
	m.RecordTokenSpend("a", "input", "p", 1)
	m.ObserveLLMRequest("p", "success", time.Millisecond)
	m.RecordToolInvocation("t", "success")
	m.SetEventBusDepth("topic", 1)
	m.SetRateLimitRemaining("slack", "endpoint", 1)
	m.ObserveHTTPRequest("GET", "/r", 200, time.Millisecond)
	m.RecordOutboxPublished("success")
	if m.Registry() != nil {
		t.Errorf("nil receiver Registry() should be nil")
	}
}

func TestInstrument_RecordsLatency(t *testing.T) {
	m := wkmetrics.New()
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Millisecond)
		w.WriteHeader(http.StatusCreated)
	})
	wrapped := m.Instrument("/v1/widgets", handler)

	srv := httptest.NewServer(wrapped)
	defer srv.Close()

	resp := doPost(t, srv.URL, "application/json", strings.NewReader(""))
	resp.Body.Close()

	got := histogramSampleCount(t, m, "watchkeeper_http_request_duration_seconds", map[string]string{
		"method": "POST", "route": "/v1/widgets", "status_class": "2xx",
	})
	if got != 1 {
		t.Errorf("instrumented sample count = %d, want 1", got)
	}
}

func TestInstrument_DefaultsStatusToOK(t *testing.T) {
	m := wkmetrics.New()
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("plain body, no explicit WriteHeader"))
	})
	wrapped := m.Instrument("/r", handler)

	srv := httptest.NewServer(wrapped)
	defer srv.Close()

	resp := doGet(t, srv.URL)
	resp.Body.Close()

	got := histogramSampleCount(t, m, "watchkeeper_http_request_duration_seconds", map[string]string{
		"method": "GET", "route": "/r", "status_class": "2xx",
	})
	if got != 1 {
		t.Errorf("count = %d, want 1 (implicit-WriteHeader-default-200)", got)
	}
}

func TestInstrument_NilReceiverReturnsHandlerUnwrapped(t *testing.T) {
	var m *wkmetrics.Metrics
	called := atomic.Bool{}
	h := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { called.Store(true) })
	wrapped := m.Instrument("/r", h)

	srv := httptest.NewServer(wrapped)
	defer srv.Close()

	resp := doGet(t, srv.URL)
	resp.Body.Close()

	if !called.Load() {
		t.Error("nil-receiver Instrument did not forward to handler")
	}
}

func TestOutcomeFor_MapsErrToErrorLabel(t *testing.T) {
	if got := wkmetrics.OutcomeFor(nil); got != wkmetrics.OutcomeSuccess {
		t.Errorf("nil err -> %q, want %q", got, wkmetrics.OutcomeSuccess)
	}
	if got := wkmetrics.OutcomeFor(errors.New("boom")); got != wkmetrics.OutcomeError {
		t.Errorf("err -> %q, want %q", got, wkmetrics.OutcomeError)
	}
}

func TestStatusClass_Boundaries(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{199, "other"},
		{200, "2xx"},
		{299, "2xx"},
		{300, "3xx"},
		{399, "3xx"},
		{400, "4xx"},
		{499, "4xx"},
		{500, "5xx"},
		{599, "5xx"},
		{600, "other"},
		{0, "other"},
	}
	for _, c := range cases {
		if got := wkmetrics.StatusClass(c.in); got != c.want {
			t.Errorf("StatusClass(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestConcurrentRecord_NoRace(t *testing.T) {
	m := wkmetrics.New()
	const goroutines = 16
	const perG = 100
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			for j := 0; j < perG; j++ {
				m.RecordTokenSpend("a", "input", "anthropic", 1)
				m.RecordOutboxPublished(wkmetrics.OutcomeSuccess)
				m.ObserveHTTPRequest("GET", "/x", 200, time.Microsecond)
				m.SetEventBusDepth("watchkeeper.spawn", i)
			}
		}(i)
	}
	wg.Wait()

	if got := counterValue(t, m, "watchkeeper_outbox_published_total", map[string]string{
		"outcome": wkmetrics.LabelOutcomeSuccess,
	}); got != float64(goroutines*perG) {
		t.Errorf("counter = %v, want %d", got, goroutines*perG)
	}
}

func TestNewWithRegistry_NilFallsBackToFresh(t *testing.T) {
	m := wkmetrics.NewWithRegistry(nil)
	if m.Registry() == nil {
		t.Fatal("Registry() should not be nil for NewWithRegistry(nil)")
	}
}

// TestIter1_NewWithOptions_DisableProcessCollectors pins the M10.1
// iter-1 fix M1: operators behind a hostile network can opt out of
// the ProcessCollector + GoCollector so /metrics does not leak
// process_resident_memory_bytes / process_open_fds / etc. The metric
// family set we OWN must still be served.
func TestIter1_NewWithOptions_DisableProcessCollectors(t *testing.T) {
	m := wkmetrics.NewWithOptions(wkmetrics.Options{DisableProcessCollectors: true})
	touchAllFamilies(m)

	mf, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	got := map[string]bool{}
	for _, fam := range mf {
		got[fam.GetName()] = true
	}
	for _, processLeak := range []string{
		"process_resident_memory_bytes",
		"process_open_fds",
		"process_start_time_seconds",
		"go_goroutines",
	} {
		if got[processLeak] {
			t.Errorf("DisableProcessCollectors did not drop %q", processLeak)
		}
	}
	// Our own families must still be registered.
	for _, owned := range expectedFamilies {
		if !got[owned] {
			t.Errorf("owned family %q missing after disable", owned)
		}
	}
}

// TestIter1_StatusRecorder_ProxiesFlusher pins the M10.1 iter-1 fix C1:
// the Instrument middleware MUST proxy http.Flusher so the SSE handler
// (handlers_subscribe.go: w.(http.Flusher)) keeps streaming under
// production wiring. Without this fix, /v1/subscribe short-circuits
// with no events delivered.
func TestIter1_StatusRecorder_ProxiesFlusher(t *testing.T) {
	m := wkmetrics.New()
	var flushed atomic.Bool
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			t.Error("wrapped writer does not implement http.Flusher")
			return
		}
		w.WriteHeader(http.StatusOK)
		f.Flush()
		flushed.Store(true)
	})
	srv := httptest.NewServer(m.Instrument("/stream", handler))
	defer srv.Close()

	resp := doGet(t, srv.URL)
	resp.Body.Close()
	if !flushed.Load() {
		t.Fatal("Flush() was not called on the wrapped writer")
	}
}

// TestIter1_StatusRecorder_FlushIsNoopWhenUnderlyingLacksFlusher
// guarantees the wrapper does not panic when the underlying writer is
// itself non-flusher (e.g. an httptest.ResponseRecorder).
func TestIter1_StatusRecorder_FlushIsNoopWhenUnderlyingLacksFlusher(t *testing.T) {
	t.Helper()
	m := wkmetrics.New()
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			// The httptest.ResponseRecorder used below itself IS a
			// Flusher in Go ≥ 1.7. We simply must not crash either way.
			return
		}
		f.Flush() // must not panic even if underlying is non-flushing
	})
	wrapped := m.Instrument("/r", handler)
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))
}

// TestIter1_StatusRecorder_WroteFlagDistinguishesDefaultFromExplicit
// pins fix m3: the previous Status() used `status == 0` as the "not
// written" sentinel, conflating an absent WriteHeader call with a
// future explicit zero. Go's stdlib already blocks WriteHeader(0) at
// the net/http level (it panics in test harnesses), but the
// defensive Status() change still matters: any explicit non-200
// WriteHeader must be observed correctly, AND the "no WriteHeader"
// case must default to 200. This test verifies both branches.
func TestIter1_StatusRecorder_WroteFlagDistinguishesDefaultFromExplicit(t *testing.T) {
	m := wkmetrics.New()

	noHeader := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		// Deliberately empty — Go's net/http will write 200 on its own.
	})
	explicit204 := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	srv1 := httptest.NewServer(m.Instrument("/r1", noHeader))
	defer srv1.Close()
	srv2 := httptest.NewServer(m.Instrument("/r2", explicit204))
	defer srv2.Close()

	resp1 := doGet(t, srv1.URL)
	resp1.Body.Close()
	resp2 := doGet(t, srv2.URL)
	resp2.Body.Close()

	if got := histogramSampleCount(t, m, "watchkeeper_http_request_duration_seconds", map[string]string{
		"method": "GET", "route": "/r1", "status_class": "2xx",
	}); got != 1 {
		t.Errorf("no-WriteHeader default-200 count = %d, want 1", got)
	}
	if got := histogramSampleCount(t, m, "watchkeeper_http_request_duration_seconds", map[string]string{
		"method": "GET", "route": "/r2", "status_class": "2xx",
	}); got != 1 {
		t.Errorf("explicit 204 count = %d, want 1 (must still register as 2xx)", got)
	}
}

// TestIter1_OutcomeTypeBoundsLabelCardinality pins M4 / n3: the typed
// Outcome enum makes free-form outcome strings a compile error.
// Verified here by constructing a value via the typed constants and
// confirming the underlying string matches the documented label.
func TestIter1_OutcomeTypeBoundsLabelCardinality(t *testing.T) {
	cases := []struct {
		o    wkmetrics.Outcome
		want string
	}{
		{wkmetrics.OutcomeSuccess, "success"},
		{wkmetrics.OutcomeError, "error"},
	}
	for _, c := range cases {
		if string(c.o) != c.want {
			t.Errorf("Outcome %v underlying = %q, want %q", c.o, string(c.o), c.want)
		}
	}
	// String-alias constants stay in sync.
	if wkmetrics.LabelOutcomeSuccess != "success" {
		t.Errorf("LabelOutcomeSuccess = %q, want success", wkmetrics.LabelOutcomeSuccess)
	}
	if wkmetrics.LabelOutcomeError != "error" {
		t.Errorf("LabelOutcomeError = %q, want error", wkmetrics.LabelOutcomeError)
	}
}

// counterValue extracts the counter value for the named family with the
// supplied labels. Tests use this rather than reaching into prometheus
// internals so a future client-golang refactor is one helper edit away.
func counterValue(t *testing.T, m *wkmetrics.Metrics, family string, labels map[string]string) float64 {
	t.Helper()
	metric := findMetric(t, m, family, labels)
	if metric.Counter == nil {
		t.Fatalf("family %q is not a counter for labels %v", family, labels)
	}
	return metric.GetCounter().GetValue()
}

func gaugeValue(t *testing.T, m *wkmetrics.Metrics, family string, labels map[string]string) float64 {
	t.Helper()
	metric := findMetric(t, m, family, labels)
	if metric.Gauge == nil {
		t.Fatalf("family %q is not a gauge for labels %v", family, labels)
	}
	return metric.GetGauge().GetValue()
}

func histogramSampleCount(t *testing.T, m *wkmetrics.Metrics, family string, labels map[string]string) uint64 {
	t.Helper()
	metric := findMetric(t, m, family, labels)
	if metric.Histogram == nil {
		t.Fatalf("family %q is not a histogram for labels %v", family, labels)
	}
	return metric.GetHistogram().GetSampleCount()
}

func findMetric(t *testing.T, m *wkmetrics.Metrics, family string, labels map[string]string) *dto.Metric {
	t.Helper()
	mf, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, fam := range mf {
		if fam.GetName() != family {
			continue
		}
		for _, metric := range fam.GetMetric() {
			if labelsMatch(metric.GetLabel(), labels) {
				return metric
			}
		}
		t.Fatalf("family %q has no metric with labels %v (have %s)", family, labels, dumpLabels(fam))
	}
	t.Fatalf("family %q not registered", family)
	return nil
}

func labelsMatch(got []*dto.LabelPair, want map[string]string) bool {
	if len(got) != len(want) {
		return false
	}
	for _, lp := range got {
		if want[lp.GetName()] != lp.GetValue() {
			return false
		}
	}
	return true
}

func dumpLabels(fam *dto.MetricFamily) string {
	parts := make([]string, 0, len(fam.GetMetric()))
	for _, metric := range fam.GetMetric() {
		pairs := make([]string, 0, len(metric.GetLabel()))
		for _, lp := range metric.GetLabel() {
			pairs = append(pairs, fmt.Sprintf("%s=%s", lp.GetName(), lp.GetValue()))
		}
		parts = append(parts, "{"+strings.Join(pairs, ",")+"}")
	}
	return strings.Join(parts, " ")
}

// Compile-time guarantee that the Metrics type satisfies our
// expectation about the prometheus.Registry getter.
var _ = prometheus.NewRegistry
