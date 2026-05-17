package peer_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vadimtrunov/watchkeepers/core/pkg/peer"
)

// fakeQuerier is a hand-rolled pgx.Querier fake that records every
// Exec call and returns a configurable error. Sufficient for the
// Publish-side tests of [PostgresEventBus] — the Subscribe-side tests
// inject [fakeListenerConn] via [fakePoolAcquirer].
type fakeQuerier struct {
	mu        sync.Mutex
	execCalls []fakeExecCall
	execErr   error
}

type fakeExecCall struct {
	sql  string
	args []any
}

func (f *fakeQuerier) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return nil, nil
}

func (f *fakeQuerier) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	return nil
}

func (f *fakeQuerier) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execCalls = append(f.execCalls, fakeExecCall{sql: sql, args: args})
	if f.execErr != nil {
		return pgconn.CommandTag{}, f.execErr
	}
	return pgconn.CommandTag{}, nil
}

func TestNewPostgresEventBus_PanicsOnNilDeps(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Error("NewPostgresEventBus(nil, ...) did not panic")
		}
	}()
	peer.NewPostgresEventBus(nil, nil)
}

func TestPostgresEventBus_Publish_HappyPath_RunsInsert(t *testing.T) {
	t.Parallel()

	q := &fakeQuerier{}
	acq := &fakePoolAcquirer{}
	bus := peer.NewPostgresEventBus(q, acq)

	orgID := uuid.New()
	ev := peer.Event{
		ID:             uuid.New(),
		OrganizationID: orgID,
		WatchkeeperID:  "wk-asker",
		EventType:      "k2k_message_sent",
		Payload:        []byte(`{"a":1}`),
	}
	if err := bus.Publish(context.Background(), ev); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.execCalls) != 1 {
		t.Fatalf("Exec calls = %d, want 1", len(q.execCalls))
	}
	call := q.execCalls[0]
	if !strings.Contains(call.sql, "INSERT INTO watchkeeper.peer_events") {
		t.Errorf("Exec SQL = %q, want INSERT INTO watchkeeper.peer_events", call.sql)
	}
	if len(call.args) != 6 {
		t.Fatalf("Exec args len = %d, want 6", len(call.args))
	}
	if call.args[0] != ev.ID {
		t.Errorf("Exec args[0] = %v, want %v", call.args[0], ev.ID)
	}
	if call.args[1] != ev.OrganizationID {
		t.Errorf("Exec args[1] = %v, want %v", call.args[1], ev.OrganizationID)
	}
	if call.args[2] != ev.WatchkeeperID {
		t.Errorf("Exec args[2] = %v, want %v", call.args[2], ev.WatchkeeperID)
	}
	if call.args[3] != ev.EventType {
		t.Errorf("Exec args[3] = %v, want %v", call.args[3], ev.EventType)
	}
	// Payload is passed as a string with an explicit ::jsonb cast in the
	// SQL so pgx encodes it as a Postgres jsonb literal rather than as a
	// bytea blob. Mirror the M9.4.a handlers_write.handleLogAppend
	// discipline.
	if !strings.Contains(call.sql, "::jsonb") {
		t.Errorf("Exec SQL = %q, want explicit ::jsonb cast", call.sql)
	}
	payloadArg, ok := call.args[4].(string)
	if !ok {
		t.Errorf("Exec args[4] type = %T, want string (for ::jsonb cast)", call.args[4])
	}
	if payloadArg != `{"a":1}` {
		t.Errorf("Exec args[4] = %q, want %q", payloadArg, `{"a":1}`)
	}
}

func TestPostgresEventBus_Publish_EmptyPayloadCoercesToEmptyObject(t *testing.T) {
	t.Parallel()

	q := &fakeQuerier{}
	acq := &fakePoolAcquirer{}
	bus := peer.NewPostgresEventBus(q, acq)

	orgID := uuid.New()
	ev := peer.Event{
		ID:             uuid.New(),
		OrganizationID: orgID,
		WatchkeeperID:  "wk",
		EventType:      "evt",
		Payload:        nil, // omit body
	}
	if err := bus.Publish(context.Background(), ev); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.execCalls) != 1 {
		t.Fatalf("Exec calls = %d, want 1", len(q.execCalls))
	}
	if got, _ := q.execCalls[0].args[4].(string); got != "{}" {
		t.Errorf("payload arg = %q, want {} (empty/nil coerces to empty JSON object)", got)
	}
}

func TestPostgresEventBus_PublishValidationFailures(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	q := &fakeQuerier{}
	acq := &fakePoolAcquirer{}
	bus := peer.NewPostgresEventBus(q, acq)
	mk := func() peer.Event {
		return peer.Event{
			ID:             uuid.New(),
			OrganizationID: orgID,
			WatchkeeperID:  "wk",
			EventType:      "evt",
		}
	}
	cases := []struct {
		name   string
		mutate func(e *peer.Event)
		want   error
	}{
		{name: "zero id", mutate: func(e *peer.Event) { e.ID = uuid.Nil }, want: peer.ErrInvalidEventID},
		{name: "zero org", mutate: func(e *peer.Event) { e.OrganizationID = uuid.Nil }, want: peer.ErrInvalidOrganizationID},
		{name: "empty wk", mutate: func(e *peer.Event) { e.WatchkeeperID = " " }, want: peer.ErrEmptyWatchkeeperID},
		{name: "empty event type", mutate: func(e *peer.Event) { e.EventType = " " }, want: peer.ErrEmptyEventType},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := mk()
			tc.mutate(&e)
			if err := bus.Publish(context.Background(), e); err != tc.want {
				t.Errorf("Publish err = %v, want %v", err, tc.want)
			}
		})
	}
	// All four validation failures must short-circuit BEFORE the SQL.
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.execCalls) != 0 {
		t.Errorf("Exec calls = %d on validation failure, want 0 (fail-fast precedes SQL)", len(q.execCalls))
	}
}

func TestPostgresEventBus_SubscribeValidatesOrgID(t *testing.T) {
	t.Parallel()
	q := &fakeQuerier{}
	acq := &fakePoolAcquirer{}
	bus := peer.NewPostgresEventBus(q, acq)
	_, _, err := bus.Subscribe(context.Background(), peer.SubscribeFilter{})
	if err != peer.ErrInvalidOrganizationID {
		t.Errorf("Subscribe err = %v, want %v", err, peer.ErrInvalidOrganizationID)
	}
}

func TestPostgresEventBus_SubscribeAcquiresAndIssuesLISTEN(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	q := &fakeQuerier{}
	conn := &fakeListenerConn{notifyCh: make(chan peer.Notification, 1)}
	acq := &fakePoolAcquirer{conn: conn}
	bus := peer.NewPostgresEventBus(q, acq)

	ctx, cancel := context.WithCancel(context.Background())
	_, cancelSub, err := bus.Subscribe(ctx, peer.SubscribeFilter{OrganizationID: orgID})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()
	defer cancelSub()

	// Give the goroutine a moment to issue LISTEN.
	time.Sleep(20 * time.Millisecond)

	conn.mu.Lock()
	gotExecs := append([]string(nil), conn.execSQLs...)
	conn.mu.Unlock()
	found := false
	for _, s := range gotExecs {
		if strings.Contains(s, "LISTEN peer_events") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("conn.Exec calls = %v, want at least one LISTEN peer_events", gotExecs)
	}
}

func TestPostgresEventBus_CtxCancelClosesChannelAndReleasesConn(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	q := &fakeQuerier{}
	conn := &fakeListenerConn{notifyCh: make(chan peer.Notification, 1)}
	acq := &fakePoolAcquirer{conn: conn}
	bus := peer.NewPostgresEventBus(q, acq)

	ctx, cancel := context.WithCancel(context.Background())
	ch, _, err := bus.Subscribe(ctx, peer.SubscribeFilter{OrganizationID: orgID})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	cancel()

	select {
	case _, ok := <-ch:
		if ok {
			t.Error("channel returned a value after ctx cancel; want closed")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for channel close after ctx cancel")
	}

	// Connection must be released.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		conn.mu.Lock()
		released := conn.releaseCount
		conn.mu.Unlock()
		if released >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	conn.mu.Lock()
	defer conn.mu.Unlock()
	if conn.releaseCount != 1 {
		t.Errorf("conn.Release calls = %d, want 1 (subscription teardown released the conn)", conn.releaseCount)
	}
}

// TestPostgresEventBus_DroppedEvents_StartsZero pins the initial state
// of the drop counter.
func TestPostgresEventBus_DroppedEventsStartsZero(t *testing.T) {
	t.Parallel()
	bus := peer.NewPostgresEventBus(&fakeQuerier{}, &fakePoolAcquirer{})
	if got := bus.DroppedEvents(); got != 0 {
		t.Errorf("DroppedEvents = %d, want 0", got)
	}
}

// ============================================================
// shared fakes for Subscribe-side tests
// ============================================================

type fakePoolAcquirer struct {
	conn       *fakeListenerConn
	acquireErr error
}

func (f *fakePoolAcquirer) Acquire(_ context.Context) (peer.ListenerConn, error) {
	if f.acquireErr != nil {
		return nil, f.acquireErr
	}
	if f.conn == nil {
		f.conn = &fakeListenerConn{notifyCh: make(chan peer.Notification, 1)}
	}
	return f.conn, nil
}

type fakeListenerConn struct {
	mu           sync.Mutex
	execSQLs     []string
	execErr      error
	notifyCh     chan peer.Notification
	notifyErr    error
	releaseCount int
}

func (f *fakeListenerConn) Exec(_ context.Context, sql string, _ ...any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execSQLs = append(f.execSQLs, sql)
	return f.execErr
}

func (f *fakeListenerConn) WaitForNotification(ctx context.Context) (peer.Notification, error) {
	f.mu.Lock()
	err := f.notifyErr
	ch := f.notifyCh
	f.mu.Unlock()
	if err != nil {
		return peer.Notification{}, err
	}
	select {
	case n, ok := <-ch:
		if !ok {
			return peer.Notification{}, context.Canceled
		}
		return n, nil
	case <-ctx.Done():
		return peer.Notification{}, ctx.Err()
	}
}

func (f *fakeListenerConn) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	// Return an empty result so the drainer's row loop is a no-op.
	return &emptyRows{}, nil
}

func (f *fakeListenerConn) Release() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releaseCount++
}

type emptyRows struct{}

func (r *emptyRows) Close()                                       {}
func (r *emptyRows) Err() error                                   { return nil }
func (r *emptyRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *emptyRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *emptyRows) Next() bool                                   { return false }
func (r *emptyRows) Scan(_ ...any) error                          { return nil }
func (r *emptyRows) Values() ([]any, error)                       { return nil, nil }
func (r *emptyRows) RawValues() [][]byte                          { return nil }
func (r *emptyRows) Conn() *pgx.Conn                              { return nil }
