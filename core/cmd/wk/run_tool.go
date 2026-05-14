package main

// run_tool.go — `wk tool <subcommand>` dispatcher.
//
// Migrates the M9.5 / M9.6 / M9.7 / M9.4.b operator surface from the
// retired `wk-tool` binary into the unified `wk` CLI under the `tool`
// noun-group. Subcommand flag shapes are preserved verbatim so existing
// runbook snippets keep working; only the invocation prefix changes
// (`wk-tool <verb>` → `wk tool <verb>`).
//
// Subcommands:
//
//	wk tool local-install  — M9.5 local-patch install
//	wk tool rollback       — M9.5 local-patch rollback
//	wk tool hosted export  — M9.7 hosted-tool export
//	wk tool hosted list    — M10.2 follow-up (exit 3)
//	wk tool hosted show    — M10.2 follow-up (exit 3)
//	wk tool share          — M9.4.b cross-customer tool share
//	wk tool list           — M10.2 follow-up (exit 3)

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

	"github.com/vadimtrunov/watchkeepers/core/pkg/github"
	"github.com/vadimtrunov/watchkeepers/core/pkg/hostedexport"
	"github.com/vadimtrunov/watchkeepers/core/pkg/localpatch"
	"github.com/vadimtrunov/watchkeepers/core/pkg/toolregistry"
	"github.com/vadimtrunov/watchkeepers/core/pkg/toolshare"
)

// dataDirEnvKey is the env var consulted for the `--data-dir` default
// across local-FS subcommands. Operators typically export it once
// globally. Mirror `core/cmd/wk-tool/run.go`'s `dataDirEnvKey`.
const dataDirEnvKey = watchkeeperDataDirEnv

// localSourcesEnvKey carries the comma-separated allowlist of source
// names the CLI is permitted to patch via `tool local-install`. Without
// the env var only the canonical default `local` source is permitted —
// see [validateSourceAgainstAllowlist] for the gate. Mirror
// `core/cmd/wk-tool/run.go` M9.5 iter-1 codex M6 fix.
const localSourcesEnvKey = "WATCHKEEPER_LOCAL_SOURCES"

// defaultLocalSource is the built-in default `--source` value for the
// `tool local-install` subcommand. Mirror ROADMAP §M9.5 prose: the
// operator may omit `--source` for the canonical local source.
const defaultLocalSource = "local"

func runTool(ctx context.Context, args []string, stdout, stderr io.Writer, env envLookup) int {
	if len(args) == 0 {
		stderrf(stderr, "wk: tool: missing subcommand (expected one of: local-install, rollback, hosted, share, list)\n")
		return 2
	}
	switch args[0] {
	case "local-install":
		return runLocalInstall(ctx, args[1:], stdout, stderr, env)
	case "rollback":
		return runRollback(ctx, args[1:], stdout, stderr, env)
	case "hosted":
		return runToolHosted(ctx, args[1:], stdout, stderr, env)
	case "share":
		return runShare(ctx, args[1:], stdout, stderr, env)
	case "list":
		return notWiredExit(stderr, "tool list", "M10.2 follow-up — requires toolregistry index seam")
	default:
		stderrf(stderr, fmt.Sprintf("wk: tool: unknown subcommand %q (expected one of: local-install, rollback, hosted, share, list)\n", args[0]))
		return 2
	}
}

func runToolHosted(ctx context.Context, args []string, stdout, stderr io.Writer, env envLookup) int {
	if len(args) == 0 {
		stderrf(stderr, "wk: tool hosted: missing subcommand (expected one of: export, list, show)\n")
		return 2
	}
	switch args[0] {
	case "export":
		return runHostedExport(ctx, args[1:], stdout, stderr, env)
	case "list":
		return notWiredExit(stderr, "tool hosted list", "M10.2 follow-up — requires hostedexport.List seam")
	case "show":
		return notWiredExit(stderr, "tool hosted show", "M10.2 follow-up — requires hostedexport.Show seam")
	default:
		stderrf(stderr, fmt.Sprintf("wk: tool hosted: unknown subcommand %q (expected one of: export, list, show)\n", args[0]))
		return 2
	}
}

// installFlags is the parsed flag bundle for `wk tool local-install`.
type installFlags struct {
	folder   string
	source   string
	reason   string
	operator string
	dataDir  string
}

// rollbackFlags is the parsed flag bundle for `wk tool rollback`.
type rollbackFlags struct {
	tool     string
	to       string
	source   string
	reason   string
	operator string
	dataDir  string
}

func runLocalInstall(ctx context.Context, args []string, stdout, stderr io.Writer, env envLookup) int {
	fs := flag.NewFlagSet("wk tool local-install", flag.ContinueOnError)
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
		stderrf(stderr, fmt.Sprintf("wk: tool local-install: %v\n", err))
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
		stderrf(stderr, fmt.Sprintf("wk: tool local-install: %v\n", err))
		return 1
	}
	printPatchSummary(stdout, "tool local-install", ev)
	return 0
}

func runRollback(ctx context.Context, args []string, stdout, stderr io.Writer, env envLookup) int {
	fs := flag.NewFlagSet("wk tool rollback", flag.ContinueOnError)
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
		stderrf(stderr, fmt.Sprintf("wk: tool rollback: %v\n", err))
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
		stderrf(stderr, fmt.Sprintf("wk: tool rollback: %v\n", err))
		return 1
	}
	printPatchSummary(stdout, "tool rollback", ev)
	return 0
}

// validateInstallFlags fills env-derived defaults, accumulates ALL
// missing-flag complaints in one pass, validates the `--source` against
// the env-supplied allowlist, and validates the `--operator` against
// the package-level identity rules. Mirrors the M9.5 iter-1 fixes.
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

// validateSourceAgainstAllowlist gates `--source` against the
// env-supplied comma-separated allowlist. An UNSET allowlist permits
// only the canonical default `local` — an operator who wants more
// MUST export `WATCHKEEPER_LOCAL_SOURCES`. Mirror M9.5 iter-1 codex M6
// fix.
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
// Mirror M9.5 iter-1 codex m4 fix: the operator gets ONE diagnostic
// listing every missing flag instead of N rerun-with-each.
type errMissingFlags struct{ names []string }

func (e errMissingFlags) Error() string {
	return "missing required flag(s): " + strings.Join(e.names, ", ")
}

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
// downstream localpatch / hostedexport / toolshare packages gate every
// resolved id via their `Validate*ID` helpers so a malformed hint
// surfaces as the package's sentinel error.
func trustHintResolver(_ context.Context, hint string) (string, error) {
	return hint, nil
}

// jsonlPublisher writes one JSON object per line to the supplied
// [io.Writer]. Each line is `{"topic":"...","event":{...},"published_at":"RFC3339"}`.
// Concurrency-safe via a mutex so a future caller dispatching multiple
// publishes from goroutines stays line-coherent. Mirror
// `core/cmd/wk-tool/run.go`'s jsonlPublisher.
//
// Iter-1 critic m8: `published_at` is the CLI host's wall clock, not
// the Keep server's. If the operator's machine has clock skew vs the
// server, the audit-record timestamp will be offset from any
// keepers_log row the same flow produces. Operators correlating CLI
// JSONL events against server-side log rows should treat the offset
// as bounded by their clock-skew SLA; a future M10.2.b refresh can
// switch to a server-side timestamp seam when the durable outbox
// publisher lands here.
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

// printPatchSummary writes the operator-facing summary for the
// local-install / rollback flows. Single line shape so a `make` recipe
// can `awk` over it.
func printPatchSummary(stdout io.Writer, op string, ev localpatch.LocalPatchApplied) {
	fmt.Fprintf(
		stdout, "wk: %s ok source=%s tool=%s version=%s diff_hash=%s correlation_id=%s\n",
		op, ev.SourceName, ev.ToolName, ev.ToolVersion, ev.DiffHash, ev.CorrelationID,
	)
}

// --- hosted-export ---

type hostedExportFlags struct {
	source      string
	tool        string
	destination string
	reason      string
	operator    string
	dataDir     string
}

func runHostedExport(ctx context.Context, args []string, stdout, stderr io.Writer, env envLookup) int {
	fs := flag.NewFlagSet("wk tool hosted export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var f hostedExportFlags
	fs.StringVar(&f.source, "source", "", "name of the kind=hosted source to export from (required)")
	fs.StringVar(&f.tool, "tool", "", "name of the tool to export (required)")
	fs.StringVar(&f.destination, "destination", "", "absolute path of the operator-supplied destination directory (required; absent or empty)")
	fs.StringVar(&f.reason, "reason", "", "operator-supplied audit text (required)")
	fs.StringVar(&f.operator, "operator", "", "operator identity (required)")
	fs.StringVar(&f.dataDir, "data-dir", "", "deployment data directory; falls back to $"+dataDirEnvKey)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if err := validateHostedExportFlags(&f, env); err != nil {
		stderrf(stderr, "wk: tool hosted export: "+err.Error()+"\n")
		return 2
	}
	if err := hostedexport.ValidateOperatorID(f.operator); err != nil {
		stderrf(stderr, "wk: tool hosted export: invalid --operator\n")
		return 2
	}
	exporter := buildExporter(f, stdout)
	res, err := exporter.Export(ctx, hostedexport.ExportRequest{
		SourceName:     f.source,
		ToolName:       f.tool,
		Destination:    f.destination,
		Reason:         f.reason,
		OperatorIDHint: f.operator,
	})
	if err != nil {
		stderrf(stderr, fmt.Sprintf("wk: tool hosted export: %v\n", err))
		return 1
	}
	fmt.Fprintf(
		stdout, "wk: tool hosted export ok source=%s tool=%s version=%s bundle_digest=%s correlation_id=%s\n",
		f.source, f.tool, res.ToolVersion, res.BundleDigest, res.CorrelationID,
	)
	return 0
}

// Iter-1 critic m6: flag-name shape unified across validators —
// every entry now prepends `--` so the operator copy-pastes a
// consistent diagnostic regardless of which subcommand fired.
func validateHostedExportFlags(f *hostedExportFlags, env envLookup) error {
	missing := []string{}
	if strings.TrimSpace(f.source) == "" {
		missing = append(missing, "--source")
	}
	if strings.TrimSpace(f.tool) == "" {
		missing = append(missing, "--tool")
	}
	if strings.TrimSpace(f.destination) == "" {
		missing = append(missing, "--destination")
	}
	if strings.TrimSpace(f.reason) == "" {
		missing = append(missing, "--reason")
	}
	if strings.TrimSpace(f.operator) == "" {
		missing = append(missing, "--operator")
	}
	if f.dataDir == "" {
		if v, ok := env.LookupEnv(dataDirEnvKey); ok && strings.TrimSpace(v) != "" {
			f.dataDir = v
		} else {
			missing = append(missing, "--data-dir (or "+dataDirEnvKey+")")
		}
	}
	if len(missing) > 0 {
		return errMissingFlags{names: missing}
	}
	return nil
}

func buildExporter(f hostedExportFlags, stdout io.Writer) *hostedexport.Exporter {
	clk := localpatch.ClockFunc(time.Now)
	pub := newJSONLPublisher(stdout, clk)
	return hostedexport.NewExporter(hostedexport.ExporterDeps{
		FS:                       hostedexport.OSFS{},
		Publisher:                pub,
		Clock:                    clk,
		SourceLookup:             hostedSourceLookup(f.source),
		OperatorIdentityResolver: trustHintResolver,
		DataDir:                  f.dataDir,
	})
}

func hostedSourceLookup(name string) hostedexport.SourceLookup {
	return func(_ context.Context, requested string) (toolregistry.SourceConfig, error) {
		if requested != name {
			return toolregistry.SourceConfig{}, fmt.Errorf("source %q not configured (cli is scoped to %q)", requested, name)
		}
		return toolregistry.SourceConfig{
			Name:       name,
			Kind:       toolregistry.SourceKindHosted,
			PullPolicy: toolregistry.PullPolicyOnDemand,
		}, nil
	}
}

// --- share ---

type shareFlags struct {
	source     string
	tool       string
	target     string
	owner      string
	repo       string
	base       string
	reason     string
	proposer   string
	dataDir    string
	tokenEnv   string
	githubBase string
}

func runShare(ctx context.Context, args []string, stdout, stderr io.Writer, env envLookup) int {
	fs := flag.NewFlagSet("wk tool share", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var f shareFlags
	fs.StringVar(&f.source, "source", "", "name of the source the tool lives under (required)")
	fs.StringVar(&f.tool, "tool", "", "name of the tool to share (required)")
	fs.StringVar(&f.target, "target", "platform", "target source: 'platform' or 'private'")
	fs.StringVar(&f.owner, "target-owner", "", "github owner (org or user) of the target repo (required)")
	fs.StringVar(&f.repo, "target-repo", "", "github repo name of the target repo (required)")
	fs.StringVar(&f.base, "target-base", "main", "base branch the PR targets for merge")
	fs.StringVar(&f.reason, "reason", "", "operator/agent-supplied audit text (required)")
	fs.StringVar(&f.proposer, "proposer", "", "proposer identity hint (required)")
	fs.StringVar(&f.dataDir, "data-dir", "", "deployment data directory; falls back to $"+dataDirEnvKey)
	fs.StringVar(&f.tokenEnv, "token-env", "WATCHKEEPER_GITHUB_TOKEN", "env var carrying the github PAT")
	fs.StringVar(&f.githubBase, "github-base-url", "", "github API base URL (default https://api.github.com)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if err := validateShareFlags(&f, env); err != nil {
		stderrf(stderr, "wk: tool share: "+err.Error()+"\n")
		return 2
	}
	if err := toolshare.ValidateProposerID(f.proposer); err != nil {
		stderrf(stderr, "wk: tool share: invalid --proposer\n")
		return 2
	}
	tokenValue, ok := env.LookupEnv(f.tokenEnv)
	if !ok || strings.TrimSpace(tokenValue) == "" {
		stderrf(stderr, fmt.Sprintf("wk: tool share: $%s unset or empty\n", f.tokenEnv))
		return 2
	}
	sharer, err := buildSharer(f, stdout, tokenValue)
	if err != nil {
		stderrf(stderr, fmt.Sprintf("wk: tool share: %v\n", err))
		return 1
	}
	res, err := sharer.Share(ctx, toolshare.ShareRequest{
		SourceName:     f.source,
		ToolName:       f.tool,
		TargetHint:     toolshare.TargetSource(f.target),
		Reason:         f.reason,
		ProposerIDHint: f.proposer,
	})
	if err != nil {
		stderrf(stderr, fmt.Sprintf("wk: tool share: %v\n", err))
		return 1
	}
	fmt.Fprintf(
		stdout, "wk: tool share ok pr=%d url=%s tool=%s version=%s branch=%s correlation_id=%s\n",
		res.PRNumber, res.PRHTMLURL, f.tool, res.ToolVersion, res.BranchName, res.CorrelationID,
	)
	return 0
}

// Iter-1 critic m6: unified `--` prefix on every missing-flag entry.
func validateShareFlags(f *shareFlags, env envLookup) error {
	missing := []string{}
	if strings.TrimSpace(f.source) == "" {
		missing = append(missing, "--source")
	}
	if strings.TrimSpace(f.tool) == "" {
		missing = append(missing, "--tool")
	}
	if strings.TrimSpace(f.owner) == "" {
		missing = append(missing, "--target-owner")
	}
	if strings.TrimSpace(f.repo) == "" {
		missing = append(missing, "--target-repo")
	}
	if strings.TrimSpace(f.reason) == "" {
		missing = append(missing, "--reason")
	}
	if strings.TrimSpace(f.proposer) == "" {
		missing = append(missing, "--proposer")
	}
	if f.target != "platform" && f.target != "private" {
		missing = append(missing, "--target (must be 'platform' or 'private')")
	}
	if f.dataDir == "" {
		if v, ok := env.LookupEnv(dataDirEnvKey); ok && strings.TrimSpace(v) != "" {
			f.dataDir = v
		} else {
			missing = append(missing, "--data-dir (or "+dataDirEnvKey+")")
		}
	}
	if len(missing) > 0 {
		return errMissingFlags{names: missing}
	}
	return nil
}

func buildSharer(f shareFlags, stdout io.Writer, tokenValue string) (*toolshare.Sharer, error) {
	clk := localpatch.ClockFunc(time.Now)
	pub := newJSONLPublisher(stdout, clk)
	opts := []github.ClientOption{github.WithTokenSource(github.StaticToken{Value: tokenValue})}
	if f.githubBase != "" {
		opts = append(opts, github.WithBaseURL(f.githubBase))
	}
	gh, err := github.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("build github client: %w", err)
	}
	target := toolshare.ResolvedTarget{
		Owner:  f.owner,
		Repo:   f.repo,
		Base:   f.base,
		Source: toolshare.TargetSource(f.target),
	}
	return toolshare.NewSharer(toolshare.SharerDeps{
		FS:                       toolshare.OSFS{},
		Publisher:                pub,
		Clock:                    clk,
		SourceLookup:             shareSourceLookup(f.source),
		ProposerIdentityResolver: trustHintResolver,
		TargetRepoResolver: func(_ context.Context, _ toolshare.ShareRequest) (toolshare.ResolvedTarget, error) {
			return target, nil
		},
		GitHubClient: gh,
		DataDir:      f.dataDir,
	}), nil
}

func shareSourceLookup(name string) toolshare.SourceLookup {
	return func(_ context.Context, requested string) (toolregistry.SourceConfig, error) {
		if requested != name {
			return toolregistry.SourceConfig{}, fmt.Errorf("source %q not configured (cli is scoped to %q)", requested, name)
		}
		return toolregistry.SourceConfig{
			Name:       name,
			Kind:       toolregistry.SourceKindHosted,
			PullPolicy: toolregistry.PullPolicyOnDemand,
		}, nil
	}
}
