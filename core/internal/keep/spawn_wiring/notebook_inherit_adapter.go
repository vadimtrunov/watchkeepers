// Package spawnwiring composes spawn-saga step seams with their
// production-side substrate dependencies. The package lives under
// `core/internal/keep/` rather than `core/cmd/keep/` (mirroring the
// `approval_wiring` precedent) so the production keep binary's
// import graph stays free of the cgo-only sqlite-vec chain pulled in
// by `core/pkg/notebook` — keep itself never imports this package,
// while the future Slack-bot binary (which can opt in to cgo) calls
// the wiring helpers here when assembling its spawn-saga step list.
//
// Phase 2 §M7.1.c lands the [NewProductionNotebookInheritor] helper
// that wraps [notebook.InheritFromArchive] for the
// [spawn.NotebookInheritStep]'s [spawn.NotebookInheritor] seam.
// Earlier M7.1.x leaves' adapters (CreateApp / OAuthInstall /
// BotProfile / NotebookProvision / RuntimeLaunch) will migrate here
// when the Slack-bot binary takes over wiring; until then this
// package holds only the one adapter the inheritance flow needs.
package spawnwiring

import (
	"context"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/notebook"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn"
)

// ProductionNotebookInheritor is the wiring adapter that satisfies
// [spawn.NotebookInheritor] by delegating to
// [notebook.InheritFromArchive] (which composes
// [notebook.ImportFromArchive] with a post-import [notebook.DB.Stats]
// count read). The adapter exists so the future Slack-bot binary's
// wiring composes the seam without re-implementing the fetch +
// open + import + count flow inline (Phase 2 §M7.1.c iter-1 critic
// P1 — wiring-drift concern). Tests stay on hand-rolled fakes
// (`core/pkg/spawn/notebookinherit_step_test.go`); production stays
// on this thin adapter.
//
// Concurrency: safe for concurrent calls with DIFFERENT
// `watchkeeperID` values; NOT safe for concurrent calls with the
// SAME `watchkeeperID` (the underlying [notebook.InheritFromArchive]
// contract). Production wiring guarantees per-saga exclusivity: a
// saga is the sole writer for its watchkeeperID-keyed notebook file
// during its run.
type ProductionNotebookInheritor struct {
	fetcher notebook.Fetcher
}

// Compile-time assertion: [*ProductionNotebookInheritor] satisfies
// [spawn.NotebookInheritor]. Pins the interface contract so a future
// signature drift on either side surfaces here.
//
//nolint:gochecknoglobals // package-level compile-time assertion; idiomatic Go pattern.
var _ spawn.NotebookInheritor = (*ProductionNotebookInheritor)(nil)

// NewProductionNotebookInheritor constructs the production adapter
// from a [notebook.Fetcher]. The fetcher is required; a nil value
// panics with a clear message (matches the panic discipline of
// the M7.1.c [spawn.NewNotebookInheritStep] family).
//
// Typical wiring: pass an `archivestore.ArchiveStore` value
// (`*archivestore.LocalFS` / `*archivestore.S3Compatible`) which
// satisfies [notebook.Fetcher] structurally.
func NewProductionNotebookInheritor(fetcher notebook.Fetcher) *ProductionNotebookInheritor {
	if fetcher == nil {
		panic("spawnwiring: NewProductionNotebookInheritor: fetcher must not be nil")
	}
	return &ProductionNotebookInheritor{fetcher: fetcher}
}

// Inherit satisfies [spawn.NotebookInheritor.Inherit] by forwarding
// to [notebook.InheritFromArchive]. The watchkeeperID is converted
// to its canonical string form (the notebook substrate's path
// validator consumes that shape — see M2b.1).
func (p *ProductionNotebookInheritor) Inherit(ctx context.Context, watchkeeperID uuid.UUID, archiveURI string) (int, error) {
	return notebook.InheritFromArchive(ctx, watchkeeperID.String(), archiveURI, p.fetcher)
}
