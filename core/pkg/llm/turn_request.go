package llm

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/vadimtrunov/watchkeepers/core/pkg/notebook"
	"github.com/vadimtrunov/watchkeepers/core/pkg/runtime"
)

// MetadataKeyRecalledMemoryStatus is the reserved [CompleteRequest.Metadata]
// key the manifest-aware turn helper [BuildTurnRequest] populates with one of
// the [RecalledMemoryStatus*] string constants. Callers reading the metadata
// bag downstream match the same key without hard-coding the string at every
// site.
const MetadataKeyRecalledMemoryStatus = "recalled_memory_status"

// MetadataKeyCoolingOffFiltered is the reserved [CompleteRequest.Metadata]
// key [BuildTurnRequest] populates with the count of notebook entries
// excluded from this turn's recall by the cooling-off
// (`active_after > now`) predicate. The value is a base-10 integer string
// (e.g. "1", "3"); callers parse it with [strconv.Atoi].
//
// The key is set ONLY when the count is strictly positive — a zero count
// yields no key in the metadata map, mirroring the
// [MetadataKeyNeedsReviewFiltered] semantics. This avoids polluting the
// metadata bag with zero-valued entries on the common no-filter path.
//
// The counter source is M5.6.d's [notebook.DB.RecallFilterCounts] which
// runs a cheap second `SELECT COUNT(*)` after the main recall succeeds;
// failures of the counter helper are swallowed (best-effort) so the turn
// continues without the diagnostic key — see the [BuildTurnRequest]
// fail-soft matrix.
const MetadataKeyCoolingOffFiltered = "cooling_off_filtered"

// MetadataKeyNeedsReviewFiltered is the reserved [CompleteRequest.Metadata]
// key [BuildTurnRequest] populates with the count of notebook entries
// excluded from this turn's recall by the needs_review = 1 predicate
// (M5.6.a). Same string-encoded-int + zero-suppression semantics as
// [MetadataKeyCoolingOffFiltered].
const MetadataKeyNeedsReviewFiltered = "needs_review_filtered"

// recallFilterCountsFn is the package-private seam through which
// [BuildTurnRequest] invokes the M5.6.d filter-counts helper. Production
// resolves to [(*notebook.DB).RecallFilterCounts]; tests swap in a stub
// to inject failures (the negative test plan bullet "counter query
// fails"). Callers MUST NOT mutate this from non-test code.
//
// The seam is a function variable rather than a full interface because
// the counter helper is a single-method dependency with a stable
// signature; an interface would add type machinery without enabling
// additional substitutability.
var recallFilterCountsFn = func(ctx context.Context, db *notebook.DB, q notebook.RecallQuery) (int, int, error) {
	return db.RecallFilterCounts(ctx, q)
}

// RecalledMemoryStatus* constants discriminate the six recall-pipeline
// outcomes [BuildTurnRequest] surfaces via [MetadataKeyRecalledMemoryStatus]
// on the returned [CompleteRequest]. Strict-mode callers MAY also inspect
// the helper's returned `error` (non-nil for embed_error / recall_error so
// the joined cause is observable); fail-soft callers ignore the error and
// rely on the metadata status alone.
const (
	// RecalledMemoryStatusApplied indicates the happy path: embed succeeded,
	// recall returned at least one match, and [WithRecalledMemory] was
	// applied to the request's System slot.
	RecalledMemoryStatusApplied = "applied"

	// RecalledMemoryStatusDisabled indicates [runtime.Manifest.NotebookTopK]
	// was zero — recall is disabled by manifest. Embed and recall are
	// skipped entirely.
	RecalledMemoryStatusDisabled = "disabled_topk_zero"

	// RecalledMemoryStatusAgentNotRegistered indicates
	// [runtime.NotebookSupervisor.Lookup] returned `(nil, false)` — the
	// agent's notebook was not opened. Embed and recall are skipped.
	RecalledMemoryStatusAgentNotRegistered = "agent_not_registered"

	// RecalledMemoryStatusEmbedError indicates [EmbeddingProvider.Embed]
	// returned an error. The error is joined to the helper's returned
	// `error` via [errors.Join]; the request is still usable.
	RecalledMemoryStatusEmbedError = "embed_error"

	// RecalledMemoryStatusRecallError indicates [notebook.DB.Recall]
	// returned an error. The error is joined to the helper's returned
	// `error` via [errors.Join]; the request is still usable.
	RecalledMemoryStatusRecallError = "recall_error"

	// RecalledMemoryStatusNoMatches indicates [notebook.DB.Recall]
	// succeeded but returned an empty slice — nothing to inject.
	RecalledMemoryStatusNoMatches = "no_matches"
)

// recallResultsToMemories projects each [notebook.RecallResult] 1:1 to a
// [RecalledMemory], copying Subject and Content verbatim and projecting the
// caller-side relevance score. The notebook layer reports cosine
// `Distance` (in [0, 2], lower is closer); we convert to a [0, 1] relevance
// score via `1 - Distance/2` and clamp to the valid range so callers see a
// portable "higher is better" number regardless of the underlying metric.
//
// Returns nil when `results` is nil; returns an empty (non-nil) slice when
// `results` is an empty (non-nil) slice. The renderer tolerates both.
func recallResultsToMemories(results []notebook.RecallResult) []RecalledMemory {
	if results == nil {
		return nil
	}
	out := make([]RecalledMemory, 0, len(results))
	for _, r := range results {
		score := float32(1 - r.Distance/2)
		if score < 0 {
			score = 0
		}
		if score > 1 {
			score = 1
		}
		out = append(out, RecalledMemory{
			Subject: r.Subject,
			Content: r.Content,
			Score:   score,
		})
	}
	return out
}

// BuildTurnRequest assembles a per-turn [CompleteRequest] for the agent
// described by `manifest`, augmenting the System slot with recalled-memory
// entries pulled from the agent's notebook via the supplied `embedder` and
// `supervisor`. The four collaborators are wired in via prior sub-items of
// M5.5.c:
//
//   - `manifest` carries [runtime.Manifest.AgentID],
//     [runtime.Manifest.NotebookTopK] and
//     [runtime.Manifest.NotebookRelevanceThreshold] (M5.5.c.b).
//   - `supervisor` exposes per-agent [notebook.DB] handles via
//     [runtime.NotebookSupervisor.Lookup] (M5.5.c.c).
//   - `embedder` converts `query` into a dense embedding vector
//     (M5.5.c.d.a).
//   - The recall results are injected into the System slot via
//     [WithRecalledMemory] (M5.5.c.d.b.a).
//
// # Fail-soft matrix
//
// The helper ALWAYS returns a usable [*CompleteRequest] except when `ctx` is
// already cancelled at entry — that is the single hard-error case. Recall-
// pipeline outcomes are surfaced via the `Metadata[MetadataKeyRecalledMemoryStatus]`
// key on the returned request:
//
//   - `manifest.NotebookTopK <= 0` → status `disabled_topk_zero`; no embed,
//     no recall, no [WithRecalledMemory] applied. Zero means "disabled by
//     manifest"; negative values are treated identically (manifest-shape
//     pathology, not a recall-pipeline failure).
//   - `supervisor.Lookup(manifest.AgentID)` returns `false` → status
//     `agent_not_registered`; no embed, no recall.
//   - `embedder.Embed` returns an error → status `embed_error`; the embed
//     error is joined to the helper's returned `error` via [errors.Join] so
//     strict-mode callers see the cause.
//   - `db.Recall` returns an error → status `recall_error`; the recall
//     error is joined to the helper's returned `error`.
//   - `db.Recall` returns an empty slice → status `no_matches`; no
//     [WithRecalledMemory] applied.
//   - Happy path → status `applied`; results projected to []RecalledMemory,
//     post-filtered by `Score >= manifest.NotebookRelevanceThreshold`, and
//     applied via [WithRecalledMemory]. When the threshold is 0 (default
//     unset) every result passes (Score is always >= 0). When all results
//     are filtered out the status falls to `no_matches`.
//
// # Caller options
//
// Caller-supplied `opts` are applied AFTER the recalled-memory option, so a
// caller can override System or Metadata if needed (caller-last-wins,
// matching the established [BuildCompleteRequest] / [BuildStreamRequest] /
// [BuildCountTokensRequest] semantics). The helper installs the
// [MetadataKeyRecalledMemoryStatus] entry AFTER `opts` so the status is
// authoritative; a caller wishing to override the status MUST do so via a
// post-processing step on the returned request.
//
// # Inputs the helper does NOT accept
//
// The current shape uses an empty [Message] slice and relies on the caller
// to seed the user-turn message via a [RequestOption] (or by mutating
// `req.Messages` after the helper returns). M5.5.c.d.b.b is the seam — M6
// will wire the per-turn message construction at the call site.
func BuildTurnRequest(
	ctx context.Context,
	manifest runtime.Manifest,
	query string,
	embedder EmbeddingProvider,
	supervisor *runtime.NotebookSupervisor,
	opts ...RequestOption,
) (*CompleteRequest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	resolution := resolveRecalledMemory(ctx, manifest, query, embedder, supervisor)

	// Build the base request with the recalled-memory option applied first
	// so caller `opts` can override (caller-last-wins). Use a synthetic
	// single-message slice so [composeBaseFields] does not reject the call;
	// the M6 wire-up lands the real user-turn message.
	msgs := []Message{{Role: RoleUser, Content: query}}

	allOpts := make([]RequestOption, 0, 1+len(opts))
	if resolution.option != nil {
		allOpts = append(allOpts, resolution.option)
	}
	allOpts = append(allOpts, opts...)

	req, err := BuildCompleteRequest(manifest, msgs, allOpts...)
	if err != nil {
		// composeBaseFields rejected the manifest (e.g. empty Model /
		// SystemPrompt). Surface the validation error verbatim — the
		// fail-soft matrix only covers the recall pipeline, not manifest
		// shape errors.
		return nil, err
	}

	// Stamp the recall-pipeline status on the metadata bag AFTER caller
	// options so the status is authoritative.
	if req.Metadata == nil {
		req.Metadata = make(map[string]string, 1)
	}
	req.Metadata[MetadataKeyRecalledMemoryStatus] = resolution.status

	// M5.6.d diagnostic counters: surface the number of rows the
	// SQL-level cooling-off / needs_review predicates excluded from this
	// turn's recall. Only set keys with strictly-positive counts so a
	// zero-filtered turn produces no metadata noise (AC3 / AC6). Counter
	// helper failures are silently swallowed (best-effort) — the recall
	// itself succeeded, so the turn must continue without the diagnostic.
	if resolution.coolingOffFiltered > 0 {
		req.Metadata[MetadataKeyCoolingOffFiltered] = strconv.Itoa(resolution.coolingOffFiltered)
	}
	if resolution.needsReviewFiltered > 0 {
		req.Metadata[MetadataKeyNeedsReviewFiltered] = strconv.Itoa(resolution.needsReviewFiltered)
	}

	return &req, resolution.err
}

// recalledMemoryResolution bundles the four outputs of
// [resolveRecalledMemory]: the metadata status string, the
// [RequestOption] to apply (nil unless status == "applied"), the joined
// error to surface to strict-mode callers, and the M5.6.d diagnostic
// counters returned by the filter-counts helper. The struct keeps
// [BuildTurnRequest] linear without exploding the helper's return
// arity.
type recalledMemoryResolution struct {
	status              string
	option              RequestOption
	err                 error
	coolingOffFiltered  int
	needsReviewFiltered int
}

// resolveRecalledMemory walks the fail-soft matrix and returns a
// [recalledMemoryResolution] capturing the metadata status, the
// [RequestOption] to apply (nil unless status == "applied"), the joined
// error to surface to strict-mode callers (nil except for embed_error /
// recall_error), and the M5.6.d diagnostic filter counts. The filter
// counts are populated ONLY when a recall actually executed (status
// `applied` or `no_matches`); short-circuit branches (`disabled_topk_zero`,
// `agent_not_registered`, `embed_error`, `recall_error`) leave them at
// their zero values so the caller's "set key only when > 0" rule
// naturally suppresses them.
//
// Filter-counts helper failures are silently swallowed: the recall
// itself succeeded so the turn proceeds; only the diagnostic counters
// are skipped. Mirrors the best-effort observer idiom from M5.6.b/c
// where a side-channel telemetry failure must not regress the primary
// outcome.
//
// The function is internal to the helper; its signature is shaped to
// keep [BuildTurnRequest] linear and readable.
func resolveRecalledMemory(
	ctx context.Context,
	manifest runtime.Manifest,
	query string,
	embedder EmbeddingProvider,
	supervisor *runtime.NotebookSupervisor,
) recalledMemoryResolution {
	if manifest.NotebookTopK <= 0 {
		return recalledMemoryResolution{status: RecalledMemoryStatusDisabled}
	}

	if supervisor == nil {
		return recalledMemoryResolution{status: RecalledMemoryStatusAgentNotRegistered}
	}
	db, ok := supervisor.Lookup(manifest.AgentID)
	if !ok {
		return recalledMemoryResolution{status: RecalledMemoryStatusAgentNotRegistered}
	}

	if embedder == nil {
		return recalledMemoryResolution{
			status: RecalledMemoryStatusEmbedError,
			err:    errors.Join(fmt.Errorf("llm: BuildTurnRequest: nil embedder")),
		}
	}

	vec, err := embedder.Embed(ctx, query)
	if err != nil {
		return recalledMemoryResolution{
			status: RecalledMemoryStatusEmbedError,
			err:    errors.Join(fmt.Errorf("llm: embed query: %w", err)),
		}
	}

	recallQuery := notebook.RecallQuery{
		Embedding: vec,
		TopK:      manifest.NotebookTopK,
	}
	results, err := db.Recall(ctx, recallQuery)
	if err != nil {
		return recalledMemoryResolution{
			status: RecalledMemoryStatusRecallError,
			err:    errors.Join(fmt.Errorf("llm: notebook recall: %w", err)),
		}
	}

	// Filter counts are computed once for any path where a recall
	// executed (success or empty results). The seam allows tests to
	// inject a sentinel failure; production resolves to
	// [(*notebook.DB).RecallFilterCounts]. Errors are best-effort: log
	// nothing (no logger threaded through this layer) and leave the
	// counts at zero so the metadata keys stay absent.
	coolingOff, needsReview, _ := recallFilterCountsFn(ctx, db, recallQuery)

	if len(results) == 0 {
		return recalledMemoryResolution{
			status:              RecalledMemoryStatusNoMatches,
			coolingOffFiltered:  coolingOff,
			needsReviewFiltered: needsReview,
		}
	}

	memories := recallResultsToMemories(results)

	// Post-filter by relevance threshold. Threshold == 0 is a no-op (every
	// Score is >= 0). Negative thresholds are also no-ops by the same math.
	threshold := float32(manifest.NotebookRelevanceThreshold)
	if threshold > 0 {
		filtered := memories[:0]
		for _, m := range memories {
			if m.Score >= threshold {
				filtered = append(filtered, m)
			}
		}
		memories = filtered
	}

	if len(memories) == 0 {
		return recalledMemoryResolution{
			status:              RecalledMemoryStatusNoMatches,
			coolingOffFiltered:  coolingOff,
			needsReviewFiltered: needsReview,
		}
	}

	return recalledMemoryResolution{
		status:              RecalledMemoryStatusApplied,
		option:              WithRecalledMemory(memories...),
		coolingOffFiltered:  coolingOff,
		needsReviewFiltered: needsReview,
	}
}
