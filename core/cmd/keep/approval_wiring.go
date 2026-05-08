// approval_wiring.go is the M7.1.b production composition entrypoint
// for the Slack inbound approval dispatcher. It hoists the
// keeperslog.Writer + saga.MemorySpawnSagaDAO + saga.Runner +
// spawn.SpawnKickoffer + approval.Dispatcher composition into a single
// helper so the future Slack-bot binary (and the M7.1.b smoke test)
// share one wiring path. The keep service itself does not yet host
// the inbound handler — the helper exists today so AC4 pins the
// composition shape and the M7.1.c–.e items have one place to extend.
package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keeperslog"
	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger/slack/inbound/approval"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn/saga"
)

// ApprovalDispatcherDeps is the construction-time bag composed into
// [composeApprovalDispatcher]. Held in a struct so a future addition
// (e.g. a Postgres-backed SpawnSagaDAO once M7.2 lands) replaces a
// single field rather than churning every call-site.
type ApprovalDispatcherDeps struct {
	// KeepClient is the [keeperslog.LocalKeepClient] the audit-emit
	// chain consumes. Production callers pass the
	// `*keepclient.Client` they already constructed for the rest of
	// the keep boot sequence.
	KeepClient keeperslog.LocalKeepClient

	// PendingApprovalDAO is the [spawn.PendingApprovalDAO] the
	// dispatcher resolves approval-token rows against. Production
	// callers wire the same DAO instance the M6.3.c DM router uses.
	PendingApprovalDAO spawn.PendingApprovalDAO

	// Replayer is the [approval.Replayer] the dispatcher consults on
	// the approved-branch for non-spawn tools. M7.1.b leaves the
	// concrete Replayer wiring to the future Slack-bot binary; this
	// helper accepts the seam so the M7.1.b smoke test can substitute
	// a hand-rolled fake.
	Replayer approval.Replayer

	// AgentID is the bot's stable agent identifier emitted on every
	// `manifest_approved_for_spawn` audit row.
	AgentID string
}

// composeApprovalDispatcher composes the M7.1.b approval dispatcher
// from the supplied [ApprovalDispatcherDeps]. Returns the dispatcher
// plus the [*spawn.SpawnKickoffer] it wraps so a smoke test can pin
// the kickoffer's non-nil presence (AC4).
//
// The composition is load-bearing — the M7.1.b ordering invariants
// (audit-before-Insert, Insert-before-Run) live in the kickoffer; the
// dispatcher's job is dispatch-on-tool-name. Reusing this helper from
// future Slack-bot wiring guarantees both halves of the contract land
// together.
func composeApprovalDispatcher(deps ApprovalDispatcherDeps) (*approval.Dispatcher, *spawn.SpawnKickoffer, error) {
	if deps.KeepClient == nil {
		return nil, nil, fmt.Errorf("keep: composeApprovalDispatcher: KeepClient must not be nil")
	}
	if deps.PendingApprovalDAO == nil {
		return nil, nil, fmt.Errorf("keep: composeApprovalDispatcher: PendingApprovalDAO must not be nil")
	}
	if deps.Replayer == nil {
		return nil, nil, fmt.Errorf("keep: composeApprovalDispatcher: Replayer must not be nil")
	}
	if deps.AgentID == "" {
		return nil, nil, fmt.Errorf("keep: composeApprovalDispatcher: AgentID must not be empty")
	}

	writer := keeperslog.New(deps.KeepClient)
	sagaDAO := saga.NewMemorySpawnSagaDAO(nil)
	runner := saga.NewRunner(saga.Dependencies{DAO: sagaDAO, Logger: writer})
	kickoffer := spawn.NewSpawnKickoffer(spawn.SpawnKickoffDeps{
		Logger:  writer,
		DAO:     sagaDAO,
		Runner:  runner,
		AgentID: deps.AgentID,
	})
	dispatcher := approval.New(
		deps.PendingApprovalDAO,
		deps.Replayer,
		kickoffer,
		approval.WithAuditAppender(writer),
	)
	return dispatcher, kickoffer, nil
}

// nopReplayer is a placeholder [approval.Replayer] used by the M7.1.b
// smoke test. The future Slack-bot binary substitutes a real
// Replayer; the smoke test only needs a non-nil seam to satisfy the
// dispatcher's panic-on-nil constructor.
//
//nolint:unused // wired via approval_wiring_test.go; the linter does not see test-side composition.
type nopReplayer struct{}

//nolint:unused // see nopReplayer.
func (nopReplayer) Replay(_ context.Context, _ string, _ json.RawMessage, _ string) error {
	return nil
}
