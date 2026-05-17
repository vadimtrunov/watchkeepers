package main

// run_channel_prod.go — production implementation of [channelResolver]
// for `wk channel reveal`. Built from env vars (Postgres DSN +
// operator org id + Slack bot token), composes the Postgres-backed
// [k2k.PostgresRepository] under
// [core/internal/keep/db.WithScope] with the [*slack.Client] from
// the M1.1.b channel primitives. Hoisted into its own file so the
// CLI's per-subcommand logic in `run_channel.go` stays focused on
// flag parsing + the test-injection seam.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vadimtrunov/watchkeepers/core/internal/keep/auth"
	"github.com/vadimtrunov/watchkeepers/core/internal/keep/db"
	"github.com/vadimtrunov/watchkeepers/core/pkg/k2k"
	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger/slack"
)

// errMissingChannelConfig is surfaced when one or more of the three
// env vars `wk channel reveal` requires is unset/blank. The
// diagnostic names ALL of them so the operator gets a single
// round-trip instead of one per missing var. Mirror [errMissingKeepConfig]
// from `keep.go`.
var errMissingChannelConfig = errors.New(
	"wk: channel reveal: env vars unset — export " +
		k2kPGDSNEnvKey + ", " + operatorOrgIDEnvKey + ", and " + slackBotTokenEnvKey,
)

// productionChannelResolver is the live [channelResolver]
// composition. Holds a pgxpool.Pool + auth.Claim + *slack.Client; the
// pool is owned by this struct (closed via the [cleanup] returned by
// the factory) so the CLI's `defer cleanup()` releases connections
// deterministically.
type productionChannelResolver struct {
	pool  *pgxpool.Pool
	claim auth.Claim
	slack *slack.Client
}

// ResolveSlackChannel runs the K2K row lookup under
// [db.WithScope](org-claim). The closure constructs a per-request
// [k2k.PostgresRepository] over the supplied [pgx.Tx] — matching the
// M1.1.a "Postgres adapter must take a per-call Querier, not a
// pgxpool.Pool" wiring contract documented on
// `core/pkg/k2k/postgres.go`. Returns the bound `slack_channel_id`
// or [k2k.ErrConversationNotFound] (wrapped) when the row is missing
// / RLS-invisible.
func (r *productionChannelResolver) ResolveSlackChannel(ctx context.Context, conversationID uuid.UUID) (string, error) {
	var channelID string
	err := db.WithScope(ctx, r.pool, r.claim, func(scopedCtx context.Context, tx pgx.Tx) error {
		repo := k2k.NewPostgresRepository(tx, nil)
		conv, getErr := repo.Get(scopedCtx, conversationID)
		if getErr != nil {
			return getErr
		}
		channelID = conv.SlackChannelID
		return nil
	})
	if err != nil {
		return "", err
	}
	return channelID, nil
}

// RevealChannel forwards to [*slack.Client.RevealChannel]. The M1.1.b
// implementation handles whitespace-trim + idempotent
// `already_in_channel` translation; no additional wrapping is needed
// at this seam.
func (r *productionChannelResolver) RevealChannel(ctx context.Context, channelID, userID string) error {
	return r.slack.RevealChannel(ctx, channelID, userID)
}

// newProductionChannelResolver resolves the three required env vars,
// opens a pgxpool.Pool, builds the Slack client, and returns a
// [channelResolver] backed by the live composition. The returned
// cleanup func closes the pool; tests should never invoke the
// production factory.
func newProductionChannelResolver(ctx context.Context, env envLookup) (channelResolver, func(), error) {
	dsn, _ := env.LookupEnv(k2kPGDSNEnvKey)
	orgID, _ := env.LookupEnv(operatorOrgIDEnvKey)
	token, _ := env.LookupEnv(slackBotTokenEnvKey)

	if strings.TrimSpace(dsn) == "" || strings.TrimSpace(orgID) == "" || strings.TrimSpace(token) == "" {
		return nil, nil, errMissingChannelConfig
	}

	// Validate org id shape client-side so a malformed env var fails
	// at the CLI boundary rather than as a confusing Postgres-side
	// type-cast error inside db.WithScope.
	if _, err := uuid.Parse(orgID); err != nil {
		return nil, nil, fmt.Errorf("wk: channel reveal: %s must be a UUID: %w", operatorOrgIDEnvKey, err)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("wk: channel reveal: open pg pool: %w", err)
	}

	client := slack.NewClient(
		slack.WithTokenSource(slack.StaticToken(token)),
	)

	return &productionChannelResolver{
			pool:  pool,
			claim: auth.Claim{Scope: "org", OrganizationID: orgID},
			slack: client,
		},
		func() { pool.Close() },
		nil
}
