package notebook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
)

// Logger is the minimal subset of the keepclient surface that
// [ArchiveOnRetire] requires for emitting the `notebook_archived` audit
// event. Defined as an interface in this package so tests can substitute a
// fake without standing up an HTTP server, and so callers retain the option
// of wrapping `*keepclient.Client` (e.g. with retry/backoff) without losing
// type compatibility. `*keepclient.Client` satisfies it as-is — the
// compile-time assertion lives in the test file.
type Logger interface {
	LogAppend(ctx context.Context, req keepclient.LogAppendRequest) (*keepclient.LogAppendResponse, error)
}

// Storer is the minimal subset of `archivestore.ArchiveStore` that
// [ArchiveOnRetire] needs (just `Put`). Defined locally rather than
// imported so this package does not pull in the archivestore package: the
// archivestore test files already import `notebook` (round-trip
// assertions against real Archive/Import bytes), and a notebook-side
// import of archivestore would close the cycle. Every concrete
// `archivestore.ArchiveStore` (LocalFS, S3Compatible, …) satisfies this
// interface structurally, so callers wire their existing store value
// straight in without any adapter shim.
type Storer interface {
	Put(ctx context.Context, agentID string, snapshot io.Reader) (uri string, err error)
}

// retirePayload is the JSON shape carried by the `notebook_archived` event
// emitted by [ArchiveOnRetire]. The three fields document _which_ agent's
// notebook was archived, _where_ the snapshot can be retrieved (the
// store-returned URI), and _when_ the archive completed (RFC3339Nano UTC).
// The shape is part of the audit-log contract — downstream consumers (M2b.7
// readers, the keep server's keepers_log subscribers) parse these fields by
// name.
type retirePayload struct {
	AgentID    string `json:"agent_id"`
	URI        string `json:"uri"`
	ArchivedAt string `json:"archived_at"`
}

// retireEventType is the `event_type` column written to keepers_log for the
// archive-on-retire audit event. Held as a const so tests can pin against
// the same string the production code emits.
const retireEventType = "notebook_archived"

// ArchiveOnRetire orchestrates the three primitives a graceful shutdown
// needs into a single call: it streams the live notebook through
// [DB.Archive] into the [Storer]'s Put (any
// `archivestore.ArchiveStore` impl satisfies the interface structurally)
// via an [io.Pipe], and on a successful Put emits a `notebook_archived`
// event to Keeper's Log via the supplied [Logger].
//
// Sequence:
//
//  1. Validate `agentID` is a canonical UUID. Returns [ErrInvalidEntry]
//     synchronously when malformed; no Archive / Put / LogAppend call is
//     made. (This mirrors the package-wide validation discipline so a
//     caller mistake fails fast and uniformly.)
//  2. Open an [io.Pipe]. In a goroutine, call `db.Archive(ctx, pipeWriter)`
//     and close the writer with the result, propagating any Archive error
//     to the consumer side via [io.PipeWriter.CloseWithError].
//  3. Call `store.Put(ctx, agentID, pipeReader)`. The Put consumes the
//     piped bytes and either persists them or returns an error.
//  4. Wait for the Archive goroutine to finish so its error (if any) is
//     drained — Put may have aborted the pipe via context cancellation
//     leaving Archive still running.
//  5. On Put success: marshal the audit payload (`agent_id`, `uri`,
//     `archived_at` in RFC3339Nano UTC) and call `logger.LogAppend(ctx,
//     {EventType: "notebook_archived", Payload: ...})`.
//
// # Partial-failure contract
//
// Callers depend on the (uri, error) return shape to distinguish three
// outcomes:
//
//   - Archive→Put failure: returns `("", error)`. The snapshot was either
//     not produced (Archive failed) or not stored (Put failed before /
//     during the upload). No retryable URI exists; the caller can re-run
//     ArchiveOnRetire later if the agent process is still alive, otherwise
//     the bytes are lost.
//   - Put→LogAppend failure: returns `(uri, error)`. The snapshot exists
//     in the store, only the audit emit failed. The caller has the URI in
//     hand and can retry just the LogAppend with the same payload shape
//     (the audit emit is idempotent at the keeper-log level — duplicate
//     events are tolerated by downstream subscribers).
//   - All success: returns `(uri, nil)`.
//
// The (uri, err) tuple in the LogAppend-failure path is the contract — do
// not collapse it to ("", err), or callers lose the ability to retry the
// audit emit independently.
//
// # Context cancellation
//
// A cancelled `ctx` aborts the in-flight Archive goroutine (the pipe writer
// is closed with `ctx.Err()`), the pending Put (every shipping
// `archivestore.ArchiveStore` impl honours ctx), and the pending
// LogAppend ([keepclient.Client.LogAppend] honours ctx). The returned
// error wraps [context.Canceled] / [context.DeadlineExceeded] so
// `errors.Is` works.
//
// # Recommended call site
//
// During graceful shutdown of the harness/agent process. A `defer` against
// a shutdown context with a generous timeout (10–30s) is the canonical
// shape. ArchiveOnRetire builds in NO retry logic — the caller decides
// whether to retry the whole call (Archive→Put failure) or just the audit
// emit (Put→LogAppend failure).
func ArchiveOnRetire(ctx context.Context, db *DB, agentID string, store Storer, logger Logger) (string, error) {
	if !uuidPattern.MatchString(agentID) {
		return "", ErrInvalidEntry
	}

	pr, pw := io.Pipe()

	// Buffered so the goroutine can deposit its result and exit without
	// blocking, even if Put returned ahead of it (e.g. on context cancel).
	archiveErrCh := make(chan error, 1)
	go func() {
		err := db.Archive(ctx, pw)
		// CloseWithError(nil) is documented as equivalent to Close(); the
		// reader observes io.EOF rather than a wrapped nil. On error, the
		// reader's next Read returns the same error so Put surfaces it.
		_ = pw.CloseWithError(err)
		archiveErrCh <- err
	}()

	uri, putErr := store.Put(ctx, agentID, pr)
	if putErr != nil {
		// Unblock the Archive goroutine if it is still blocked on pw.Write.
		// A real ArchiveStore implementation may return an error before
		// reading any bytes (auth failure, ECONNREFUSED, etc.). Without this
		// call, pw.Write would block indefinitely waiting for a reader, and
		// the goroutine would leak. CloseWithError makes the next pw.Write
		// return io.ErrClosedPipe so the goroutine exits cleanly.
		_ = pr.CloseWithError(putErr)
	}
	// Drain the goroutine's result AFTER Put returns so we cannot race a
	// still-running Archive. CloseWithError above guarantees the goroutine
	// always finishes — Archive returns once VACUUM and io.Copy unwind, and
	// CloseWithError is non-blocking.
	archiveErr := <-archiveErrCh

	// Surface Archive's error first when both are non-nil: a Put failure
	// triggered by ctx cancel during Archive is downstream of the original
	// Archive error (or vice versa), and Archive sits upstream of Put in
	// the pipeline so its error is the more meaningful root cause.
	if archiveErr != nil {
		return "", fmt.Errorf("archive: %w", archiveErr)
	}
	if putErr != nil {
		return "", fmt.Errorf("put: %w", putErr)
	}

	payloadBytes, err := json.Marshal(retirePayload{
		AgentID:    agentID,
		URI:        uri,
		ArchivedAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		// json.Marshal of three string fields cannot fail in practice; if
		// it does, the snapshot is already in the store and the caller
		// should know about it. Surface the URI alongside the error so the
		// partial-failure contract holds.
		return uri, fmt.Errorf("audit marshal: %w", err)
	}

	if _, logErr := logger.LogAppend(ctx, keepclient.LogAppendRequest{
		EventType: retireEventType,
		Payload:   payloadBytes,
	}); logErr != nil {
		return uri, fmt.Errorf("audit emit: %w", logErr)
	}
	return uri, nil
}
