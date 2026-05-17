package server

// handlers_peers.go serves GET /v1/peers — the M1.2 list-peers endpoint
// that powers `keepclient.list_peers()`. The shape is purpose-built for
// the M1.3.\* peer.\* tool family (peer.ask resolves a target by role or
// id; peer.broadcast filters by Roles / Languages / Capabilities), not a
// general watchkeeper read — every row carries the role description,
// language, capabilities, and availability tuple the AC names.
//
// Scope boundaries (per docs/lessons/M1.md):
//   - This handler does NOT apply role / language / capability filters
//     server-side. M1.3.d's `peer.RoleFilter` resolves the full active
//     set and applies the filter in-memory; keeping the server response
//     unfiltered preserves a single source of truth for "all active
//     peers at this instant" across every future M1.3.\* consumer.
//   - This handler does NOT emit audit. M1.4 owns the K2K event
//     taxonomy; the read path is observation-only. A source-grep AC in
//     `handlers_peers_test.go` pins the audit ban.
//   - "Availability" is currently a string enum carrying a single value
//     ("available") because the SQL filter `status = 'active'` is the
//     only availability signal Phase 2 ships. The column is reserved
//     for future M1.5 (token-budget) / M1.6 (escalation) extensions
//     that will introduce additional states (e.g. "throttled",
//     "escalated") without breaking the wire shape.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// Peer-list pagination bounds. The default mirrors the watchkeeper-list
// default (50); the cap mirrors the watchkeeper-list cap (200) so a
// single authenticated caller cannot force a megabyte response. The
// cursor envelope field is reserved for a future seek-pagination
// follow-up; this milestone always returns null.
const (
	defaultPeerListLimit = 50
	maxPeerListLimit     = 200
)

// peerAvailabilityAvailable is the string enum value emitted for every
// active watchkeeper row. Declared as a const so the test suite and the
// handler stay in lockstep and future enum values (e.g. "throttled" /
// "escalated") land as additional consts rather than scattered string
// literals.
const peerAvailabilityAvailable = "available"

// peerRow is the JSON shape of one row in `GET /v1/peers`. The field
// names are stable wire contract; renaming any of them is a breaking
// change for `keepclient.list_peers`. Empty-string Role / Description /
// Language come back as the empty string, NOT omitted — the
// `omitempty`-free encoding keeps the wire shape predictable for the
// M1.3.d in-memory filter that branches on field presence rather than
// value.
type peerRow struct {
	// WatchkeeperID is the watchkeeper.watchkeeper row UUID.
	WatchkeeperID string `json:"watchkeeper_id"`
	// Role is the closed-set role name (manifest.display_name). Used
	// by M1.3.d's `peer.RoleFilter.Roles` filter. Always non-empty for
	// a well-seeded deployment but defensive-decoded as a string in
	// case a future migration loosens the NOT NULL constraint.
	Role string `json:"role"`
	// Description is the prose role description
	// (manifest_version.personality). Empty string when the column is
	// NULL — the projection emits "" rather than `null` so the wire
	// shape stays uniform for non-typed callers.
	Description string `json:"description"`
	// Language is the BCP-47-lite language code
	// (manifest_version.language). Empty string when NULL, same
	// reasoning as Description.
	Language string `json:"language"`
	// Capabilities is the list of tool names declared in the
	// manifest's active version. Extracted server-side from the
	// `tools` jsonb (each element is an object whose `name` field is
	// the capability identifier). An empty array is `[]`, never
	// `null`, so consumers can range over the value without a nil
	// guard.
	Capabilities []string `json:"capabilities"`
	// Availability is the string enum currently carrying a single
	// value ("available"); see file-level docblock for the future
	// extension plan.
	Availability string `json:"availability"`
}

// listPeersResponse is the envelope returned by `GET /v1/peers`. The
// shape mirrors `listWatchkeepersResponse` exactly (items + next_cursor)
// so future seek-pagination lands without a wire-shape break.
type listPeersResponse struct {
	Items      []peerRow `json:"items"`
	NextCursor *string   `json:"next_cursor"`
}

// handleListPeers serves `GET /v1/peers`. It returns every
// `watchkeeper.watchkeeper` row whose `status = 'active'` joined with
// its `active_manifest_version_id`'s `manifest_version` row (role
// description, language, tools jsonb) and the parent `manifest` row
// (role display_name). Rows without an `active_manifest_version_id`
// (M3.5 spawn-in-progress paths) are SKIPPED — only watchkeepers
// ready to participate in K2K conversations are surfaced. Rows are
// ordered by `created_at DESC` for deterministic output.
//
// Query parameters:
//   - `?limit=<n>`: 1..200 inclusive; default 50. Zero, negative, or
//     oversized values return 400 `invalid_request` before the runner
//     fires.
//
// Audit / PII discipline: no `keeperslog.` references, no `.Append(`
// calls — pinned by a source-grep AC in `handlers_peers_test.go`. The
// M1.4 audit subscriber is the canonical observer for K2K events; this
// handler is purely a read projection.
func handleListPeers(r scopedRunner) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		claim, ok := ClaimFromContext(req.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		limit := defaultPeerListLimit
		if raw := req.URL.Query().Get("limit"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n <= 0 || n > maxPeerListLimit {
				writeError(w, http.StatusBadRequest, "invalid_request")
				return
			}
			limit = n
		}

		out := make([]peerRow, 0, limit)
		err := r.WithScope(req.Context(), claim, func(ctx context.Context, tx pgx.Tx) error {
			// JOIN watchkeeper -> manifest_version -> manifest. The
			// INNER join on active_manifest_version_id excludes rows
			// that have not yet been bound to a manifest version
			// (pending status without a pinned version, or rare
			// active rows that pre-date M2.7's pin discipline). The
			// JOIN against `manifest` carries the role name
			// (display_name) — manifest_version itself does NOT
			// store the role name; that lives one hop up.
			// JOIN chain note (iter-1 codex P2 fix): the manifest join
			// follows `mv.manifest_id`, NOT `wk.manifest_id`. Migration
			// 002's docblock spells out that the composite-FK invariant
			// "`watchkeeper.active_manifest_version_id` references a
			// `manifest_version` whose `manifest_id` matches
			// `watchkeeper.manifest_id`" is deferred to Phase 2 and is
			// NOT enforced at the SQL layer today. If a write path
			// (legacy or malformed) lands a row where the two ids
			// disagree, joining `manifest` on `wk.manifest_id` would
			// surface a peer whose `role` (from `m.display_name`) and
			// `description` / `language` / `capabilities` (from
			// `mv.*`) belong to two different manifests — an
			// impossible peer record. Threading the join through
			// `mv.manifest_id` keeps every projected row internally
			// consistent: all four manifest-derived fields come from
			// the SAME manifest version that the watchkeeper is
			// currently bound to.
			rows, err := tx.Query(ctx, `
                SELECT wk.id,
                       coalesce(m.display_name, ''),
                       coalesce(mv.personality, ''),
                       coalesce(mv.language, ''),
                       mv.tools
                FROM watchkeeper.watchkeeper wk
                JOIN watchkeeper.manifest_version mv
                  ON mv.id = wk.active_manifest_version_id
                JOIN watchkeeper.manifest m
                  ON m.id = mv.manifest_id
                WHERE wk.status = 'active'
                ORDER BY wk.created_at DESC
                LIMIT $1
            `, limit)
			if err != nil {
				return fmt.Errorf("list_peers query: %w", err)
			}
			defer rows.Close()
			for rows.Next() {
				var (
					rec      peerRow
					toolsRaw []byte
				)
				if err := rows.Scan(&rec.WatchkeeperID, &rec.Role, &rec.Description, &rec.Language, &toolsRaw); err != nil {
					return fmt.Errorf("list_peers scan: %w", err)
				}
				rec.Capabilities = extractCapabilities(toolsRaw)
				rec.Availability = peerAvailabilityAvailable
				out = append(out, rec)
			}
			return rows.Err()
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "list_peers_failed")
			return
		}

		writeJSON(w, http.StatusOK, listPeersResponse{Items: out, NextCursor: nil})
	})
}

// extractCapabilities pulls the closed-set list of tool names from the
// `manifest_version.tools` jsonb column. The on-disk shape is a JSON
// array of objects whose `name` field is the capability identifier
// (e.g. `[{"name":"update_ticket_field"}]` — see migrations 024+ for
// the seed shape). Empty / NULL / non-array payloads return an empty
// slice rather than nil so the JSON encoder emits `[]` not `null`,
// which keeps the wire shape uniform for the M1.3.d filter that
// ranges over the slice without a nil guard.
//
// Defensive parse: an entry whose `name` is missing or non-string is
// SKIPPED (not appended as ""). A malformed tools payload (invalid
// JSON, top-level non-array) returns the empty slice without
// surfacing an error — `tools` carries a CHECK-free jsonb shape today
// and the handler is read-only; a malformed row is a manifest
// authoring bug surfaced through the M1.4 audit path, not a 500 from
// the read endpoint. The empty-slice fallback keeps every other
// well-formed row in the response visible.
func extractCapabilities(raw []byte) []string {
	out := make([]string, 0)
	if len(raw) == 0 {
		return out
	}
	// Use a typed slice with name-only decoder so unknown sibling
	// fields (description, schema, etc.) in a future tools shape do
	// not break the parse. json.Unmarshal silently drops unknown
	// fields by default.
	var entries []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		return out
	}
	for _, e := range entries {
		if e.Name == "" {
			continue
		}
		out = append(out, e.Name)
	}
	return out
}
