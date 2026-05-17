package keepclient

// read_peers.go — `keepclient.ListPeers` (M1.2). The client method
// powers the M1.3.* peer.* tool family: `peer.Ask` resolves a target
// (watchkeeper id or role name) over this list; `peer.Broadcast`
// applies its in-memory `RoleFilter` over this list. Filters are
// applied client-side so a single source of truth ("all active peers
// at this instant") underlies every consumer.

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

// maxPeerListLimit mirrors the server's hard cap (200). Reproducing it
// client-side spares a round trip on obvious bugs and keeps the client
// and server bounds in lockstep documentation-wise.
const maxPeerListLimit = 200

// PeerAvailability is the stable string enum reported by the server in
// the `availability` column of each peer row. Phase 2 ships a single
// value (`PeerAvailabilityAvailable`); future M1.5 / M1.6 milestones
// may introduce additional states (e.g. throttled / escalated). The
// type is a `string` alias so consumers can compare with `==` without
// importing a sentinel function.
type PeerAvailability = string

// PeerAvailabilityAvailable is the only availability value emitted by
// the M1.2 endpoint. M1.3.d's `RoleFilter` treats any other value as
// "unavailable, skip this peer" — a forward-compatible default that
// keeps a future "throttled" state out of the broadcast fan-out
// without a client release.
const PeerAvailabilityAvailable PeerAvailability = "available"

// Peer mirrors one row from `GET /v1/peers`. Field names + JSON tags
// match the server's `peerRow` verbatim. Capabilities is always a
// non-nil slice (`[]` on the wire); Role / Description / Language are
// always strings (empty on NULL columns, never null on the wire).
type Peer struct {
	// WatchkeeperID is the watchkeeper.watchkeeper row UUID.
	WatchkeeperID string `json:"watchkeeper_id"`
	// Role is the closed-set role name (manifest.display_name).
	Role string `json:"role"`
	// Description is the prose role description
	// (manifest_version.personality). May be the empty string.
	Description string `json:"description"`
	// Language is the BCP-47-lite language code (e.g. "en", "en-US").
	// May be the empty string.
	Language string `json:"language"`
	// Capabilities is the list of tool names declared by the peer's
	// active manifest version. Always non-nil (the server emits `[]`
	// for an empty toolset, never `null`).
	Capabilities []string `json:"capabilities"`
	// Availability is the peer's reported availability state. Phase 2
	// callers can match against [PeerAvailabilityAvailable] directly.
	Availability PeerAvailability `json:"availability"`
}

// ListPeersRequest configures a [Client.ListPeers] call. The zero
// value is valid and means "server-default limit (50), no filter".
// The client deliberately does not surface role / language /
// capability filters — M1.3.d applies those in-memory after the
// fetch so every M1.3.* consumer sees the same active set.
type ListPeersRequest struct {
	// Limit caps the number of rows returned. 0 means "do not send
	// the query parameter at all and let the server apply its default
	// (50)". Negative or > 200 returns [ErrInvalidRequest] without a
	// network round-trip; the server enforces the same upper bound.
	Limit int
}

// ListPeersResponse mirrors the server's `listPeersResponse`
// envelope. NextCursor is reserved for a future seek-pagination
// follow-up; M1.2 always returns nil.
type ListPeersResponse struct {
	// Items is the list of peers in `created_at DESC` order. Always
	// non-nil; an empty active set surfaces as `[]Peer{}`.
	Items []Peer `json:"items"`
	// NextCursor is reserved for a future pagination cursor; nil today.
	NextCursor *string `json:"next_cursor"`
}

// ListPeers calls `GET /v1/peers` with the optional limit. Out-of-
// range limit values return [ErrInvalidRequest] synchronously without
// a network round-trip.
//
// The returned [ListPeersResponse.Items] slice is always non-nil — an
// empty active set decodes as `[]Peer{}` so callers can range over
// the slice without a nil guard. Each [Peer.Capabilities] is likewise
// non-nil (server emits `[]` not `null`).
//
// M1.3.* consumers resolve targets / apply filters in memory over the
// returned slice; this client method intentionally does NOT surface a
// role / language / capability filter API.
func (c *Client) ListPeers(ctx context.Context, req ListPeersRequest) (*ListPeersResponse, error) {
	if req.Limit < 0 || req.Limit > maxPeerListLimit {
		return nil, ErrInvalidRequest
	}

	q := url.Values{}
	if req.Limit > 0 {
		q.Set("limit", strconv.Itoa(req.Limit))
	}
	path := "/v1/peers"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}

	var out ListPeersResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	// Server emits `[]` on empty; defensive client-side normalization
	// keeps the documented "non-nil Items / non-nil Capabilities"
	// contract intact even if a future server bug regresses to
	// emitting `null` for either shape.
	if out.Items == nil {
		out.Items = []Peer{}
	}
	for i := range out.Items {
		if out.Items[i].Capabilities == nil {
			out.Items[i].Capabilities = []string{}
		}
	}
	return &out, nil
}
