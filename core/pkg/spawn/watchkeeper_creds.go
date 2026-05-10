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
	"time"

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

	// PutInstallTokens persists the M7.1.c.b.b OAuthInstall step's
	// encrypted bot/user/refresh tokens onto the existing row keyed by
	// `watchkeeperID`. The three byte slices MUST already be ciphertexts
	// produced by [secrets.Encrypter.Encrypt] (M7.1.c.b.a primitive);
	// the DAO contract treats them as opaque bytes — no encryption layer
	// lives on this side of the seam.
	//
	// `refreshCT` MUST be nil or zero-length when the OAuth response did
	// not carry a refresh_token (rotation disabled on the app manifest).
	// Storing an encrypted-empty-string here would silently disagree with
	// downstream `len() == 0` callers (encrypting an empty plaintext
	// produces a 28-byte ciphertext). The caller short-circuits the
	// encryption call when the plaintext is empty.
	//
	// `expiresAt` is the UTC expiry derived from the response
	// `expires_in`; the zero [time.Time] is the documented sentinel for
	// "no expiry" (rotation disabled).
	//
	// Returns [ErrCredsNotFound] when no row exists for the supplied
	// `watchkeeperID` (the row must have been created by the M7.1.c.a
	// CreateAppStep first). Idempotent on second call (overwrites — re-
	// install scenario; no `ErrAlreadyInstalled` sentinel to keep retry
	// semantics simple).
	PutInstallTokens(
		ctx context.Context,
		watchkeeperID uuid.UUID,
		botCT []byte,
		userCT []byte,
		refreshCT []byte,
		expiresAt time.Time,
	) error

	// WipeInstallTokens is the M7.3.c rollback method the
	// [OAuthInstallStep.Compensate] dispatches when the saga aborts.
	// It clears the bot/user/refresh ciphertexts + the expiry/install
	// timestamps for the row keyed by `watchkeeperID`, leaving the
	// row itself in place (the M7.1.c.a `slack_app_creds` columns
	// — `client_id`, `client_secret`, `signing_secret`,
	// `verification_token` — survive so the future
	// [SlackAppTeardown.TeardownApp] production wrapper can read
	// the abandoned `app_id` before its own platform-side wipe).
	//
	// MUST be idempotent: calling Wipe twice on the same
	// `watchkeeperID` returns nil on the second call (no row =
	// already wiped, treated as success). A missing row is NOT an
	// error — the rollback chain is best-effort and double-Compensate
	// is allowed (M7.3.b discipline).
	//
	// Implementations MUST overwrite (not zero-len-clear) the
	// ciphertext columns so a future select against the row
	// observes `bot_access_token IS NULL` rather than an
	// empty-bytea. The in-memory variant simply deletes the install-
	// tokens map entry; a Postgres adapter SETs the columns to NULL.
	//
	// Future M7.3.d-or-M7.4 reconciler landing the production
	// [SlackAppTeardown] wrapper will introduce a companion
	// `WipeAll` (full-row deletion including the M7.1.c.a app
	// credentials) — deferred per the M6.3.b "ship in-memory DAO
	// with consumer" rhythm; a method without a caller is dead
	// surface that drifts out of date.
	WipeInstallTokens(ctx context.Context, watchkeeperID uuid.UUID) error
}
