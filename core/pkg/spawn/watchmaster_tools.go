// watchmaster_tools.go implements the three M6.2.a read-only Watchmaster
// tools that consume existing keepclient read paths without invoking
// any approval gate:
//
//  1. ListWatchkeepers — projects keepclient.ListWatchkeepers rows into
//     the wire-shape result the Watchmaster surfaces to its harness.
//  2. ReportCost      — aggregates keepers_log cost events for a given
//     `agent_id` (or org-wide) over a configurable window of
//     keepers_log entries (limit-based, NOT time-based — M6.3 owns
//     daily/weekly time rollups).
//  3. ReportHealth    — returns the lifecycle-state aggregation
//     derivable from the watchkeeper rows: counts by
//     status in {pending, active, retired}. There is no dedicated
//     runtime-health column today; M6.3 (operator surface) owns the
//     heartbeat / last_seen_at delta.
//
// Authorisation discipline: every tool validates `claim.OrganizationID`
// non-empty per the M3.5.a tenant-scoping rule. NONE of the three tools
// validate an approval token — they are READ-ONLY. NONE emit a
// keepers_log audit row — read paths in Phase 1 do not write to the
// audit chain (M6.1.b's audit-chain contract gates only privileged
// actions; M6.3 may add read-side telemetry on a separate channel).
package spawn

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
)

// defaultReportCostEventTypePrefix is the event_type prefix the
// ReportCost tool aggregates by default. Phase 1 has not yet wired
// the runtime-side cost emission (M6.3 owns it); the prefix is
// declared here so the tool stays useful as soon as the runtime
// starts emitting `llm_turn_cost_*` rows.
const defaultReportCostEventTypePrefix = "llm_turn_cost"

// defaultReportCostLogLimit caps the number of keepers_log rows
// ReportCost scans when the caller leaves the window unset. The cap
// matches the keepclient `LogTail` server-side default (50) so the
// tool's default behaviour mirrors the existing read path.
const defaultReportCostLogLimit = 50

// maxReportCostLogLimit mirrors the keepers_log server-side hard cap.
// Clamping client-side spares the round trip on obvious over-asks and
// keeps the documented contract symmetrical with the underlying read.
const maxReportCostLogLimit = 500

// payloadKeyPromptTokens / payloadKeyCompletionTokens are the
// `data.<key>` payload keys the M6.3 cost-event emission will publish.
// Pinned here so a TS-side renamer cannot drift the keys silently — a
// rename is a one-line change that the unit tests pick up via the
// compiler.
const (
	payloadKeyPromptTokens     = "prompt_tokens"
	payloadKeyCompletionTokens = "completion_tokens"
)

// WatchmasterReadClient is the minimal subset of the keepclient surface
// the M6.2.a read-only tools consume. Defined as an interface in this
// package so tests can substitute a hand-rolled fake without standing
// up an HTTP server, and so production code never imports the concrete
// `*keepclient.Client` type at all (mirrors the
// keeperslog.LocalKeepClient pattern documented in `docs/LESSONS.md`).
//
// `*keepclient.Client` satisfies this interface as-is; the compile-time
// assertion lives below.
type WatchmasterReadClient interface {
	ListWatchkeepers(
		ctx context.Context,
		req keepclient.ListWatchkeepersRequest,
	) (*keepclient.ListWatchkeepersResponse, error)
	LogTail(
		ctx context.Context,
		opts keepclient.LogTailOptions,
	) (*keepclient.LogTailResponse, error)
}

// Compile-time assertion: every [*keepclient.Client] satisfies
// [WatchmasterReadClient] by definition. Pins the integration shape
// against future drift in the keepclient package.
var _ WatchmasterReadClient = (*keepclient.Client)(nil)

// ListWatchkeepersRequest is the typed request for [ListWatchkeepers].
// Status filter is optional ("pending" | "active" | "retired" or empty
// for "no filter"); Limit caps row count (0 → server default, max 200
// per the keepclient hard cap).
type ListWatchkeepersRequest struct {
	// Status optionally filters by lifecycle state. Empty means
	// "no filter" — the server returns every visible row.
	Status string

	// Limit caps the number of rows returned. 0 means the server's
	// default; values > the server cap are rejected by the underlying
	// keepclient with [keepclient.ErrInvalidRequest].
	Limit int
}

// WatchkeeperRow is the shape ListWatchkeepers projects from
// [keepclient.Watchkeeper] for the wire surface. Names use snake_case
// to match the M6.1.b builtin-tool wire convention; the TS-side zod
// schema mirrors this shape verbatim.
type WatchkeeperRow struct {
	// ID is the watchkeeper row UUID.
	ID string `json:"id"`
	// ManifestID is the parent manifest UUID.
	ManifestID string `json:"manifest_id"`
	// LeadHumanID is the lead-operator human UUID.
	LeadHumanID string `json:"lead_human_id"`
	// ActiveManifestVersionID is the optional pinned version UUID;
	// empty when the column was NULL on the server.
	ActiveManifestVersionID string `json:"active_manifest_version_id,omitempty"`
	// Status is one of "pending" | "active" | "retired".
	Status string `json:"status"`
	// SpawnedAt is the RFC3339 timestamp of the pending→active
	// transition, or empty when still pending.
	SpawnedAt string `json:"spawned_at,omitempty"`
	// RetiredAt is the RFC3339 timestamp of the active→retired
	// transition, or empty before retire.
	RetiredAt string `json:"retired_at,omitempty"`
	// CreatedAt is the row's created_at timestamp (RFC3339).
	CreatedAt string `json:"created_at"`
}

// ListWatchkeepersResult is the response shape returned on success.
type ListWatchkeepersResult struct {
	// Items is the list of rows in `created_at DESC` order (the
	// ordering keepclient returns).
	Items []WatchkeeperRow `json:"items"`
}

// ListWatchkeepers calls [WatchmasterReadClient.ListWatchkeepers] and
// projects the result into the wire-shape [ListWatchkeepersResult].
//
// Resolution order:
//
//  1. Validate ctx (cancellation takes precedence over input shape).
//  2. Validate claim.OrganizationID non-empty (M3.5.a discipline) →
//     [ErrInvalidClaim].
//  3. Forward the request to the read client; surface
//     [keepclient.ErrInvalidRequest] (out-of-range limit, unknown
//     status) wrapped with `spawn:`.
//  4. Project the rows into [WatchkeeperRow] (pointer-time → empty
//     string preserves the SQL NULL → omitempty contract).
func ListWatchkeepers(
	ctx context.Context,
	client WatchmasterReadClient,
	req ListWatchkeepersRequest,
	claim Claim,
) (ListWatchkeepersResult, error) {
	if err := ctx.Err(); err != nil {
		return ListWatchkeepersResult{}, err
	}
	if claim.OrganizationID == "" {
		return ListWatchkeepersResult{}, fmt.Errorf("%w: empty OrganizationID", ErrInvalidClaim)
	}
	if client == nil {
		return ListWatchkeepersResult{}, fmt.Errorf("%w: nil read client", ErrInvalidRequest)
	}

	resp, err := client.ListWatchkeepers(ctx, keepclient.ListWatchkeepersRequest{
		Status: req.Status,
		Limit:  req.Limit,
	})
	if err != nil {
		return ListWatchkeepersResult{}, fmt.Errorf("spawn: list_watchkeepers: %w", err)
	}

	out := make([]WatchkeeperRow, 0, len(resp.Items))
	for i := range resp.Items {
		out = append(out, projectWatchkeeperRow(resp.Items[i]))
	}
	return ListWatchkeepersResult{Items: out}, nil
}

// projectWatchkeeperRow turns a keepclient.Watchkeeper into the wire
// shape. Pulled into a helper so the time / nullable-string handling
// stays scannable and reusable from future M6.2.b-d wiring.
func projectWatchkeeperRow(w keepclient.Watchkeeper) WatchkeeperRow {
	row := WatchkeeperRow{
		ID:          w.ID,
		ManifestID:  w.ManifestID,
		LeadHumanID: w.LeadHumanID,
		Status:      w.Status,
		CreatedAt:   w.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
	if w.ActiveManifestVersionID != nil {
		row.ActiveManifestVersionID = *w.ActiveManifestVersionID
	}
	if w.SpawnedAt != nil {
		row.SpawnedAt = w.SpawnedAt.UTC().Format(time.RFC3339Nano)
	}
	if w.RetiredAt != nil {
		row.RetiredAt = w.RetiredAt.UTC().Format(time.RFC3339Nano)
	}
	return row
}

// ReportCostRequest is the typed request for [ReportCost].
type ReportCostRequest struct {
	// AgentID optionally narrows aggregation to a single watchkeeper.
	// Empty means "org-wide" — the tool sums every visible row's
	// cost-event payload tokens.
	AgentID string

	// EventTypePrefix optionally narrows the aggregation by
	// keepers_log row event_type prefix. Empty defaults to
	// `llm_turn_cost`. The tool matches by case-sensitive prefix so
	// downstream emitters can publish multiple event types under the
	// same family (e.g. `llm_turn_cost_streaming`,
	// `llm_turn_cost_completion`) without forcing a tool re-deploy.
	EventTypePrefix string

	// Limit caps the number of keepers_log rows scanned. 0 →
	// [defaultReportCostLogLimit]; values > [maxReportCostLogLimit]
	// are clamped to the cap. Negative values return
	// [ErrInvalidRequest] synchronously.
	Limit int
}

// ReportCostResult is the response shape returned on success.
type ReportCostResult struct {
	// AgentID echoes back the requested narrowing (empty for
	// org-wide). Lets the caller correlate the response to the
	// request without re-reading its own state.
	AgentID string `json:"agent_id,omitempty"`
	// EventTypePrefix echoes the prefix filter the tool applied.
	EventTypePrefix string `json:"event_type_prefix"`
	// PromptTokens is the sum of `data.prompt_tokens` across every
	// matching cost event. Zero when no events match the filter.
	PromptTokens int64 `json:"prompt_tokens"`
	// CompletionTokens is the sum of `data.completion_tokens` across
	// every matching cost event.
	CompletionTokens int64 `json:"completion_tokens"`
	// EventCount is the number of keepers_log rows that contributed
	// to the totals. Useful for surfacing "no data yet" without
	// confusing it with "the rows had zero tokens".
	EventCount int `json:"event_count"`
	// ScannedRows is the number of keepers_log rows actually scanned
	// (≤ Limit). Lets the caller paginate by re-asking with a higher
	// limit when EventCount is small but ScannedRows == Limit.
	ScannedRows int `json:"scanned_rows"`
}

// ReportCost reads the keepers_log via [WatchmasterReadClient.LogTail],
// filters rows by event_type prefix and (optionally) by
// `data.agent_id`, and sums the `data.prompt_tokens` /
// `data.completion_tokens` payload fields.
//
// Phase 1 caveat: the runtime does NOT yet emit cost events; this tool
// reports zero totals against an empty match set until M6.3 wires the
// emission. The contract is stable so the harness can invoke it today
// — empty-result handling is well-defined.
func ReportCost(
	ctx context.Context,
	client WatchmasterReadClient,
	req ReportCostRequest,
	claim Claim,
) (ReportCostResult, error) {
	if err := ctx.Err(); err != nil {
		return ReportCostResult{}, err
	}
	if claim.OrganizationID == "" {
		return ReportCostResult{}, fmt.Errorf("%w: empty OrganizationID", ErrInvalidClaim)
	}
	if client == nil {
		return ReportCostResult{}, fmt.Errorf("%w: nil read client", ErrInvalidRequest)
	}
	if req.Limit < 0 {
		return ReportCostResult{}, fmt.Errorf("%w: negative Limit", ErrInvalidRequest)
	}

	prefix := req.EventTypePrefix
	if prefix == "" {
		prefix = defaultReportCostEventTypePrefix
	}
	limit := req.Limit
	if limit == 0 {
		limit = defaultReportCostLogLimit
	}
	if limit > maxReportCostLogLimit {
		limit = maxReportCostLogLimit
	}

	resp, err := client.LogTail(ctx, keepclient.LogTailOptions{Limit: limit})
	if err != nil {
		return ReportCostResult{}, fmt.Errorf("spawn: report_cost: %w", err)
	}

	result := ReportCostResult{
		AgentID:         req.AgentID,
		EventTypePrefix: prefix,
		ScannedRows:     len(resp.Events),
	}
	for i := range resp.Events {
		ev := resp.Events[i]
		if !strings.HasPrefix(ev.EventType, prefix) {
			continue
		}
		prompt, completion, payloadAgent, ok := decodeCostPayload(ev.Payload)
		if !ok {
			continue
		}
		if req.AgentID != "" && payloadAgent != req.AgentID {
			continue
		}
		result.PromptTokens += prompt
		result.CompletionTokens += completion
		result.EventCount++
	}
	return result, nil
}

// decodeCostPayload extracts the prompt / completion token counts and
// the `data.agent_id` field from a keepers_log row payload (the JSON
// envelope keeperslog.Writer produces). Returns (_, _, _, false) when
// the row has no `data` envelope; missing token fields decode as 0
// (the row counts but does not move the totals).
func decodeCostPayload(payload json.RawMessage) (int64, int64, string, bool) {
	if len(payload) == 0 {
		return 0, 0, "", false
	}
	var envelope struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return 0, 0, "", false
	}
	if envelope.Data == nil {
		return 0, 0, "", false
	}
	prompt := readInt64(envelope.Data, payloadKeyPromptTokens)
	completion := readInt64(envelope.Data, payloadKeyCompletionTokens)
	agent, _ := envelope.Data[payloadKeyAgentID].(string)
	return prompt, completion, agent, true
}

// readInt64 decodes a JSON number from `m[key]` to int64. JSON numbers
// land as float64 after json.Unmarshal into map[string]any; the cast
// preserves token counts up to 2^53. Missing or non-numeric keys
// return 0 — token counts are never negative so 0 is a safe sentinel
// for "absent".
func readInt64(m map[string]any, key string) int64 {
	v, ok := m[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case float64:
		return int64(n)
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0
		}
		return i
	default:
		return 0
	}
}

// ReportHealthRequest is the typed request for [ReportHealth].
type ReportHealthRequest struct {
	// AgentID optionally narrows the report to a single watchkeeper
	// row. Empty means "org-wide" — the tool returns counts grouped
	// by status. When AgentID is set, the tool returns a single-row
	// snapshot in `Item` and zeroes the count fields.
	AgentID string
}

// WatchkeeperHealth is the shape ReportHealth returns for a single-row
// snapshot when the request narrows by AgentID.
type WatchkeeperHealth struct {
	// ID is the watchkeeper row UUID.
	ID string `json:"id"`
	// Status is one of "pending" | "active" | "retired".
	Status string `json:"status"`
	// SpawnedAt is the RFC3339 timestamp of the pending→active
	// transition, or empty when still pending.
	SpawnedAt string `json:"spawned_at,omitempty"`
	// RetiredAt is the RFC3339 timestamp of the active→retired
	// transition, or empty before retire.
	RetiredAt string `json:"retired_at,omitempty"`
}

// ReportHealthResult is the response shape returned on success. When
// the request narrows by AgentID, Item carries the single-row snapshot
// and the count fields stay zero. When the request is org-wide, the
// counts are populated and Item stays nil.
type ReportHealthResult struct {
	// Item is the single-row snapshot when the request narrowed by
	// AgentID; nil for org-wide queries.
	Item *WatchkeeperHealth `json:"item,omitempty"`
	// CountPending is the number of watchkeeper rows with
	// status="pending". Zero when the request narrowed by AgentID.
	CountPending int `json:"count_pending"`
	// CountActive is the number of watchkeeper rows with
	// status="active".
	CountActive int `json:"count_active"`
	// CountRetired is the number of watchkeeper rows with
	// status="retired".
	CountRetired int `json:"count_retired"`
	// CountTotal is the sum of the three status counts. Hoisted into
	// the response so a caller does not have to add them client-side.
	CountTotal int `json:"count_total"`
}

// ReportHealth returns the lifecycle-state aggregation derivable from
// the watchkeeper rows. With an empty AgentID the tool returns counts
// grouped by status; with an AgentID set the tool returns the single
// matching row's status snapshot.
//
// Phase 1 caveat: there is no dedicated runtime-health column today
// (no `last_seen_at`, no heartbeat, no error counter). M6.3 owns the
// heartbeat delta; this tool's contract widens then.
func ReportHealth(
	ctx context.Context,
	client WatchmasterReadClient,
	req ReportHealthRequest,
	claim Claim,
) (ReportHealthResult, error) {
	if err := ctx.Err(); err != nil {
		return ReportHealthResult{}, err
	}
	if claim.OrganizationID == "" {
		return ReportHealthResult{}, fmt.Errorf("%w: empty OrganizationID", ErrInvalidClaim)
	}
	if client == nil {
		return ReportHealthResult{}, fmt.Errorf("%w: nil read client", ErrInvalidRequest)
	}

	// Pull every visible row up to the keepclient hard cap. The tool
	// is intentionally non-paginating: a healthy Phase 1 deployment
	// has a small number of watchkeeper rows; M6.3 may add cursor
	// pagination on top.
	resp, err := client.ListWatchkeepers(ctx, keepclient.ListWatchkeepersRequest{
		Limit: 200,
	})
	if err != nil {
		return ReportHealthResult{}, fmt.Errorf("spawn: report_health: %w", err)
	}

	if req.AgentID != "" {
		for i := range resp.Items {
			row := resp.Items[i]
			if row.ID != req.AgentID {
				continue
			}
			snap := &WatchkeeperHealth{
				ID:     row.ID,
				Status: row.Status,
			}
			if row.SpawnedAt != nil {
				snap.SpawnedAt = row.SpawnedAt.UTC().Format(time.RFC3339Nano)
			}
			if row.RetiredAt != nil {
				snap.RetiredAt = row.RetiredAt.UTC().Format(time.RFC3339Nano)
			}
			return ReportHealthResult{Item: snap}, nil
		}
		// Narrowing by AgentID and finding no match is NOT an error
		// — the caller may be probing for a watchkeeper that has not
		// been spawned yet. Return an empty result.
		return ReportHealthResult{}, nil
	}

	out := ReportHealthResult{}
	for i := range resp.Items {
		switch resp.Items[i].Status {
		case watchkeeperStatusPending:
			out.CountPending++
		case watchkeeperStatusActive:
			out.CountActive++
		case watchkeeperStatusRetired:
			out.CountRetired++
		}
	}
	out.CountTotal = out.CountPending + out.CountActive + out.CountRetired
	return out, nil
}

// watchkeeperStatus* mirror the closed set of lifecycle states the
// keepclient surface accepts. Hoisted to constants so a future re-key
// (e.g., `archived` joining the set) is a one-line change here.
const (
	watchkeeperStatusPending = "pending"
	watchkeeperStatusActive  = "active"
	watchkeeperStatusRetired = "retired"
)
