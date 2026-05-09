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

	"github.com/google/uuid"

	slackmessenger "github.com/vadimtrunov/watchkeepers/core/pkg/messenger/slack"
)

// MemoryWatchkeeperSlackAppCredsDAO is a process-local
// [WatchkeeperSlackAppCredsDAO] implementation backed by a
// `map[uuid.UUID]slack.CreateAppCredentials`. Constructed via
// [NewMemoryWatchkeeperSlackAppCredsDAO]; the zero value is NOT
// usable — callers must always go through the constructor so the
// internal map is non-nil.
//
// Concurrency: read methods (`Get`) take an RLock so concurrent
// reads across distinct ids never block each other. Write methods
// (`Put`) take the write lock for the duration of the call.
type MemoryWatchkeeperSlackAppCredsDAO struct {
	mu   sync.RWMutex
	rows map[uuid.UUID]slackmessenger.CreateAppCredentials
}

// NewMemoryWatchkeeperSlackAppCredsDAO returns an empty in-memory
// store.
func NewMemoryWatchkeeperSlackAppCredsDAO() *MemoryWatchkeeperSlackAppCredsDAO {
	return &MemoryWatchkeeperSlackAppCredsDAO{
		rows: make(map[uuid.UUID]slackmessenger.CreateAppCredentials),
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
