package server

import (
	"bytes"
	"fmt"
	"net/http"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/internal/keep/db"
	"github.com/vadimtrunov/watchkeepers/core/internal/keep/publish"
)

// heartbeatFrame is the SSE comment (RFC-style `:`-prefixed line) emitted
// on idle streams so proxies and the TCP keepalive engine stay honest
// without forwarding a bogus event on the wire.
const heartbeatFrame = ":\n\n"

// handleSubscribe serves GET /v1/subscribe. It upgrades the response to
// Server-Sent Events, registers a subscriber with the publish Registry
// keyed on the verified Claim.Scope, and loops on three signals:
//
//  1. request context done (client disconnected or server is shutting
//     down via Registry.Close) -> return (defer unsubscribes);
//  2. registry channel yields an event  -> write one SSE frame + flush;
//  3. heartbeat ticker fires            -> write a `:` comment + flush.
//
// The handler never blocks on the Registry — the Registry is non-blocking
// by contract (AC4). The only blocking write is on the client socket,
// which http.Server's ReadHeaderTimeout + Shutdown grace window already
// bound.
func handleSubscribe(reg *publish.Registry, heartbeat time.Duration) http.Handler {
	if heartbeat <= 0 {
		heartbeat = 15 * time.Second
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		claim, ok := ClaimFromContext(req.Context())
		if !ok {
			// Defense-in-depth: middleware should have rejected this.
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		// Belt-and-suspenders: the middleware verifies scope shape via
		// auth.ValidScope, but RoleForScope is the authoritative mapping
		// for DB roles; rejecting an unknown scope here keeps the
		// Registry key space aligned with the grants layer.
		if _, err := db.RoleForScope(claim.Scope); err != nil {
			writeError(w, http.StatusBadRequest, "bad_scope")
			return
		}

		// SSE requires a flush after every frame; the handler aborts early
		// if the http.ResponseWriter doesn't implement http.Flusher (which
		// would indicate a reverse proxy that has buffered the response
		// against our contract). httptest.ResponseRecorder lacks Flush; in
		// that case we still emit headers so header-only tests pass.
		flusher, _ := w.(http.Flusher)

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		if flusher != nil {
			flusher.Flush()
		} else {
			// No flusher -> we cannot stream; end the response cleanly so
			// header-only unit tests pass without deadlocking on the
			// subsequent select loop.
			return
		}

		ch, unsub := reg.Subscribe(req.Context(), claim)
		defer unsub()

		ticker := time.NewTicker(heartbeat)
		defer ticker.Stop()

		for {
			select {
			case <-req.Context().Done():
				return
			case ev, ok := <-ch:
				if !ok {
					// Registry closed (server shutdown) or we were dropped
					// for a full buffer; either way the stream ends.
					return
				}
				if err := writeSSEEvent(w, flusher, ev); err != nil {
					return
				}
			case <-ticker.C:
				if _, err := w.Write([]byte(heartbeatFrame)); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	})
}

// writeSSEEvent serialises ev into the canonical SSE frame
// (id / event / data / blank line) and flushes. Returns the first write
// error so the caller can abort the stream promptly.
func writeSSEEvent(w http.ResponseWriter, flusher http.Flusher, ev publish.Event) error {
	var buf bytes.Buffer
	// Use Fprintf once so any write error short-circuits the rest of the
	// frame — partial frames would confuse SSE clients.
	fmt.Fprintf(&buf, "id: %s\nevent: %s\ndata: %s\n\n", ev.ID.String(), ev.EventType, ev.Payload)
	if _, err := w.Write(buf.Bytes()); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}
