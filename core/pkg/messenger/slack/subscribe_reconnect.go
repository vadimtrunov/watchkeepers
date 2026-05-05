package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger"
)

// sessionLoop is the outer per-subscription loop. Each iteration owns
// one healthy WSS connection (already past `hello`) until it surfaces
// either a disconnect envelope, a transport error, or a heartbeat
// timeout. The loop then closes the dead connection, dials a fresh
// `apps.connections.open` URL with backoff-with-jitter, and re-enters
// the read loop on the new connection. The caller's
// [messenger.MessageHandler] keeps receiving events without observing
// the reconnect.
//
// On runCtx cancellation (caller-initiated Stop) the loop exits with
// no error. On exhausted retry budget the loop exits with the wrapped
// [ErrReconnectExhausted] surfaced via [messenger.Subscription.Stop].
func (s *socketSubscription) sessionLoop(handler messenger.MessageHandler) {
	defer close(s.done)

	for {
		// Run one read session against the current conn. Returns nil
		// on caller-initiated Stop, or a non-nil trigger asking for a
		// reconnect.
		reconn := s.readSession(handler)
		if reconn == nil {
			return
		}

		// readSession asked for a reconnect. Tear the dead conn down
		// (write side may already be torn) and dial a fresh one with
		// bounded backoff.
		if err := s.tearDownConn(); err != nil {
			s.client.cfg.logger.Log(
				s.runCtx, "slack: socket mode: tear down conn failed",
				"err_type", fmt.Sprintf("%T", err),
			)
		}

		reconnErr := s.reconnect(reconn)
		// Always honour caller-initiated Stop FIRST: if runCtx is
		// cancelled the reconnect either exited cleanly (nil) or
		// returned the cancellation error — either way the
		// subscription should unwind without re-entering readSession
		// against a possibly-nil conn.
		if s.runCtx.Err() != nil {
			return
		}
		if reconnErr != nil {
			s.stopErr = reconnErr
			return
		}
		// New connection swapped in; loop and read from it.
	}
}

// reconnectTrigger captures why the read session unwound and asked
// for a reconnect. Carries the last observed transport-level error so
// it can ride into [ErrReconnectExhausted] if the budget is burned.
type reconnectTrigger struct {
	// reason is a short stable label suitable for log entries.
	reason string
	// lastErr is the most recent error, if any, that the read session
	// observed. May be nil for clean disconnect-envelope unwinds.
	lastErr error
}

// readSession is the inner per-connection event-pump. Single
// read goroutine + this select-loop, single shared *websocket.Conn —
// no concurrent reads. Acks are written back on the same connection
// but always BEFORE the handler dispatch so a slow handler cannot
// miss the 3s ack budget.
//
// Returns:
//   - nil — caller-initiated Stop (runCtx cancelled). Loop exits
//     cleanly without entering the reconnect path.
//   - non-nil trigger — read session asked for a reconnect. The
//     trigger carries the reason label + last observed error.
//
//nolint:gocyclo // single state machine; splitting hides the timing.
func (s *socketSubscription) readSession(handler messenger.MessageHandler) *reconnectTrigger {
	pingInterval := s.client.socketPingInterval()
	pingTimeout := s.client.socketPingTimeout()

	// sessCtx is a per-session ctx derived from runCtx. Cancelling it
	// unblocks the read pump's conn.Read on a reconnect-trigger path
	// where runCtx is still live (caller has not Stopped yet); on
	// runCtx cancellation sessCtx is also cancelled via the parent.
	sessCtx, sessCancel := context.WithCancel(s.runCtx)
	defer sessCancel()

	// Activity timer: any inbound envelope (event, disconnect, pong)
	// resets it. When it fires we send a ping; when the pong-deadline
	// fires we surface a reconnect trigger.
	activity := time.NewTimer(pingInterval)
	defer activity.Stop()

	// pongDeadline runs from the moment we send a ping. nil when no
	// ping is in flight.
	var pongDeadline *time.Timer
	defer func() {
		if pongDeadline != nil {
			pongDeadline.Stop()
		}
	}()

	// readResult / readPump pump the blocking conn.Read off the
	// goroutine running the timer-select so a stuck Read never blocks
	// the heartbeat. The pump goroutine joins automatically when the
	// session exits — sessCancel unblocks the Read on a clean exit.
	reads := make(chan readResult, 1)
	pumpDone := make(chan struct{})

	conn := s.currentConn()
	go func() {
		defer close(pumpDone)
		for {
			_, raw, err := conn.Read(sessCtx)
			select {
			case reads <- readResult{raw: raw, err: err}:
			case <-sessCtx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	// ackPump captures envelope ids the read goroutine wants to ack.
	// Single-goroutine ack writes (this method) so a concurrent ping
	// frame cannot interleave with a half-written ack frame.
	for {
		select {
		case <-s.runCtx.Done():
			// Stop initiated. Cancel sessCtx (the deferred sessCancel
			// runs on return; we explicitly cancel here so the pump
			// is unblocked promptly), drain any in-flight read, and
			// return clean.
			sessCancel()
			s.drainReadPump(reads, pumpDone)
			return nil

		case <-activity.C:
			// No envelope received within pingInterval. Send a ping
			// and start the pong deadline.
			if err := s.sendPing(); err != nil {
				s.client.cfg.logger.Log(
					s.runCtx, "slack: socket mode: ping write failed",
					"err_type", fmt.Sprintf("%T", err),
				)
				sessCancel()
				s.drainReadPump(reads, pumpDone)
				return &reconnectTrigger{reason: "ping_write_error", lastErr: err}
			}
			pongDeadline = time.NewTimer(pingTimeout)

		case <-deadlineC(pongDeadline):
			// Pong did not arrive in time. Reconnect.
			s.client.cfg.logger.Log(
				s.runCtx, "slack: socket mode: pong timeout",
			)
			sessCancel()
			s.drainReadPump(reads, pumpDone)
			return &reconnectTrigger{reason: "pong_timeout"}

		case res := <-reads:
			if res.err != nil {
				if errors.Is(res.err, context.Canceled) || errors.Is(res.err, context.DeadlineExceeded) {
					// sessCtx cancel triggered the read failure. If
					// the parent runCtx was also cancelled (caller-
					// initiated Stop), exit cleanly.
					if s.runCtx.Err() != nil {
						sessCancel()
						<-pumpDone
						return nil
					}
				}
				s.logReadError(res.err)
				sessCancel()
				<-pumpDone
				return &reconnectTrigger{reason: "read_error", lastErr: res.err}
			}

			// Reset the activity / pong state — any inbound frame
			// counts as proof the connection is alive.
			if !activity.Stop() {
				select {
				case <-activity.C:
				default:
				}
			}
			activity.Reset(pingInterval)
			if pongDeadline != nil {
				pongDeadline.Stop()
				pongDeadline = nil
			}

			var env rawEnvelope
			if jerr := json.Unmarshal(res.raw, &env); jerr != nil {
				s.client.cfg.logger.Log(
					s.runCtx, "slack: socket mode: decode envelope failed",
					"err_type", fmt.Sprintf("%T", jerr),
				)
				continue
			}
			if env.Type == envelopeTypeDisconnect {
				s.client.cfg.logger.Log(
					s.runCtx, "slack: socket mode: disconnect requested",
					"reason", env.Reason,
				)
				sessCancel()
				<-pumpDone
				return &reconnectTrigger{reason: "disconnect_envelope_" + env.Reason}
			}
			if env.Type == envelopeTypePong {
				// Application-layer pong — already reset the state
				// above; nothing else to do.
				continue
			}
			// Ack-bearing envelope: events_api / slash_commands /
			// interactive carry an envelope_id we MUST echo. Empty
			// envelope_id on these types is a Slack protocol violation;
			// log a warning and continue (no ack possible).
			if isAckBearing(env.Type) {
				if env.EnvelopeID == "" {
					s.client.cfg.logger.Log(
						s.runCtx, "slack: socket mode: ack-bearing envelope with empty envelope_id; protocol violation",
						"type", env.Type,
					)
				} else {
					s.ackEnvelope(env.EnvelopeID)
				}
			}
			s.dispatch(env, handler)
		}
	}
}

// readResult is the per-frame outcome of one conn.Read. The read
// session pumps the blocking Read off its select-loop so a stuck
// transport never blocks the heartbeat timer.
type readResult struct {
	raw []byte
	err error
}

// drainReadPump consumes any in-flight read so the pump goroutine
// exits cleanly. Must be called before returning from readSession on
// the runCtx-cancel and reconnect paths so the pump goroutine joins.
func (s *socketSubscription) drainReadPump(reads <-chan readResult, done <-chan struct{}) {
	// Race between "pump already exited" and "pump still parked in
	// Read on a now-cancelled ctx". Either way `done` will eventually
	// close; drain `reads` defensively to unblock a pending send.
	select {
	case <-done:
		return
	default:
	}
	for {
		select {
		case <-done:
			return
		case <-reads:
			// drop
		}
	}
}

// deadlineC returns the timer channel for the pong deadline, or a
// nil channel when no deadline is in flight (nil channels block
// forever in select).
func deadlineC(t *time.Timer) <-chan time.Time {
	if t == nil {
		return nil
	}
	return t.C
}

// isAckBearing reports whether `type` value carries an envelope_id we
// must echo back. Slack documents events_api / slash_commands /
// interactive as ack-required; hello / disconnect / pong are not.
func isAckBearing(typ string) bool {
	switch typ {
	case envelopeTypeEventsAPI, envelopeTypeSlashCommands, envelopeTypeInteractive:
		return true
	}
	return false
}

// reconnect drives the bounded backoff loop. trigger carries the
// triggering reason + last error which rides into a wrapped
// [ErrReconnectExhausted] once the attempt budget is exhausted.
//
// Honours runCtx cancellation: a Stop() during the backoff sleep
// surfaces as a clean (no-error) unwind so the sessionLoop returns
// without setting stopErr.
func (s *socketSubscription) reconnect(trigger *reconnectTrigger) error {
	maxAttempts := s.client.socketMaxReconnectAttempts()
	initial := s.client.socketReconnectInitialDelay()
	maxDelay := s.client.socketReconnectMaxDelay()
	sleepFn := s.client.socketSleeper()
	randFn := s.client.socketRand()

	lastErr := trigger.lastErr
	for attempt := 0; attempt < maxAttempts; attempt++ {
		delay := socketBackoffFor(attempt, initial, maxDelay, randFn)
		if err := sleepFn.Sleep(s.runCtx, delay); err != nil {
			// runCtx cancelled during backoff — clean exit, no
			// stopErr (caller asked for it).
			if s.runCtx.Err() != nil {
				return nil
			}
			return err
		}

		s.client.cfg.logger.Log(
			s.runCtx, "slack: socket mode: reconnect attempt",
			"reason", trigger.reason,
			"attempt", attempt+1,
		)

		newConn, err := s.client.openSocketModeConn(s.runCtx)
		if err != nil {
			// runCtx cancellation surfaces from openSocketModeConn
			// as a wrapped ctx error. Treat as clean exit.
			if s.runCtx.Err() != nil {
				return nil
			}
			lastErr = err
			s.client.cfg.logger.Log(
				s.runCtx, "slack: socket mode: reconnect open failed",
				"reason", trigger.reason,
				"attempt", attempt+1,
				"err_type", fmt.Sprintf("%T", err),
			)
			continue
		}

		// Successful reconnect — swap in the new conn. The dead old
		// conn was already torn down by [tearDownConn] before
		// reconnect started.
		_ = s.swapConn(newConn)
		s.client.cfg.logger.Log(
			s.runCtx, "slack: socket mode: reconnected",
			"reason", trigger.reason,
			"attempt", attempt+1,
		)
		return nil
	}

	// Budget burned — surface the wrapped sentinel via Stop().
	if lastErr == nil {
		return ErrReconnectExhausted
	}
	return fmt.Errorf("%w: %w", ErrReconnectExhausted, lastErr)
}

// tearDownConn closes the current connection (if any) and clears the
// pointer. Errors are non-fatal — the conn is being discarded.
func (s *socketSubscription) tearDownConn() error {
	old := s.swapConn(nil)
	if old == nil {
		return nil
	}
	return old.Close(websocket.StatusGoingAway, "reconnect")
}

// logReadError writes a structured log entry for a read-side failure.
// Distinguishes ctx-cancellation (clean Stop) from network errors so
// the operator can tell the two apart in production.
func (s *socketSubscription) logReadError(err error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		s.client.cfg.logger.Log(
			s.runCtx, "slack: socket mode: read loop stopped",
			"reason", "context canceled",
		)
		return
	}
	// websocket.CloseError carries the close-frame status; expose
	// the numeric code so operators can correlate with Slack's
	// disconnect log without leaking the close-frame reason
	// (which Slack documents as free-text and may carry PII).
	var ce websocket.CloseError
	if errors.As(err, &ce) {
		s.client.cfg.logger.Log(
			s.runCtx, "slack: socket mode: read error",
			"reason", "ws close",
			"code", int(ce.Code),
		)
		return
	}
	s.client.cfg.logger.Log(
		s.runCtx, "slack: socket mode: read error",
		"reason", "transport error",
		"err_type", fmt.Sprintf("%T", err),
	)
}

// ackEnvelope writes a `{"envelope_id": "..."}` text frame back to
// Slack. Slack's contract: ack within 3s. The write uses a per-ack
// timeout independent of runCtx so a stuck Slack write never blocks
// the read loop forever.
func (s *socketSubscription) ackEnvelope(envelopeID string) {
	body, err := json.Marshal(ackPayload{EnvelopeID: envelopeID})
	if err != nil {
		// Marshalling a single-string struct cannot fail in
		// practice; defensive log + continue.
		s.client.cfg.logger.Log(
			s.runCtx, "slack: socket mode: marshal ack failed",
			"err_type", fmt.Sprintf("%T", err),
		)
		return
	}
	conn := s.currentConn()
	if conn == nil {
		return
	}
	ackCtx, cancel := context.WithTimeout(s.runCtx, defaultAckTimeout)
	defer cancel()
	if werr := conn.Write(ackCtx, websocket.MessageText, body); werr != nil {
		s.client.cfg.logger.Log(
			s.runCtx, "slack: socket mode: ack write failed",
			"err_type", fmt.Sprintf("%T", werr),
		)
		return
	}
	s.client.cfg.logger.Log(
		s.runCtx, "slack: socket mode: ack sent",
		"type", "events_api",
	)
}

// sendPing writes an application-layer `{"type":"ping","id":"..."}`
// frame to the active connection. Returns the write error so the
// caller can decide between log-and-reconnect (transport error) vs
// log-only (write succeeded; await the pong).
func (s *socketSubscription) sendPing() error {
	conn := s.currentConn()
	if conn == nil {
		return errors.New("slack: socket mode: ping: no active conn")
	}
	body, err := json.Marshal(pingPayload{Type: envelopeTypePing, ID: nextPingID()})
	if err != nil {
		return err
	}
	pingCtx, cancel := context.WithTimeout(s.runCtx, defaultAckTimeout)
	defer cancel()
	return conn.Write(pingCtx, websocket.MessageText, body)
}

// dispatch decodes a non-hello, non-disconnect envelope and calls the
// handler when the envelope contains a Slack `message` event. Other
// payload kinds (slash_commands, interactive, non-message events) are
// dropped — M4.2.c.1 wires the message-shaped path only.
//
// Handler errors are logged but not redelivered (per the
// [messenger.MessageHandler] contract).
func (s *socketSubscription) dispatch(env rawEnvelope, handler messenger.MessageHandler) {
	msg, ok := decodeIncoming(env)
	if !ok {
		s.client.cfg.logger.Log(
			s.runCtx, "slack: socket mode: envelope dropped",
			"type", env.Type,
		)
		return
	}
	if err := handler(s.runCtx, msg); err != nil {
		s.client.cfg.logger.Log(
			s.runCtx, "slack: socket mode: handler error",
			"err_type", fmt.Sprintf("%T", err),
		)
	}
}

// socketBackoffFor computes the delay for retry `n` (zero-based) with
// exponential growth `initial * 2^n`, clamped at maxDelay, and ±25%
// jitter sourced from randFn. Mirrors the outbox / keepclient
// resilient-stream model. randFn must return a value in [0, 1); the
// realised delay is therefore in `[base * 0.75, base * 1.25]`.
func socketBackoffFor(attempt int, initial, maxDelay time.Duration, randFn func() float64) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	base := initial
	for i := 0; i < attempt; i++ {
		next := base * 2
		if next < base || next > maxDelay {
			base = maxDelay
			break
		}
		base = next
	}
	if base > maxDelay {
		base = maxDelay
	}
	if randFn == nil {
		return base
	}
	span := float64(base) * reconnectJitterFraction
	offset := (randFn()*2 - 1) * span
	jittered := time.Duration(float64(base) + offset)
	if jittered < 0 {
		jittered = 0
	}
	return jittered
}

// sleeper is the seam the reconnect loop uses to wait between
// attempts. Production code uses [realSleeper] (which honours ctx
// cancellation); unit tests inject a fake that records the requested
// durations and returns immediately. Mirrors the outbox / keepclient
// shape so the test seam is familiar across packages.
type sleeper interface {
	Sleep(ctx context.Context, d time.Duration) error
}

// realSleeper is the production sleeper. It selects on ctx.Done() and
// a time.After(d) channel so a cancelled context unblocks the sleep
// promptly.
type realSleeper struct{}

// Sleep blocks until d elapses or ctx is cancelled. A cancelled ctx
// returns ctx.Err() immediately.
func (realSleeper) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// pingPayload is the wire shape for an application-layer ping frame
// the client sends to keep the WSS alive. Slack documents the type
// as `ping`; the server replies with `{"type":"pong","id":"<id>"}`.
type pingPayload struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// pingIDCounter is a monotonic counter feeding [nextPingID]. The
// resulting id is informational — the read loop does not currently
// match request/response — and only varies across pings so log
// entries can be correlated.
var (
	pingIDMu      sync.Mutex
	pingIDCounter uint64
)

// nextPingID returns a per-process increasing id for ping frames.
func nextPingID() string {
	pingIDMu.Lock()
	pingIDCounter++
	id := pingIDCounter
	pingIDMu.Unlock()
	return fmt.Sprintf("p%d", id)
}
