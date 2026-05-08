// retire_watchkeeper.go implements the M6.2.c lead-approval-gated
// Watchmaster tool that flips a Watchkeeper's lifecycle flag from
// `active` to `retired` via [keepclient.Client.UpdateWatchkeeperStatus].
//
// Unlike the M6.2.b manifest-bump tools, this is a status-row mutation
// on the watchkeeper row in Keep — NOT a `manifest_version` write. The
// keep server enforces the `active→retired` state-machine transition
// (see core/pkg/keepclient/write_watchkeeper.go); an invalid transition
// surfaces as a typed error from the underlying client and is captured
// on the `failed` audit row.
//
// Shape mirrors [ProposeSpawn] (write-only, no pre-read): there is no
// reason to GetWatchkeeper before calling UpdateWatchkeeperStatus
// because the server is the authoritative source of the current
// status, and a stale read would race the actual transition anyway.
//
// The gate stack mirrors M6.1.b SlackAppRPC.CreateApp / M6.2.b manifest
// bumps verbatim — validate `claim.OrganizationID`, validate
// `claim.AuthorityMatrix["retire_watchkeeper"] == "lead_approval"`,
// validate `req.ApprovalToken` non-empty, emit
// `watchmaster_retire_watchkeeper_requested` →
// `watchmaster_retire_watchkeeper_succeeded` |
// `watchmaster_retire_watchkeeper_failed` keepers_log audit chain.
package spawn

import (
	"context"
	"fmt"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keeperslog"
)

// authorityActionRetireWatchkeeper is the action string the Watchmaster
// manifest publishes in `authority_matrix` to gate the retire tool.
// Hoisted to a package constant so a re-key (e.g. a future
// per-environment suffix) is a one-line change here that the gate AND
// the keepers_log payload pick up via the compiler.
const authorityActionRetireWatchkeeper = "retire_watchkeeper"

// keepersLogEventRetire* values the retire tool emits on the audit
// chain. Project convention: snake_case `<tool>_<event_class>` past
// tense, prefixed with the surface owner (`watchmaster`) so a
// downstream consumer can group audit rows by emitting agent without
// parsing the payload.
const (
	keepersLogEventRetireRequested = "watchmaster_retire_watchkeeper_requested"
	keepersLogEventRetireSucceeded = "watchmaster_retire_watchkeeper_succeeded"
	keepersLogEventRetireFailed    = "watchmaster_retire_watchkeeper_failed"
)

// payloadValueRetireWatchkeeper is the `tool_name` payload value the
// retire audit rows carry. Matches the wire-side tool registry name in
// `harness/src/builtinTools.ts` so a downstream consumer can group
// audit rows by tool_name without string-rewriting.
const payloadValueRetireWatchkeeper = "retire_watchkeeper"

// statusValueRetired is the watchkeeper status the retire tool writes
// via [keepclient.Client.UpdateWatchkeeperStatus]. Pinned to a constant
// to match the closed-set vocabulary the keepclient and keep server
// share; a typo here would burn a network round-trip on a
// guaranteed-reject input.
const statusValueRetired = "retired"

// RetireWatchkeeperRequest is the value supplied to [RetireWatchkeeper].
// Unlike the M6.2.b manifest-bump requests, this carries no
// manifest fields — the tool's job is solely to flip the status row.
type RetireWatchkeeperRequest struct {
	// AgentID is the watchkeeper row id (NOT the manifest UUID — the
	// server's PATCH /v1/watchkeepers/{id}/status endpoint takes the
	// row id). Required; empty fails synchronously with
	// [ErrInvalidRequest].
	AgentID string

	// ApprovalToken is the opaque token the lead-approval saga (M6.3)
	// issues. M6.2.c validation is non-empty only; M6.3 owns the
	// cryptography. Required; empty fails with [ErrApprovalRequired]
	// AFTER the authority-matrix gate passes (so a forbidden caller
	// is told `unauthorized`, not `approval required`).
	ApprovalToken string
}

// RetireWatchkeeper validates the lead-approval gate stack and flips
// the watchkeeper's status row from `active` to `retired`. The
// returned error wraps the underlying [keepclient.Client] error on
// transition rejection so callers can errors.Is the specific sentinel
// (e.g. [keepclient.ErrInvalidStatusTransition]).
//
// Resolution order (pinned to mirror [ProposeSpawn]):
//
//  1. Validate ctx (cancellation takes precedence over input shape).
//  2. Validate client + logger non-nil → [ErrInvalidRequest].
//  3. Validate claim.OrganizationID non-empty (M3.5.a discipline) →
//     [ErrInvalidClaim].
//  4. Validate claim.AuthorityMatrix["retire_watchkeeper"] equals
//     "lead_approval" → [ErrUnauthorized] otherwise.
//  5. Validate req.AgentID non-empty → [ErrInvalidRequest] otherwise.
//  6. Validate req.ApprovalToken non-empty → [ErrApprovalRequired]
//     otherwise. (Order matters: gate runs BEFORE token check.)
//  7. Emit `watchmaster_retire_watchkeeper_requested` keepers_log event
//     BEFORE the keep write — `no audit row, no privileged action`.
//  8. Call client.UpdateWatchkeeperStatus(ctx, AgentID, "retired").
//  9. On success, emit `watchmaster_retire_watchkeeper_succeeded`. On
//     failure, emit `watchmaster_retire_watchkeeper_failed` and return
//     the wrapped error.
func RetireWatchkeeper(
	ctx context.Context,
	client WatchmasterWriteClient,
	logger keepersLogAppender,
	req RetireWatchkeeperRequest,
	claim Claim,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateClaimAndDeps(client, logger, claim); err != nil {
		return err
	}
	if !claim.HasAuthority(authorityActionRetireWatchkeeper) {
		return ErrUnauthorized
	}
	if req.AgentID == "" {
		return fmt.Errorf("%w: empty AgentID", ErrInvalidRequest)
	}
	if req.ApprovalToken == "" {
		return ErrApprovalRequired
	}

	if _, err := logger.Append(ctx, retireRequestedEvent(req.AgentID, claim)); err != nil {
		return fmt.Errorf("spawn: keepers_log requested: %w", err)
	}

	if err := client.UpdateWatchkeeperStatus(ctx, req.AgentID, statusValueRetired); err != nil {
		_, _ = logger.Append(ctx, retireFailedEvent(req.AgentID, claim, err))
		return fmt.Errorf("spawn: retire_watchkeeper update_status: %w", err)
	}

	_, _ = logger.Append(ctx, retireSucceededEvent(req.AgentID, claim))
	return nil
}

// retireRequestedEvent / retireSucceededEvent / retireFailedEvent build
// the audit-chain events for the retire tool. Mirror the M6.1.b /
// M6.2.b helper layout — shared payload keys (agent_id, tool_name,
// event_class) come first, per-event keys (error_class on failed) go
// next. The retire payload deliberately does NOT carry
// `manifest_version_id` keys because retire is a status-row mutation,
// not a manifest bump.

func retireRequestedEvent(agentID string, claim Claim) keeperslog.Event {
	return keeperslog.Event{
		EventType: keepersLogEventRetireRequested,
		Payload: map[string]any{
			payloadKeyAgentID:              pickAgentForBump(agentID, claim),
			payloadKeyToolName:             payloadValueRetireWatchkeeper,
			payloadKeyEventClass:           payloadValueEventRequested,
			payloadKeyApprovalTokenPresent: true,
		},
	}
}

func retireSucceededEvent(agentID string, claim Claim) keeperslog.Event {
	return keeperslog.Event{
		EventType: keepersLogEventRetireSucceeded,
		Payload: map[string]any{
			payloadKeyAgentID:    pickAgentForBump(agentID, claim),
			payloadKeyToolName:   payloadValueRetireWatchkeeper,
			payloadKeyEventClass: payloadValueEventSucceeded,
		},
	}
}

func retireFailedEvent(agentID string, claim Claim, err error) keeperslog.Event {
	return keeperslog.Event{
		EventType: keepersLogEventRetireFailed,
		Payload: map[string]any{
			payloadKeyAgentID:    pickAgentForBump(agentID, claim),
			payloadKeyToolName:   payloadValueRetireWatchkeeper,
			payloadKeyEventClass: payloadValueEventFailed,
			payloadKeyErrorClass: classifyError(err),
		},
	}
}
