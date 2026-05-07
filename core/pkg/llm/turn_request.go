package llm

import (
	"context"
	"errors"
	"fmt"

	"github.com/vadimtrunov/watchkeepers/core/pkg/notebook"
	"github.com/vadimtrunov/watchkeepers/core/pkg/runtime"
)

// MetadataKeyRecalledMemoryStatus is the reserved [CompleteRequest.Metadata]
// key the manifest-aware turn helper [BuildTurnRequest] populates with one of
// the [RecalledMemoryStatus*] string constants. Callers reading the metadata
// bag downstream match the same key without hard-coding the string at every
// site.
const MetadataKeyRecalledMemoryStatus = "recalled_memory_status"

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

	status, memOpt, recallErr := resolveRecalledMemory(ctx, manifest, query, embedder, supervisor)

	// Build the base request with the recalled-memory option applied first
	// so caller `opts` can override (caller-last-wins). Use a synthetic
	// single-message slice so [composeBaseFields] does not reject the call;
	// the M6 wire-up lands the real user-turn message.
	msgs := []Message{{Role: RoleUser, Content: query}}

	allOpts := make([]RequestOption, 0, 1+len(opts))
	if memOpt != nil {
		allOpts = append(allOpts, memOpt)
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
	req.Metadata[MetadataKeyRecalledMemoryStatus] = status

	return &req, recallErr
}

// resolveRecalledMemory walks the fail-soft matrix and returns:
//
//   - the [MetadataKeyRecalledMemoryStatus] string the helper will stamp,
//   - the [RequestOption] to apply (nil unless status == "applied"),
//   - the joined error to surface to strict-mode callers (nil except for
//     embed_error / recall_error).
//
// The function is internal to the helper; its signature is shaped to keep
// [BuildTurnRequest] linear and readable.
func resolveRecalledMemory(
	ctx context.Context,
	manifest runtime.Manifest,
	query string,
	embedder EmbeddingProvider,
	supervisor *runtime.NotebookSupervisor,
) (string, RequestOption, error) {
	if manifest.NotebookTopK <= 0 {
		return RecalledMemoryStatusDisabled, nil, nil
	}

	if supervisor == nil {
		return RecalledMemoryStatusAgentNotRegistered, nil, nil
	}
	db, ok := supervisor.Lookup(manifest.AgentID)
	if !ok {
		return RecalledMemoryStatusAgentNotRegistered, nil, nil
	}

	if embedder == nil {
		return RecalledMemoryStatusEmbedError, nil, errors.Join(fmt.Errorf("llm: BuildTurnRequest: nil embedder"))
	}

	vec, err := embedder.Embed(ctx, query)
	if err != nil {
		return RecalledMemoryStatusEmbedError, nil, errors.Join(fmt.Errorf("llm: embed query: %w", err))
	}

	results, err := db.Recall(ctx, notebook.RecallQuery{
		Embedding: vec,
		TopK:      manifest.NotebookTopK,
	})
	if err != nil {
		return RecalledMemoryStatusRecallError, nil, errors.Join(fmt.Errorf("llm: notebook recall: %w", err))
	}

	if len(results) == 0 {
		return RecalledMemoryStatusNoMatches, nil, nil
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
		return RecalledMemoryStatusNoMatches, nil, nil
	}

	return RecalledMemoryStatusApplied, WithRecalledMemory(memories...), nil
}
