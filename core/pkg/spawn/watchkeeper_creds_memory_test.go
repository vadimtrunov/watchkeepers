package spawn_test

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

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

// TestMemoryWatchkeeperSlackAppCredsDAO_PutInstallTokens_RoundTrip pins
// the M7.1.c.b.b extension: PutInstallTokens stores the supplied
// ciphertext bundle keyed by watchkeeperID and the test-facing
// GetInstallTokens accessor returns the bytes verbatim. The DAO does
// NOT decrypt — it treats the byte slices as opaque (the encryption
// layer lives one level up in the OAuthInstall step).
func TestMemoryWatchkeeperSlackAppCredsDAO_PutInstallTokens_RoundTrip(t *testing.T) {
	t.Parallel()

	dao := spawn.NewMemoryWatchkeeperSlackAppCredsDAO()
	wkID := uuid.New()

	// Pre-seed the row via Put so the install-tokens write has a row
	// to update (mirrors the saga ordering: CreateApp creates the row,
	// OAuthInstall extends it).
	if err := dao.Put(context.Background(), wkID, newTestCreds()); err != nil {
		t.Fatalf("Put: %v", err)
	}

	wantBot := []byte{0x01, 0x02, 0x03}
	wantUser := []byte{0x04, 0x05}
	wantRefresh := []byte{0x06}
	wantExpiry := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	if err := dao.PutInstallTokens(
		context.Background(), wkID, wantBot, wantUser, wantRefresh, wantExpiry,
	); err != nil {
		t.Fatalf("PutInstallTokens: %v", err)
	}

	gotBot, gotUser, gotRefresh, gotExpiry, gotInstalled, ok := dao.GetInstallTokens(wkID)
	if !ok {
		t.Fatalf("GetInstallTokens: ok = false, want true")
	}
	if !bytes.Equal(gotBot, wantBot) {
		t.Errorf("botCT = %v, want %v", gotBot, wantBot)
	}
	if !bytes.Equal(gotUser, wantUser) {
		t.Errorf("userCT = %v, want %v", gotUser, wantUser)
	}
	if !bytes.Equal(gotRefresh, wantRefresh) {
		t.Errorf("refreshCT = %v, want %v", gotRefresh, wantRefresh)
	}
	if !gotExpiry.Equal(wantExpiry) {
		t.Errorf("expiresAt = %v, want %v", gotExpiry, wantExpiry)
	}
	if gotInstalled.IsZero() {
		t.Error("installedAt is zero; want non-zero (DAO stamps on write)")
	}
}

// TestMemoryWatchkeeperSlackAppCredsDAO_PutInstallTokens_NoRow returns
// [ErrCredsNotFound] — the install step's contract requires a prior
// Put from the M7.1.c.a CreateAppStep before install tokens land.
func TestMemoryWatchkeeperSlackAppCredsDAO_PutInstallTokens_NoRow(t *testing.T) {
	t.Parallel()

	dao := spawn.NewMemoryWatchkeeperSlackAppCredsDAO()
	err := dao.PutInstallTokens(
		context.Background(),
		uuid.New(),
		[]byte("bot"), []byte("user"), nil, time.Time{},
	)
	if !errors.Is(err, spawn.ErrCredsNotFound) {
		t.Fatalf("PutInstallTokens with no row err = %v, want ErrCredsNotFound", err)
	}
}

// TestMemoryWatchkeeperSlackAppCredsDAO_PutInstallTokens_Idempotent
// pins the documented overwrite-on-second-call behaviour: a re-install
// scenario is expressed by re-calling PutInstallTokens with a fresh
// bundle; no `ErrAlreadyInstalled` sentinel exists (kept simple per
// the M7.1.c.b.b plan).
func TestMemoryWatchkeeperSlackAppCredsDAO_PutInstallTokens_Idempotent(t *testing.T) {
	t.Parallel()

	dao := spawn.NewMemoryWatchkeeperSlackAppCredsDAO()
	wkID := uuid.New()
	if err := dao.Put(context.Background(), wkID, newTestCreds()); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := dao.PutInstallTokens(
		context.Background(), wkID, []byte("bot1"), nil, nil, time.Time{},
	); err != nil {
		t.Fatalf("PutInstallTokens #1: %v", err)
	}
	if err := dao.PutInstallTokens(
		context.Background(), wkID, []byte("bot2"), nil, nil, time.Time{},
	); err != nil {
		t.Fatalf("PutInstallTokens #2 (overwrite): %v", err)
	}

	gotBot, _, _, _, _, ok := dao.GetInstallTokens(wkID)
	if !ok {
		t.Fatalf("GetInstallTokens: ok = false, want true")
	}
	if !bytes.Equal(gotBot, []byte("bot2")) {
		t.Errorf("botCT after overwrite = %q, want %q", gotBot, "bot2")
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
