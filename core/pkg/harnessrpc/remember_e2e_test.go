// Package harnessrpc_test — end-to-end dispatch chain test (M5.5.d.c).
//
// # Test layer decision (AC5)
//
// The e2e test lives on the Go side for two reasons:
//  1. Spawning a real Go process from TS tests is heavy (compile + exec
//     overhead, process supervision, stdio teardown). The io.Pipe pattern
//     already used in host_integration_test.go gives the same guarantees
//     with none of the overhead.
//  2. The existing rpcShim (host_integration_test.go) already emits the
//     exact NDJSON shape the TS rememberEntry helper would send — the params
//     map {agentID, category, subject, content} is identical. Go-side tests
//     hand-craft the same NDJSON, proving the wire contract without needing
//     a live TS process.
//
// # What this test proves
//
// TestRememberE2E_FullDispatchChain exercises the complete path:
//
//	harness-shape NDJSON → harnessrpc.Host dispatch → notebook.remember
//	handler → notebook.DB.Remember → SQLite entry row + entry_vec row
//
// The NDJSON request shape is taken verbatim from
// harness/src/notebookClient.ts::rememberEntry:
//
//	{jsonrpc:"2.0", id:1, method:"notebook.remember",
//	 params:{agentID, category, subject, content}}
//
// After the call succeeds the test opens a fresh *sql.DB against the same
// SQLite file to assert: (a) the entry row exists with the right content,
// (b) entry_vec carries a non-empty embedding.
package harnessrpc_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/harnessrpc"
	"github.com/vadimtrunov/watchkeepers/core/pkg/llm"

	_ "github.com/mattn/go-sqlite3"
)

// TestRememberE2E_FullDispatchChain drives invokeTool-shaped NDJSON through
// the full chain:
//
//	rpcShim.request("notebook.remember", params)
//	  → harnessrpc.Host dispatch
//	  → NewNotebookRememberHandler closure
//	  → runtime.NotebookSupervisor.Lookup → notebook.DB.Remember
//	  → SQLite entry row + entry_vec row
//
// The params map {agentID, category, subject, content} matches exactly the
// shape harness/src/notebookClient.ts::rememberEntry serialises on the wire,
// so this test is the cross-language wire-contract proof for M5.5.d.c AC4.
func TestRememberE2E_FullDispatchChain(t *testing.T) {
	// Deterministic agent UUID — matches the recommendation in the TASK brief.
	const agentID = "00000000-0000-4000-8000-000000000001"
	dir := t.TempDir()

	// Stand up a supervisor with the agent pre-opened (same helper as
	// notebook_remember_test.go so the pattern is symmetric across tests).
	sup := openedSupervisor(t, dir, agentID)
	embedder := llm.NewFakeEmbeddingProvider()

	// Register only the notebook.remember handler — the same set the real
	// harness boots with once M5.5.d is complete.
	host := harnessrpc.NewHost()
	host.Register("notebook.remember", harnessrpc.NewNotebookRememberHandler(sup, embedder))

	shim, teardown := startBridge(t, host)
	defer teardown()

	// Wire-shape params from harness/src/notebookClient.ts::rememberEntry.
	// The TS implementation serialises a RememberEntryParams struct as:
	//   {agentID: string, category: string, subject: string, content: string}
	// The Go-side rpcShim.request() wraps this into the standard JSON-RPC
	// envelope, so the Host receives exactly what a real TS process would send.
	params := makeRememberParams(agentID, "lesson", "e2e", "hello from e2e test")

	raw, rpcErr, err := shim.request("notebook.remember", params)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if rpcErr != nil {
		t.Fatalf("unexpected RPC error: %+v", rpcErr)
	}

	// AC4 assertion 1: response carries a non-empty id (UUID v7 from substrate).
	var result struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.ID == "" {
		t.Fatal("response id must be non-empty (want UUID v7 from notebook.DB.Remember)")
	}

	// AC4 assertion 2: SQLite entry row persisted with the supplied content.
	dbPath := filepath.Join(dir, "notebook", agentID+".sqlite")
	sqlDB, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()

	var gotCategory, gotSubject, gotContent string
	err = sqlDB.QueryRowContext(context.Background(),
		"SELECT category, subject, content FROM entry WHERE id = ?", result.ID).
		Scan(&gotCategory, &gotSubject, &gotContent)
	if err != nil {
		t.Fatalf("query entry row: %v", err)
	}
	if gotCategory != "lesson" {
		t.Errorf("category = %q, want %q", gotCategory, "lesson")
	}
	if gotSubject != "e2e" {
		t.Errorf("subject = %q, want %q", gotSubject, "e2e")
	}
	if gotContent != "hello from e2e test" {
		t.Errorf("content = %q, want %q", gotContent, "hello from e2e test")
	}

	// AC4 assertion 3: entry_vec row carries a non-empty embedding.
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
