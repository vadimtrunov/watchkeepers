package publish_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/internal/keep/publish"
	"github.com/vadimtrunov/watchkeepers/core/pkg/wkmetrics"
)

// stubPublisher records every event passed to Publish and always returns nil.
type stubPublisher struct {
	published []publish.Event
}

func (s *stubPublisher) Publish(_ context.Context, ev publish.Event) error {
	s.published = append(s.published, ev)
	return nil
}

// TestRowToEvent verifies that Worker converts an outbox row into a publish.Event
// with every field correctly mapped, including Scope forwarding.
func TestRowToEvent(t *testing.T) {
	id := uuid.New()
	aggregateID := uuid.New()
	payload := json.RawMessage(`{"hello":"world"}`)
	createdAt := time.Now().UTC().Truncate(time.Second)

	row := publish.OutboxRow{
		ID:            id,
		AggregateType: "watchkeeper",
		AggregateID:   aggregateID,
		EventType:     "watchkeeper.spawned",
		Payload:       payload,
		Scope:         "org",
		CreatedAt:     createdAt,
	}

	ev := publish.RowToEvent(row)

	if ev.ID != id {
		t.Errorf("ID = %s, want %s", ev.ID, id)
	}
	if ev.Scope != "org" {
		t.Errorf("Scope = %q, want org", ev.Scope)
	}
	if ev.AggregateType != "watchkeeper" {
		t.Errorf("AggregateType = %q, want watchkeeper", ev.AggregateType)
	}
	if ev.AggregateID != aggregateID {
		t.Errorf("AggregateID = %s, want %s", ev.AggregateID, aggregateID)
	}
	if ev.EventType != "watchkeeper.spawned" {
		t.Errorf("EventType = %q, want watchkeeper.spawned", ev.EventType)
	}
	if string(ev.Payload) != string(payload) {
		t.Errorf("Payload = %s, want %s", ev.Payload, payload)
	}
	if !ev.CreatedAt.Equal(createdAt) {
		t.Errorf("CreatedAt = %v, want %v", ev.CreatedAt, createdAt)
	}
}

// TestWorkerCancelStops verifies that Run returns when ctx is cancelled with
// an empty outbox (idle-worker cancel path).
func TestWorkerCancelStops(t *testing.T) {
	// Use a nil pool — the worker must stop before trying to query when ctx
	// is cancelled during the first select on the ticker.
	pub := &stubPublisher{}
	cfg := publish.WorkerConfig{PollInterval: 10 * time.Second}
	w := publish.NewWorkerWithPublisher(nil, pub, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	select {
	case err := <-done:
		if err != nil && err != context.Canceled {
			t.Errorf("Run returned %v, want nil or context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s after context cancel")
	}
}

// TestWorker_WithObservability_NilSurfacesAreNoOp asserts the M10.1
// contract: a worker built via NewWorker/NewWorkerWithPublisher and
// NEVER given a logger or metrics must not panic on any code path.
// Production main.go always wires both, but tests and ad-hoc callers
// should be free to skip the call without surprise.
func TestWorker_WithObservability_NilSurfacesAreNoOp(t *testing.T) {
	pub := &stubPublisher{}
	cfg := publish.WorkerConfig{PollInterval: 10 * time.Second}
	w := publish.NewWorkerWithPublisher(nil, pub, cfg)

	// Both nil — explicitly. Mirrors a hypothetical caller that wants
	// to clear an earlier WithObservability binding.
	w.WithObservability(nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.Run(ctx); err != nil && err != context.Canceled {
		t.Errorf("Run with nil observability surfaces: %v", err)
	}
}

// TestWorker_WithObservability_LoggerAndMetricsAreReused asserts that
// repeated WithObservability calls swap previous bindings without
// leaking state — important because production main.go calls it
// exactly once but tests may chain it across cases.
func TestWorker_WithObservability_LoggerAndMetricsAreReused(t *testing.T) {
	pub := &stubPublisher{}
	cfg := publish.WorkerConfig{PollInterval: 10 * time.Second}
	w := publish.NewWorkerWithPublisher(nil, pub, cfg)

	var buf1, buf2 bytes.Buffer
	m1 := wkmetrics.New()
	m2 := wkmetrics.New()

	w.WithObservability(slog.New(slog.NewJSONHandler(&buf1, nil)), m1)
	w.WithObservability(slog.New(slog.NewJSONHandler(&buf2, nil)), m2)

	// We cannot directly observe the swap without reaching into
	// unexported fields, but we can prove Run still terminates cleanly
	// after the swap — the regression we are guarding against is a
	// panic on cleared / reused fields.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.Run(ctx); err != nil && err != context.Canceled {
		t.Errorf("Run after WithObservability swap: %v", err)
	}
}
