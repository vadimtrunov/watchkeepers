package spawn_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	slackmessenger "github.com/vadimtrunov/watchkeepers/core/pkg/messenger/slack"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn"
)

// PII-safe placeholder credentials. Test logs are grep-asserted
// against these tokens NOT appearing in any error path; we keep the
// strings obviously synthetic ("test-…") rather than realistic so a
// failed redaction is unmistakable.
//
//nolint:gosec // G101: synthetic test placeholders, not real credentials.
func newTestCreds() slackmessenger.CreateAppCredentials {
	return slackmessenger.CreateAppCredentials{
		AppID:             "test-app-id",
		ClientID:          "test-client-id",
		ClientSecret:      "test-client-secret",
		VerificationToken: "test-verification-token",
		SigningSecret:     "test-signing-secret",
	}
}

func TestMemoryWatchkeeperSlackAppCredsDAO_PutGet_RoundTrip(t *testing.T) {
	t.Parallel()

	dao := spawn.NewMemoryWatchkeeperSlackAppCredsDAO()
	wkID := uuid.New()
	want := newTestCreds()

	if err := dao.Put(context.Background(), wkID, want); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := dao.Get(context.Background(), wkID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != want {
		t.Errorf("Get returned %+v, want %+v", got, want)
	}
}

func TestMemoryWatchkeeperSlackAppCredsDAO_Get_Missing_ReturnsErrCredsNotFound(t *testing.T) {
	t.Parallel()

	dao := spawn.NewMemoryWatchkeeperSlackAppCredsDAO()

	_, err := dao.Get(context.Background(), uuid.New())
	if !errors.Is(err, spawn.ErrCredsNotFound) {
		t.Fatalf("Get on missing id err = %v, want ErrCredsNotFound", err)
	}
}

func TestMemoryWatchkeeperSlackAppCredsDAO_Put_DuplicateID_ReturnsErrCredsAlreadyStored(t *testing.T) {
	t.Parallel()

	dao := spawn.NewMemoryWatchkeeperSlackAppCredsDAO()
	wkID := uuid.New()
	creds := newTestCreds()

	if err := dao.Put(context.Background(), wkID, creds); err != nil {
		t.Fatalf("Put #1: %v", err)
	}
	err := dao.Put(context.Background(), wkID, creds)
	if !errors.Is(err, spawn.ErrCredsAlreadyStored) {
		t.Fatalf("Put #2 err = %v, want ErrCredsAlreadyStored", err)
	}
}

// TestMemoryWatchkeeperSlackAppCredsDAO_Concurrency_DistinctIDs runs
// 16 concurrent Puts for distinct watchkeeper ids and asserts every
// row is retrievable. Combined with `go test -race`, this pins the
// DAO mutex contract (AC5 / test-plan §"Concurrency").
func TestMemoryWatchkeeperSlackAppCredsDAO_Concurrency_DistinctIDs(t *testing.T) {
	t.Parallel()

	dao := spawn.NewMemoryWatchkeeperSlackAppCredsDAO()

	const n = 16
	ids := make([]uuid.UUID, n)
	for i := range ids {
		ids[i] = uuid.New()
	}

	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id uuid.UUID) {
			defer wg.Done()
			if err := dao.Put(context.Background(), id, newTestCreds()); err != nil {
				t.Errorf("Put(%v): %v", id, err)
			}
		}(id)
	}
	wg.Wait()

	for _, id := range ids {
		if _, err := dao.Get(context.Background(), id); err != nil {
			t.Errorf("Get(%v): %v", id, err)
		}
	}
}
