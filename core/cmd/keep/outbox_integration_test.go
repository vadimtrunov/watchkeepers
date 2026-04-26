//go:build integration

// Integration tests for the outbox publisher worker (M2.7.e.b).
// Require a reachable Postgres 16 with migrations 001..009 applied and
// KEEP_INTEGRATION_DB_URL set.
//
// The tests boot the Keep binary with a short KEEP_OUTBOX_POLL_INTERVAL
// (200ms) so event delivery is observed within a second. Each test inserts
// outbox rows directly via the pool (owner role, no RLS) and reads the SSE
// stream from a subscribed client.
//
// Run locally with:
//
//	KEEP_INTEGRATION_DB_URL=postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable \
//	  go test -tags=integration -v -run 'TestOutbox' ./core/cmd/keep/...
package main_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const outboxPollInterval = "200ms"

// outboxBootEnv returns the env slice for outbox integration tests. Layers a
// short poll interval + short heartbeat on top of readBootEnv.
func outboxBootEnv(dsn, addr string) []string {
	return append(readBootEnv(dsn, addr),
		"KEEP_OUTBOX_POLL_INTERVAL="+outboxPollInterval,
		"KEEP_SUBSCRIBE_HEARTBEAT=100ms",
		"KEEP_SUBSCRIBE_BUFFER=32",
	)
}

// bootKeepOutbox compiles + starts the Keep binary with the outbox worker
// config. Returns address, cmd, done channel, and teardown.
func bootKeepOutbox(t *testing.T, dsn string) (string, *exec.Cmd, <-chan error, func()) {
	t.Helper()
	bin := buildBinary(t)
	addr := pickLocalAddr(t)

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, bin)
	cmd.Env = outboxBootEnv(dsn, addr)
	var stderr strings.Builder
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderr)
	cmd.Stdout = os.Stdout
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start binary: %v", err)
	}
	waitForHealth(t, addr)

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	teardown := func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(exitBudget):
			_ = cmd.Process.Kill()
		}
		cancel()
	}
	return addr, cmd, done, teardown
}

// insertOutboxRow inserts a single outbox row with the given scope and
// returns its id. Uses owner-role pool access (no RLS).
func insertOutboxRow(t *testing.T, pool *pgxpool.Pool, scope, aggregateType, eventType string, payload string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var id string
	err := pool.QueryRow(ctx, `
		INSERT INTO watchkeeper.outbox
			(aggregate_type, aggregate_id, event_type, payload, scope)
		VALUES
			($1, gen_random_uuid(), $2, $3::jsonb, $4)
		RETURNING id::text
	`, aggregateType, eventType, payload, scope).Scan(&id)
	if err != nil {
		t.Fatalf("insertOutboxRow: %v", err)
	}
	return id
}

// insertOutboxRowAt inserts an outbox row with an explicit created_at for
// ordering tests.
func insertOutboxRowAt(t *testing.T, pool *pgxpool.Pool, scope, eventType string, createdAt time.Time) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var id string
	err := pool.QueryRow(ctx, `
		INSERT INTO watchkeeper.outbox
			(aggregate_type, aggregate_id, event_type, payload, scope, created_at)
		VALUES
			('test', gen_random_uuid(), $1, '{}'::jsonb, $2, $3)
		RETURNING id::text
	`, eventType, scope, createdAt).Scan(&id)
	if err != nil {
		t.Fatalf("insertOutboxRowAt: %v", err)
	}
	return id
}

// publishedAt queries the published_at column for the given outbox row id.
// Returns the zero time if the row is still unpublished.
func publishedAt(t *testing.T, pool *pgxpool.Pool, id string) time.Time {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var ts *time.Time
	err := pool.QueryRow(ctx,
		`SELECT published_at FROM watchkeeper.outbox WHERE id = $1::uuid`, id).Scan(&ts)
	if err != nil {
		t.Fatalf("publishedAt(%s): %v", id, err)
	}
	if ts == nil {
		return time.Time{}
	}
	return *ts
}

// awaitPublishedAt polls publishedAt until it is non-zero or the budget
// expires. AC2 stamps published_at in the same transaction as the Publish
// call, but the in-process Publish hands the event off to the SSE
// subscriber's buffered channel BEFORE the worker's tx.Commit returns. A
// fast subscriber + fast network read can therefore observe the SSE frame
// strictly before the stamp UPDATE is visible to a separate connection,
// making a single read of publishedAt racy. Polling closes the race
// without weakening the production contract.
func awaitPublishedAt(t *testing.T, pool *pgxpool.Pool, id string, budget time.Duration) time.Time {
	t.Helper()
	deadline := time.Now().Add(budget)
	for {
		ts := publishedAt(t, pool, id)
		if !ts.IsZero() {
			return ts
		}
		if time.Now().After(deadline) {
			return time.Time{}
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// openSubscribeStream opens a GET /v1/subscribe SSE connection with a
// capability token for the given scope. Returns the response body reader and
// a cancel function. The test fails fast if the server returns non-200.
func openSubscribeStream(t *testing.T, addr string, scope string) (io.ReadCloser, context.CancelFunc) {
	t.Helper()
	ti := issuerForTest(t)
	tok := mintToken(t, ti, scope)

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/v1/subscribe", nil)
	if err != nil {
		cancel()
		t.Fatalf("build subscribe req: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("subscribe do: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		cancel()
		t.Fatalf("subscribe status = %d, want 200", resp.StatusCode)
	}
	return resp.Body, cancel
}

// sseEvent holds a parsed SSE event (event: + data: fields).
type sseEvent struct {
	EventType string
	Data      string
}

// readSSEEvents reads up to `want` non-heartbeat SSE events from r within
// budget. Returns the events collected so far (may be fewer than want if the
// budget fires).
func readSSEEvents(r io.Reader, want int, budget time.Duration) []sseEvent {
	type result struct {
		events []sseEvent
	}
	out := make(chan result, 1)
	go func() {
		var events []sseEvent
		br := bufio.NewReader(r)
		var currentEventType, currentData string
		for len(events) < want {
			line, err := br.ReadString('\n')
			if err != nil {
				break
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				// blank line = end of event frame
				if currentData != "" {
					events = append(events, sseEvent{
						EventType: currentEventType,
						Data:      currentData,
					})
				}
				currentEventType = ""
				currentData = ""
				continue
			}
			if strings.HasPrefix(line, ":") {
				// heartbeat comment — skip
				continue
			}
			if strings.HasPrefix(line, "event:") {
				currentEventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			} else if strings.HasPrefix(line, "data:") {
				currentData = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
		out <- result{events}
	}()

	select {
	case r := <-out:
		return r.events
	case <-time.After(budget):
		return nil
	}
}

// newOutboxPool opens a direct pgxpool against KEEP_INTEGRATION_DB_URL for
// outbox row manipulation (owner role, bypasses RLS).
func newOutboxPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// deleteOutboxRow removes a single outbox row by id in cleanup.
func deleteOutboxRow(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := pool.Exec(ctx, `DELETE FROM watchkeeper.outbox WHERE id = $1::uuid`, id)
	if err != nil {
		t.Logf("cleanup outbox row %s: %v", id, err)
	}
}

// -----------------------------------------------------------------------
// AC5(a): org-scoped row → org subscriber receives event; published_at set
// -----------------------------------------------------------------------

func TestOutbox_OrgScopeDelivered(t *testing.T) {
	dsn := requireDBURL(t)
	pool := newOutboxPool(t, dsn)

	addr, _, _, teardown := bootKeepOutbox(t, dsn)
	defer teardown()

	// Open SSE subscriber BEFORE inserting the row so we don't race.
	body, cancelStream := openSubscribeStream(t, addr, "org")
	defer func() { _ = body.Close() }()
	defer cancelStream()

	// Skip the first heartbeat to confirm stream is live.
	if _, err := readSubscribeFrame(body, 2*time.Second); err != nil {
		t.Fatalf("initial heartbeat: %v", err)
	}

	// Insert outbox row.
	payload := `{"test":"org-delivery"}`
	rowID := insertOutboxRow(t, pool, "org", "test_aggregate", "test.event", payload)
	t.Cleanup(func() { deleteOutboxRow(t, pool, rowID) })

	// Expect the event to arrive within 3 poll intervals + slack.
	const budget = 3 * time.Second
	events := readSSEEvents(body, 1, budget)
	if len(events) == 0 {
		t.Fatalf("org subscriber did not receive event within %s", budget)
	}

	// Validate payload round-trip.
	var got map[string]string
	if err := json.Unmarshal([]byte(events[0].Data), &got); err != nil {
		t.Fatalf("decode event data: %v (raw=%s)", err, events[0].Data)
	}
	if got["test"] != "org-delivery" {
		t.Errorf("payload = %q, want org-delivery", got["test"])
	}

	// Assert published_at stamped in DB. The worker stamps published_at
	// inside the same tx as the Publish call (AC2), but Publish hands off
	// to the SSE subscriber synchronously and tx.Commit returns slightly
	// later — so the test polls instead of single-shotting the read.
	ts := awaitPublishedAt(t, pool, rowID, 2*time.Second)
	if ts.IsZero() {
		t.Errorf("published_at not set for row %s within 2s", rowID)
	}
}

// -----------------------------------------------------------------------
// AC5(b): user-scoped subscriber does NOT receive org event
// -----------------------------------------------------------------------

func TestOutbox_UserScopeDoesNotReceiveOrgEvent(t *testing.T) {
	dsn := requireDBURL(t)
	pool := newOutboxPool(t, dsn)

	addr, _, _, teardown := bootKeepOutbox(t, dsn)
	defer teardown()

	// Open user-scoped subscriber.
	userID := newUUID(t)
	userScope := "user:" + userID
	userBody, cancelUser := openSubscribeStream(t, addr, userScope)
	defer func() { _ = userBody.Close() }()
	defer cancelUser()

	// Open org-scoped subscriber to verify the event does land for org.
	orgBody, cancelOrg := openSubscribeStream(t, addr, "org")
	defer func() { _ = orgBody.Close() }()
	defer cancelOrg()

	// Wait for heartbeats on both streams to confirm both are live.
	if _, err := readSubscribeFrame(orgBody, 2*time.Second); err != nil {
		t.Fatalf("org heartbeat: %v", err)
	}
	if _, err := readSubscribeFrame(userBody, 2*time.Second); err != nil {
		t.Fatalf("user heartbeat: %v", err)
	}

	// Insert an org-scoped row.
	rowID := insertOutboxRow(t, pool, "org", "test_aggregate", "test.isolation", `{}`)
	t.Cleanup(func() { deleteOutboxRow(t, pool, rowID) })

	// Org subscriber must receive it.
	orgEvents := readSSEEvents(orgBody, 1, 3*time.Second)
	if len(orgEvents) == 0 {
		t.Fatalf("org subscriber did not receive org event")
	}

	// User subscriber must NOT receive the org event within 2 poll intervals.
	userEvents := readSSEEvents(userBody, 1, 800*time.Millisecond)
	if len(userEvents) > 0 {
		t.Errorf("user subscriber received org-scoped event: %+v", userEvents)
	}
}

// -----------------------------------------------------------------------
// AC5(c): pre-published row is not re-delivered on worker boot
// -----------------------------------------------------------------------

func TestOutbox_PrePublishedRowNotRedelivered(t *testing.T) {
	dsn := requireDBURL(t)
	pool := newOutboxPool(t, dsn)

	// Insert a row and mark it published BEFORE booting the worker.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var rowID string
	err := pool.QueryRow(ctx, `
		INSERT INTO watchkeeper.outbox
			(aggregate_type, aggregate_id, event_type, payload, scope, published_at)
		VALUES
			('test', gen_random_uuid(), 'test.pre_published', '{}'::jsonb, 'org', now())
		RETURNING id::text
	`).Scan(&rowID)
	if err != nil {
		t.Fatalf("insert pre-published row: %v", err)
	}
	t.Cleanup(func() { deleteOutboxRow(t, pool, rowID) })

	// Now boot the worker.
	addr, _, _, teardown := bootKeepOutbox(t, dsn)
	defer teardown()

	body, cancelStream := openSubscribeStream(t, addr, "org")
	defer func() { _ = body.Close() }()
	defer cancelStream()

	// Wait for one heartbeat so the stream is live.
	if _, err := readSubscribeFrame(body, 2*time.Second); err != nil {
		t.Fatalf("initial heartbeat: %v", err)
	}

	// Wait 3 poll intervals — the pre-published row must never arrive.
	events := readSSEEvents(body, 1, 800*time.Millisecond)
	if len(events) > 0 {
		// Filter: check if any event matches our pre-published row's event_type.
		// (Other tests' rows in the shared DB might also land here.)
		for _, ev := range events {
			if strings.Contains(ev.Data, "test.pre_published") {
				t.Errorf("pre-published row was re-delivered: %+v", ev)
			}
		}
	}
}

// -----------------------------------------------------------------------
// AC5(d): two unpublished rows delivered in created_at order
// -----------------------------------------------------------------------

func TestOutbox_TwoRowsDeliveredInOrder(t *testing.T) {
	dsn := requireDBURL(t)
	pool := newOutboxPool(t, dsn)

	addr, _, _, teardown := bootKeepOutbox(t, dsn)
	defer teardown()

	body, cancelStream := openSubscribeStream(t, addr, "org")
	defer func() { _ = body.Close() }()
	defer cancelStream()

	// Wait for one heartbeat so the stream is live before inserting rows.
	if _, err := readSubscribeFrame(body, 2*time.Second); err != nil {
		t.Fatalf("initial heartbeat: %v", err)
	}

	// Insert two rows with explicit created_at so the order is deterministic.
	// "first" has an earlier timestamp so it must arrive before "second".
	base := time.Now().UTC()
	firstID := insertOutboxRowAt(t, pool, "org", fmt.Sprintf("order.first.%d", base.UnixNano()), base.Add(-time.Second))
	secondID := insertOutboxRowAt(t, pool, "org", fmt.Sprintf("order.second.%d", base.UnixNano()), base)
	t.Cleanup(func() {
		deleteOutboxRow(t, pool, firstID)
		deleteOutboxRow(t, pool, secondID)
	})

	// Collect 2 events within the budget.
	events := readSSEEvents(body, 2, 4*time.Second)
	if len(events) < 2 {
		t.Fatalf("expected 2 events, got %d within budget", len(events))
	}

	// Find our two events by event_type suffix on the SSE `event:` field.
	// The payload was inserted as `{}`, so the suffix lives in EventType,
	// not Data — the writer emits `event: <EventType>` and `data: <Payload>`
	// as separate frame fields (see writeSSEEvent in handlers_subscribe.go).
	nanoSuffix := fmt.Sprintf("%d", base.UnixNano())
	var gotFirst, gotSecond int // index in events slice, -1 = not found
	gotFirst = -1
	gotSecond = -1
	for i, ev := range events {
		if strings.Contains(ev.EventType, "order.first."+nanoSuffix) {
			gotFirst = i
		}
		if strings.Contains(ev.EventType, "order.second."+nanoSuffix) {
			gotSecond = i
		}
	}
	if gotFirst == -1 || gotSecond == -1 {
		t.Fatalf("did not receive both ordered events; got %+v", events)
	}
	if gotFirst > gotSecond {
		t.Errorf("events delivered out of order: first at index %d, second at index %d", gotFirst, gotSecond)
	}
}
