package notebook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
)

// Fetcher is the minimal subset of `archivestore.ArchiveStore` that
// [ImportFromArchive] needs (just `Get`). Defined as a single-method
// interface alongside [Storer] so each helper takes only what it needs and
// callers can hand in the same `archivestore.ArchiveStore` value to either
// without an adapter shim. Both `archivestore.LocalFS` and
// `archivestore.S3Compatible` satisfy this interface structurally.
//
// The interface lives in this package rather than being imported from
// archivestore because archivestore's test files already import notebook
// (round-trip Archive/Import assertions); a notebook-side production
// import of archivestore would close the cycle. Defining a local single-
// method interface keeps notebook free of the archivestore import while
// still letting callers wire any concrete archivestore implementation
// straight in.
type Fetcher interface {
	Get(ctx context.Context, uri string) (io.ReadCloser, error)
}

// importPayload is the JSON shape carried by the `notebook_imported`
// event emitted by [ImportFromArchive]. Mirrors the field discipline of
// [retirePayload]: the three fields document _which_ agent's notebook
// received the import, _which_ archive URI was the source, and _when_
// the import completed (RFC3339Nano UTC). The shape is part of the
// audit-log contract — downstream consumers parse these fields by name.
type importPayload struct {
	AgentID    string `json:"agent_id"`
	ArchiveURI string `json:"archive_uri"`
	ImportedAt string `json:"imported_at"`
}

// importEventType is the `event_type` column written to keepers_log for
// the import audit event. Held as a const so tests can pin against the
// same string the production code emits.
const importEventType = "notebook_imported"

// ImportFromArchive orchestrates the three already-merged primitives —
// [Fetcher.Get], [Open], and [DB.Import] — into one operator-callable
// "restore a predecessor's archive into a fresh agent file" call. On
// success it emits a `notebook_imported` event to Keeper's Log via the
// supplied [Logger]; the helper opens AND closes the destination
// notebook itself so callers do not pass a `*DB`.
//
// Sequence:
//
//  1. Validate inputs synchronously: `agentID` must be a canonical UUID
//     and `archiveURI` must be non-empty. Either failure returns
//     [ErrInvalidEntry] without any I/O — the [Fetcher] impl owns
//     scheme/path validation downstream.
//  2. `rc, err := fetcher.Get(ctx, archiveURI)`. Failure wraps as
//     `fmt.Errorf("fetch: %w", err)` so callers can `errors.Is` against
//     archivestore sentinels (`ErrNotFound`, `ErrInvalidURI`).
//  3. `defer rc.Close()`.
//  4. `db, err := Open(ctx, agentID)`. Failure wraps as
//     `fmt.Errorf("open: %w", err)`.
//  5. `defer db.Close()`.
//  6. `db.Import(ctx, rc)`. Failure wraps as `fmt.Errorf("import: %w",
//     err)`; callers can `errors.Is` against [ErrCorruptArchive] or
//     [ErrTargetNotEmpty] through the wrap.
//  7. If `logger != nil`, emit a `notebook_imported` event with payload
//     `{agent_id, archive_uri, imported_at}` (RFC3339Nano UTC). On
//     LogAppend failure, return the wrapped error — the import has
//     already succeeded, so the data is in but the audit didn't land.
//     Caller can retry just the audit emit.
//
// # Partial-failure contract
//
//   - Validation failure → `ErrInvalidEntry`, no I/O.
//   - Fetch failure → wrapped error matching `errors.Is(err, fetcherErr)`.
//   - Open failure → wrapped error.
//   - Import failure → wrapped error matching [ErrCorruptArchive] or
//     [ErrTargetNotEmpty].
//   - LogAppend failure → wrapped error; the import succeeded.
//   - Context cancel → wrapped `context.Canceled` /
//     `context.DeadlineExceeded`.
//
// # Recommended call site
//
// Bootstrapping a successor agent that needs to inherit a predecessor's
// memory. ImportFromArchive builds in NO retry logic — the caller
// decides whether to retry the whole call (fetch / open / import
// failure) or just the audit emit (LogAppend failure).
func ImportFromArchive(
	ctx context.Context,
	agentID string,
	archiveURI string,
	fetcher Fetcher,
	logger Logger,
) error {
	if !uuidPattern.MatchString(agentID) {
		return ErrInvalidEntry
	}
	if archiveURI == "" {
		return ErrInvalidEntry
	}
	if fetcher == nil {
		return ErrInvalidEntry
	}

	rc, err := fetcher.Get(ctx, archiveURI)
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	defer func() { _ = rc.Close() }()

	db, err := Open(ctx, agentID)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer func() { _ = db.Close() }()

	if err := db.Import(ctx, rc); err != nil {
		return fmt.Errorf("import: %w", err)
	}

	if logger == nil {
		return nil
	}

	payloadBytes, err := json.Marshal(importPayload{
		AgentID:    agentID,
		ArchiveURI: archiveURI,
		ImportedAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		// json.Marshal of three string fields cannot fail in practice;
		// surface it for completeness so the contract holds even in
		// pathological cases (e.g. a future field addition that reverts
		// to a non-marshalable type).
		return fmt.Errorf("audit marshal: %w", err)
	}

	if _, logErr := logger.LogAppend(ctx, keepclient.LogAppendRequest{
		EventType: importEventType,
		Payload:   payloadBytes,
	}); logErr != nil {
		return fmt.Errorf("audit emit: %w", logErr)
	}
	return nil
}
