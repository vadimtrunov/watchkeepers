package main

// run_stubs.go — exit-3 stubs for noun-groups whose backend seam has
// not yet landed. Each stub pins the CLI contract (the subcommand
// exists, has the right flag prefix, and returns the documented
// exit code), and names the follow-up M sub-item that will wire it.
// Mirrors the M10.1 pattern of pinning contracts NOW and wiring call
// sites later.

import (
	"context"
	"fmt"
	"io"
)

func runTools(_ context.Context, args []string, _, stderr io.Writer, _ envLookup) int {
	if len(args) == 0 {
		stderrf(stderr, "wk: tools: missing subcommand (expected: sources)\n")
		return 2
	}
	if args[0] != "sources" {
		stderrf(stderr, fmt.Sprintf("wk: tools: unknown subcommand %q (expected: sources)\n", args[0]))
		return 2
	}
	if len(args) < 2 {
		stderrf(stderr, "wk: tools sources: missing subcommand (expected one of: list, status, sync)\n")
		return 2
	}
	switch args[1] {
	case "list":
		return notWiredExit(stderr, "tools sources list", "M10.2.b follow-up — requires toolregistry source-state read seam exposed to operator")
	case "status":
		return notWiredExit(stderr, "tools sources status", "M10.2.b follow-up — requires toolregistry sync-status read seam exposed to operator")
	case "sync":
		return notWiredExit(stderr, "tools sources sync", "M10.2.b follow-up — requires M9.1.b scheduler operator-trigger seam (today sync runs on cron only)")
	default:
		stderrf(stderr, fmt.Sprintf("wk: tools sources: unknown subcommand %q (expected one of: list, status, sync)\n", args[1]))
		return 2
	}
}

func runApprovals(_ context.Context, args []string, _, stderr io.Writer, _ envLookup) int {
	if len(args) == 0 {
		stderrf(stderr, "wk: approvals: missing subcommand (expected one of: pending, inspect)\n")
		return 2
	}
	switch args[0] {
	case "pending":
		return notWiredExit(stderr, "approvals pending", "M10.2.c follow-up — requires Keep approvals-queue read endpoint (today the proposalstore is in-process only)")
	case "inspect":
		// Iter-1 critic m7: return exit-3 BEFORE flag.Parse so a
		// future-shape flag (e.g. `--id <uuid>`) returns the
		// not-wired sentinel rather than exit 2 (unknown flag).
		// The "contract is pinned NOW" rationale requires the same
		// invocation to yield exit 3 today and exit 0 once the
		// backend lands; parsing args eagerly would have leaked
		// future-flag-typos through the wrong exit code.
		return notWiredExit(stderr, "approvals inspect", "M10.2.c follow-up — requires Keep approvals-inspect read endpoint")
	default:
		stderrf(stderr, fmt.Sprintf("wk: approvals: unknown subcommand %q (expected one of: pending, inspect)\n", args[0]))
		return 2
	}
}
