package wkmetrics

import (
	"bufio"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Namespace is the Prometheus-name prefix applied to every collector
// in this package. Renaming the namespace is a Grafana-dashboard-
// breaking change; M10.1 pins it and every future Phase-1 change MUST
// preserve it.
const Namespace = "watchkeeper"

// Outcome is the bounded enum used by every `outcome` label in this
// package. Keeping it a named string type (rather than a bare string
// constant) makes the bound enforceable through the compiler — any
// caller passing a free-form `"ok"` or `"OK"` would fail to type-check.
// Iter-1 review flagged that a doc-comment "tests grep the source"
// claim is brittle compared to a real type; this is the type-system
// answer.
type Outcome string

// The two values every `outcome` label takes. Iter-1 review (codex +
// critic) treated these as the canonical contract: cardinality bound
// to 2, name stability part of the Grafana-dashboard surface.
const (
	OutcomeSuccess Outcome = "success"
	OutcomeError   Outcome = "error"
)

// LabelOutcomeSuccess / LabelOutcomeError preserve the M10.1-original
// `string`-typed constants for callers that compose label values
// dynamically. New code SHOULD prefer the typed [Outcome] constants;
// these aliases remain so the existing API surface is not broken.
const (
	LabelOutcomeSuccess = string(OutcomeSuccess)
	LabelOutcomeError   = string(OutcomeError)
)

// Default bucket sets. Latency buckets cover 1ms..10s with a deliberate
// long tail — Slack/Jira round-trips routinely sit at the 1–3s mark and
// we want histograms wide enough to distinguish "slow" from "stuck"
// without rebucketing later. They are exposed so tests can compare the
// registered descriptors byte-for-byte against what the contract pins.
var (
	// LatencyBucketsSeconds drives every *_duration_seconds histogram.
	//
	//nolint:gochecknoglobals // bucket-set is a stable contract pin, not
	// mutable state.
	LatencyBucketsSeconds = []float64{
		0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
	}
)

// Options configures a [*Metrics] at construction time. Pass via
// [NewWithOptions]; the zero value is valid (matches [New] /
// [NewWithRegistry] defaults).
type Options struct {
	// Registry, when non-nil, is the [*prometheus.Registry] the metrics
	// register their collectors on. A nil Registry causes a fresh
	// private one to be built.
	Registry *prometheus.Registry

	// DisableProcessCollectors, when true, skips registration of the
	// [collectors.NewProcessCollector] + [collectors.NewGoCollector]
	// collectors. Iter-1 review flagged a deployment risk: when
	// `/metrics` is exposed without a network ACL, the process/Go
	// collectors leak `process_resident_memory_bytes`,
	// `process_open_fds`, `process_start_time_seconds`, `go_goroutines`,
	// etc. Operators deploying behind a hostile or unknown network
	// boundary should set this true and instead scrape the metrics
	// surface on a dedicated, ACL-gated listener (`watchkeeper.Metrics`
	// + a separate http.Server in main.go). The default is `false`
	// (collectors registered) to preserve the documented out-of-the-box
	// dashboard story.
	DisableProcessCollectors bool
}

// Metrics is the process-wide bag of named collectors. Build via [New]
// / [NewWithRegistry] / [NewWithOptions]; the zero value is NOT
// usable. All record-* methods are nil-safe.
type Metrics struct {
	registry *prometheus.Registry

	// llmTokens tracks per-Watchkeeper input/output token spend per
	// provider. Cardinality concern: agent_id is unbounded, but the
	// Phase-1 deployment caps live Watchkeepers in the low single
	// digits per organisation so cardinality is bounded by deployment
	// scale; see docs/ROADMAP-phase1.md §M10.1 for the trade-off.
	llmTokens *prometheus.CounterVec

	// llmRequestDuration records LLM provider round-trip latency per
	// provider with success/error outcomes.
	llmRequestDuration *prometheus.HistogramVec

	// toolInvocations counts tool runtime invocations partitioned by
	// tool name and outcome.
	toolInvocations *prometheus.CounterVec

	// eventBusQueueDepth tracks per-topic queue length in the in-
	// process event bus. Gauge because depth is sampled, not summed.
	eventBusQueueDepth *prometheus.GaugeVec

	// rateLimitRemaining exposes the latest Slack/Jira rate-limit
	// headroom seen by the messenger adapters.
	rateLimitRemaining *prometheus.GaugeVec

	// httpRequestDuration records per-route HTTP latency on the Keep
	// service. status_class is "2xx"/"3xx"/"4xx"/"5xx" — full status
	// codes would balloon cardinality without operational value.
	httpRequestDuration *prometheus.HistogramVec

	// outboxPublished counts Keep outbox rows transitioning from
	// unpublished to published, partitioned by success/error.
	outboxPublished *prometheus.CounterVec
}

// New builds a [*Metrics] backed by a fresh private *prometheus.Registry.
// The default registry is NOT used; callers wanting the default-registry
// process collectors should call [NewWithRegistry] with prometheus.DefaultRegisterer.
//
// The constructor also registers process + Go-runtime collectors so
// operators get goroutine count, gc pause, fd count etc. for free.
// Operators deploying without a network ACL on `/metrics` should
// instead use [NewWithOptions] and set
// [Options.DisableProcessCollectors] = true.
func New() *Metrics {
	return NewWithOptions(Options{})
}

// NewWithRegistry builds a [*Metrics] that registers its collectors on
// reg. The same registry is exposed by [Handler]; callers MUST NOT
// register collectors with names colliding with Namespace_* elsewhere.
//
// Process + Go-runtime collectors are added to reg as well so /metrics
// includes them out of the box.
func NewWithRegistry(reg *prometheus.Registry) *Metrics {
	return NewWithOptions(Options{Registry: reg})
}

// NewWithOptions is the full-control constructor; New and
// NewWithRegistry are thin wrappers. Iter-1 review introduced the
// Options.DisableProcessCollectors knob in response to the `/metrics`-
// outside-auth deployment concern.
func NewWithOptions(opts Options) *Metrics {
	reg := opts.Registry
	if reg == nil {
		reg = prometheus.NewRegistry()
	}
	if !opts.DisableProcessCollectors {
		reg.MustRegister(
			collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
			collectors.NewGoCollector(),
		)
	}

	m := &Metrics{registry: reg}

	m.llmTokens = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: Namespace,
			Subsystem: "llm",
			Name:      "tokens_total",
			Help:      "Cumulative LLM tokens consumed by Watchkeepers. Kind is one of input|output.",
		},
		[]string{"agent_id", "kind", "provider"},
	)
	m.llmRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: Namespace,
			Subsystem: "llm",
			Name:      "request_duration_seconds",
			Help:      "Latency of LLM provider round-trips, partitioned by provider and outcome.",
			Buckets:   LatencyBucketsSeconds,
		},
		[]string{"provider", "outcome"},
	)
	m.toolInvocations = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: Namespace,
			Subsystem: "tool",
			Name:      "invocations_total",
			Help:      "Cumulative tool-runtime invocations partitioned by tool and outcome.",
		},
		[]string{"tool", "outcome"},
	)
	m.eventBusQueueDepth = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: Namespace,
			Subsystem: "eventbus",
			Name:      "queue_depth",
			Help:      "Current per-topic queue length in the in-process event bus.",
		},
		[]string{"topic"},
	)
	m.rateLimitRemaining = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: Namespace,
			Subsystem: "messenger",
			Name:      "rate_limit_remaining",
			Help:      "Latest rate-limit headroom reported by the messenger adapter for provider/endpoint.",
		},
		[]string{"provider", "endpoint"},
	)
	m.httpRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: Namespace,
			Subsystem: "http",
			Name:      "request_duration_seconds",
			Help:      "Latency of Keep HTTP requests partitioned by method, route, and status class (2xx/3xx/4xx/5xx).",
			Buckets:   LatencyBucketsSeconds,
		},
		[]string{"method", "route", "status_class"},
	)
	m.outboxPublished = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: Namespace,
			Subsystem: "outbox",
			Name:      "published_total",
			Help:      "Cumulative Keep outbox publish attempts partitioned by outcome (success/error).",
		},
		[]string{"outcome"},
	)

	reg.MustRegister(
		m.llmTokens,
		m.llmRequestDuration,
		m.toolInvocations,
		m.eventBusQueueDepth,
		m.rateLimitRemaining,
		m.httpRequestDuration,
		m.outboxPublished,
	)
	return m
}

// Registry returns the underlying *prometheus.Registry. Callers SHOULD
// NOT register collectors directly on it — define them via Metrics so
// the contract stays explicit. Exposed for tests that need to gather
// values for assertion (`registry.Gather()`).
func (m *Metrics) Registry() *prometheus.Registry {
	if m == nil {
		return nil
	}
	return m.registry
}

// Handler returns the http.Handler that serves the Prometheus exposition
// format from the underlying registry. A nil receiver returns a 503
// handler so a misconfigured boot does not silently expose the default
// registry through a stray promhttp.Handler.
func (m *Metrics) Handler() http.Handler {
	if m == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("wkmetrics: not initialised\n"))
		})
	}
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{
		Registry:      m.registry,
		ErrorHandling: promhttp.ContinueOnError,
	})
}

// RecordTokenSpend adds tokens to the per-agent counter for the given
// kind ("input" or "output") and provider. A nil receiver is a no-op.
func (m *Metrics) RecordTokenSpend(agentID, kind, provider string, tokens int) {
	if m == nil || tokens <= 0 {
		return
	}
	m.llmTokens.WithLabelValues(agentID, kind, provider).Add(float64(tokens))
}

// ObserveLLMRequest records one provider round-trip's duration. A nil
// receiver is a no-op.
func (m *Metrics) ObserveLLMRequest(provider string, outcome Outcome, d time.Duration) {
	if m == nil {
		return
	}
	m.llmRequestDuration.WithLabelValues(provider, string(outcome)).Observe(d.Seconds())
}

// RecordToolInvocation increments the per-tool/outcome counter. A nil
// receiver is a no-op.
func (m *Metrics) RecordToolInvocation(tool string, outcome Outcome) {
	if m == nil {
		return
	}
	m.toolInvocations.WithLabelValues(tool, string(outcome)).Inc()
}

// SetEventBusDepth replaces the gauge value for topic with depth. A nil
// receiver is a no-op.
func (m *Metrics) SetEventBusDepth(topic string, depth int) {
	if m == nil {
		return
	}
	m.eventBusQueueDepth.WithLabelValues(topic).Set(float64(depth))
}

// SetRateLimitRemaining replaces the gauge value for (provider,endpoint).
// A nil receiver is a no-op.
func (m *Metrics) SetRateLimitRemaining(provider, endpoint string, remaining int) {
	if m == nil {
		return
	}
	m.rateLimitRemaining.WithLabelValues(provider, endpoint).Set(float64(remaining))
}

// ObserveHTTPRequest records one Keep HTTP request duration. status is
// the integer HTTP status (the helper folds it to a 2xx/3xx/4xx/5xx
// class so cardinality stays bounded). A nil receiver is a no-op.
func (m *Metrics) ObserveHTTPRequest(method, route string, status int, d time.Duration) {
	if m == nil {
		return
	}
	m.httpRequestDuration.WithLabelValues(method, route, StatusClass(status)).Observe(d.Seconds())
}

// RecordOutboxPublished increments the outbox-published counter for the
// resolved outcome. A nil receiver is a no-op.
func (m *Metrics) RecordOutboxPublished(outcome Outcome) {
	if m == nil {
		return
	}
	m.outboxPublished.WithLabelValues(string(outcome)).Inc()
}

// StatusClass folds an HTTP status code into a 2xx/3xx/4xx/5xx class
// label. Codes outside the 100..599 range are labelled "other" so a
// malformed status does not leak as a high-cardinality value.
func StatusClass(status int) string {
	switch {
	case status >= 200 && status < 300:
		return "2xx"
	case status >= 300 && status < 400:
		return "3xx"
	case status >= 400 && status < 500:
		return "4xx"
	case status >= 500 && status < 600:
		return "5xx"
	default:
		return "other"
	}
}

// OutcomeFor maps a Go error into the success/error label value used by
// every outcome-labelled metric in this package. Centralising the
// mapping prevents per-call drift across the codebase. Renamed from
// the iter-0 `Outcome(err)` helper because that name now collides with
// the typed [Outcome] enum.
func OutcomeFor(err error) Outcome {
	if err == nil {
		return OutcomeSuccess
	}
	return OutcomeError
}

// statusRecorder wraps an http.ResponseWriter so the surrounding
// instrumentation middleware can read back the response status the
// handler wrote.
//
// Iter-1 review fix: the wrapper MUST proxy [http.Flusher] and
// [http.Hijacker] when the underlying writer supports them. Without
// this, the SSE handler (handlers_subscribe.go) does
// `flusher, _ := w.(http.Flusher)` and gets a nil interface, which
// short-circuits the streaming loop. The proxy is interface-narrow:
// each capability is forwarded only if the underlying writer actually
// implements it, so the wrapper does not advertise a flush/hijack
// surface it cannot service.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if !r.wrote {
		r.status = status
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(status)
}

// Status returns the captured status, defaulting to 200 when the
// handler never called WriteHeader. Iter-1 review: the previous
// implementation used `status == 0` as the "not-written" sentinel,
// which gave a wrong 200 result if a handler explicitly called
// WriteHeader(0). Now driven by the dedicated `wrote` flag.
func (r *statusRecorder) Status() int {
	if !r.wrote {
		return http.StatusOK
	}
	return r.status
}

// Flush forwards to the underlying [http.Flusher] if implemented;
// otherwise it is a no-op. This is the M10.1 iter-1 fix for the SSE
// streaming regression that landed in critic finding C1.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack forwards to the underlying [http.Hijacker] if implemented;
// otherwise it returns http.ErrNotSupported so callers that test for
// hijacking know to fall back. Reserved for connection-upgrade
// callers (none today; pinned so the wrapper does not regress as
// those land).
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := r.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

// Instrument wraps next in a middleware that records duration into
// httpRequestDuration. route is the static route template the handler
// is mounted on (e.g. "/v1/search"); using the template instead of the
// raw URL path keeps cardinality bounded regardless of path
// parameters.
//
// A nil receiver returns next unchanged so middleware-mounting code
// can call `m.Instrument(route, h)` unconditionally during boot.
//
// Iter-1 review (codex P1a): mount Instrument OUTSIDE the auth
// middleware so 401 responses are still counted in the HTTP latency
// histogram. The call-site discipline lives in
// `core/internal/keep/server/server.go:NewRouterWithMetrics`.
func (m *Metrics) Instrument(route string, next http.Handler) http.Handler {
	if m == nil || next == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		m.ObserveHTTPRequest(r.Method, route, rec.Status(), time.Since(start))
	})
}
