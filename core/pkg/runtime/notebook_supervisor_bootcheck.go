package runtime

import (
	"context"
	"fmt"

	"github.com/vadimtrunov/watchkeepers/core/pkg/notebook"
)

// BootCheck runs the M5.6.e.b boot-time superseded-lesson scan against
// the per-agent [notebook.DB] handle the supervisor holds for
// `agentID`. It looks up the handle via [NotebookSupervisor.Lookup] and
// delegates to [notebook.DB.FlagSupersededLessons]. The returned
// `newlyFlagged` is the count of lesson rows whose `needs_review`
// transitioned from 0 to 1; see [notebook.DB.FlagSupersededLessons]
// for the comparison rule.
//
// Errors:
//
//   - [ErrAgentNotOpened] when the supervisor has no live handle for
//     `agentID`. The runtime expects [NotebookSupervisor.Open] to have
//     been called for the agent before BootCheck — a missing entry is a
//     wiring bug, not a transient error, and the sentinel lets callers
//     distinguish it from infrastructure failures via [errors.Is].
//   - the verbatim error from [notebook.DB.FlagSupersededLessons]
//     otherwise (wrapped to preserve the supervisor breadcrumb).
//
// Best-effort wiring: per the M5.6.b/c precedent, the runtime caller
// can choose to log + continue on a non-nil error. BootCheck itself
// surfaces the error so the caller has the choice — it does NOT
// swallow internally. Options that the [notebook.DB.FlagSupersededLessons]
// helper accepts (currently [notebook.WithFlagLogger]) are out of
// scope here; if a caller needs to thread a logger they can call the
// helper directly via [NotebookSupervisor.Lookup].
func (s *NotebookSupervisor) BootCheck(
	ctx context.Context,
	agentID string,
	currentVersions map[string]string,
) (newlyFlagged int, err error) {
	db, ok := s.Lookup(agentID)
	if !ok {
		return 0, fmt.Errorf("runtime: bootcheck for %q: %w", agentID, ErrAgentNotOpened)
	}

	// Compile-time assertion that the supervisor's handle satisfies the
	// FlagSupersededLessons receiver — guards against a future refactor
	// in the notebook package that moves the helper to a different
	// receiver shape.
	var _ interface {
		FlagSupersededLessons(context.Context, map[string]string, ...notebook.FlagOption) (int, error)
	} = db

	n, err := db.FlagSupersededLessons(ctx, currentVersions)
	if err != nil {
		return n, fmt.Errorf("runtime: bootcheck flag superseded for %q: %w", agentID, err)
	}
	return n, nil
}
