package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/github"
	"github.com/vadimtrunov/watchkeepers/core/pkg/localpatch"
	"github.com/vadimtrunov/watchkeepers/core/pkg/toolregistry"
	"github.com/vadimtrunov/watchkeepers/core/pkg/toolshare"
)

// shareFlags is the parsed flag bundle for `wk-tool share`.
type shareFlags struct {
	source     string
	tool       string
	target     string // platform | private
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
	fs := flag.NewFlagSet("wk-tool share", flag.ContinueOnError)
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
		stderrf(stderr, "wk-tool: share: "+err.Error()+"\n")
		return 2
	}
	if err := toolshare.ValidateProposerID(f.proposer); err != nil {
		stderrf(stderr, "wk-tool: share: invalid --proposer\n")
		return 2
	}
	tokenValue, ok := env.LookupEnv(f.tokenEnv)
	if !ok || strings.TrimSpace(tokenValue) == "" {
		stderrf(stderr, fmt.Sprintf("wk-tool: share: $%s unset or empty\n", f.tokenEnv))
		return 2
	}
	sharer, err := buildSharer(f, stdout, tokenValue)
	if err != nil {
		stderrf(stderr, fmt.Sprintf("wk-tool: share: %v\n", err))
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
		stderrf(stderr, fmt.Sprintf("wk-tool: share: %v\n", err))
		return 1
	}
	fmt.Fprintf(
		stdout, "wk-tool: share ok pr=%d url=%s tool=%s version=%s branch=%s correlation_id=%s\n",
		res.PRNumber, res.PRHTMLURL, f.tool, res.ToolVersion, res.BranchName, res.CorrelationID,
	)
	return 0
}

func validateShareFlags(f *shareFlags, env envLookup) error {
	missing := []string{}
	if strings.TrimSpace(f.source) == "" {
		missing = append(missing, "source")
	}
	if strings.TrimSpace(f.tool) == "" {
		missing = append(missing, "tool")
	}
	if strings.TrimSpace(f.owner) == "" {
		missing = append(missing, "target-owner")
	}
	if strings.TrimSpace(f.repo) == "" {
		missing = append(missing, "target-repo")
	}
	if strings.TrimSpace(f.reason) == "" {
		missing = append(missing, "reason")
	}
	if strings.TrimSpace(f.proposer) == "" {
		missing = append(missing, "proposer")
	}
	// Iter-1 M10 fix (reviewer A) + M5 fix (reviewer B): treat
	// invalid --target as a missing-flag entry (so accumulation
	// works with the other missing flags) AND do NOT echo the bad
	// value to stderr (mirror M9.5 iter-1 M2 hygiene for
	// --operator / --proposer).
	if f.target != "platform" && f.target != "private" {
		missing = append(missing, "target (must be 'platform' or 'private')")
	}
	if f.dataDir == "" {
		if v, ok := env.LookupEnv(dataDirEnvKey); ok && strings.TrimSpace(v) != "" {
			f.dataDir = v
		} else {
			missing = append(missing, "data-dir (or "+dataDirEnvKey+")")
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

// shareSourceLookup mirrors [singleSourceLookup] for the share
// flow: returns a `kind: hosted` config when the operator-supplied
// name matches; refuses anything else. Production deployments swap
// for a config-backed lookup. The kind reported here is informational
// only — `toolshare.Sharer.Share` does NOT gate on source kind
// (any configured source may be shared).
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
