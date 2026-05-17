package main

import (
	"context"
	"fmt"
	"io"
)

// subcommandFunc is the closed contract every noun-group dispatcher
// satisfies. The dispatch table at module scope routes args[0] to one
// of these in O(1); a flat switch with 13+ cases trips gocyclo and a
// generated parser would be overkill for a CLI that fits in one
// directory. The table is intentionally a `var` (not `const`) because
// Go forbids `const` of function-typed values.
type subcommandFunc func(ctx context.Context, args []string, stdout, stderr io.Writer, env envLookup) int

//nolint:gochecknoglobals // intentional package-scoped dispatch table; see [subcommandFunc].
var rootSubcommands = map[string]subcommandFunc{
	"spawn":            runSpawn,
	"retire":           runRetire,
	"list":             runList,
	"inspect":          runInspect,
	"logs":             runLogs,
	"tail-keepers-log": runTailKeepersLog,
	"tool":             runTool,
	"tools":            runTools,
	"notebook":         runNotebook,
	"personality":      runPersonality,
	"language":         runLanguage,
	"budget":           runBudget,
	"approvals":        runApprovals,
	"channel":          runChannel,
}

// run is the testable entrypoint. The signature mirrors
// `core/cmd/wk-tool/run.go` (M9.5) so test wiring stays consistent
// across the cmd-tree.
//
// Exit codes (see top-of-package doc):
//
//	0 — success
//	1 — runtime failure
//	2 — usage error (no subcommand, unknown subcommand, missing flag)
//	3 — feature not yet wired (contract exists, backend seam pending)
func run(ctx context.Context, args []string, stdout, stderr io.Writer, env envLookup) int {
	if len(args) == 0 {
		stderrf(stderr, "wk: missing subcommand (run `wk --help` for the noun-group list)\n")
		return 2
	}
	switch args[0] {
	case "-h", "--help", "help":
		writeUsage(stdout)
		return 0
	}
	if fn, ok := rootSubcommands[args[0]]; ok {
		return fn(ctx, args[1:], stdout, stderr, env)
	}
	stderrf(stderr, fmt.Sprintf("wk: unknown subcommand %q (run `wk --help` for the noun-group list)\n", args[0]))
	return 2
}

// writeUsage prints the operator-facing help. Iter-1 critic fixes:
//   - M3: budget show flag set rewritten to match the actual
//     implementation (was advertising a non-existent `--scope`).
//   - M4: notebook show no longer claims a non-existent `--limit`
//     flag (Stats has no limit knob).
//   - m4: `tool share` flag list expanded to all flags the binary
//     parses (the original `...` truncation hid --target-base,
//     --token-env, --github-base-url).
//   - m5: `WATCHKEEPER_DATA` (notebook env) added to the env-var
//     block; the M10.1 lesson promised it would surface in help.
//   - n1, n3, n5: noted the "flags precede positional" rule and
//     surfaced the `wk logs` default limit (1000) so the
//     help-text contract is self-describing.
func writeUsage(stdout io.Writer) {
	fmt.Fprintln(stdout, "Usage: wk <noun-group> [subcommand] [flags] [positional]")
	fmt.Fprintln(stdout, "Convention: flags MUST precede the positional <wk-id>/<entry-id> when both are present.")
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Watchkeeper lifecycle:")
	fmt.Fprintln(stdout, "  wk spawn   --manifest <id> --lead <id> [--active-version <id>] --reason <text>")
	fmt.Fprintln(stdout, "  wk retire  <wk-id> --archive-uri <uri> --reason <text>")
	fmt.Fprintln(stdout, "  wk list    [--status pending|active|retired] [--limit N]")
	fmt.Fprintln(stdout, "  wk inspect <wk-id>")
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Logs & events:")
	fmt.Fprintln(stdout, "  wk logs <wk-id> [--limit N]  (default --limit 1000; client-side filter by actor_watchkeeper_id; UUID-shape enforced)")
	fmt.Fprintln(stdout, "  wk tail-keepers-log          (SSE stream; --limit not honored — server pushes all visible frames)")
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Tool lifecycle (subsumes wk-tool):")
	fmt.Fprintln(stdout, "  wk tool local-install  --folder <path> --source <name> --reason <text> --operator <id> [--data-dir <dir>]")
	fmt.Fprintln(stdout, "  wk tool rollback       --name <tool> --to <version> [--source <name>] --reason <text> --operator <id> [--data-dir <dir>]")
	fmt.Fprintln(stdout, "  wk tool hosted export  --source <name> --tool <name> --destination <abs-path> --reason <text> --operator <id> [--data-dir <dir>]")
	fmt.Fprintln(stdout, "  wk tool hosted list    (M10.2.b follow-up — exit 3)")
	fmt.Fprintln(stdout, "  wk tool hosted show    <name>  (M10.2.b follow-up — exit 3)")
	fmt.Fprintln(stdout, "  wk tool share          --source <name> --tool <name> --target <platform|private>")
	fmt.Fprintln(stdout, "                         --target-owner <owner> --target-repo <repo> [--target-base main]")
	fmt.Fprintln(stdout, "                         --reason <text> --proposer <id>")
	fmt.Fprintln(stdout, "                         [--token-env WATCHKEEPER_GITHUB_TOKEN] [--github-base-url <url>] [--data-dir <dir>]")
	fmt.Fprintln(stdout, "  wk tool list           (M10.2.b follow-up — exit 3)")
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Tool sources:")
	fmt.Fprintln(stdout, "  wk tools sources {list|status|sync}  (M10.2.b follow-up — exit 3)")
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Notebook (per-Watchkeeper; honors $WATCHKEEPER_DATA, NOT $WATCHKEEPER_DATA_DIR):")
	fmt.Fprintln(stdout, "  wk notebook show          <wk-id>                  (Stats JSON)")
	fmt.Fprintln(stdout, "  wk notebook forget        <wk-id> <entry-id>")
	fmt.Fprintln(stdout, "  wk notebook export        --destination <abs-path> <wk-id>")
	fmt.Fprintln(stdout, "  wk notebook import        --archive <abs-path>     <wk-id>")
	fmt.Fprintln(stdout, "  wk notebook archive       <wk-id>                  (M10.2.b follow-up — exit 3)")
	fmt.Fprintln(stdout, "  wk notebook list-archives <wk-id>                  (M10.2.b follow-up — exit 3)")
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Manifest-backed knobs:")
	fmt.Fprintln(stdout, "  wk personality show <wk-id>")
	fmt.Fprintln(stdout, "  wk personality set  --value <text> --reason <text> [--system-prompt <text>] <wk-id>")
	fmt.Fprintln(stdout, "  wk language show    <wk-id>")
	fmt.Fprintln(stdout, "  wk language set     --value <bcp47> --reason <text> [--system-prompt <text>] <wk-id>")
	fmt.Fprintln(stdout, "  wk budget show      --agent <wk-id> --from <RFC3339> --to <RFC3339> [--grain daily|weekly]")
	fmt.Fprintln(stdout, "  wk budget set       (M10.2.c follow-up — exit 3)")
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Approvals:")
	fmt.Fprintln(stdout, "  wk approvals {pending|inspect <id>}  (M10.2.c follow-up — exit 3)")
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "K2K channels (Phase 2 M1.1.c):")
	fmt.Fprintln(stdout, "  wk channel reveal --user <slack-user-id> <conversation-id>")
	fmt.Fprintln(stdout, "                    (looks up slack_channel_id from K2K row; calls Slack conversations.invite)")
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Env vars:")
	fmt.Fprintln(stdout, "  WATCHKEEPER_KEEP_BASE_URL   Keep base URL (e.g. http://keep:8080) — Keep-backed subcommands")
	fmt.Fprintln(stdout, "  WATCHKEEPER_OPERATOR_TOKEN  bearer token for Keep-backed subcommands")
	fmt.Fprintln(stdout, "  WATCHKEEPER_DATA_DIR        deployment data root for `wk tool *` (toolregistry / localpatch)")
	fmt.Fprintln(stdout, "  WATCHKEEPER_DATA            data root for `wk notebook *` (notebook package — distinct from DATA_DIR)")
	fmt.Fprintln(stdout, "  WATCHKEEPER_LOCAL_SOURCES   comma-separated allowlist for `tool local-install --source`")
	fmt.Fprintln(stdout, "  WATCHKEEPER_GITHUB_TOKEN    default token env for `tool share` (override via --token-env)")
	fmt.Fprintln(stdout, "  WATCHKEEPER_K2K_PG_DSN      Postgres DSN for `wk channel reveal` (M1.1.c)")
	fmt.Fprintln(stdout, "  WATCHKEEPER_OPERATOR_ORG_ID operator's organization UUID for `wk channel reveal` (M1.1.c)")
	fmt.Fprintln(stdout, "  WATCHKEEPER_SLACK_BOT_TOKEN bot xoxb-* token for `wk channel reveal` (M1.1.c)")
}

// notWiredExit prints a uniform "feature not yet wired" diagnostic and
// returns the exit-3 sentinel. The label is the human-facing noun-group
// chain ("approvals pending"); the followup names the M sub-item that
// will wire the backend. Mirror the M10.1 pattern of pinning contract
// surfaces NOW and wiring call sites later.
func notWiredExit(stderr io.Writer, label, followup string) int {
	stderrf(stderr, fmt.Sprintf("wk: %s: not yet wired (%s)\n", label, followup))
	return 3
}
