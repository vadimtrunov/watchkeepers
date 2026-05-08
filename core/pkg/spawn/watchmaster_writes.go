// watchmaster_writes.go implements the three M6.2.b lead-approval-gated
// Watchmaster manifest-bump tools that each draft a new
// `manifest_version` row via [keepclient.PutManifestVersion]:
//
//  1. ProposeSpawn       — drafts the very first manifest_version for a
//     freshly-allocated `agent_id` (manifest UUID), carrying the
//     proposed personality + language. The actual spawn-saga (Slack App
//     creation, watchkeeper row insert) is M7's job; M6.2.b only
//     persists the manifest fields.
//  2. AdjustPersonality  — loads the existing latest manifest_version
//     for an `agent_id`, copies fields, overrides Personality, and
//     writes a new version row.
//  3. AdjustLanguage     — same shape as AdjustPersonality but
//     overrides Language.
//
// All three: mirror the M6.1.b SlackAppRPC.CreateApp gate stack —
// validate `claim.OrganizationID`, validate
// `claim.AuthorityMatrix[<tool>] == "lead_approval"`, validate
// `req.ApprovalToken` non-empty, emit `<tool>_requested` →
// `<tool>_succeeded` | `<tool>_failed` keepers_log audit chain. NO
// secret-source read is needed (the underlying [keepclient.Client]
// already carries the keep capability token via its TokenSource).
package spawn

import (
	"context"
	"fmt"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
	"github.com/vadimtrunov/watchkeepers/core/pkg/keeperslog"
)

// authorityActionPropose / authorityActionAdjustPersonality /
// authorityActionAdjustLanguage are the action strings the Watchmaster
// manifest publishes in `authority_matrix` to gate each manifest-bump
// tool. Hoisted to package constants so a re-key (e.g. a future
// per-environment suffix) is a one-line change here that the gate AND
// the keepers_log payload pick up via the compiler.
const (
	authorityActionPropose           = "propose_spawn"
	authorityActionAdjustPersonality = "adjust_personality"
	authorityActionAdjustLanguage    = "adjust_language"
)

// keepersLogEventType* values the manifest-bump tools emit on the
// audit chain. Pinned per AC2 / TASK Scope. Project convention:
// snake_case `<tool>_<event_class>` past tense, prefixed with the
// surface owner (`watchmaster`) so a downstream consumer can group
// audit rows by emitting agent without parsing the payload.
const (
	keepersLogEventProposeRequested           = "watchmaster_propose_spawn_requested"
	keepersLogEventProposeSucceeded           = "watchmaster_propose_spawn_succeeded"
	keepersLogEventProposeFailed              = "watchmaster_propose_spawn_failed"
	keepersLogEventAdjustPersonalityRequested = "watchmaster_adjust_personality_requested"
	keepersLogEventAdjustPersonalitySucceeded = "watchmaster_adjust_personality_succeeded"
	keepersLogEventAdjustPersonalityFailed    = "watchmaster_adjust_personality_failed"
	keepersLogEventAdjustLanguageRequested    = "watchmaster_adjust_language_requested"
	keepersLogEventAdjustLanguageSucceeded    = "watchmaster_adjust_language_succeeded"
	keepersLogEventAdjustLanguageFailed       = "watchmaster_adjust_language_failed"
)

// payloadKey* and payloadValue* constants reused across the three
// manifest-bump tools. Keep aligned with M6.1.b's slack_app.go set —
// shared keys (agent_id, tool_name, event_class, error_class) live
// there; this file adds only manifest-bump-specific keys.
const (
	payloadKeyVersionID            = "manifest_version_id"
	payloadKeyVersionNo            = "version_no"
	payloadKeyApprovalTokenPresent = "approval_token_present"

	payloadValueProposeSpawn      = "propose_spawn"
	payloadValueAdjustPersonality = "adjust_personality"
	payloadValueAdjustLanguage    = "adjust_language"
)

// WatchmasterWriteClient is the minimal subset of the keepclient surface
// the Watchmaster write-side tools consume. Defined as an interface in
// this package so tests can substitute a hand-rolled fake without
// standing up an HTTP server, and so production code never imports the
// concrete `*keepclient.Client` type at all (mirrors the
// keeperslog.LocalKeepClient + spawn.WatchmasterReadClient
// import-cycle-break pattern documented in `docs/LESSONS.md`).
//
// `*keepclient.Client` satisfies this interface as-is; the compile-time
// assertion lives below.
//
// The first two methods (GetManifest, PutManifestVersion) back the
// M6.2.b manifest-bump tools (propose_spawn / adjust_personality /
// adjust_language). The third method (UpdateWatchkeeperStatus) backs
// the M6.2.c retire_watchkeeper tool — a status-row mutation, not a
// manifest_version write. The fourth method (Store) backs the M6.2.d
// promote_to_keep tool — inserts a fresh `knowledge_chunk` row from a
// notebook proposal.
type WatchmasterWriteClient interface {
	GetManifest(ctx context.Context, manifestID string) (*keepclient.ManifestVersion, error)
	PutManifestVersion(
		ctx context.Context,
		manifestID string,
		req keepclient.PutManifestVersionRequest,
	) (*keepclient.PutManifestVersionResponse, error)
	UpdateWatchkeeperStatus(ctx context.Context, id, status string) error
	Store(ctx context.Context, req keepclient.StoreRequest) (*keepclient.StoreResponse, error)
}

// Compile-time assertion: every [*keepclient.Client] satisfies
// [WatchmasterWriteClient] by definition. Pins the integration shape
// against future drift in the keepclient package.
var _ WatchmasterWriteClient = (*keepclient.Client)(nil)

// keepersLogAppender is reused from slack_app.go (same package) — the
// Append-only subset of [*keeperslog.Writer]. Both privileged-RPC
// surfaces share the audit-chain seam.

// ── ProposeSpawn ─────────────────────────────────────────────────────────────

// ProposeSpawnRequest is the value supplied to [ProposeSpawn]. The
// caller is responsible for allocating `AgentID` (a fresh manifest
// UUID) before this call lands; M6.2.b's job is to persist the first
// manifest_version row carrying the proposed personality + language.
// The spawn-saga (Slack App creation, watchkeeper row insert) is M7.
type ProposeSpawnRequest struct {
	// AgentID is the freshly-allocated manifest UUID the new
	// watchkeeper will be pinned to. Required; empty fails
	// synchronously with [ErrInvalidRequest].
	AgentID string

	// SystemPrompt is the manifest system prompt text. Required by
	// the underlying [keepclient.PutManifestVersion]; empty fails
	// synchronously with [ErrInvalidRequest].
	SystemPrompt string

	// Personality is the proposed free-text personality (≤ 1024
	// runes per the keepclient cap). Optional — empty round-trips
	// as SQL NULL on the server.
	Personality string

	// Language is the proposed language code (BCP 47-lite shape).
	// Optional — empty round-trips as SQL NULL on the server.
	Language string

	// ApprovalToken is the opaque token the lead-approval saga
	// (M6.3) issues. M6.2.b validation is non-empty only;
	// M6.3 owns the cryptography. Required; empty fails with
	// [ErrApprovalRequired] AFTER the authority-matrix gate
	// passes (so a forbidden caller is told `unauthorized`,
	// not `approval required`).
	ApprovalToken string
}

// ManifestBumpResult is the value returned from a successful
// [ProposeSpawn] / [AdjustPersonality] / [AdjustLanguage] call.
// Carries the freshly-inserted manifest_version UUID and its
// version_no so the caller can correlate the new row to the audit
// chain without re-reading the manifest.
type ManifestBumpResult struct {
	// ManifestVersionID is the freshly-inserted manifest_version row
	// UUID. Always populated on success.
	ManifestVersionID string
	// VersionNo is the monotonically-increasing version number the
	// new row carries (1 for ProposeSpawn, prev+1 for adjustments).
	VersionNo int
}

// ProposeSpawn validates the lead-approval gate stack and writes the
// initial manifest_version row for a freshly-allocated agent_id.
//
// Resolution order (pinned by M6.1.b precedent — see slack_app.go):
//
//  1. Validate ctx (cancellation takes precedence over input shape).
//  2. Validate claim.OrganizationID non-empty (M3.5.a discipline) →
//     [ErrInvalidClaim].
//  3. Validate claim.AuthorityMatrix["propose_spawn"] equals
//     "lead_approval" → [ErrUnauthorized] otherwise.
//  4. Validate req.AgentID + req.SystemPrompt non-empty →
//     [ErrInvalidRequest] otherwise.
//  5. Validate req.ApprovalToken non-empty → [ErrApprovalRequired]
//     otherwise. (Order matters: gate runs BEFORE token check.)
//  6. Emit `watchmaster_propose_spawn_requested` keepers_log event
//     BEFORE the keep write — `no audit row, no privileged action`.
//  7. Call writer.PutManifestVersion (VersionNo=1, the first row).
//  8. On success, emit `watchmaster_propose_spawn_succeeded`. On
//     failure, emit `watchmaster_propose_spawn_failed` and return the
//     wrapped error.
func ProposeSpawn(
	ctx context.Context,
	client WatchmasterWriteClient,
	logger keepersLogAppender,
	req ProposeSpawnRequest,
	claim Claim,
) (ManifestBumpResult, error) {
	if err := ctx.Err(); err != nil {
		return ManifestBumpResult{}, err
	}
	if err := validateClaimAndDeps(client, logger, claim); err != nil {
		return ManifestBumpResult{}, err
	}
	if !claim.HasAuthority(authorityActionPropose) {
		return ManifestBumpResult{}, ErrUnauthorized
	}
	if req.AgentID == "" {
		return ManifestBumpResult{}, fmt.Errorf("%w: empty AgentID", ErrInvalidRequest)
	}
	if req.SystemPrompt == "" {
		return ManifestBumpResult{}, fmt.Errorf("%w: empty SystemPrompt", ErrInvalidRequest)
	}
	if req.ApprovalToken == "" {
		return ManifestBumpResult{}, ErrApprovalRequired
	}

	if _, err := logger.Append(ctx, manifestBumpRequestedEvent(
		keepersLogEventProposeRequested,
		payloadValueProposeSpawn,
		req.AgentID,
		claim,
	)); err != nil {
		return ManifestBumpResult{}, fmt.Errorf("spawn: keepers_log requested: %w", err)
	}

	resp, err := client.PutManifestVersion(ctx, req.AgentID, keepclient.PutManifestVersionRequest{
		VersionNo:    1,
		SystemPrompt: req.SystemPrompt,
		Personality:  req.Personality,
		Language:     req.Language,
	})
	if err != nil {
		_, _ = logger.Append(ctx, manifestBumpFailedEvent(
			keepersLogEventProposeFailed,
			payloadValueProposeSpawn,
			req.AgentID,
			claim,
			err,
		))
		return ManifestBumpResult{}, fmt.Errorf("spawn: propose_spawn put_manifest_version: %w", err)
	}

	_, _ = logger.Append(ctx, manifestBumpSucceededEvent(
		keepersLogEventProposeSucceeded,
		payloadValueProposeSpawn,
		req.AgentID,
		claim,
		resp.ID,
		1,
	))
	return ManifestBumpResult{ManifestVersionID: resp.ID, VersionNo: 1}, nil
}

// ── AdjustPersonality ────────────────────────────────────────────────────────

// AdjustPersonalityRequest is the value supplied to [AdjustPersonality].
// The caller supplies the existing watchkeeper's `AgentID` (manifest
// UUID) plus the new personality text; the tool reads the current
// latest manifest_version, copies its fields, overrides Personality,
// and writes a new version row.
type AdjustPersonalityRequest struct {
	// AgentID is the existing watchkeeper's manifest UUID. Required;
	// empty fails synchronously with [ErrInvalidRequest].
	AgentID string

	// NewPersonality is the new personality text to write onto the
	// fresh manifest_version row. Empty values are allowed
	// (round-trips as SQL NULL on the server — the keepclient
	// preflight only caps the upper bound at 1024 runes).
	NewPersonality string

	// ApprovalToken is the opaque token the lead-approval saga
	// (M6.3) issues. Required; empty fails with [ErrApprovalRequired]
	// AFTER the authority-matrix gate passes.
	ApprovalToken string
}

// AdjustPersonality validates the lead-approval gate stack, reads the
// existing latest manifest_version, copies fields, overrides
// Personality, and writes a new version row.
//
// Resolution order mirrors [ProposeSpawn]; see that godoc for detail.
func AdjustPersonality(
	ctx context.Context,
	client WatchmasterWriteClient,
	logger keepersLogAppender,
	req AdjustPersonalityRequest,
	claim Claim,
) (ManifestBumpResult, error) {
	if err := ctx.Err(); err != nil {
		return ManifestBumpResult{}, err
	}
	if err := validateClaimAndDeps(client, logger, claim); err != nil {
		return ManifestBumpResult{}, err
	}
	if !claim.HasAuthority(authorityActionAdjustPersonality) {
		return ManifestBumpResult{}, ErrUnauthorized
	}
	if req.AgentID == "" {
		return ManifestBumpResult{}, fmt.Errorf("%w: empty AgentID", ErrInvalidRequest)
	}
	if req.ApprovalToken == "" {
		return ManifestBumpResult{}, ErrApprovalRequired
	}

	return runManifestBump(
		ctx, client, logger, claim,
		manifestBumpEventTypes{
			Requested: keepersLogEventAdjustPersonalityRequested,
			Succeeded: keepersLogEventAdjustPersonalitySucceeded,
			Failed:    keepersLogEventAdjustPersonalityFailed,
			ToolName:  payloadValueAdjustPersonality,
		},
		req.AgentID,
		func(latest *keepclient.ManifestVersion) keepclient.PutManifestVersionRequest {
			next := copyManifestForBump(latest)
			next.Personality = req.NewPersonality
			return next
		},
	)
}

// ── AdjustLanguage ───────────────────────────────────────────────────────────

// AdjustLanguageRequest is the value supplied to [AdjustLanguage].
// Same shape as [AdjustPersonalityRequest] but carries the new
// language code instead.
type AdjustLanguageRequest struct {
	// AgentID is the existing watchkeeper's manifest UUID. Required.
	AgentID string

	// NewLanguage is the new language code (BCP 47-lite shape) to
	// write onto the fresh manifest_version row. Empty values are
	// allowed (round-trips as SQL NULL on the server).
	NewLanguage string

	// ApprovalToken is the opaque token the lead-approval saga
	// (M6.3) issues.
	ApprovalToken string
}

// AdjustLanguage validates the lead-approval gate stack, reads the
// existing latest manifest_version, copies fields, overrides Language,
// and writes a new version row.
func AdjustLanguage(
	ctx context.Context,
	client WatchmasterWriteClient,
	logger keepersLogAppender,
	req AdjustLanguageRequest,
	claim Claim,
) (ManifestBumpResult, error) {
	if err := ctx.Err(); err != nil {
		return ManifestBumpResult{}, err
	}
	if err := validateClaimAndDeps(client, logger, claim); err != nil {
		return ManifestBumpResult{}, err
	}
	if !claim.HasAuthority(authorityActionAdjustLanguage) {
		return ManifestBumpResult{}, ErrUnauthorized
	}
	if req.AgentID == "" {
		return ManifestBumpResult{}, fmt.Errorf("%w: empty AgentID", ErrInvalidRequest)
	}
	if req.ApprovalToken == "" {
		return ManifestBumpResult{}, ErrApprovalRequired
	}

	return runManifestBump(
		ctx, client, logger, claim,
		manifestBumpEventTypes{
			Requested: keepersLogEventAdjustLanguageRequested,
			Succeeded: keepersLogEventAdjustLanguageSucceeded,
			Failed:    keepersLogEventAdjustLanguageFailed,
			ToolName:  payloadValueAdjustLanguage,
		},
		req.AgentID,
		func(latest *keepclient.ManifestVersion) keepclient.PutManifestVersionRequest {
			next := copyManifestForBump(latest)
			next.Language = req.NewLanguage
			return next
		},
	)
}

// ── shared helpers ───────────────────────────────────────────────────────────

// manifestBumpEventTypes bundles the three event_type strings + the
// tool-name payload string a manifest-bump tool emits onto the audit
// chain. Held in a struct so [runManifestBump] takes a single
// parameter rather than four parallel strings — keeps the call sites
// in [AdjustPersonality] / [AdjustLanguage] scannable.
type manifestBumpEventTypes struct {
	Requested string
	Succeeded string
	Failed    string
	ToolName  string
}

// runManifestBump is the shared adjust-flow body: read existing
// manifest, emit `requested`, mutate via `mutate`, write new version,
// emit `succeeded` | `failed`. Pulled into a helper so the two
// adjust-* tools stay parallel without duplicating the audit-chain
// scaffolding.
func runManifestBump(
	ctx context.Context,
	client WatchmasterWriteClient,
	logger keepersLogAppender,
	claim Claim,
	eventTypes manifestBumpEventTypes,
	agentID string,
	mutate func(latest *keepclient.ManifestVersion) keepclient.PutManifestVersionRequest,
) (ManifestBumpResult, error) {
	latest, err := client.GetManifest(ctx, agentID)
	if err != nil {
		// GetManifest failure happens BEFORE the audit chain begins
		// (no `requested` row was written). Surface wrapped without
		// emitting any audit row — consistent with the M6.1.b
		// secrets-source short-circuit pattern.
		return ManifestBumpResult{}, fmt.Errorf("spawn: %s get_manifest: %w", eventTypes.ToolName, err)
	}

	if _, appendErr := logger.Append(ctx, manifestBumpRequestedEvent(
		eventTypes.Requested,
		eventTypes.ToolName,
		agentID,
		claim,
	)); appendErr != nil {
		return ManifestBumpResult{}, fmt.Errorf("spawn: keepers_log requested: %w", appendErr)
	}

	next := mutate(latest)
	next.VersionNo = latest.VersionNo + 1
	if next.SystemPrompt == "" {
		// Defensive: GetManifest always returns SystemPrompt (it is
		// NOT NULL on the server). An empty value on the read path
		// would round-trip as a keepclient preflight rejection at
		// PUT time; explicitly keeping the read value here makes the
		// failure mode obvious.
		next.SystemPrompt = latest.SystemPrompt
	}

	resp, err := client.PutManifestVersion(ctx, agentID, next)
	if err != nil {
		_, _ = logger.Append(ctx, manifestBumpFailedEvent(
			eventTypes.Failed,
			eventTypes.ToolName,
			agentID,
			claim,
			err,
		))
		return ManifestBumpResult{}, fmt.Errorf("spawn: %s put_manifest_version: %w", eventTypes.ToolName, err)
	}

	_, _ = logger.Append(ctx, manifestBumpSucceededEvent(
		eventTypes.Succeeded,
		eventTypes.ToolName,
		agentID,
		claim,
		resp.ID,
		next.VersionNo,
	))
	return ManifestBumpResult{ManifestVersionID: resp.ID, VersionNo: next.VersionNo}, nil
}

// copyManifestForBump turns a [keepclient.ManifestVersion] (the read
// shape) into the matching [keepclient.PutManifestVersionRequest]
// (the write shape), copying every persisted field. The caller
// overrides the targeted field (Personality or Language) and bumps
// VersionNo before passing the result to PutManifestVersion.
func copyManifestForBump(latest *keepclient.ManifestVersion) keepclient.PutManifestVersionRequest {
	return keepclient.PutManifestVersionRequest{
		// VersionNo is set by the caller (latest.VersionNo + 1).
		SystemPrompt:               latest.SystemPrompt,
		Tools:                      latest.Tools,
		AuthorityMatrix:            latest.AuthorityMatrix,
		KnowledgeSources:           latest.KnowledgeSources,
		Personality:                latest.Personality,
		Language:                   latest.Language,
		Model:                      latest.Model,
		Autonomy:                   latest.Autonomy,
		NotebookTopK:               latest.NotebookTopK,
		NotebookRelevanceThreshold: latest.NotebookRelevanceThreshold,
	}
}

// validateClaimAndDeps centralises the per-tool dependency + claim
// shape validation: nil client / nil logger / empty OrganizationID.
// Pulled into a helper so the three tool entrypoints stay scannable
// and share the same error vocabulary.
func validateClaimAndDeps(client WatchmasterWriteClient, logger keepersLogAppender, claim Claim) error {
	if client == nil {
		return fmt.Errorf("%w: nil write client", ErrInvalidRequest)
	}
	if logger == nil {
		return fmt.Errorf("%w: nil keepers_log writer", ErrInvalidRequest)
	}
	if claim.OrganizationID == "" {
		return fmt.Errorf("%w: empty OrganizationID", ErrInvalidClaim)
	}
	return nil
}

// manifestBumpRequestedEvent / manifestBumpSucceededEvent /
// manifestBumpFailedEvent build the audit-chain events for a
// manifest-bump tool. Mirror the M6.1.b helpers in slack_app.go —
// shared payload keys (agent_id, tool_name, event_class) come first,
// per-event keys (manifest_version_id, version_no, error_class) go
// next.

func manifestBumpRequestedEvent(eventType, toolName, agentID string, claim Claim) keeperslog.Event {
	return keeperslog.Event{
		EventType: eventType,
		Payload: map[string]any{
			payloadKeyAgentID:              pickAgentForBump(agentID, claim),
			payloadKeyToolName:             toolName,
			payloadKeyEventClass:           payloadValueEventRequested,
			payloadKeyApprovalTokenPresent: true,
		},
	}
}

func manifestBumpSucceededEvent(
	eventType, toolName, agentID string,
	claim Claim,
	manifestVersionID string,
	versionNo int,
) keeperslog.Event {
	return keeperslog.Event{
		EventType: eventType,
		Payload: map[string]any{
			payloadKeyAgentID:    pickAgentForBump(agentID, claim),
			payloadKeyToolName:   toolName,
			payloadKeyEventClass: payloadValueEventSucceeded,
			payloadKeyVersionID:  manifestVersionID,
			payloadKeyVersionNo:  versionNo,
		},
	}
}

func manifestBumpFailedEvent(
	eventType, toolName, agentID string,
	claim Claim,
	err error,
) keeperslog.Event {
	return keeperslog.Event{
		EventType: eventType,
		Payload: map[string]any{
			payloadKeyAgentID:    pickAgentForBump(agentID, claim),
			payloadKeyToolName:   toolName,
			payloadKeyEventClass: payloadValueEventFailed,
			payloadKeyErrorClass: classifyError(err),
		},
	}
}

// pickAgentForBump prefers the request's `agentID` over the claim's,
// since the request carries the targeted watchkeeper id (the claim's
// AgentID is the calling Watchmaster's id). Falls back to the claim
// for callers that route via a Watchmaster claim and pre-populate the
// request from it.
func pickAgentForBump(agentID string, claim Claim) string {
	if agentID != "" {
		return agentID
	}
	return claim.AgentID
}
