// watchkeeper_creds.go defines the persistence seam the M7.1.c.a
// CreateApp saga step uses to store the OUT-OF-BAND Slack credential
// bundle returned by `apps.manifest.create`. The DAO is keyed by
// watchkeeper_id (NOT by Slack-assigned app_id) because the
// watchkeeper id is the stable saga-row id while a Slack app id can
// change across re-create scenarios.
//
// The in-memory implementation lives in `watchkeeper_creds_memory.go`;
// a Postgres-backed adapter is deferred per the M6.3.b "ship in-memory
// DAO + tests with consumer" lesson.
package spawn

import (
	"context"
	"errors"

	"github.com/google/uuid"

	slackmessenger "github.com/vadimtrunov/watchkeepers/core/pkg/messenger/slack"
)

// ErrCredsAlreadyStored is the typed error
// [WatchkeeperSlackAppCredsDAO.Put] returns when a row already exists
// for the supplied watchkeeper id. Matchable via [errors.Is].
//
// The idempotency boundary lives at the DAO layer rather than at the
// step layer because reconciliation belongs upstream — a re-run of
// the saga that finds existing creds is a programmer / operator
// concern (re-create scenario, manual recovery), not a silent
// no-op the step swallows.
var ErrCredsAlreadyStored = errors.New("spawn: watchkeeper slack app creds already stored")

// ErrCredsNotFound is the typed error
// [WatchkeeperSlackAppCredsDAO.Get] returns when no row matches the
// supplied watchkeeper id. Matchable via [errors.Is].
var ErrCredsNotFound = errors.New("spawn: watchkeeper slack app creds not found")

// WatchkeeperSlackAppCredsDAO is the persistence seam for the
// OUT-OF-BAND Slack credential bundle the CreateApp saga step
// receives via the M4.2.d [slack.CreateAppCredsSink] callback. The
// DAO is keyed by watchkeeper_id (the stable saga-row id), NOT by
// Slack-assigned app_id (which can change across re-create
// scenarios).
//
// All methods are safe for concurrent use across goroutines on the
// same DAO value; per-call state lives on the goroutine stack.
//
// Persistence discipline (mirrored on the SQL side in migration
// `020_watchkeeper_slack_app_creds.sql`): every secret field is
// stored as opaque text. A Phase-2 migration will rotate to
// encrypted bytea alongside the broader secrets-at-rest pass; the
// DAO contract treats the columns as opaque-bytes-with-extra-steps
// so the rotation does not churn the consumer surface.
type WatchkeeperSlackAppCredsDAO interface {
	// Put persists the supplied credentials bundle keyed by
	// `watchkeeperID`. Returns [ErrCredsAlreadyStored] when a row
	// already exists for the supplied id (the idempotency boundary
	// belongs upstream — see the package doc above).
	Put(ctx context.Context, watchkeeperID uuid.UUID, creds slackmessenger.CreateAppCredentials) error

	// Get returns the credentials bundle persisted for
	// `watchkeeperID` or [ErrCredsNotFound] when no such row exists.
	// The returned value is a value copy; mutating it does not affect
	// the persisted row.
	Get(ctx context.Context, watchkeeperID uuid.UUID) (slackmessenger.CreateAppCredentials, error)
}
