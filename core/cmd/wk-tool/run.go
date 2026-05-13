package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/localpatch"
	"github.com/vadimtrunov/watchkeepers/core/pkg/toolregistry"
)

// run is the testable entrypoint. The signature mirrors
// `core/cmd/spawn-dev-bot/run.go` (M4.3) so test wiring stays
// consistent across the cmd-tree.
//
// Exit codes:
//
//	0 — success
//	1 — runtime failure (FS, source-lookup, identity-resolver, publish)
//	2 — usage error (no subcommand, unknown subcommand, missing flag,
//	    malformed input caught by the validator)
func run(ctx context.Context, args []string, stdout, stderr io.Writer, env envLookup) int {
	if len(args) == 0 {
		stderrf(stderr, "wk-tool: missing subcommand (expected one of: local-install, rollback, hosted-export, share)\n")
		return 2
	}
	switch args[0] {
	case "local-install":
		return runLocalInstall(ctx, args[1:], stdout, stderr, env)
	case "rollback":
		return runRollback(ctx, args[1:], stdout, stderr, env)
	case "hosted-export":
		return runHostedExport(ctx, args[1:], stdout, stderr, env)
	case "share":
		return runShare(ctx, args[1:], stdout, stderr, env)
	case "-h", "--help", "help":
		writeUsage(stdout)
		return 0
	default:
		stderrf(stderr, fmt.Sprintf("wk-tool: unknown subcommand %q (expected one of: local-install, rollback, hosted-export, share)\n", args[0]))
		return 2
	}
}

// installFlags is the parsed flag bundle for `wk-tool local-install`.
type installFlags struct {
	folder   string
	source   string
	reason   string
	operator string
	dataDir  string
}

// rollbackFlags is the parsed flag bundle for `wk-tool rollback`.
type rollbackFlags struct {
	tool     string
	to       string
	source   string
	reason   string
	operator string
	dataDir  string
}

// dataDirEnvKey is the env var consulted for the `--data-dir`
// default. Operator deployments typically export it once globally.
const dataDirEnvKey = "WATCHKEEPER_DATA_DIR"

// localSourcesEnvKey is the env var carrying the comma-separated
// allowlist of source names the CLI is permitted to patch. Iter-1
// codex M6 fix: without an allowlist, an operator could `--source
// github-tools --reason "fix"` and silently write into a `kind: git`
// source's data dir; the M9.1.b scheduler would overwrite the patch
// on its next sync AND leave abandoned snapshots. The CLI cannot
// read the operator's YAML config directly (M9.5 scope), but it CAN
// enforce a user-supplied allowlist so a typo lands a clean
// diagnostic.
const localSourcesEnvKey = "WATCHKEEPER_LOCAL_SOURCES"

// defaultLocalSource is the built-in default `--source` value for
// the local-install subcommand. Mirror the ROADMAP §M9.5 prose: the
// operator may omit `--source` for the canonical local source.
const defaultLocalSource = "local"

func runLocalInstall(ctx context.Context, args []string, stdout, stderr io.Writer, env envLookup) int {
	fs := flag.NewFlagSet("wk-tool local-install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var f installFlags
	fs.StringVar(&f.folder, "folder", "", "operator-supplied folder containing the new tool tree (required)")
	fs.StringVar(&f.source, "source", defaultLocalSource, "name of the kind=local source to patch")
	fs.StringVar(&f.reason, "reason", "", "operator-supplied audit text (required)")
	fs.StringVar(&f.operator, "operator", "", "operator identity (required)")
	fs.StringVar(&f.dataDir, "data-dir", "", "deployment data directory; falls back to $"+dataDirEnvKey)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if err := validateInstallFlags(&f, env); err != nil {
		stderrf(stderr, fmt.Sprintf("wk-tool: %v\n", err))
		return 2
	}

	installer := buildInstaller(f, stdout)
	ev, err := installer.Install(ctx, localpatch.InstallRequest{
		SourceName:     f.source,
		FolderPath:     f.folder,
		Reason:         f.reason,
		OperatorIDHint: f.operator,
	})
	if err != nil {
		stderrf(stderr, fmt.Sprintf("wk-tool: install: %v\n", err))
		return 1
	}
	printSummary(stdout, "local-install", ev)
	return 0
}

func runRollback(ctx context.Context, args []string, stdout, stderr io.Writer, env envLookup) int {
	fs := flag.NewFlagSet("wk-tool rollback", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var f rollbackFlags
	fs.StringVar(&f.tool, "name", "", "tool name to roll back (required)")
	fs.StringVar(&f.to, "to", "", "version to restore (required); MUST exist as a snapshot")
	fs.StringVar(&f.source, "source", defaultLocalSource, "name of the kind=local source")
	fs.StringVar(&f.reason, "reason", "", "operator-supplied audit text (required)")
	fs.StringVar(&f.operator, "operator", "", "operator identity (required)")
	fs.StringVar(&f.dataDir, "data-dir", "", "deployment data directory; falls back to $"+dataDirEnvKey)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if err := validateRollbackFlags(&f, env); err != nil {
		stderrf(stderr, fmt.Sprintf("wk-tool: %v\n", err))
		return 2
	}

	rollbacker := buildRollbacker(f, stdout)
	ev, err := rollbacker.Rollback(ctx, localpatch.RollbackRequest{
		SourceName:     f.source,
		ToolName:       f.tool,
		TargetVersion:  f.to,
		Reason:         f.reason,
		OperatorIDHint: f.operator,
	})
	if err != nil {
		stderrf(stderr, fmt.Sprintf("wk-tool: rollback: %v\n", err))
		return 1
	}
	printSummary(stdout, "rollback", ev)
	return 0
}

// validateInstallFlags fills env-derived defaults, accumulates ALL
// missing-flag complaints in one pass (iter-1 codex m4 fix —
// operator learns N missing flags in one round-trip), validates the
// `--source` against the env-supplied allowlist (M6 fix), and
// validates the `--operator` against the same allowlist the package
// applies post-resolver (M2 fix).
func validateInstallFlags(f *installFlags, env envLookup) error {
	missing := []string{}
	if strings.TrimSpace(f.folder) == "" {
		missing = append(missing, "--folder")
	}
	if strings.TrimSpace(f.reason) == "" {
		missing = append(missing, "--reason")
	}
	if strings.TrimSpace(f.operator) == "" {
		missing = append(missing, "--operator")
	}
	if f.dataDir == "" {
		if v, ok := env.LookupEnv(dataDirEnvKey); ok && v != "" {
			f.dataDir = v
		}
	}
	if strings.TrimSpace(f.dataDir) == "" {
		missing = append(missing, "--data-dir")
	}
	if strings.TrimSpace(f.source) == "" {
		missing = append(missing, "--source")
	}
	if len(missing) > 0 {
		return errMissingFlags{names: missing}
	}
	if err := validateSourceAgainstAllowlist(f.source, env); err != nil {
		return err
	}
	if err := localpatch.ValidateOperatorID(f.operator); err != nil {
		return fmt.Errorf("invalid --operator: %w", err)
	}
	return nil
}

// validateRollbackFlags is the rollback-side twin.
func validateRollbackFlags(f *rollbackFlags, env envLookup) error {
	missing := []string{}
	if strings.TrimSpace(f.tool) == "" {
		missing = append(missing, "--name")
	}
	if strings.TrimSpace(f.to) == "" {
		missing = append(missing, "--to")
	}
	if strings.TrimSpace(f.reason) == "" {
		missing = append(missing, "--reason")
	}
	if strings.TrimSpace(f.operator) == "" {
		missing = append(missing, "--operator")
	}
	if f.dataDir == "" {
		if v, ok := env.LookupEnv(dataDirEnvKey); ok && v != "" {
			f.dataDir = v
		}
	}
	if strings.TrimSpace(f.dataDir) == "" {
		missing = append(missing, "--data-dir")
	}
	if strings.TrimSpace(f.source) == "" {
		missing = append(missing, "--source")
	}
	if len(missing) > 0 {
		return errMissingFlags{names: missing}
	}
	if err := validateSourceAgainstAllowlist(f.source, env); err != nil {
		return err
	}
	if err := localpatch.ValidateOperatorID(f.operator); err != nil {
		return fmt.Errorf("invalid --operator: %w", err)
	}
	return nil
}

// validateSourceAgainstAllowlist gates `--source` against the env-
// supplied comma-separated allowlist. An UNSET allowlist permits
// only the canonical default `local` — an operator who wants more
// MUST export `WATCHKEEPER_LOCAL_SOURCES`. Iter-1 codex M6 fix.
func validateSourceAgainstAllowlist(source string, env envLookup) error {
	allowedRaw, _ := env.LookupEnv(localSourcesEnvKey)
	allowed := map[string]struct{}{}
	if strings.TrimSpace(allowedRaw) == "" {
		allowed[defaultLocalSource] = struct{}{}
	} else {
		for _, s := range strings.Split(allowedRaw, ",") {
			s = strings.TrimSpace(s)
			if s != "" {
				allowed[s] = struct{}{}
			}
		}
	}
	if _, ok := allowed[source]; !ok {
		names := make([]string, 0, len(allowed))
		for n := range allowed {
			names = append(names, n)
		}
		return fmt.Errorf("source %q not in allowlist (export %s=name1,name2 to extend; current: %v)", source, localSourcesEnvKey, names)
	}
	return nil
}

// errMissingFlags carries the accumulated set of missing flag names.
// Iter-1 codex m4 fix: the operator gets ONE diagnostic listing
// every missing flag instead of N rerun-with-each.
type errMissingFlags struct{ names []string }

func (e errMissingFlags) Error() string {
	return "missing required flag(s): " + strings.Join(e.names, ", ")
}

// buildInstaller wires a production [localpatch.Installer] with the
// JSONL stdout publisher + a trust-the-hint operator resolver + a
// single-source lookup that resolves only the configured source name
// to a `kind: local` config. Iter-1 critic M3 fix: no recover wrapper
// — `NewInstaller` panics only on nil-dep / invalid DataDir contracts
// which the CLI's validateInstallFlags has already enforced. A
// genuine panic propagates so the operator sees the stack.
func buildInstaller(f installFlags, stdout io.Writer) *localpatch.Installer {
	clk := localpatch.ClockFunc(time.Now)
	pub := newJSONLPublisher(stdout, clk)
	return localpatch.NewInstaller(localpatch.InstallerDeps{
		FS:                       localpatch.OSFS{},
		Publisher:                pub,
		Clock:                    clk,
		SourceLookup:             singleSourceLookup(f.source),
		OperatorIdentityResolver: trustHintResolver,
		DataDir:                  f.dataDir,
	})
}

func buildRollbacker(f rollbackFlags, stdout io.Writer) *localpatch.Rollbacker {
	clk := localpatch.ClockFunc(time.Now)
	pub := newJSONLPublisher(stdout, clk)
	return localpatch.NewRollbacker(localpatch.RollbackerDeps{
		FS:                       localpatch.OSFS{},
		Publisher:                pub,
		Clock:                    clk,
		SourceLookup:             singleSourceLookup(f.source),
		OperatorIdentityResolver: trustHintResolver,
		DataDir:                  f.dataDir,
	})
}

// singleSourceLookup returns a [localpatch.SourceLookup] that
// resolves ONE source name to a `kind: local` config and refuses
// anything else. Production deployments will swap this for a real
// config-backed lookup that reads `tool_sources` from the operator's
// YAML; for the M9.5 atomic ship the CLI is a one-shot operator
// tool and trusting the operator's `--source <name>` flag is the
// right scope (the operator's config is the source of truth — the
// CLI cannot revalidate it without parsing the same config the
// running watchkeeper already has).
func singleSourceLookup(name string) localpatch.SourceLookup {
	return func(_ context.Context, requested string) (toolregistry.SourceConfig, error) {
		if requested != name {
			return toolregistry.SourceConfig{}, fmt.Errorf("source %q not configured (cli is scoped to %q)", requested, name)
		}
		return toolregistry.SourceConfig{
			Name:       name,
			Kind:       toolregistry.SourceKindLocal,
			PullPolicy: toolregistry.PullPolicyOnDemand,
		}, nil
	}
}

// trustHintResolver returns the operator-supplied hint verbatim. A
// hosted deployment swaps this for a verified-OIDC resolver that
// rejects when the hint disagrees with the bound identity. The
// localpatch package's [localpatch.MaxOperatorIDLength] +
// [localpatch.validOperatorID] gate every resolved id so a malformed
// hint surfaces as [localpatch.ErrInvalidOperatorID].
func trustHintResolver(_ context.Context, hint string) (string, error) {
	return hint, nil
}

// jsonlPublisher is the production [localpatch.Publisher]: writes
// one JSON object per line to the supplied [io.Writer]. Each line is
// `{"topic":"...","event":{...},"published_at":"RFC3339"}`. Concurrency-
// safe via a mutex so a future caller dispatching multiple publishes
// from goroutines stays line-coherent. The `published_at` timestamp
// comes from the supplied [localpatch.Clock] (iter-1 critic M4 fix:
// previously `time.Now()` directly, breaking determinism for any
// downstream test capturing the envelope timestamp).
type jsonlPublisher struct {
	mu  sync.Mutex
	w   io.Writer
	clk localpatch.Clock
}

func newJSONLPublisher(w io.Writer, clk localpatch.Clock) *jsonlPublisher {
	return &jsonlPublisher{w: w, clk: clk}
}

func (p *jsonlPublisher) Publish(_ context.Context, topic string, event any) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	line := struct {
		Topic       string    `json:"topic"`
		Event       any       `json:"event"`
		PublishedAt time.Time `json:"published_at"`
	}{
		Topic:       topic,
		Event:       event,
		PublishedAt: p.clk.Now().UTC(),
	}
	data, err := json.Marshal(line)
	if err != nil {
		return fmt.Errorf("jsonl publish: marshal: %w", err)
	}
	if _, err := p.w.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("jsonl publish: write: %w", err)
	}
	return nil
}

// printSummary writes the operator-facing summary to stdout. Single
// line shape so a `make` recipe can `awk` over it. Mirror
// `core/cmd/spawn-dev-bot/run.go`'s `printRedactedSummary`.
func printSummary(stdout io.Writer, op string, ev localpatch.LocalPatchApplied) {
	fmt.Fprintf(
		stdout, "wk-tool: %s ok source=%s tool=%s version=%s diff_hash=%s correlation_id=%s\n",
		op, ev.SourceName, ev.ToolName, ev.ToolVersion, ev.DiffHash, ev.CorrelationID,
	)
}

// writeUsage prints the top-level usage to stdout.
func writeUsage(stdout io.Writer) {
	fmt.Fprintln(stdout, "Usage: wk-tool <subcommand> [flags]")
	fmt.Fprintln(stdout, "Subcommands:")
	fmt.Fprintln(stdout, "  local-install --folder <path> --source <name> --reason <text> --operator <id> [--data-dir <dir>]")
	fmt.Fprintln(stdout, "  rollback --name <tool> --to <version> [--source <name>] --reason <text> --operator <id> [--data-dir <dir>]")
	fmt.Fprintln(stdout, "  hosted-export --source <name> --tool <name> --destination <abs-path>")
	fmt.Fprintln(stdout, "                --reason <text> --operator <id> [--data-dir <dir>]")
	fmt.Fprintln(stdout, "  share --source <name> --tool <name> --target <platform|private>")
	fmt.Fprintln(stdout, "        --target-owner <owner> --target-repo <repo> [--target-base main]")
	fmt.Fprintln(stdout, "        --reason <text> --proposer <id>")
	fmt.Fprintln(stdout, "        [--token-env WATCHKEEPER_GITHUB_TOKEN] [--data-dir <dir>]")
	fmt.Fprintln(stdout, "Audit channels: localpatch.local_patch_applied, hostedexport.hosted_tool_exported, toolshare.tool_share_proposed, toolshare.tool_share_pr_opened — JSONL events written to stdout, one per line.")
	fmt.Fprintln(stdout, "Source allowlist (local-install only): set "+localSourcesEnvKey+"=name1,name2 to extend the default ('local').")
	fmt.Fprintln(stdout, "Snapshot retention: <DataDir>/_history/<source>/<tool>/<version>/ accumulates indefinitely; operator must prune manually.")
}
