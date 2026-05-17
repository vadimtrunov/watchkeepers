// Package k2kwiring is the M1.1.c production composition entrypoint
// for the K2K conversation lifecycle. It hoists the
// [k2k.Repository] + [k2k.SlackChannels] composition into a single
// helper so the future K2K consumer surfaces (the M1.3 peer tool
// suite, the M1.6 escalation saga, the `wk channel reveal` CLI in
// `core/cmd/wk/`) share one wiring path. Mirrors the M7.2.a
// `retire_wiring` and M7.1.b `approval_wiring` helper shapes.
//
// DEFERRED CONSUMER (intentional): [ComposeLifecycle] is wired into
// the `wk channel reveal` CLI in M1.1.c; the broader consumer surface
// (M1.3 peer.ask / peer.reply / peer.broadcast tools, M1.6 escalation
// saga) lands in later milestones. The compile-time assertions in
// this file pin the seam shapes so future consumers can rely on the
// composition without re-deriving it.
//
// Package location note: this helper lives under
// `core/internal/keep/` rather than `core/cmd/wk/` (or the future
// Watchmaster binary's location) so the keep binary's import graph
// continues to own the K2K wiring path the way retire_wiring /
// approval_wiring own theirs. Future consumer binaries import this
// package to obtain the composed [*k2k.Lifecycle] without re-shaping
// the deps bundle.
package k2kwiring

import (
	"fmt"

	"github.com/vadimtrunov/watchkeepers/core/pkg/k2k"
	"github.com/vadimtrunov/watchkeepers/core/pkg/k2k/audit"
	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger/slack"
)

// Compile-time assertion: [*slack.Client] satisfies
// [k2k.SlackChannels]. Pins the integration shape so a future change
// to either seam (a method renamed on slack.Client, a method added to
// k2k.SlackChannels) fails the build here rather than at the M1.1.c
// CLI's first run. Mirrors
// `var _ messenger.Adapter = (*slack.Client)(nil)` from
// `core/pkg/messenger/slack/adapter_assertion_test.go`.
var _ k2k.SlackChannels = (*slack.Client)(nil)

// LifecycleDeps is the construction-time bag composed into
// [ComposeLifecycle]. The fields mirror [k2k.LifecycleDeps] but are
// surfaced through the wiring boundary so callers can pass production
// types ([*k2k.PostgresRepository], [*slack.Client]) without re-typing
// the k2k.LifecycleDeps struct at every call site.
type LifecycleDeps struct {
	// Repo is the K2K persistence seam. Production wiring passes
	// [*k2k.PostgresRepository]; tests inject a fake satisfying
	// [k2k.Repository]. Required (non-nil).
	Repo k2k.Repository

	// Slack is the channel-primitives seam. Production wiring passes
	// [*slack.Client]; tests inject a fake satisfying
	// [k2k.SlackChannels]. Required (non-nil).
	Slack k2k.SlackChannels

	// Auditor is the M1.4 K2K audit-emission seam. Production wiring
	// passes a [*audit.Writer] wrapping the keeperslog writer; tests
	// inject a fake satisfying [audit.Emitter], or leave nil (the
	// lifecycle skips emission). OPTIONAL.
	Auditor audit.Emitter
}

// ComposeLifecycle composes the M1.1.c K2K lifecycle from the
// supplied [LifecycleDeps]. Returns the lifecycle + an error when the
// deps are degenerate (mirrors the M7.2.a `ComposeRetireKickoffer`
// shape — surface the bad-wiring branch as an error rather than a
// panic at this level, so the binary can print a friendly diagnostic
// and exit with a usage code).
//
// The underlying [k2k.NewLifecycle] panics on nil deps; this wrapper
// catches the obvious "wiring forgotten" case as an error so the
// boot sequence does not stack-trace.
func ComposeLifecycle(deps LifecycleDeps) (*k2k.Lifecycle, error) {
	if deps.Repo == nil {
		return nil, fmt.Errorf("k2kwiring: ComposeLifecycle: Repo must not be nil")
	}
	if deps.Slack == nil {
		return nil, fmt.Errorf("k2kwiring: ComposeLifecycle: Slack must not be nil")
	}
	return k2k.NewLifecycle(k2k.LifecycleDeps{
		Repo:    deps.Repo,
		Slack:   deps.Slack,
		Auditor: deps.Auditor,
	}), nil
}
