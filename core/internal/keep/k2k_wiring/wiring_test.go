package k2kwiring_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	k2kwiring "github.com/vadimtrunov/watchkeepers/core/internal/keep/k2k_wiring"
	"github.com/vadimtrunov/watchkeepers/core/pkg/k2k"
)

// fakeSlack is a no-op stand-in for [*slack.Client] in the wiring
// smoke test. The smoke test only pins the composition shape; it
// never drives a real Slack round-trip.
type fakeSlack struct{}

func (fakeSlack) CreateChannel(_ context.Context, _ string, _ bool) (string, error) {
	return "C-SMOKE", nil
}
func (fakeSlack) InviteToChannel(_ context.Context, _ string, _ []string) error { return nil }
func (fakeSlack) ArchiveChannel(_ context.Context, _ string) error              { return nil }

func TestComposeLifecycle_PinsCompositionShape(t *testing.T) {
	t.Parallel()

	repo := k2k.NewMemoryRepository(nil, nil)
	lc, err := k2kwiring.ComposeLifecycle(k2kwiring.LifecycleDeps{
		Repo:  repo,
		Slack: fakeSlack{},
	})
	if err != nil {
		t.Fatalf("ComposeLifecycle: %v", err)
	}
	if lc == nil {
		t.Fatal("ComposeLifecycle returned nil lifecycle")
	}

	// Drive Open() once to pin the composition end-to-end. The K2K
	// in-memory repo + fakeSlack should produce a happy-path Open.
	conv, err := lc.Open(context.Background(), k2k.OpenParams{
		OrganizationID: uuid.New(),
		Participants:   []string{"bot-a"},
		Subject:        "smoke",
		TokenBudget:    1,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if conv.SlackChannelID != "C-SMOKE" {
		t.Errorf("SlackChannelID = %q, want %q", conv.SlackChannelID, "C-SMOKE")
	}
}

func TestComposeLifecycle_NilRepo(t *testing.T) {
	t.Parallel()

	_, err := k2kwiring.ComposeLifecycle(k2kwiring.LifecycleDeps{Slack: fakeSlack{}})
	if err == nil {
		t.Fatal("err = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "Repo must not be nil") {
		t.Errorf("err = %v, want substring 'Repo must not be nil'", err)
	}
}

func TestComposeLifecycle_NilSlack(t *testing.T) {
	t.Parallel()

	repo := k2k.NewMemoryRepository(nil, nil)
	_, err := k2kwiring.ComposeLifecycle(k2kwiring.LifecycleDeps{Repo: repo})
	if err == nil {
		t.Fatal("err = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "Slack must not be nil") {
		t.Errorf("err = %v, want substring 'Slack must not be nil'", err)
	}
}

// TestComposeLifecycle_ReturnsTypedSentinel exists to keep the
// errors.Is contract obvious for future call sites. The current
// wiring returns plain wrapped errors (not sentinels), so this test
// asserts that future contributors who introduce sentinels keep them
// matchable. Today the assertion is the trivial nil-check on a
// happy-path return.
func TestComposeLifecycle_HappyPathError(t *testing.T) {
	t.Parallel()

	repo := k2k.NewMemoryRepository(nil, nil)
	_, err := k2kwiring.ComposeLifecycle(k2kwiring.LifecycleDeps{Repo: repo, Slack: fakeSlack{}})
	if err != nil {
		t.Errorf("happy-path err = %v, want nil", err)
	}

	// Defensive: a nil-deps error must NOT match a stray context
	// sentinel; pins that the wrapping does not leak.
	_, errNil := k2kwiring.ComposeLifecycle(k2kwiring.LifecycleDeps{})
	if errors.Is(errNil, context.Canceled) {
		t.Errorf("nil-deps err = %v, must not match context.Canceled", errNil)
	}
}
