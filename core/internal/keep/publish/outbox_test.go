package publish_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/internal/keep/publish"
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
