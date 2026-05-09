// Package retirewiring is the M7.2.a production composition entrypoint
// for the retire-saga kickoff seam. It hoists the keeperslog.Writer +
// saga.MemorySpawnSagaDAO + saga.Runner + spawn.RetireKickoffer
// composition into a single helper so the future M6.2.c-or-equivalent
// retire-trigger surface (M7.2.c) and the M7.2.a smoke test share one
// wiring path. Mirrors the M7.1.b approvalwiring helper shape.
//
// DEFERRED WIRING (intentional): [ComposeRetireKickoffer] is NOT yet
// called from any running binary. The trigger surface that invokes it
// lands in M7.2.c (the M6.2.c synchronous retire tool routed through
// the saga). This file ships ahead so the kickoffer + saga DAO
// composition is pinned + smoke-tested before that surface arrives;
// reviewers reading this code in isolation should NOT expect to find a
// `ComposeRetireKickoffer(...)` call site in M7.2.a.
//
// Package location note: this helper lives under core/internal/keep/
// rather than core/cmd/keep/ so the production keep binary's import
// graph stays free of any cgo-only dependencies the future M7.2.b
// NotebookArchive step's wiring may pull in (notebook substrate uses
// CGo SQLite). The Dockerfile builds keep with CGO_ENABLED=0;
// relocating here keeps that build green until the future Slack-bot
// binary takes over the wiring.
package retirewiring

import (
	"fmt"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keeperslog"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn/saga"
)

// RetireKickofferDeps is the construction-time bag composed into
// [ComposeRetireKickoffer]. Held in a struct so a future addition
// (e.g. a Postgres-backed SpawnSagaDAO once shared persistence lands)
// replaces a single field rather than churning every call-site.
type RetireKickofferDeps struct {
	// KeepClient is the [keeperslog.LocalKeepClient] the audit-emit
	// chain consumes. Production callers pass the
	// `*keepclient.Client` they already constructed for the rest of
	// the keep boot sequence.
	KeepClient keeperslog.LocalKeepClient

	// AgentID is the bot's stable agent identifier emitted on every
	// `retire_approved_for_watchkeeper` audit row.
	AgentID string

	// Steps is the saga step list the kickoffer hands to
	// [saga.Runner.Run] on every Kickoff. Optional — a nil / empty
	// slice keeps the M7.1.b zero-step behaviour. With M7.2.b
	// [spawn.NotebookArchiveStep] and M7.2.c [spawn.MarkRetiredStep]
	// shipped, production wiring populates this slot with
	// [NotebookArchive, MarkRetired] in that order; the M7.2.c
	// MarkRetired step reads the archive_uri the M7.2.b step
	// publishes via [saga.RetireResult.ArchiveURI] and persists it
	// onto the watchkeeper row through
	// [keepclient.Client.UpdateWatchkeeperRetired].
	Steps []saga.Step
}

// ComposeRetireKickoffer composes the M7.2.a retire kickoffer from the
// supplied [RetireKickofferDeps]. Returns the kickoffer plus the
// underlying [saga.SpawnSagaDAO] so a smoke test can pin the
// composition shape and a future M7.2.c wiring can share the DAO if
// it needs cross-saga visibility.
//
// The composition is load-bearing — the M7.2.a ordering invariants
// (audit-before-Insert, Insert-before-Run) live in the kickoffer.
// Reusing this helper from future retire-trigger wiring guarantees
// both halves of the contract land together.
//
// TODO(M7.2.c): wire this helper into the M6.2.c retire tool's
// post-gate dispatch path. Today this helper has no production call
// site by design — see the file docblock's DEFERRED WIRING note. The
// smoke test in wiring_test.go pins the composition shape until
// then.
func ComposeRetireKickoffer(deps RetireKickofferDeps) (*spawn.RetireKickoffer, saga.SpawnSagaDAO, error) {
	if deps.KeepClient == nil {
		return nil, nil, fmt.Errorf("retirewiring: ComposeRetireKickoffer: KeepClient must not be nil")
	}
	if deps.AgentID == "" {
		return nil, nil, fmt.Errorf("retirewiring: ComposeRetireKickoffer: AgentID must not be empty")
	}

	writer := keeperslog.New(deps.KeepClient)
	sagaDAO := saga.NewMemorySpawnSagaDAO(nil)
	runner := saga.NewRunner(saga.Dependencies{DAO: sagaDAO, Logger: writer})
	kickoffer := spawn.NewRetireKickoffer(spawn.RetireKickoffDeps{
		Logger:  writer,
		DAO:     sagaDAO,
		Runner:  runner,
		AgentID: deps.AgentID,
		Steps:   deps.Steps,
	})
	return kickoffer, sagaDAO, nil
}
