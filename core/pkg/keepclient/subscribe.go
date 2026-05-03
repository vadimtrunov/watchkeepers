package keepclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// subscribePath is the SSE streaming endpoint on the Keep server (M2.7.e).
// Token injection is required (see authPathPrefix in do.go).
const subscribePath = "/v1/subscribe"

// ErrStreamClosed is returned by [Stream.Next] after [Stream.Close] has been
// called. It is intentionally distinct from [io.EOF] so callers can tell a
// clean server-side end-of-stream apart from a local close.
var ErrStreamClosed = errors.New("keepclient: stream closed")

// Event is one decoded SSE frame from the Keep server's `/v1/subscribe`
// stream. The fields mirror the wire frame verbatim:
//
//	id:    -> ID
//	event: -> EventType
//	data:  -> Payload (kept as raw JSON, decoder does not unmarshal)
//
// An event without a `data:` line yields Payload == nil; an event without an
// `event:` line yields EventType == "". Heartbeat comment lines (`:` prefix)
// are silently skipped by the parser and never surface as Event values.
type Event struct {
	// ID is the server-emitted UUID string from the `id:` field. Captured
	// even though M2.8.d.a does not use it; M2.8.d.b's reconnect/resume
	// will read it without a struct change.
	ID string
	// EventType is the value of the `event:` field. Empty when absent.
	EventType string
	// Payload is the raw JSON value of the (joined) `data:` field. Nil
	// when the frame contained no `data:` line. Multi-line `data:` per
	// the SSE spec is concatenated with `\n` between segments.
	Payload json.RawMessage
}

// Stream is the iterator returned by [Client.Subscribe]. Use [Stream.Next] to
// pull one [Event] at a time and [Stream.Close] (typically deferred) to
// release the underlying HTTP connection. Stream is NOT safe for concurrent
// use across goroutines — guard external callers if needed.
type Stream struct {
	body   io.ReadCloser
	reader *bufio.Reader

	closeOnce sync.Once
	closeErr  error

	mu     sync.Mutex // guards closed
	closed bool
}

// Next reads and returns the next [Event] from the stream. Heartbeat frames
// are silently skipped. End-of-stream returns [io.EOF]; calling Next after
// [Stream.Close] returns [ErrStreamClosed]; context cancellation returns an
// error wrapping [context.Canceled] / [context.DeadlineExceeded].
//
// The supplied context is checked before reading so a cancelled context
// short-circuits even if the underlying reader is already buffered. The
// transport's *http.Request context (set in [Client.Subscribe]) is the
// authoritative cancel source for the network read itself; this method
// observes the same signal via the body reader.
func (s *Stream) Next(ctx context.Context) (Event, error) {
	if err := ctx.Err(); err != nil {
		return Event{}, fmt.Errorf("keepclient: subscribe: %w", err)
	}

	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return Event{}, ErrStreamClosed
	}

	ev, err := scanEvent(s.reader)
	if err != nil {
		// Surface context cancellation cleanly — the http transport
		// closes the body when the request context is cancelled, which
		// produces an io.ErrUnexpectedEOF or net.OpError. Re-wrap as
		// context.Canceled so callers can match with errors.Is.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Event{}, fmt.Errorf("keepclient: subscribe: %w", ctxErr)
		}
		// Distinguish a local Close() (returns ErrStreamClosed) from
		// the server-side EOF. Body reads after Close return errors
		// like "http: read on closed response body" — re-classify as
		// ErrStreamClosed when the local flag is set.
		s.mu.Lock()
		closed := s.closed
		s.mu.Unlock()
		if closed {
			return Event{}, ErrStreamClosed
		}
		return Event{}, err
	}
	return ev, nil
}

// Close releases the underlying HTTP response body. It is idempotent; the
// first call closes the body, subsequent calls return the same result without
// touching the body again. After Close, [Stream.Next] returns
// [ErrStreamClosed] (NOT [io.EOF], so callers can distinguish a clean
// server-side end-of-stream from a local close).
func (s *Stream) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		if s.body != nil {
			s.closeErr = s.body.Close()
		}
	})
	return s.closeErr
}

// Subscribe opens a streaming SSE connection to the Keep server's
// `GET /v1/subscribe` endpoint. The returned [Stream] yields one [Event] per
// [Stream.Next] call until clean EOF, context cancel, or transport error.
//
// Auth: Subscribe injects `Authorization: Bearer <token>` from the
// configured [TokenSource] (identical to the unary `do` helper). Calling
// without [WithTokenSource] returns [ErrNoTokenSource] synchronously, before
// any network round-trip.
//
// Initial-status mapping: a non-2xx open returns a [*ServerError] whose
// Unwrap chain matches the Err* sentinels (400→[ErrInvalidRequest],
// 401→[ErrUnauthorized], 403→[ErrForbidden], 404→[ErrNotFound],
// 5xx→[ErrInternal]). On 2xx the body is wrapped in a [Stream]; the caller
// owns Close().
//
// Subscribe is a single-attempt open: it does NOT reconnect, retry, or
// resume after a transport error. Callers that need a long-lived stream
// across drops should use [Client.SubscribeResilient] (M2.8.d.b), which
// layers reconnect + Last-Event-ID resume + dedup on top of this primitive.
func (c *Client) Subscribe(ctx context.Context) (*Stream, error) {
	return c.subscribeWithLastEventID(ctx, "")
}

// subscribeWithLastEventID opens the SSE stream like [Client.Subscribe] but
// optionally sets the `Last-Event-ID` request header to lastID when lastID
// is non-empty. Used by [Client.SubscribeResilient] to forward the last
// observed [Event.ID] across reconnects.
//
// Note on server behavior: the current Keep server (M2.7.e) does NOT read
// the `Last-Event-ID` header — it always streams events from the moment
// the subscription is registered. The client sends the header for
// forward-compatibility with a future server-side replay implementation;
// callers must not assume gap-free delivery across a reconnect. The
// [Client.SubscribeResilient] dedup hook (`WithDedup`/`WithDedupLRU`) is
// the safety net for the duplicate-on-reconnect case once the server does
// honor the header.
func (c *Client) subscribeWithLastEventID(ctx context.Context, lastID string) (*Stream, error) {
	c.cfg.logger(ctx, "keepclient.subscribe begin", "path", subscribePath, "last_event_id", lastID)

	if c.cfg.tokenSource == nil {
		c.cfg.logger(ctx, "keepclient.subscribe end", "status", 0, "err", ErrNoTokenSource)
		return nil, ErrNoTokenSource
	}

	endpoint, err := joinURL(c.cfg.baseURL, subscribePath)
	if err != nil {
		c.cfg.logger(ctx, "keepclient.subscribe end", "status", 0, "err", err)
		return nil, fmt.Errorf("keepclient: join url: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		c.cfg.logger(ctx, "keepclient.subscribe end", "status", 0, "err", err)
		return nil, fmt.Errorf("keepclient: build request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}

	// Resolve the token BEFORE issuing the request so a refresh failure
	// never becomes a stale-token request (mirrors do.go discipline).
	tok, err := c.cfg.tokenSource.Token(ctx)
	if err != nil {
		c.cfg.logger(ctx, "keepclient.subscribe end", "status", 0, "err", err)
		return nil, fmt.Errorf("keepclient: token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := c.cfg.httpClient.Do(req)
	if err != nil {
		c.cfg.logger(ctx, "keepclient.subscribe end", "status", 0, "err", err)
		return nil, fmt.Errorf("keepclient: GET %s: %w", subscribePath, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		se := parseServerError(resp)
		// parseServerError reads the body but does not close it; close
		// here so the connection can be reused on the early-error path.
		_ = resp.Body.Close()
		c.cfg.logger(ctx, "keepclient.subscribe end", "status", resp.StatusCode, "err", se)
		return nil, se
	}

	c.cfg.logger(ctx, "keepclient.subscribe end", "status", resp.StatusCode, "err", nil)
	return &Stream{
		body:   resp.Body,
		reader: bufio.NewReader(resp.Body),
	}, nil
}

// scanEvent reads one SSE event from r, skipping heartbeat comment frames
// (`:`-prefixed lines) per the SSE spec. Returns the decoded [Event], or
// [io.EOF] on a clean end-of-stream, or the underlying transport error.
//
// Wire shape (per the server in core/internal/keep/server/handlers_subscribe.go):
//
//	id: <uuid>\n
//	event: <event-type>\n
//	data: <jsonb>\n
//	\n
//
// Heartbeats:
//
//	:\n
//	\n
//
// SSE spec quirks the parser handles:
//   - Multi-line `data:` continuation: every `data:` line in the same event
//     contributes a segment; segments join with `\n` (matching the spec) so
//     the caller can re-parse the result as JSON containing newlines, or as
//     multiple JSON values on separate lines.
//   - An event without `data:` decodes to Payload==nil so the caller can
//     distinguish a payload-less ping from an empty-string payload.
//   - A frame consisting solely of comment (`:`) lines plus the terminator
//     blank line is silently dropped — the loop continues into the next
//     event without surfacing a zero-value Event.
//   - A trailing event without a final blank line (server closed the
//     connection mid-frame) is NOT emitted; the caller sees io.EOF only.
//     This matches the spec's "interpret on dispatch" rule.
func scanEvent(r *bufio.Reader) (Event, error) {
	var (
		ev        Event
		dataParts []string
		hasField  bool // any non-comment field seen in current frame
		hasData   bool // at least one data: field seen
	)

	for {
		line, err := r.ReadString('\n')
		if err != nil {
			// Per the SSE spec, an event without a final blank
			// line is discarded. Mirror the spec: signal io.EOF
			// (or the underlying transport error) regardless of
			// any partial buffered fields. The buffered `line`
			// (which may hold the trailing partial event) is
			// intentionally dropped here.
			return Event{}, err
		}

		// Strip the trailing \n; tolerate \r\n on the wire.
		line = strings.TrimSuffix(line, "\n")
		line = strings.TrimSuffix(line, "\r")

		// Blank line: dispatch the current frame, or skip if no fields
		// were seen (e.g. a heartbeat-only frame).
		if line == "" {
			if !hasField {
				// Reset and continue to next frame.
				ev = Event{}
				dataParts = dataParts[:0]
				hasField = false
				hasData = false
				continue
			}
			if hasData {
				ev.Payload = json.RawMessage(strings.Join(dataParts, "\n"))
			}
			return ev, nil
		}

		// Comment line: SSE uses `:` prefix as a no-op (heartbeat).
		// Do NOT mark hasField — a comment-only frame must not
		// dispatch as an empty event.
		if strings.HasPrefix(line, ":") {
			continue
		}

		// Field parse: `name: value` (single colon) or `name` (no
		// value). Per the spec, a single space after the colon is
		// stripped.
		var name, value string
		if idx := strings.IndexByte(line, ':'); idx >= 0 {
			name = line[:idx]
			value = line[idx+1:]
			value = strings.TrimPrefix(value, " ")
		} else {
			name = line
		}

		hasField = true
		switch name {
		case "id":
			ev.ID = value
		case "event":
			ev.EventType = value
		case "data":
			hasData = true
			dataParts = append(dataParts, value)
		default:
			// Unknown field: per the SSE spec, ignore. We still
			// treat it as having "seen a field" so a frame of only
			// unknown fields does not collide with the comment-only
			// reset path above.
		}
	}
}
