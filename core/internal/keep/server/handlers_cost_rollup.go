// handlers_cost_rollup.go implements GET /v1/cost-rollups — daily/weekly
// per-agent token rollups aggregated on-demand from the keepers_log
// stream M6.3.e's `LoggingProvider` populates with
// `llm_turn_cost_completed` rows.
//
// Implementation choice (M6.3.f): Option B — on-demand SQL aggregation.
// No new tables, no migration, no scheduled job. The endpoint composes a
// single SELECT against `keepers_log`, filters by event_type prefix and
// agent_id, then groups by `date_trunc(<grain>, created_at)` + model.
// Phase-1 scale fits comfortably; a future M-stage may introduce
// materialised rollup tables without changing the wire shape.
//
// The response shape is intentionally a closed set of keys
// (`bucket | agent_id | model | input_tokens | output_tokens | n_calls`).
// PII discipline (M2b.7 / M6.3.b / M6.3.e): no agent name, no message
// body, no model credentials. A future JOIN that adds an
// `agent_name` field would have to be reflected in the response struct,
// at which point a code review picks it up.

package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/vadimtrunov/watchkeepers/core/internal/keep/auth"
)

// costRollupEventTypePrefix is the keepers_log event_type prefix the
// rollup query aggregates over. The prefix MUST stay byte-equal to
// `defaultReportCostEventTypePrefix` in
// `core/pkg/spawn/watchmaster_tools.go` and to the wire vocabulary
// emitted by `core/pkg/llm/cost.EventTypeLLMCallCompleted`. A regression
// test (`TestCostRollupsEventTypeFilter_HasReportCostPrefix`) pins the
// invariant `strings.HasPrefix(EventTypeLLMCallCompleted, prefix)` so a
// future drift trips a build failure.
const costRollupEventTypePrefix = "llm_turn_cost"

// CostRollupEventTypePrefix exposes the rollup event_type prefix to the
// _test files (and to a future cross-package vocabulary regression
// test). Production callers MUST NOT branch on this — the keepclient
// surface stays the canonical entry point.
const CostRollupEventTypePrefix = costRollupEventTypePrefix

// costRollupGrain is the closed set of bucket grains the handler
// accepts. Postgres `date_trunc` understands many more values, but
// allowing them here would silently change response shapes.
const (
	grainDaily  = "daily"
	grainWeekly = "weekly"
)

// costRollupBucket is one row of the `buckets` array. Field names are a
// fixed closed set per the AC3 wire shape; adding a field is a code
// review-visible change.
type costRollupBucket struct {
	// Bucket is the ISO date of the bucket boundary. For grain=daily
	// it is the calendar date of the events; for grain=weekly it is
	// the Monday of the ISO week (Postgres `date_trunc('week', ...)`
	// always returns Monday).
	Bucket string `json:"bucket"`
	// AgentID is the watchkeeper UUID the bucket aggregates for.
	AgentID string `json:"agent_id"`
	// Model is the LLM model identifier from the cost event payload.
	Model string `json:"model"`
	// InputTokens is the SUM of `data.input_tokens` over the bucket.
	InputTokens int64 `json:"input_tokens"`
	// OutputTokens is the SUM of `data.output_tokens` over the bucket.
	OutputTokens int64 `json:"output_tokens"`
	// NCalls is the COUNT(*) of cost events in the bucket.
	NCalls int64 `json:"n_calls"`
}

// costRollupResponse is the JSON envelope returned by the handler. An
// empty window yields `{"buckets": []}` (200, not 404) — clients should
// treat absence-of-data as a normal response, not an error.
type costRollupResponse struct {
	Buckets []costRollupBucket `json:"buckets"`
}

// handleCostRollups serves GET /v1/cost-rollups. Required query
// parameters: `agent_id` (UUID), `from` / `to` (RFC3339, with `to >=
// from`), `grain` (`daily` | `weekly`). Any missing or malformed
// parameter surfaces as 400 with a stable error code; the handler
// emits zero PII / SQL details in error bodies.
//
// Pagination is NOT supported; a future M-stage may add a cursor.
// Callers requesting a window so wide that it would return more than a
// few thousand buckets should narrow the range or aggregate
// client-side.
func handleCostRollups(r scopedRunner) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		claim, ok := ClaimFromContext(req.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		params, ok := parseCostRollupParams(w, req)
		if !ok {
			return
		}

		out, err := runCostRollupQuery(req.Context(), r, claim, params)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "cost_rollups_failed")
			return
		}

		writeJSON(w, http.StatusOK, costRollupResponse{Buckets: out})
	})
}

// costRollupParams is the validated, in-memory representation of the
// request's query parameters. Pulled into a struct so the SQL builder
// stays free of `req.URL.Query()` lookups.
type costRollupParams struct {
	agentID string
	from    time.Time
	to      time.Time
	grain   string
}

// parseCostRollupParams extracts and validates the four required query
// parameters. On failure it writes the canonical 400 envelope and
// returns ok=false.
func parseCostRollupParams(w http.ResponseWriter, req *http.Request) (costRollupParams, bool) {
	var out costRollupParams

	out.agentID = req.URL.Query().Get("agent_id")
	if out.agentID == "" {
		writeError(w, http.StatusBadRequest, "missing_agent_id")
		return out, false
	}
	if !uuidPattern.MatchString(out.agentID) {
		writeError(w, http.StatusBadRequest, "invalid_agent_id")
		return out, false
	}

	rawFrom := req.URL.Query().Get("from")
	if rawFrom == "" {
		writeError(w, http.StatusBadRequest, "missing_from")
		return out, false
	}
	from, err := time.Parse(time.RFC3339, rawFrom)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_from")
		return out, false
	}
	out.from = from

	rawTo := req.URL.Query().Get("to")
	if rawTo == "" {
		writeError(w, http.StatusBadRequest, "missing_to")
		return out, false
	}
	to, err := time.Parse(time.RFC3339, rawTo)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_to")
		return out, false
	}
	out.to = to

	if out.to.Before(out.from) {
		writeError(w, http.StatusBadRequest, "invalid_range")
		return out, false
	}

	out.grain = req.URL.Query().Get("grain")
	switch out.grain {
	case grainDaily, grainWeekly:
		// allowed
	case "":
		writeError(w, http.StatusBadRequest, "missing_grain")
		return out, false
	default:
		writeError(w, http.StatusBadRequest, "invalid_grain")
		return out, false
	}

	return out, true
}

// runCostRollupQuery executes the aggregation under the scoped
// transaction. RLS scoping is provided by `r.WithScope`; defense-in-
// depth filters by `event_type LIKE 'llm_turn_cost%'` AND by
// `payload->'data'->>'agent_id'` so a stray non-cost row that survived
// RLS cannot leak into the response.
//
// The SQL grain literal is selected from a closed Go set BEFORE the
// statement is built, so the small bit of string interpolation around
// `date_trunc(<grain>, ...)` cannot be driven by attacker-controlled
// input.
func runCostRollupQuery(
	ctx context.Context,
	r scopedRunner,
	claim auth.Claim,
	p costRollupParams,
) ([]costRollupBucket, error) {
	pgGrain, err := postgresGrainLiteral(p.grain)
	if err != nil {
		// Should never happen — parseCostRollupParams already validated.
		return nil, err
	}

	// Defensive: pre-allocate a non-nil slice so an empty query result
	// JSON-encodes as `[]` not `null`.
	out := make([]costRollupBucket, 0, 8)

	err = r.WithScope(ctx, claim, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
            SELECT
                to_char(date_trunc('`+pgGrain+`', created_at), 'YYYY-MM-DD') AS bucket,
                payload->'data'->>'agent_id'                                  AS agent_id,
                coalesce(payload->'data'->>'model', '')                       AS model,
                coalesce(sum((payload->'data'->>'input_tokens')::bigint), 0)  AS input_tokens,
                coalesce(sum((payload->'data'->>'output_tokens')::bigint), 0) AS output_tokens,
                count(*)                                                      AS n_calls
            FROM watchkeeper.keepers_log
            WHERE event_type LIKE $1
              AND payload->'data'->>'agent_id' = $2
              AND created_at >= $3
              AND created_at <  $4
            GROUP BY bucket, agent_id, model
            ORDER BY bucket ASC, model ASC
        `, costRollupEventTypePrefix+"%", p.agentID, p.from, p.to)
		if err != nil {
			return fmt.Errorf("cost_rollups query: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var rec costRollupBucket
			if err := rows.Scan(
				&rec.Bucket, &rec.AgentID, &rec.Model,
				&rec.InputTokens, &rec.OutputTokens, &rec.NCalls,
			); err != nil {
				return fmt.Errorf("cost_rollups scan: %w", err)
			}
			out = append(out, rec)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// postgresGrainLiteral maps the validated request grain to the literal
// Postgres `date_trunc` accepts. Validated against a closed set BEFORE
// the SQL is built so caller-supplied input never reaches Postgres
// concatenated.
func postgresGrainLiteral(grain string) (string, error) {
	switch grain {
	case grainDaily:
		return "day", nil
	case grainWeekly:
		return "week", nil
	default:
		return "", errors.New("server: cost rollup: unknown grain " + grain)
	}
}

// CostRollupAllowedBucketKeys exposes the closed-set bucket-key set to
// the _test PII regression so a future field rename surfaces in a
// build failure rather than silently broadening the wire surface.
var CostRollupAllowedBucketKeys = []string{
	"bucket", "agent_id", "model",
	"input_tokens", "output_tokens", "n_calls",
}

// EventTypePrefixHasCostFamily is the cross-PR vocabulary regression
// helper: the rollup endpoint MUST aggregate over events emitted by
// `core/pkg/llm/cost`. A future re-key of either the rollup filter or
// the cost-event constant trips
// `TestCostRollupsEventTypeFilter_HasReportCostPrefix` rather than
// silently producing empty rollups.
func EventTypePrefixHasCostFamily(eventType string) bool {
	return strings.HasPrefix(eventType, costRollupEventTypePrefix)
}
