package main

// run_logs.go — `wk logs <wk-id>` and `wk tail-keepers-log`.
//
// `wk logs` fetches GET /v1/keepers-log via keepclient and filters
// client-side to events whose ActorWatchkeeperID matches the requested
// wk-id. The server has no per-watchkeeper filter as of M10.2; a
// follow-up sub-item (M10.2.d) can add one when the keepers-log volume
// makes a 1000-row client-side filter inefficient. The CLI's default
// `--limit 1000` keeps that horizon distant for the dev workspace.
//
// `wk tail-keepers-log` uses keepclient.Subscribe and prints each event
// frame's payload to stdout, one frame per line. Heartbeat frames are
// silently dropped by the keepclient scanner. The stream stays open
// until ctx is cancelled (SIGINT/SIGTERM, plumbed via main.go's
// signal.NotifyContext).
//
// Iter-1 critic fixes:
//
//   - M5: tail-keepers-log now writes through a `bufio.Writer` and
//     loops on short writes via `bufio.Writer.Write`. A transient
//     `EAGAIN` on the terminal (e.g. operator pressed Ctrl-S) no
//     longer kills the stream — the buffered write retries the
//     remainder transparently. Flushed once per frame so an
//     interactive operator still sees lines arrive in real time.
//
//   - M6: `wk logs <wk-id>` now pre-flights the positional through a
//     canonical UUID gate and rejects negative `--limit` client-side
//     (mirror the keepclient's own bounds so the operator gets a
//     usage error instead of a runtime `ErrInvalidRequest`).

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
)

// wkIDPattern matches the canonical RFC 4122 text form (8-4-4-4-12 hex
// with hyphens, any version/variant). Mirrors the keepclient's
// `uuidPattern` so the CLI rejects obvious typos before any HTTP
// round-trip. Iter-1 critic M6: without this gate a malformed
// positional silently produced "no matching events" on the client
// filter side, indistinguishable from a real empty result.
//
//nolint:gochecknoglobals // intentional package-scoped precompiled regex.
var wkIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func runLogs(ctx context.Context, args []string, stdout, stderr io.Writer, env envLookup) int {
	fs := flag.NewFlagSet("wk logs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var limit int
	fs.IntVar(&limit, "limit", 1000, "number of recent rows to scan before client-side filter (positive; keepclient + server clamp the upper bound)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	rest := fs.Args()
	if len(rest) == 0 {
		stderrf(stderr, "wk: logs: missing positional <wk-id>\n")
		return 2
	}
	if len(rest) > 1 {
		stderrf(stderr, fmt.Sprintf("wk: logs: extra positional args after <wk-id>: %v\n", rest[1:]))
		return 2
	}
	wkID := strings.TrimSpace(rest[0])
	if wkID == "" {
		stderrf(stderr, "wk: logs: <wk-id> must be non-empty\n")
		return 2
	}
	// Iter-1 critic M6: reject malformed UUIDs client-side. The
	// keepclient accepts any string and the server returns all events
	// regardless; the CLI's client-side filter would silently produce
	// zero matches.
	if !wkIDPattern.MatchString(wkID) {
		stderrf(stderr, "wk: logs: <wk-id> must be a canonical UUID (8-4-4-4-12 hex with hyphens)\n")
		return 2
	}
	// Iter-1 critic M6: surface a negative limit as exit 2 with a
	// usage diagnostic rather than the keepclient's terse
	// "invalid request" (exit 1).
	if limit < 0 {
		stderrf(stderr, "wk: logs: --limit must be non-negative\n")
		return 2
	}
	return withKeepClient(env, stderr, "logs", func(c *keepclient.Client) int {
		resp, err := c.LogTail(ctx, keepclient.LogTailOptions{Limit: limit})
		if err != nil {
			stderrf(stderr, fmt.Sprintf("wk: logs: %v\n", err))
			return 1
		}
		for _, ev := range resp.Events {
			if ev.ActorWatchkeeperID == nil || *ev.ActorWatchkeeperID != wkID {
				continue
			}
			fmt.Fprintf(stdout, "%s\t%s\t%s\n", ev.CreatedAt, ev.EventType, string(ev.Payload))
		}
		return 0
	})
}

func runTailKeepersLog(ctx context.Context, args []string, stdout, stderr io.Writer, env envLookup) int {
	fs := flag.NewFlagSet("wk tail-keepers-log", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	return withKeepClient(env, stderr, "tail-keepers-log", func(c *keepclient.Client) int {
		stream, err := c.Subscribe(ctx)
		if err != nil {
			stderrf(stderr, fmt.Sprintf("wk: tail-keepers-log: %v\n", err))
			return 1
		}
		defer func() { _ = stream.Close() }()

		// Iter-1 critic M5: wrap stdout in a bufio.Writer so each
		// frame's write loops on short writes internally (the wrapped
		// writer retries the remainder transparently); flush per
		// frame so the operator still sees lines arrive
		// interactively. A transient EAGAIN on the operator's
		// terminal (Ctrl-S) no longer kills the stream.
		bw := bufio.NewWriter(stdout)
		defer func() { _ = bw.Flush() }()

		for {
			ev, err := stream.Next(ctx)
			if err != nil {
				if errors.Is(err, io.EOF) {
					return 0
				}
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return 0
				}
				if errors.Is(err, keepclient.ErrStreamClosed) {
					return 0
				}
				stderrf(stderr, fmt.Sprintf("wk: tail-keepers-log: %v\n", err))
				return 1
			}
			line := struct {
				ID        string          `json:"id,omitempty"`
				EventType string          `json:"event_type,omitempty"`
				Payload   json.RawMessage `json:"payload,omitempty"`
			}{
				ID:        ev.ID,
				EventType: ev.EventType,
				Payload:   ev.Payload,
			}
			data, mErr := json.Marshal(line)
			if mErr != nil {
				stderrf(stderr, fmt.Sprintf("wk: tail-keepers-log: marshal: %v\n", mErr))
				return 1
			}
			if _, wErr := bw.Write(data); wErr != nil {
				stderrf(stderr, fmt.Sprintf("wk: tail-keepers-log: write: %v\n", wErr))
				return 1
			}
			if wErr := bw.WriteByte('\n'); wErr != nil {
				stderrf(stderr, fmt.Sprintf("wk: tail-keepers-log: write: %v\n", wErr))
				return 1
			}
			if fErr := bw.Flush(); fErr != nil {
				stderrf(stderr, fmt.Sprintf("wk: tail-keepers-log: flush: %v\n", fErr))
				return 1
			}
		}
	})
}
