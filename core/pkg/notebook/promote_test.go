package notebook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	sqlitevec "github.com/asg017/sqlite-vec-go-bindings/cgo"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
)

// promoteSeed is the canonical fixture produced by [seedPromoteEntry] —
// caller-supplied values for the fields the M2b.8 acceptance criteria
// pin against the resulting [Proposal].
type promoteSeed struct {
	category    string
	subject     string
	content     string
	toolVersion *string
	embedding   []float32
}

// defaultPromoteSeed returns a fully-populated [promoteSeed] suitable for
// the happy-path tests. Distinct embedding seed and a non-empty
// ToolVersion so the round-trip assertions actually exercise the
// non-default branches.
func defaultPromoteSeed() promoteSeed {
	tv := "v1.0.0"
	return promoteSeed{
		category:    CategoryLesson,
		subject:     "promote-fixture",
		content:     "promotion test content",
		toolVersion: &tv,
		embedding:   makeEmbedding(41),
	}
}

// seedPromoteEntry calls [DB.Remember] with `s` and returns the assigned
// id. Centralised so each test's Arrange step is one line.
func seedPromoteEntry(ctx context.Context, t *testing.T, db *DB, s promoteSeed) string {
	t.Helper()
	id, err := db.Remember(ctx, Entry{
		Category:    s.category,
		Subject:     s.subject,
		Content:     s.content,
		ToolVersion: s.toolVersion,
		Embedding:   s.embedding,
	})
	if err != nil {
		t.Fatalf("seed Remember: %v", err)
	}
	return id
}

// requireMissingFields asserts every name in `banned` is absent from
// `payload`. Mirrors the M2b.7 PII-discipline assertion helper that the
// existing audit-emit tests use inline; promoted to a named helper here
// to make the intent obvious at every call site (banned = "must NOT be
// in the payload, ever").
func requireMissingFields(t *testing.T, payload map[string]any, banned ...string) {
	t.Helper()
	for _, name := range banned {
		if _, ok := payload[name]; ok {
			t.Fatalf("payload contains banned field %q: %v", name, payload)
		}
	}
}

// countNotebookRows returns (entry_count, entry_vec_count) for `db`.
// Used by the read-only-on-Notebook-tables test to assert PromoteToKeep
// performs zero writes.
func countNotebookRows(ctx context.Context, t *testing.T, db *DB) (int, int) {
	t.Helper()
	var entryN, vecN int
	if err := db.sql.QueryRowContext(ctx, "SELECT count(*) FROM entry").Scan(&entryN); err != nil {
		t.Fatalf("count entry: %v", err)
	}
	if err := db.sql.QueryRowContext(ctx, "SELECT count(*) FROM entry_vec").Scan(&vecN); err != nil {
		t.Fatalf("count entry_vec: %v", err)
	}
	return entryN, vecN
}

// assertProposalScalarFields verifies every non-vector field on `p`
// matches the seed `s` and the supplied id / agent. Extracted from
// [TestPromoteToKeep_ReturnsProposalForExistingEntry] so the test body
// stays under the gocyclo threshold; the AC's "all 11 Proposal fields"
// requirement is enforced here verbatim.
func assertProposalScalarFields(t *testing.T, p *Proposal, s promoteSeed, id string, before, after int64) {
	t.Helper()
	if p.Subject != s.subject {
		t.Fatalf("Subject = %q, want %q", p.Subject, s.subject)
	}
	if p.Content != s.content {
		t.Fatalf("Content = %q, want %q", p.Content, s.content)
	}
	if p.ToolVersion == nil || *p.ToolVersion != *s.toolVersion {
		t.Fatalf("ToolVersion = %v, want %q", p.ToolVersion, *s.toolVersion)
	}
	if p.ProposalID == "" || !uuidPattern.MatchString(p.ProposalID) {
		t.Fatalf("ProposalID = %q is not a canonical UUID", p.ProposalID)
	}
	if p.AgentID != auditAgentID {
		t.Fatalf("AgentID = %q, want %q", p.AgentID, auditAgentID)
	}
	if p.NotebookEntryID != id {
		t.Fatalf("NotebookEntryID = %q, want %q", p.NotebookEntryID, id)
	}
	if p.Category != s.category {
		t.Fatalf("Category = %q, want %q", p.Category, s.category)
	}
	if p.Scope != ScopeOrg {
		t.Fatalf("Scope = %q, want %q", p.Scope, ScopeOrg)
	}
	if p.SourceCreatedAt == 0 {
		t.Fatal("SourceCreatedAt is zero; should mirror entry.created_at")
	}
	if p.ProposedAt < before || p.ProposedAt > after {
		t.Fatalf("ProposedAt = %d, want in [%d, %d]", p.ProposedAt, before, after)
	}
}

// assertEmbeddingByteRoundTrip serialises the proposal's float32 vector
// and compares to the raw `entry_vec.embedding` blob in `db`. M2b.2.b
// LESSONS guard — catches truncation or endianness regressions in the
// deserialise path. Extracted so the round-trip test body stays
// under the gocyclo threshold.
func assertEmbeddingByteRoundTrip(ctx context.Context, t *testing.T, db *DB, p *Proposal, id string) {
	t.Helper()
	if len(p.Embedding) != EmbeddingDim {
		t.Fatalf("len(Embedding) = %d, want %d", len(p.Embedding), EmbeddingDim)
	}
	want, err := sqlitevec.SerializeFloat32(p.Embedding)
	if err != nil {
		t.Fatalf("SerializeFloat32: %v", err)
	}
	var got []byte
	if err := db.sql.QueryRowContext(ctx,
		"SELECT embedding FROM entry_vec WHERE id = ?", id,
	).Scan(&got); err != nil {
		t.Fatalf("SELECT entry_vec.embedding: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("proposal embedding bytes differ from entry_vec blob")
	}
}

// TestPromoteToKeep_ReturnsProposalForExistingEntry — Remember an entry,
// call PromoteToKeep, assert all 11 Proposal fields match the source
// entry. Includes embedding-byte equality via bytes.Equal on the
// entry_vec blob (M2b.2.b LESSONS embedding-byte round-trip discipline).
func TestPromoteToKeep_ReturnsProposalForExistingEntry(t *testing.T) {
	db, ctx := freshDBWithLogger(t, nil)
	s := defaultPromoteSeed()
	id := seedPromoteEntry(ctx, t, db, s)

	before := time.Now().UnixMilli()
	p, err := db.PromoteToKeep(ctx, id)
	after := time.Now().UnixMilli()
	if err != nil {
		t.Fatalf("PromoteToKeep: %v", err)
	}
	if p == nil {
		t.Fatal("PromoteToKeep returned nil proposal")
	}
	assertProposalScalarFields(t, p, s, id, before, after)
	assertEmbeddingByteRoundTrip(ctx, t, db, p, id)
}

// TestPromoteToKeep_DefaultScopeIsOrg asserts proposal.Scope == "org" on
// a fresh proposal — the scope value the AC pins as the default.
func TestPromoteToKeep_DefaultScopeIsOrg(t *testing.T) {
	db, ctx := freshDBWithLogger(t, nil)
	id := seedPromoteEntry(ctx, t, db, defaultPromoteSeed())

	p, err := db.PromoteToKeep(ctx, id)
	if err != nil {
		t.Fatalf("PromoteToKeep: %v", err)
	}
	if p.Scope != "org" {
		t.Fatalf("Scope = %q, want %q", p.Scope, "org")
	}
	if p.Scope != ScopeOrg {
		t.Fatalf("Scope = %q, want ScopeOrg=%q", p.Scope, ScopeOrg)
	}
}

// TestPromoteToKeep_GeneratesUUIDv7ProposalID — call twice, assert two
// different non-empty UUID v7 strings; assert lex order matches call
// order (UUID v7 monotonic property).
func TestPromoteToKeep_GeneratesUUIDv7ProposalID(t *testing.T) {
	db, ctx := freshDBWithLogger(t, nil)
	id := seedPromoteEntry(ctx, t, db, defaultPromoteSeed())

	p1, err := db.PromoteToKeep(ctx, id)
	if err != nil {
		t.Fatalf("PromoteToKeep #1: %v", err)
	}
	// Sleep 1ms so the second UUID v7 lands in a strictly later
	// millisecond bucket; UUID v7 monotonicity guarantees per-process
	// ordering within a tick but cross-tick ordering is the AC's
	// observable property.
	time.Sleep(time.Millisecond)
	p2, err := db.PromoteToKeep(ctx, id)
	if err != nil {
		t.Fatalf("PromoteToKeep #2: %v", err)
	}
	if p1.ProposalID == "" || p2.ProposalID == "" {
		t.Fatalf("ProposalIDs empty: p1=%q p2=%q", p1.ProposalID, p2.ProposalID)
	}
	if p1.ProposalID == p2.ProposalID {
		t.Fatalf("ProposalIDs equal: %q", p1.ProposalID)
	}
	if p1.ProposalID >= p2.ProposalID {
		t.Fatalf("ProposalID lex order broken: p1=%q p2=%q (UUID v7 must be monotonic)",
			p1.ProposalID, p2.ProposalID)
	}
}

// TestPromoteToKeep_PreservesNullableToolVersion — entry with nil
// ToolVersion → proposal ToolVersion is nil; entry with *string
// ToolVersion → proposal ToolVersion mirrors it.
func TestPromoteToKeep_PreservesNullableToolVersion(t *testing.T) {
	t.Run("nil tool_version", func(t *testing.T) {
		db, ctx := freshDBWithLogger(t, nil)
		s := defaultPromoteSeed()
		s.toolVersion = nil
		id := seedPromoteEntry(ctx, t, db, s)

		p, err := db.PromoteToKeep(ctx, id)
		if err != nil {
			t.Fatalf("PromoteToKeep: %v", err)
		}
		if p.ToolVersion != nil {
			t.Fatalf("ToolVersion = %v, want nil", *p.ToolVersion)
		}
	})
	t.Run("non-nil tool_version", func(t *testing.T) {
		db, ctx := freshDBWithLogger(t, nil)
		tv := "v9.9.9"
		s := defaultPromoteSeed()
		s.toolVersion = &tv
		id := seedPromoteEntry(ctx, t, db, s)

		p, err := db.PromoteToKeep(ctx, id)
		if err != nil {
			t.Fatalf("PromoteToKeep: %v", err)
		}
		if p.ToolVersion == nil {
			t.Fatal("ToolVersion = nil, want non-nil")
		}
		if *p.ToolVersion != tv {
			t.Fatalf("ToolVersion = %q, want %q", *p.ToolVersion, tv)
		}
	})
}

// TestPromoteToKeep_AuditEmittedWithCorrectShape — wire fake Logger via
// WithLogger; assert one notebook_promotion_proposed event emitted with
// payload containing exactly agent_id, entry_id, proposal_id, category,
// proposed_at; assert payload does NOT contain content, embedding,
// subject (M2b.7 PII discipline).
func TestPromoteToKeep_AuditEmittedWithCorrectShape(t *testing.T) {
	logger := &fakeLogger{}
	db, ctx := freshDBWithLogger(t, logger)
	id := seedPromoteEntry(ctx, t, db, defaultPromoteSeed())

	// The seed Remember itself emitted one event; reset the recorded
	// receipt so the subsequent assertions only see PromoteToKeep's emit.
	logger.called.Store(0)
	logger.received = keepclient.LogAppendRequest{}

	p, err := db.PromoteToKeep(ctx, id)
	if err != nil {
		t.Fatalf("PromoteToKeep: %v", err)
	}

	if got := logger.called.Load(); got != 1 {
		t.Fatalf("logger.LogAppend called %d times, want 1", got)
	}
	if logger.received.EventType != promoteEventType {
		t.Fatalf("event_type = %q, want %q", logger.received.EventType, promoteEventType)
	}

	var raw map[string]any
	if err := json.Unmarshal(logger.received.Payload, &raw); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	for _, key := range []string{"agent_id", "entry_id", "proposal_id", "category", "proposed_at"} {
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
	if raw["proposal_id"] != p.ProposalID {
		t.Fatalf("payload.proposal_id = %v, want %q", raw["proposal_id"], p.ProposalID)
	}
	if raw["category"] != CategoryLesson {
		t.Fatalf("payload.category = %v, want %q", raw["category"], CategoryLesson)
	}
	proposedAt, ok := raw["proposed_at"].(string)
	if !ok {
		t.Fatalf("payload.proposed_at = %v, want string", raw["proposed_at"])
	}
	if _, err := time.Parse(time.RFC3339Nano, proposedAt); err != nil {
		t.Fatalf("payload.proposed_at = %q does not parse as RFC3339Nano: %v", proposedAt, err)
	}

	// PII / large-field exclusion: the AC bans these three explicitly.
	requireMissingFields(t, raw, "content", "embedding", "subject")
}

// TestPromoteToKeep_ErrNotFoundOnMissingEntry — call with a fresh UUID
// the DB never saw; assert errors.Is(err, ErrNotFound) and that no
// audit event was emitted.
func TestPromoteToKeep_ErrNotFoundOnMissingEntry(t *testing.T) {
	logger := &fakeLogger{}
	db, ctx := freshDBWithLogger(t, logger)

	const stray = "ffffffff-ffff-ffff-ffff-ffffffffffff"
	p, err := db.PromoteToKeep(ctx, stray)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if p != nil {
		t.Fatalf("proposal = %+v, want nil on ErrNotFound", p)
	}
	if got := logger.called.Load(); got != 0 {
		t.Fatalf("logger.LogAppend called %d times on ErrNotFound, want 0", got)
	}
}

// TestPromoteToKeep_ErrInvalidEntryOnEmptyID — call with "", assert
// errors.Is(err, ErrInvalidEntry) and that no audit event was emitted.
func TestPromoteToKeep_ErrInvalidEntryOnEmptyID(t *testing.T) {
	logger := &fakeLogger{}
	db, ctx := freshDBWithLogger(t, logger)

	p, err := db.PromoteToKeep(ctx, "")
	if !errors.Is(err, ErrInvalidEntry) {
		t.Fatalf("err = %v, want ErrInvalidEntry", err)
	}
	if p != nil {
		t.Fatalf("proposal = %+v, want nil on ErrInvalidEntry", p)
	}
	if got := logger.called.Load(); got != 0 {
		t.Fatalf("logger.LogAppend called %d times on ErrInvalidEntry, want 0", got)
	}
}

// TestPromoteToKeep_NoLoggerNoAuditEmit — open WITHOUT WithLogger; call
// PromoteToKeep, assert proposal returned and no panic. The implicit
// assertion is that the audit-emit code path is unreachable when
// d.logger == nil (mirrors the M2b.7 backward-compat guarantee).
func TestPromoteToKeep_NoLoggerNoAuditEmit(t *testing.T) {
	db, ctx := freshDBWithLogger(t, nil)
	id := seedPromoteEntry(ctx, t, db, defaultPromoteSeed())

	p, err := db.PromoteToKeep(ctx, id)
	if err != nil {
		t.Fatalf("PromoteToKeep without logger: %v", err)
	}
	if p == nil {
		t.Fatal("PromoteToKeep returned nil proposal without logger")
	}
	if p.NotebookEntryID != id {
		t.Fatalf("NotebookEntryID = %q, want %q", p.NotebookEntryID, id)
	}
}

// TestPromoteToKeep_SupersededEntryIsPromotable — entry with non-NULL
// superseded_by is still loadable + promotable. Documents the behavior
// that superseded entries are NOT filtered out at promote-time; the
// caller's responsibility (M6.2 may filter on its end).
func TestPromoteToKeep_SupersededEntryIsPromotable(t *testing.T) {
	db, ctx := freshDBWithLogger(t, nil)

	// Seed two entries, then mark the older one as superseded by the newer.
	olderID := seedPromoteEntry(ctx, t, db, defaultPromoteSeed())
	newer := defaultPromoteSeed()
	newer.embedding = makeEmbedding(42)
	newer.content = "newer content"
	newerID := seedPromoteEntry(ctx, t, db, newer)

	if _, err := db.sql.ExecContext(ctx,
		`UPDATE entry SET superseded_by = ? WHERE id = ?`, newerID, olderID,
	); err != nil {
		t.Fatalf("UPDATE superseded_by: %v", err)
	}

	p, err := db.PromoteToKeep(ctx, olderID)
	if err != nil {
		t.Fatalf("PromoteToKeep on superseded entry: %v", err)
	}
	if p == nil || p.NotebookEntryID != olderID {
		t.Fatalf("got %+v, want a proposal for %q", p, olderID)
	}
}

// TestPromoteToKeep_AuditEmitFailureReturnsProposalAndErr — fake Logger
// returns an error; assert proposal is non-nil, error wraps "audit
// emit:" and the underlying error (M2b.7 partial-failure shape).
func TestPromoteToKeep_AuditEmitFailureReturnsProposalAndErr(t *testing.T) {
	logger := &fakeLogger{} // happy on the seed Remember
	db, ctx := freshDBWithLogger(t, logger)
	id := seedPromoteEntry(ctx, t, db, defaultPromoteSeed())

	auditBoom := errors.New("audit boom (promote)")
	logger.logErr = auditBoom

	p, err := db.PromoteToKeep(ctx, id)
	if err == nil {
		t.Fatal("PromoteToKeep returned nil error on audit-emit failure")
	}
	if p == nil {
		t.Fatal("PromoteToKeep returned nil proposal; partial-failure contract requires the proposal")
	}
	if !errors.Is(err, auditBoom) {
		t.Fatalf("err = %v, want one wrapping audit boom", err)
	}
	if !strings.Contains(err.Error(), "audit emit:") {
		t.Fatalf("err = %v, want prefix 'audit emit:'", err)
	}
	if p.NotebookEntryID != id {
		t.Fatalf("proposal.NotebookEntryID = %q, want %q", p.NotebookEntryID, id)
	}
}

// TestPromoteToKeep_ReadOnlyOnNotebookTables — count rows in entry and
// entry_vec before and after a happy-path PromoteToKeep AND after a
// PromoteToKeep that fails on audit emit; assert counts unchanged in
// both cases. AC5: "PromoteToKeep performs zero writes to entry or
// entry_vec".
func TestPromoteToKeep_ReadOnlyOnNotebookTables(t *testing.T) {
	logger := &fakeLogger{}
	db, ctx := freshDBWithLogger(t, logger)
	id := seedPromoteEntry(ctx, t, db, defaultPromoteSeed())

	wantEntry, wantVec := countNotebookRows(ctx, t, db)

	// Happy path emit — counts must not change.
	if _, err := db.PromoteToKeep(ctx, id); err != nil {
		t.Fatalf("PromoteToKeep happy: %v", err)
	}
	gotEntry, gotVec := countNotebookRows(ctx, t, db)
	if gotEntry != wantEntry || gotVec != wantVec {
		t.Fatalf("post-happy counts differ: entry %d→%d, entry_vec %d→%d",
			wantEntry, gotEntry, wantVec, gotVec)
	}

	// Audit-fail path — counts still must not change.
	logger.logErr = errors.New("audit boom (read-only)")
	if _, err := db.PromoteToKeep(ctx, id); err == nil {
		t.Fatal("PromoteToKeep returned nil error on audit-fail; setup wrong")
	}
	gotEntry, gotVec = countNotebookRows(ctx, t, db)
	if gotEntry != wantEntry || gotVec != wantVec {
		t.Fatalf("post-audit-fail counts differ: entry %d→%d, entry_vec %d→%d",
			wantEntry, gotEntry, wantVec, gotVec)
	}
}
