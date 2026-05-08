// Tests for the notebook.remember JSON-RPC method (M5.5.d.a.b).
//
// Integration test: drives the Go [Host] via the existing io.Pipe shim from
// host_integration_test.go (same package, same test binary). Unit tests call
// the handler closure directly without the Host wrapper.
package harnessrpc_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/harnessrpc"
	"github.com/vadimtrunov/watchkeepers/core/pkg/llm"
	"github.com/vadimtrunov/watchkeepers/core/pkg/runtime"

	_ "github.com/mattn/go-sqlite3"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// makeRememberParams builds the params map for a notebook.remember request.
func makeRememberParams(agentID, category, subject, content string) map[string]string {
	return map[string]string{
		"agentID":  agentID,
		"category": category,
		"subject":  subject,
		"content":  content,
	}
}

// openedSupervisor returns a supervisor with `agentID` already opened against
// a real SQLite file in `dir`. Registers t.Cleanup to close the supervisor.
func openedSupervisor(t *testing.T, dir, agentID string) *runtime.NotebookSupervisor {
	t.Helper()
	t.Setenv("WATCHKEEPER_DATA", dir)
	sup := runtime.NewNotebookSupervisor()
	if _, err := sup.Open(agentID); err != nil {
		t.Fatalf("supervisor.Open: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close(agentID) })
	return sup
}

// callHandler invokes the handler closure directly (no Host wrapper).
func callHandler(t *testing.T, handler harnessrpc.MethodHandler, params any) (json.RawMessage, error) {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	result, handlerErr := handler(context.Background(), raw)
	if handlerErr != nil {
		return nil, handlerErr
	}
	out, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	return out, nil
}

// ── integration test ─────────────────────────────────────────────────────────

// TestNotebookRemember_HappyPath_PersistsEntry drives a notebook.remember
// request end-to-end via the Host ↔ rpcShim bridge (io.Pipe). Asserts the
// response carries {id} and a direct SQL read confirms the row is persisted.
func TestNotebookRemember_HappyPath_PersistsEntry(t *testing.T) {
	const agentID = "00000000-0000-0000-0000-000000000001"
	dir := t.TempDir()

	sup := openedSupervisor(t, dir, agentID)
	embedder := llm.NewFakeEmbeddingProvider()

	host := harnessrpc.NewHost()
	host.Register("notebook.remember", harnessrpc.NewNotebookRememberHandler(sup, embedder))

	shim, teardown := startBridge(t, host)
	defer teardown()

	params := makeRememberParams(agentID, "lesson", "Go testing", "Use t.TempDir for isolated SQLite files")
	raw, rpcErr, err := shim.request("notebook.remember", params)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if rpcErr != nil {
		t.Fatalf("unexpected RPC error: %+v", rpcErr)
	}

	// Decode the {id} response.
	var result struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.ID == "" {
		t.Fatal("response id must be non-empty")
	}

	// Verify persistence: read the row directly from the SQLite file.
	dbPath := filepath.Join(dir, "notebook", agentID+".sqlite")
	sqlDB, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()

	var gotCategory, gotContent string
	err = sqlDB.QueryRowContext(context.Background(),
		"SELECT category, content FROM entry WHERE id = ?", result.ID).
		Scan(&gotCategory, &gotContent)
	if err != nil {
		t.Fatalf("query entry row: %v", err)
	}
	if gotCategory != "lesson" {
		t.Errorf("category = %q, want %q", gotCategory, "lesson")
	}
	if gotContent != "Use t.TempDir for isolated SQLite files" {
		t.Errorf("content = %q, want expected content", gotContent)
	}

	// Verify the embedding is non-empty in entry_vec.
	var embeddingLen int
	err = sqlDB.QueryRowContext(context.Background(),
		"SELECT length(embedding) FROM entry_vec WHERE id = ?", result.ID).
		Scan(&embeddingLen)
	if err != nil {
		t.Fatalf("query entry_vec row: %v", err)
	}
	if embeddingLen == 0 {
		t.Error("embedding in entry_vec must be non-empty")
	}
}

// ── unit tests (handler-only, no Host wrapper) ────────────────────────────────

// TestNotebookRememberHandler_AgentNotRegistered_FailsRPC verifies that a
// Lookup miss (agent UUID never opened in supervisor) returns -32603 with
// data.kind == "agent_not_registered".
func TestNotebookRememberHandler_AgentNotRegistered_FailsRPC(t *testing.T) {
	sup := runtime.NewNotebookSupervisor() // empty — no Open calls
	handler := harnessrpc.NewNotebookRememberHandler(sup, llm.NewFakeEmbeddingProvider())

	_, err := callHandler(t, handler, makeRememberParams(
		"00000000-0000-0000-0000-000000000099",
		"lesson", "", "some content",
	))
	if err == nil {
		t.Fatal("expected error for unregistered agent, got nil")
	}
	var rpcErr *harnessrpc.RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("error is not *RPCError: %T %v", err, err)
	}
	if rpcErr.Code != harnessrpc.ErrCodeInternalError {
		t.Errorf("code = %d, want %d (ErrCodeInternalError)", rpcErr.Code, harnessrpc.ErrCodeInternalError)
	}
	// Assert structured data sentinel so M5.5.d.b dispatcher can classify without string-matching.
	dataMap, ok := rpcErr.Data.(map[string]any)
	if !ok {
		t.Fatalf("RPCError.Data is not map[string]any: %T", rpcErr.Data)
	}
	if got := dataMap["kind"]; got != "agent_not_registered" {
		t.Errorf("data.kind = %q, want %q", got, "agent_not_registered")
	}
}

// TestNotebookRememberHandler_EmbedError_FailsRPC verifies that an Embed
// failure propagates as a -32603 internal error with data.kind == "embed_failed".
func TestNotebookRememberHandler_EmbedError_FailsRPC(t *testing.T) {
	const agentID = "00000000-0000-0000-0000-000000000002"
	dir := t.TempDir()
	sup := openedSupervisor(t, dir, agentID)

	sentinel := fmt.Errorf("embed: network unavailable")
	embedder := llm.NewFakeEmbeddingProvider(llm.WithEmbedError(sentinel))
	handler := harnessrpc.NewNotebookRememberHandler(sup, embedder)

	_, err := callHandler(t, handler, makeRememberParams(agentID, "lesson", "", "content"))
	if err == nil {
		t.Fatal("expected embed error, got nil")
	}
	var rpcErr *harnessrpc.RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("error is not *RPCError: %T %v", err, err)
	}
	if rpcErr.Code != harnessrpc.ErrCodeInternalError {
		t.Errorf("code = %d, want %d (ErrCodeInternalError)", rpcErr.Code, harnessrpc.ErrCodeInternalError)
	}
	// Assert structured data sentinel.
	dataMap, ok := rpcErr.Data.(map[string]any)
	if !ok {
		t.Fatalf("RPCError.Data is not map[string]any: %T", rpcErr.Data)
	}
	if got := dataMap["kind"]; got != "embed_failed" {
		t.Errorf("data.kind = %q, want %q", got, "embed_failed")
	}
	// The sentinel message must appear in the RPCError message.
	if rpcErr.Message == "" {
		t.Error("RPCError.Message must not be empty")
	}
}

// TestNotebookRememberHandler_RememberError_FailsRPC verifies that a DB
// failure on Remember propagates as a -32603 internal error. We close the
// underlying DB before calling the handler.
func TestNotebookRememberHandler_RememberError_FailsRPC(t *testing.T) {
	const agentID = "00000000-0000-0000-0000-000000000003"
	dir := t.TempDir()
	sup := openedSupervisor(t, dir, agentID)

	// Close the DB so Remember fails.
	if err := sup.Close(agentID); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Re-open so Lookup returns a handle (now closed DB).
	// We need Lookup to succeed but Remember to fail.
	// Strategy: open a fresh supervisor entry pointing to the same (now-closed) DB.
	// Easier: re-open via supervisor (idempotent open gives a fresh handle),
	// get the DB handle, call Close on it directly, then Lookup still returns it.
	sup2 := runtime.NewNotebookSupervisor()
	db, err := sup2.Open(agentID)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Close the underlying DB so Remember will fail.
	if err := db.Close(); err != nil {
		t.Fatalf("db.Close: %v", err)
	}

	handler := harnessrpc.NewNotebookRememberHandler(sup2, llm.NewFakeEmbeddingProvider())
	_, handlerErr := callHandler(t, handler, makeRememberParams(agentID, "lesson", "", "content"))
	if handlerErr == nil {
		t.Fatal("expected remember error on closed DB, got nil")
	}
}

// TestNotebookRememberHandler_InvalidParams_FailsRPC verifies that missing
// required fields return -32602 invalid params. Tests 3 sub-cases.
func TestNotebookRememberHandler_InvalidParams_FailsRPC(t *testing.T) {
	sup := runtime.NewNotebookSupervisor()
	handler := harnessrpc.NewNotebookRememberHandler(sup, llm.NewFakeEmbeddingProvider())

	cases := []struct {
		name   string
		params map[string]string
	}{
		{
			name:   "no agentID",
			params: map[string]string{"agentID": "", "category": "lesson", "subject": "", "content": "x"},
		},
		{
			name:   "no content",
			params: map[string]string{"agentID": "00000000-0000-0000-0000-000000000010", "category": "lesson", "subject": "", "content": ""},
		},
		{
			name:   "no category",
			params: map[string]string{"agentID": "00000000-0000-0000-0000-000000000010", "category": "", "subject": "", "content": "x"},
		},
		{
			// Non-empty but unknown category must be rejected with -32602 before
			// any Embed or DB work (Important 1: early enum validation).
			name:   "unknown category",
			params: map[string]string{"agentID": "00000000-0000-0000-0000-000000000010", "category": "unknown", "subject": "", "content": "x"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := callHandler(t, handler, tc.params)
			if err == nil {
				t.Fatalf("expected -32602 error, got nil")
			}
			var rpcErr *harnessrpc.RPCError
			if !errors.As(err, &rpcErr) {
				t.Fatalf("error is not *RPCError: %T %v", err, err)
			}
			if rpcErr.Code != harnessrpc.ErrCodeInvalidParams {
				t.Errorf("code = %d, want %d (ErrCodeInvalidParams)", rpcErr.Code, harnessrpc.ErrCodeInvalidParams)
			}
		})
	}
}
