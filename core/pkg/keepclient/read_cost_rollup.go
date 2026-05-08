package keepclient

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// CostRollupGrain is the closed set of bucket grains [Client.CostRollups]
// accepts. Anything else short-circuits with [ErrInvalidRequest] before a
// network round-trip.
type CostRollupGrain string

const (
	// CostRollupGrainDaily aggregates events per calendar day. The
	// returned bucket date is the calendar date the events occurred on.
	CostRollupGrainDaily CostRollupGrain = "daily"
	// CostRollupGrainWeekly aggregates events per ISO week. The
	// returned bucket date is the Monday of the ISO week.
	CostRollupGrainWeekly CostRollupGrain = "weekly"
)

// CostRollupsRequest configures a [Client.CostRollups] call. The zero
// value is invalid: AgentID, From, To, and Grain are all required.
// Negative time spans (To.Before(From)) and unknown grains short-circuit
// with [ErrInvalidRequest] before any network round-trip.
type CostRollupsRequest struct {
	// AgentID narrows the rollup to one watchkeeper. Required UUID.
	AgentID string
	// From is the inclusive lower bound of the time window. Required.
	// Sent as RFC3339 on the wire.
	From time.Time
	// To is the exclusive upper bound. Required. Must be ≥ From or the
	// call returns [ErrInvalidRequest] without contacting the server.
	To time.Time
	// Grain selects the bucket size. Required; must be daily or weekly.
	Grain CostRollupGrain
}

// CostRollupBucket mirrors the server's per-bucket JSON shape. Field
// names are a closed set (see M6.3.f PII discipline regression in the
// server package); a future server-side rename surfaces as a JSON
// decode mismatch here.
type CostRollupBucket struct {
	// Bucket is the ISO date of the bucket boundary (calendar day for
	// daily; Monday of the ISO week for weekly).
	Bucket string `json:"bucket"`
	// AgentID is the watchkeeper UUID this bucket aggregates for.
	AgentID string `json:"agent_id"`
	// Model is the LLM model identifier from the cost-event payload.
	Model string `json:"model"`
	// InputTokens is the SUM of `data.input_tokens` over the bucket.
	InputTokens int64 `json:"input_tokens"`
	// OutputTokens is the SUM of `data.output_tokens` over the bucket.
	OutputTokens int64 `json:"output_tokens"`
	// NCalls is the COUNT(*) of cost events in the bucket.
	NCalls int64 `json:"n_calls"`
}

// CostRollupsResponse mirrors the server's `{"buckets":[...]}` envelope.
// A no-event window decodes as a non-nil empty slice (matches the
// server's allocated-empty shape).
type CostRollupsResponse struct {
	// Buckets is the rollup result, sorted by `(bucket asc, model asc)`.
	Buckets []CostRollupBucket `json:"buckets"`
}

// validCostRollupGrains is the closed set the client validates against
// before any network round-trip. Mirrors the server's accepted set; a
// caller typo never burns a request.
//
//nolint:gochecknoglobals // intentional module-scoped lookup table.
var validCostRollupGrains = map[CostRollupGrain]struct{}{
	CostRollupGrainDaily:  {},
	CostRollupGrainWeekly: {},
}

// CostRollups calls GET /v1/cost-rollups. The four required parameters
// are validated client-side: empty AgentID, non-positive time bounds,
// `To.Before(From)`, and unknown grain all return [ErrInvalidRequest]
// without a network round-trip.
//
// Pagination is NOT supported by the Phase-1 server; the full bucket
// list for the requested window is returned in one response. Callers
// requesting wide windows should narrow the range or aggregate
// client-side.
func (c *Client) CostRollups(ctx context.Context, req CostRollupsRequest) (*CostRollupsResponse, error) {
	if req.AgentID == "" {
		return nil, ErrInvalidRequest
	}
	if req.From.IsZero() || req.To.IsZero() {
		return nil, ErrInvalidRequest
	}
	if req.To.Before(req.From) {
		return nil, ErrInvalidRequest
	}
	if _, ok := validCostRollupGrains[req.Grain]; !ok {
		return nil, ErrInvalidRequest
	}

	q := url.Values{}
	q.Set("agent_id", req.AgentID)
	q.Set("from", req.From.UTC().Format(time.RFC3339))
	q.Set("to", req.To.UTC().Format(time.RFC3339))
	q.Set("grain", string(req.Grain))
	path := "/v1/cost-rollups?" + q.Encode()

	var out CostRollupsResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
