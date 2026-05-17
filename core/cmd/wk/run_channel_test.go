package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// fakeChannelResolver is the hand-rolled test fake injected via the
// [channelResolverFactory] hook. Hand-rolled (no mocking lib) per the
// project test discipline. Captures every call so tests can assert
// resolver / Slack-side interactions independently.
type fakeChannelResolver struct {
	resolveID            uuid.UUID
	resolveReturnsChanID string
	// resolveReturnsStatus defaults to "open" (the lifecycle's
	// initial state) so tests that don't care about status can leave
	// it zero-valued via the "open"-default branch.
	resolveReturnsStatus string
	resolveErr           error
	resolveCalls         int

	revealChannel string
	revealUser    string
	revealErr     error
	revealCalls   int
}

func (f *fakeChannelResolver) ResolveSlackChannel(_ context.Context, id uuid.UUID) (string, string, error) {
	f.resolveCalls++
	f.resolveID = id
	status := f.resolveReturnsStatus
	if status == "" {
		status = "open"
	}
	return f.resolveReturnsChanID, status, f.resolveErr
}

func (f *fakeChannelResolver) RevealChannel(_ context.Context, channelID, userID string) error {
	f.revealCalls++
	f.revealChannel = channelID
	f.revealUser = userID
	return f.revealErr
}

// withFakeResolver swaps the package-scoped factory for the supplied
// fake AND a t.Cleanup that restores the prior factory. Mirrors the
// `t.Setenv` discipline — every test that swaps a package global
// MUST register the restore so test ordering does not matter.
func withFakeResolver(t *testing.T, fake *fakeChannelResolver, factoryErr error) {
	t.Helper()
	prev := channelResolverFactory
	channelResolverFactory = func(_ context.Context, _ envLookup) (channelResolver, func(), error) {
		if factoryErr != nil {
			return nil, nil, factoryErr
		}
		return fake, nil, nil
	}
	t.Cleanup(func() { channelResolverFactory = prev })
}

func TestChannel_NoSubcommand_UsageError(t *testing.T) {
	withFakeResolver(t, &fakeChannelResolver{}, nil)
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"channel"}, &stdout, &stderr, &fakeEnv{})
	if code != 2 {
		t.Errorf("exit: got %d want 2", code)
	}
	if !strings.Contains(stderr.String(), "missing subcommand") {
		t.Errorf("stderr missing diagnostic: %q", stderr.String())
	}
}

func TestChannel_UnknownSubcommand_UsageError(t *testing.T) {
	withFakeResolver(t, &fakeChannelResolver{}, nil)
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"channel", "frob"}, &stdout, &stderr, &fakeEnv{})
	if code != 2 {
		t.Errorf("exit: got %d want 2", code)
	}
	if !strings.Contains(stderr.String(), "unknown subcommand") {
		t.Errorf("stderr missing diagnostic: %q", stderr.String())
	}
}

func TestChannelReveal_MissingPositional_UsageError(t *testing.T) {
	withFakeResolver(t, &fakeChannelResolver{}, nil)
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"channel", "reveal", "--user", "U-1",
	}, &stdout, &stderr, &fakeEnv{})
	if code != 2 {
		t.Errorf("exit: got %d want 2", code)
	}
	if !strings.Contains(stderr.String(), "missing positional") {
		t.Errorf("stderr: %q", stderr.String())
	}
}

func TestChannelReveal_ExtraPositional_UsageError(t *testing.T) {
	withFakeResolver(t, &fakeChannelResolver{}, nil)
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"channel", "reveal", "--user", "U-1",
		uuid.New().String(), "extra",
	}, &stdout, &stderr, &fakeEnv{})
	if code != 2 {
		t.Errorf("exit: got %d want 2", code)
	}
	if !strings.Contains(stderr.String(), "extra positional") {
		t.Errorf("stderr: %q", stderr.String())
	}
}

func TestChannelReveal_MissingUserFlag_UsageError(t *testing.T) {
	withFakeResolver(t, &fakeChannelResolver{}, nil)
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"channel", "reveal", uuid.New().String(),
	}, &stdout, &stderr, &fakeEnv{})
	if code != 2 {
		t.Errorf("exit: got %d want 2", code)
	}
	if !strings.Contains(stderr.String(), "--user") {
		t.Errorf("stderr missing --user diagnostic: %q", stderr.String())
	}
}

func TestChannelReveal_NonUUIDPositional_UsageError(t *testing.T) {
	withFakeResolver(t, &fakeChannelResolver{}, nil)
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"channel", "reveal", "--user", "U-1", "not-a-uuid",
	}, &stdout, &stderr, &fakeEnv{})
	if code != 2 {
		t.Errorf("exit: got %d want 2", code)
	}
	if !strings.Contains(stderr.String(), "must be a UUID") {
		t.Errorf("stderr: %q", stderr.String())
	}
}

func TestChannelReveal_FactoryError_UsageExit(t *testing.T) {
	factoryErr := errMissingChannelConfig
	withFakeResolver(t, nil, factoryErr)
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"channel", "reveal", "--user", "U-1", uuid.New().String(),
	}, &stdout, &stderr, &fakeEnv{})
	if code != 2 {
		t.Errorf("exit: got %d want 2", code)
	}
	if !strings.Contains(stderr.String(), "env vars unset") {
		t.Errorf("stderr: %q", stderr.String())
	}
}

func TestChannelReveal_ResolveError_RuntimeExit(t *testing.T) {
	fake := &fakeChannelResolver{resolveErr: errors.New("postgres: dial failed")}
	withFakeResolver(t, fake, nil)

	convID := uuid.New()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"channel", "reveal", "--user", "U-1", convID.String(),
	}, &stdout, &stderr, &fakeEnv{})
	if code != 1 {
		t.Errorf("exit: got %d want 1", code)
	}
	if fake.resolveCalls != 1 {
		t.Errorf("resolve calls = %d, want 1", fake.resolveCalls)
	}
	if fake.resolveID != convID {
		t.Errorf("resolve id = %s, want %s", fake.resolveID, convID)
	}
	if fake.revealCalls != 0 {
		t.Errorf("reveal calls = %d, want 0 (resolve failure short-circuits)", fake.revealCalls)
	}
	if !strings.Contains(stderr.String(), "resolve conversation") {
		t.Errorf("stderr missing resolve diagnostic: %q", stderr.String())
	}
}

func TestChannelReveal_ArchivedRow_FailsFastBeforeSlack(t *testing.T) {
	fake := &fakeChannelResolver{resolveReturnsChanID: "C-X", resolveReturnsStatus: "archived"}
	withFakeResolver(t, fake, nil)

	convID := uuid.New()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"channel", "reveal", "--user", "U-1", convID.String(),
	}, &stdout, &stderr, &fakeEnv{})
	if code != 1 {
		t.Errorf("exit: got %d want 1", code)
	}
	if fake.revealCalls != 0 {
		t.Errorf("reveal calls = %d, want 0 (archived row must fail before Slack)", fake.revealCalls)
	}
	if !strings.Contains(stderr.String(), "already archived") {
		t.Errorf("stderr missing archived diagnostic: %q", stderr.String())
	}
}

func TestChannelReveal_EmptyChannelID_RuntimeExit(t *testing.T) {
	fake := &fakeChannelResolver{resolveReturnsChanID: ""}
	withFakeResolver(t, fake, nil)

	convID := uuid.New()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"channel", "reveal", "--user", "U-1", convID.String(),
	}, &stdout, &stderr, &fakeEnv{})
	if code != 1 {
		t.Errorf("exit: got %d want 1", code)
	}
	if fake.revealCalls != 0 {
		t.Errorf("reveal calls = %d, want 0 (no channel bound)", fake.revealCalls)
	}
	if !strings.Contains(stderr.String(), "no slack_channel_id") {
		t.Errorf("stderr missing orphan diagnostic: %q", stderr.String())
	}
}

func TestChannelReveal_RevealError_RuntimeExit(t *testing.T) {
	fake := &fakeChannelResolver{
		resolveReturnsChanID: "C-X",
		revealErr:            errors.New("slack: conversations.invite: 401"),
	}
	withFakeResolver(t, fake, nil)

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"channel", "reveal", "--user", "U-1", uuid.New().String(),
	}, &stdout, &stderr, &fakeEnv{})
	if code != 1 {
		t.Errorf("exit: got %d want 1", code)
	}
	if fake.revealCalls != 1 {
		t.Errorf("reveal calls = %d, want 1", fake.revealCalls)
	}
	if !strings.Contains(stderr.String(), "invite") {
		t.Errorf("stderr missing invite diagnostic: %q", stderr.String())
	}
}

func TestChannelReveal_HappyPath(t *testing.T) {
	fake := &fakeChannelResolver{resolveReturnsChanID: "C-ABC"}
	withFakeResolver(t, fake, nil)

	convID := uuid.New()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"channel", "reveal", "--user", "U-HUMAN", convID.String(),
	}, &stdout, &stderr, &fakeEnv{})
	if code != 0 {
		t.Errorf("exit: got %d want 0; stderr: %q", code, stderr.String())
	}
	if fake.resolveCalls != 1 || fake.resolveID != convID {
		t.Errorf("resolve calls = %d, id = %s, want 1 / %s", fake.resolveCalls, fake.resolveID, convID)
	}
	if fake.revealCalls != 1 {
		t.Errorf("reveal calls = %d, want 1", fake.revealCalls)
	}
	if fake.revealChannel != "C-ABC" {
		t.Errorf("reveal channel = %q, want %q", fake.revealChannel, "C-ABC")
	}
	if fake.revealUser != "U-HUMAN" {
		t.Errorf("reveal user = %q, want %q", fake.revealUser, "U-HUMAN")
	}
	out := stdout.String()
	if !strings.Contains(out, "ok") {
		t.Errorf("stdout missing ok: %q", out)
	}
	if !strings.Contains(out, convID.String()) {
		t.Errorf("stdout missing conversation id: %q", out)
	}
	if !strings.Contains(out, "C-ABC") {
		t.Errorf("stdout missing channel id: %q", out)
	}
}

func TestChannelReveal_HelpInUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"help"}, &stdout, &stderr, &fakeEnv{})
	if code != 0 {
		t.Errorf("exit: got %d want 0", code)
	}
	if !strings.Contains(stdout.String(), "wk channel reveal") {
		t.Errorf("help missing channel reveal: %q", stdout.String())
	}
}

func TestNewProductionChannelResolver_MissingEnv(t *testing.T) {
	// Hit the production factory directly to pin the env-miss
	// diagnostic; this is the only test that calls the production
	// constructor (with no env set, the function never opens a
	// Postgres pool, so the side-effect-free branch is safe).
	_, _, err := newProductionChannelResolver(context.Background(), &fakeEnv{})
	if !errors.Is(err, errMissingChannelConfig) {
		t.Errorf("err = %v, want errMissingChannelConfig", err)
	}
}

func TestNewProductionChannelResolver_BadOrgID(t *testing.T) {
	env := &fakeEnv{values: map[string]string{
		k2kPGDSNEnvKey:      "postgres://localhost/db",
		operatorOrgIDEnvKey: "not-a-uuid",
		slackBotTokenEnvKey: "xoxb-test",
	}}
	_, _, err := newProductionChannelResolver(context.Background(), env)
	if err == nil {
		t.Fatal("err = nil, want UUID parse error")
	}
	if !strings.Contains(err.Error(), "must be a UUID") {
		t.Errorf("err = %v, want substring 'must be a UUID'", err)
	}
}
