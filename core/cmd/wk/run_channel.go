package main

// run_channel.go — `wk channel reveal` subcommand.
//
// `wk channel reveal --user <slack-user-id> <conversation-id>` resolves
// the K2K conversation row's `slack_channel_id` and calls Slack
// `conversations.invite` with the supplied user id. Mirrors the
// M1.1.b `Client.RevealChannel` boundary verbatim: empty channel id /
// empty user id fail-fast without contacting Slack.
//
// Wiring contract:
//
//   - Postgres pool — built from `WATCHKEEPER_K2K_PG_DSN`.
//   - auth.Claim — `scope=org` with `OrganizationID` resolved from
//     `WATCHKEEPER_OPERATOR_ORG_ID`. The org claim activates the K2K
//     row's per-tenant RLS policy from migration 029.
//   - slack.Client — built from `WATCHKEEPER_SLACK_BOT_TOKEN`. The
//     token never reaches argv per the package-level PII discipline.
//
// Test-injection seam: [channelResolver] is the surface
// `runChannel` consumes. Production wiring constructs the Postgres
// pool + slack.Client; tests inject a fake. This mirrors the
// envLookup / fileSystem injection pattern in `run_watchkeeper.go` /
// `run_tool.go` / `core/cmd/spawn-dev-bot/run.go`.
//
// Exit codes match the wk top-of-package contract:
//
//	0 — success
//	1 — runtime failure (Postgres / Slack / scope)
//	2 — usage error (missing flag, malformed UUID, env var unset)

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/google/uuid"
)

// Env var keys consulted by [runChannel] to build the Postgres pool +
// Slack client. Hoisted to constants so the help-text writer and the
// resolver constructor stay in sync.
const (
	k2kPGDSNEnvKey      = "WATCHKEEPER_K2K_PG_DSN"
	operatorOrgIDEnvKey = "WATCHKEEPER_OPERATOR_ORG_ID"
	slackBotTokenEnvKey = "WATCHKEEPER_SLACK_BOT_TOKEN"
)

// channelResolver is the test-injection seam [runChannel] consumes.
// It abstracts the two side effects `wk channel reveal` performs:
// looking up the slack channel id from a K2K row, and issuing the
// Slack `conversations.invite` call. The production implementation
// composes Postgres + slack.Client; tests inject a fake.
//
// The interface is intentionally narrower than the [k2k.Repository] /
// [k2k.SlackChannels] surfaces — the CLI only needs the resolve +
// reveal pair, so binding the broader surfaces here would force test
// stubs to implement methods the CLI never calls. Mirrors the
// "narrow per-call seam" discipline from M1.1.a's [k2k.Querier].
type channelResolver interface {
	// ResolveSlackChannel returns the `slack_channel_id` bound to the
	// supplied K2K conversation id, scoped to the operator's
	// organization. Returns the empty string when the row exists but
	// has no Slack channel bound (an M1.1.c orphan from a failed
	// Open()); the caller surfaces a typed error in that case so the
	// operator gets a clear "channel not provisioned" diagnostic
	// rather than a Slack-side `channel_not_found`.
	ResolveSlackChannel(ctx context.Context, conversationID uuid.UUID) (string, error)

	// RevealChannel calls Slack `conversations.invite` (single-user
	// payload) to reveal the channel to the supplied human user. The
	// implementation mirrors `*slack.Client.RevealChannel` exactly.
	RevealChannel(ctx context.Context, channelID, userID string) error
}

// channelResolverFactory is the constructor [runChannel] calls to
// obtain a [channelResolver]. Hoisted to a package-scoped variable so
// tests swap it for a fake without monkey-patching the production
// constructor. Production wiring (`newProductionChannelResolver`)
// reads env vars + opens a Postgres pool + builds the slack client.
//
//nolint:gochecknoglobals // intentional package-scoped test seam; the test files swap this for a fake.
var channelResolverFactory = newProductionChannelResolver

// runChannel dispatches the `channel` noun-group to its
// per-subcommand handler. Only `reveal` is wired at M1.1.c; future
// subcommands (e.g. `wk channel close`, `wk channel summarize`) ride
// here with the M1.6 / M1.7 follow-ups.
func runChannel(ctx context.Context, args []string, stdout, stderr io.Writer, env envLookup) int {
	if len(args) == 0 {
		stderrf(stderr, "wk: channel: missing subcommand (expected: reveal)\n")
		return 2
	}
	switch args[0] {
	case "reveal":
		return runChannelReveal(ctx, args[1:], stdout, stderr, env)
	default:
		stderrf(stderr, fmt.Sprintf("wk: channel: unknown subcommand %q (expected: reveal)\n", args[0]))
		return 2
	}
}

// runChannelReveal parses `wk channel reveal --user <id> <conv-id>`,
// resolves the row's `slack_channel_id`, and calls the Slack
// `conversations.invite` boundary via the injected [channelResolver].
//
// Required flag/positional:
//
//   - `--user <slack-user-id>`: the human Slack user id the channel
//     is being revealed to. Required; matches M1.1.b's
//     `RevealChannel(channelID, userID)` contract.
//   - `<conversation-id>`: the K2K conversation's UUID. Validated
//     client-side (UUID-shape) before the resolver call so a malformed
//     positional fails-fast with exit 2 rather than a confusing
//     Postgres-side "invalid input syntax for type uuid".
//
// Iter-1-aligned diagnostics: env-var miss surfaces a single line
// naming ALL three required envs so the operator gets one round-trip;
// per-step failures surface the offending boundary (resolver / Slack)
// so the operator knows which side to debug.
func runChannelReveal(ctx context.Context, args []string, stdout, stderr io.Writer, env envLookup) int {
	fs := flag.NewFlagSet("wk channel reveal", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var userID string
	fs.StringVar(&userID, "user", "", "Slack user id to invite into the K2K channel (required)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	rest := fs.Args()
	if len(rest) == 0 {
		stderrf(stderr, "wk: channel reveal: missing positional <conversation-id>\n")
		return 2
	}
	if len(rest) > 1 {
		stderrf(stderr, fmt.Sprintf("wk: channel reveal: extra positional args after <conversation-id>: %v\n", rest[1:]))
		return 2
	}

	convIDArg := strings.TrimSpace(rest[0])
	if convIDArg == "" {
		stderrf(stderr, "wk: channel reveal: <conversation-id> must be non-empty\n")
		return 2
	}
	if strings.TrimSpace(userID) == "" {
		stderrf(stderr, "wk: channel reveal: missing required flag --user\n")
		return 2
	}

	convID, err := uuid.Parse(convIDArg)
	if err != nil {
		stderrf(stderr, fmt.Sprintf("wk: channel reveal: <conversation-id> must be a UUID: %v\n", err))
		return 2
	}

	resolver, cleanup, err := channelResolverFactory(ctx, env)
	if err != nil {
		stderrf(stderr, fmt.Sprintf("wk: channel reveal: %v\n", err))
		return 2
	}
	if cleanup != nil {
		defer cleanup()
	}

	channelID, err := resolver.ResolveSlackChannel(ctx, convID)
	if err != nil {
		stderrf(stderr, fmt.Sprintf("wk: channel reveal: resolve conversation %s: %v\n", convID, err))
		return 1
	}
	if channelID == "" {
		stderrf(stderr, fmt.Sprintf(
			"wk: channel reveal: conversation %s has no slack_channel_id bound (orphan from a failed K2K Open)\n",
			convID,
		))
		return 1
	}

	if err := resolver.RevealChannel(ctx, channelID, userID); err != nil {
		stderrf(stderr, fmt.Sprintf("wk: channel reveal: invite %s to %s: %v\n", userID, channelID, err))
		return 1
	}

	fmt.Fprintf(
		stdout, "wk: channel reveal ok conversation_id=%s slack_channel_id=%s user_id=%s\n",
		convID, channelID, userID,
	)
	return 0
}
