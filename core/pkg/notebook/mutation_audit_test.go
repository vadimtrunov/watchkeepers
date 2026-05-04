package notebook

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
)

// auditAgentID is the canonical UUID stamped onto the M2b.7 mutation-
// audit tests. Distinct from `retireAgentID` so a future package-level
// search-and-replace cannot conflate the two test surfaces.
const auditAgentID = "dddddddd-dddd-dddd-dddd-dddddddddddd"

// freshDBWithLogger returns a *DB whose `agentID` matches [auditAgentID]
// and whose `logger` field is the supplied [Logger]. Mirrors [freshDB]
// (test-friendly seam over `openAt`) but pre-populates the two fields
// the M2b.7 audit-emit code path reads. A nil `logger` argument leaves
// `db.logger` nil — exercises the backward-compat no-op path.
func freshDBWithLogger(t *testing.T, logger Logger) (*DB, context.Context) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit.sqlite")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var opts []DBOption
	if logger != nil {
		opts = append(opts, WithLogger(logger))
	}
	db, err := openAt(ctx, path, opts...)
	if err != nil {
		t.Fatalf("openAt: %v", err)
	}
	// `openAt` is the test seam and skips the agent-id resolver in `Open`,
	// so we stamp the field manually here so the audit payload carries a
	// realistic value.
	db.agentID = auditAgentID
	t.Cleanup(func() { _ = db.Close() })
	return db, ctx
}

// validEntry is the canonical fixture used by every audit-emit test.
// Distinct embedding seeds across tests are unnecessary because none of
// these tests exercise the recall ranking — only the audit emit shape.
func validEntry() Entry {
	return Entry{
		Category:  CategoryLesson,
		Content:   "audit-emit fixture entry",
		Embedding: makeEmbedding(31),
	}
}

// TestRemember_AuditEmitted — AC2/AC5/AC6: open with WithLogger, Remember
// once, expect exactly one `notebook_entry_remembered` event whose
// payload carries `agent_id`/`entry_id`/`category`/`created_at` AND does
// NOT carry the PII / large fields the audit log explicitly excludes.
func TestRemember_AuditEmitted(t *testing.T) {
	logger := &fakeLogger{}
	db, ctx := freshDBWithLogger(t, logger)

	id, err := db.Remember(ctx, validEntry())
	if err != nil {
		t.Fatalf("Remember: %v", err)
	}
	if id == "" {
		t.Fatal("Remember returned empty id")
	}

	if got := logger.called.Load(); got != 1 {
		t.Fatalf("logger.LogAppend called %d times, want 1", got)
	}
	if logger.received.EventType != rememberEventType {
		t.Fatalf("event_type = %q, want %q", logger.received.EventType, rememberEventType)
	}

	var raw map[string]any
	if err := json.Unmarshal(logger.received.Payload, &raw); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	for _, key := range []string{"agent_id", "entry_id", "category", "created_at"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("payload missing %q field: %v", key, raw)
		}
	}
	if raw["agent_id"] != auditAgentID {
		t.Fatalf("payload.agent_id = %v, want %q", raw["agent_id"], auditAgentID)
	}
	if raw["entry_id"] != id {
		t.Fatalf("payload.entry_id = %v, want %q", raw["entry_id"], id)
	}
	if raw["category"] != CategoryLesson {
		t.Fatalf("payload.category = %v, want %q", raw["category"], CategoryLesson)
	}
	createdAt, ok := raw["created_at"].(string)
	if !ok {
		t.Fatalf("payload.created_at = %v, want string", raw["created_at"])
	}
	if _, err := time.Parse(time.RFC3339Nano, createdAt); err != nil {
		t.Fatalf("payload.created_at = %q does not parse as RFC3339Nano: %v", createdAt, err)
	}

	// PII / large-field exclusion (AC5): every banned field must be
	// absent from the payload.
	for _, banned := range []string{
		"content", "subject", "embedding", "relevance_score",
		"evidence_log_ref", "tool_version", "superseded_by",
		"last_used_at", "active_after",
	} {
		if _, ok := raw[banned]; ok {
			t.Fatalf("payload contains banned field %q: %v", banned, raw)
		}
	}
}

// TestForget_AuditEmitted — AC3/AC5/AC6: same shape as
// TestRemember_AuditEmitted but for the `notebook_entry_forgotten` event.
// Payload carries only `agent_id`/`entry_id`/`forgotten_at`; no
// `category`, no `content`, etc.
func TestForget_AuditEmitted(t *testing.T) {
	logger := &fakeLogger{}
	db, ctx := freshDBWithLogger(t, logger)

	// Seed via Remember; that emit counts toward `called` so we reset
	// the recorded receipt before the Forget under test.
	id, err := db.Remember(ctx, validEntry())
	if err != nil {
		t.Fatalf("seed Remember: %v", err)
	}
	logger.called.Store(0)
	logger.received = keepclient.LogAppendRequest{}

	if err := db.Forget(ctx, id); err != nil {
		t.Fatalf("Forget: %v", err)
	}

	if got := logger.called.Load(); got != 1 {
		t.Fatalf("logger.LogAppend called %d times after Forget, want 1", got)
	}
	if logger.received.EventType != forgetEventType {
		t.Fatalf("event_type = %q, want %q", logger.received.EventType, forgetEventType)
	}

	var raw map[string]any
	if err := json.Unmarshal(logger.received.Payload, &raw); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	for _, key := range []string{"agent_id", "entry_id", "forgotten_at"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("payload missing %q field: %v", key, raw)
		}
	}
	if raw["agent_id"] != auditAgentID {
		t.Fatalf("payload.agent_id = %v, want %q", raw["agent_id"], auditAgentID)
	}
	if raw["entry_id"] != id {
		t.Fatalf("payload.entry_id = %v, want %q", raw["entry_id"], id)
	}
	forgottenAt, ok := raw["forgotten_at"].(string)
	if !ok {
		t.Fatalf("payload.forgotten_at = %v, want string", raw["forgotten_at"])
	}
	if _, err := time.Parse(time.RFC3339Nano, forgottenAt); err != nil {
		t.Fatalf("payload.forgotten_at = %q does not parse as RFC3339Nano: %v", forgottenAt, err)
	}

	// PII / large-field exclusion: forget payload is even stricter than
	// remember (no category either).
	for _, banned := range []string{
		"category", "content", "subject", "embedding",
		"relevance_score", "evidence_log_ref", "tool_version",
		"superseded_by", "last_used_at", "active_after", "created_at",
	} {
		if _, ok := raw[banned]; ok {
			t.Fatalf("payload contains banned field %q: %v", banned, raw)
		}
	}
}

// TestRemember_NoLogger_Backcompat — AC4/AC6: open WITHOUT WithLogger;
// Remember succeeds, no panic, no audit emit attempted (there's no
// logger to call so this is implicit, but we also confirm the row
// landed).
func TestRemember_NoLogger_Backcompat(t *testing.T) {
	db, ctx := freshDBWithLogger(t, nil)

	id, err := db.Remember(ctx, validEntry())
	if err != nil {
		t.Fatalf("Remember without logger: %v", err)
	}
	if id == "" {
		t.Fatal("Remember returned empty id")
	}

	var n int
	if err := db.sql.QueryRowContext(ctx,
		`SELECT count(*) FROM entry WHERE id = ?`, id,
	).Scan(&n); err != nil {
		t.Fatalf("count entry: %v", err)
	}
	if n != 1 {
		t.Fatalf("entry count = %d, want 1", n)
	}
}

// TestForget_NoLogger_Backcompat — AC4/AC6: open WITHOUT WithLogger;
// Forget on a freshly-Remembered id succeeds, no panic, the row is
// gone.
func TestForget_NoLogger_Backcompat(t *testing.T) {
	db, ctx := freshDBWithLogger(t, nil)

	id, err := db.Remember(ctx, validEntry())
	if err != nil {
		t.Fatalf("seed Remember: %v", err)
	}
	if err := db.Forget(ctx, id); err != nil {
		t.Fatalf("Forget without logger: %v", err)
	}

	var n int
	if err := db.sql.QueryRowContext(ctx,
		`SELECT count(*) FROM entry WHERE id = ?`, id,
	).Scan(&n); err != nil {
		t.Fatalf("count entry: %v", err)
	}
	if n != 0 {
		t.Fatalf("entry count after Forget = %d, want 0", n)
	}
}

// TestRemember_AuditEmitFails_DataInDB — AC2/AC6: fakeLogger returns an
// error; Remember returns `(id, wrapped err)` matching `errors.Is`. The
// entry IS in the DB — verify by direct SELECT (the row landed and the
// commit happened before LogAppend).
func TestRemember_AuditEmitFails_DataInDB(t *testing.T) {
	auditBoom := errors.New("audit boom (remember)")
	logger := &fakeLogger{logErr: auditBoom}
	db, ctx := freshDBWithLogger(t, logger)

	id, err := db.Remember(ctx, validEntry())
	if err == nil {
		t.Fatal("Remember returned nil error on audit-emit failure")
	}
	if id == "" {
		t.Fatal("Remember returned empty id; partial-failure contract requires the id")
	}
	if !errors.Is(err, auditBoom) {
		t.Fatalf("err = %v, want one wrapping audit boom", err)
	}
	if !strings.Contains(err.Error(), "audit emit:") {
		t.Fatalf("err = %v, want prefix 'audit emit:'", err)
	}

	// Post-failure-data-presence guard: the entry IS in the DB. A
	// downstream subscriber retrying just the audit emit can read it
	// out by id.
	var gotID string
	if err := db.sql.QueryRowContext(ctx,
		`SELECT id FROM entry WHERE id = ?`, id,
	).Scan(&gotID); err != nil {
		t.Fatalf("entry select after audit failure: %v", err)
	}
	if gotID != id {
		t.Fatalf("got id %q, want %q", gotID, id)
	}
}

// TestForget_AuditEmitFails_DataGone — AC3/AC6: fakeLogger returns an
// error; Forget returns wrapped err. The entry IS gone — verify by
// direct SELECT returning sql.ErrNoRows (the DELETE committed before
// LogAppend).
func TestForget_AuditEmitFails_DataGone(t *testing.T) {
	logger := &fakeLogger{} // happy on the seed Remember
	db, ctx := freshDBWithLogger(t, logger)

	id, err := db.Remember(ctx, validEntry())
	if err != nil {
		t.Fatalf("seed Remember: %v", err)
	}
	auditBoom := errors.New("audit boom (forget)")
	logger.logErr = auditBoom

	err = db.Forget(ctx, id)
	if err == nil {
		t.Fatal("Forget returned nil error on audit-emit failure")
	}
	if !errors.Is(err, auditBoom) {
		t.Fatalf("err = %v, want one wrapping audit boom", err)
	}
	if !strings.Contains(err.Error(), "audit emit:") {
		t.Fatalf("err = %v, want prefix 'audit emit:'", err)
	}

	// The row IS gone.
	var n int
	if err := db.sql.QueryRowContext(ctx,
		`SELECT count(*) FROM entry WHERE id = ?`, id,
	).Scan(&n); err != nil {
		t.Fatalf("count entry: %v", err)
	}
	if n != 0 {
		t.Fatalf("entry count after Forget+audit-fail = %d, want 0", n)
	}
}

// TestRemember_PreCommitFailure_NoAuditEmit — AC2/AC6: invalid Entry
// (empty content) returns ErrInvalidEntry from the `validate` step
// before any tx work; the audit-emit code path is unreachable so the
// fakeLogger sees ZERO events.
func TestRemember_PreCommitFailure_NoAuditEmit(t *testing.T) {
	logger := &fakeLogger{}
	db, ctx := freshDBWithLogger(t, logger)

	bad := validEntry()
	bad.Content = "" // forces ErrInvalidEntry from validate(*Entry)

	_, err := db.Remember(ctx, bad)
	if !errors.Is(err, ErrInvalidEntry) {
		t.Fatalf("err = %v, want ErrInvalidEntry", err)
	}
	if got := logger.called.Load(); got != 0 {
		t.Fatalf("logger.LogAppend called %d times on pre-commit failure, want 0", got)
	}
}

// TestForget_NotFound_NoAuditEmit — AC3/AC6: well-formed UUID but no
// matching row → Forget returns ErrNotFound after rolling back the tx;
// the audit-emit code path is unreachable so the fakeLogger sees ZERO
// events.
func TestForget_NotFound_NoAuditEmit(t *testing.T) {
	logger := &fakeLogger{}
	db, ctx := freshDBWithLogger(t, logger)

	const stray = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"
	err := db.Forget(ctx, stray)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if got := logger.called.Load(); got != 0 {
		t.Fatalf("logger.LogAppend called %d times on not-found Forget, want 0", got)
	}
}
