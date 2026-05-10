// watchkeeper_creds_memory.go ships the in-memory
// [WatchkeeperSlackAppCredsDAO] implementation used by the M7.1.c.a
// CreateApp saga step's unit tests AND (intentionally) by callers
// that want a zero-dep credential store for dev / smoke runs without
// a Postgres-backed adapter wired up.
//
// The store is goroutine-safe: a single per-instance read/write
// mutex guards the underlying map. Throughput is irrelevant —
// production callers wire a real adapter; this type exists so the
// test surface and the dev-loop wiring share one implementation.
package spawn

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

	slackmessenger "github.com/vadimtrunov/watchkeepers/core/pkg/messenger/slack"
)

// MemoryWatchkeeperSlackAppCredsDAO is a process-local
// [WatchkeeperSlackAppCredsDAO] implementation backed by a
// `map[uuid.UUID]slack.CreateAppCredentials`. Constructed via
// [NewMemoryWatchkeeperSlackAppCredsDAO]; the zero value is NOT
// usable — callers must always go through the constructor so the
// internal maps are non-nil.
//
// Concurrency: read methods (`Get`) take an RLock so concurrent
// reads across distinct ids never block each other. Write methods
// (`Put`, `PutInstallTokens`) take the write lock for the duration of
// the call.
type MemoryWatchkeeperSlackAppCredsDAO struct {
	mu     sync.RWMutex
	rows   map[uuid.UUID]slackmessenger.CreateAppCredentials
	tokens map[uuid.UUID]memoryInstallTokens
}

// memoryInstallTokens mirrors the migration-021 install columns
// (`bot_access_token`, `user_access_token`, `refresh_token`,
// `bot_token_expires_at`, `installed_at`) on the in-memory side. All
// three byte slices carry AES-GCM ciphertexts (the DAO contract treats
// them as opaque bytes — encryption is the caller's job per the
// M7.1.c.b.b plan).
type memoryInstallTokens struct {
	botCT       []byte
	userCT      []byte
	refreshCT   []byte
	expiresAt   time.Time
	installedAt time.Time
}

// NewMemoryWatchkeeperSlackAppCredsDAO returns an empty in-memory
// store.
func NewMemoryWatchkeeperSlackAppCredsDAO() *MemoryWatchkeeperSlackAppCredsDAO {
	return &MemoryWatchkeeperSlackAppCredsDAO{
		rows:   make(map[uuid.UUID]slackmessenger.CreateAppCredentials),
		tokens: make(map[uuid.UUID]memoryInstallTokens),
	}
}

// Compile-time assertion: [*MemoryWatchkeeperSlackAppCredsDAO]
// satisfies [WatchkeeperSlackAppCredsDAO] (AC4 / AC5). Pins the
// integration shape so a future change to the interface surface
// fails the build here.
var _ WatchkeeperSlackAppCredsDAO = (*MemoryWatchkeeperSlackAppCredsDAO)(nil)

// Put satisfies [WatchkeeperSlackAppCredsDAO.Put]. Returns
// [ErrCredsAlreadyStored] when a row already exists for the supplied
// `watchkeeperID`.
func (m *MemoryWatchkeeperSlackAppCredsDAO) Put(
	_ context.Context,
	watchkeeperID uuid.UUID,
	creds slackmessenger.CreateAppCredentials,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.rows[watchkeeperID]; exists {
		return ErrCredsAlreadyStored
	}
	m.rows[watchkeeperID] = creds
	return nil
}

// Get satisfies [WatchkeeperSlackAppCredsDAO.Get]. Returns
// [ErrCredsNotFound] when no row matches the supplied
// `watchkeeperID`.
func (m *MemoryWatchkeeperSlackAppCredsDAO) Get(
	_ context.Context,
	watchkeeperID uuid.UUID,
) (slackmessenger.CreateAppCredentials, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	creds, ok := m.rows[watchkeeperID]
	if !ok {
		return slackmessenger.CreateAppCredentials{}, ErrCredsNotFound
	}
	return creds, nil
}

// PutInstallTokens satisfies [WatchkeeperSlackAppCredsDAO.PutInstallTokens].
// Returns [ErrCredsNotFound] when no row exists for the supplied
// `watchkeeperID` (the row must have been created by the M7.1.c.a
// CreateAppStep first). Idempotent on second call (overwrites — re-
// install scenario).
//
// The byte slices are stored verbatim — the DAO does not copy them
// because the in-memory implementation is process-local and the
// caller (the M7.1.c.b.b OAuthInstall step) does not retain references
// after the call. Production callers wiring a Postgres-backed adapter
// MUST treat the columns as `bytea`; this in-memory variant exists for
// dev / smoke runs and unit tests only.
func (m *MemoryWatchkeeperSlackAppCredsDAO) PutInstallTokens(
	_ context.Context,
	watchkeeperID uuid.UUID,
	botCT []byte,
	userCT []byte,
	refreshCT []byte,
	expiresAt time.Time,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.rows[watchkeeperID]; !ok {
		return ErrCredsNotFound
	}
	m.tokens[watchkeeperID] = memoryInstallTokens{
		botCT:       botCT,
		userCT:      userCT,
		refreshCT:   refreshCT,
		expiresAt:   expiresAt,
		installedAt: time.Now().UTC(),
	}
	return nil
}

// GetInstallTokens is a test-facing accessor that returns the install
// token bundle persisted by [MemoryWatchkeeperSlackAppCredsDAO.PutInstallTokens].
// Returns the zero value + `false` when no install row exists for the
// supplied `watchkeeperID`. Not part of the [WatchkeeperSlackAppCredsDAO]
// contract — production reads of the install tokens travel through the
// secrets-decrypter path; this accessor exists so M7.1.c.b.b unit tests
// can assert the encrypted bytes were stored verbatim.
func (m *MemoryWatchkeeperSlackAppCredsDAO) GetInstallTokens(
	watchkeeperID uuid.UUID,
) (botCT, userCT, refreshCT []byte, expiresAt, installedAt time.Time, ok bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, found := m.tokens[watchkeeperID]
	if !found {
		return nil, nil, nil, time.Time{}, time.Time{}, false
	}
	return t.botCT, t.userCT, t.refreshCT, t.expiresAt, t.installedAt, true
}

// WipeInstallTokens satisfies [WatchkeeperSlackAppCredsDAO.WipeInstallTokens].
// Idempotent: a missing tokens row returns nil (treated as
// already-wiped). The companion `slack_app_creds` row (the M7.1.c.a
// `client_id` / `client_secret` / etc.) is left in place so the
// future [SlackAppTeardown] production wrapper can still read the
// abandoned `app_id` before its own platform-side wipe.
func (m *MemoryWatchkeeperSlackAppCredsDAO) WipeInstallTokens(
	_ context.Context,
	watchkeeperID uuid.UUID,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.tokens, watchkeeperID)
	return nil
}
