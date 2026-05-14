// Package wkmetrics is the Watchkeeper Prometheus-metrics surface
// (M10.1).
//
// One [*Metrics] instance per process owns the named collectors that
// every Watchkeeper subsystem reports through. The instance:
//
//   - registers a private *prometheus.Registry (the default registry is
//     NOT used, so a stray prometheus.MustRegister anywhere in the
//     process cannot collide with our names);
//
//   - exposes that registry through [*Metrics.Handler] as the
//     /metrics http.Handler the Keep service mounts outside the auth
//     wall;
//
//   - declares the full Phase-1 metric set as stable contract:
//
//     watchkeeper_llm_tokens_total                 counter   (agent_id, kind, provider)
//     watchkeeper_llm_request_duration_seconds     histogram (provider, outcome)
//     watchkeeper_tool_invocations_total           counter   (tool, outcome)
//     watchkeeper_eventbus_queue_depth             gauge     (topic)
//     watchkeeper_messenger_rate_limit_remaining   gauge     (provider, endpoint)
//     watchkeeper_http_request_duration_seconds    histogram (method, route, status_class)
//     watchkeeper_outbox_published_total           counter   (outcome)
//
// All record-* methods on [*Metrics] are nil-safe: callers can hold a
// `*wkmetrics.Metrics` field and call `m.RecordToolInvocation(...)`
// unconditionally; a nil receiver short-circuits. This keeps the
// instrumentation diff at call sites trivial — no `if m != nil` noise.
//
// # Stable label contract
//
// Label sets are part of the public Prometheus contract; renaming a
// label is a Grafana-dashboard-breaking change. Any future label
// addition MUST go through a roadmap item and bump the lessons file.
// The Phase-1 set is intentionally conservative.
//
// # Where the call sites live
//
//   - Keep HTTP server: HTTP request-duration histogram (wired in M10.1
//     as the middleware shipped alongside this package).
//   - Outbox worker: outbox_published_total (wired in M10.1).
//   - Eventbus: eventbus_queue_depth via [Metrics.SetEventBusDepth]
//     (wired in M10.1; the Bus accepts an optional depth callback).
//
// Token-spend / tool-invocation / rate-limit-headroom callers wire in
// once the relevant subsystems land their per-call hooks; the metric
// names + labels are pinned now so dashboards can be built once and
// kept stable through Phase 1.
package wkmetrics
