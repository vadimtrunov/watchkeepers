package spawn_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger"
	slackmessenger "github.com/vadimtrunov/watchkeepers/core/pkg/messenger/slack"
	"github.com/vadimtrunov/watchkeepers/core/pkg/secrets"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn/saga"
)

// ────────────────────────────────────────────────────────────────────────
// Hand-rolled fakes — no mocking lib (M3.6 / M6.3.e / M7.1.c.a pattern).
// ────────────────────────────────────────────────────────────────────────

// fakeInstaller is a hand-rolled [spawn.SlackAppInstaller] used by the
// OAuthInstall step tests. It records every InstallApp call (with the
// supplied AppID + workspace) onto a SHARED record set, optionally
// returns a configured error to drive negative paths, and (on the
// success path) drives the supplied resolver + sink the same way the
// real `slack.Client.InstallApp` would: resolver-first, then sink with
// the canned [slackmessenger.InstallTokens] payload.
//
// Concurrency: all mutable state lives behind a mutex / atomics so 16
// concurrent Execute() calls can drive the same fake without data
// races (`go test -race` clean).
type fakeInstaller struct {
	mu             sync.Mutex
	calls          []recordedInstallCall
	resolverCalls  atomic.Int32
	sinkCalls      atomic.Int32
	resolverParams slackmessenger.InstallParams // captured per-call
	tokens         slackmessenger.InstallTokens // canned response
	returnErr      error
}

type recordedInstallCall struct {
	appID     messenger.AppID
	workspace messenger.WorkspaceRef
}

func newFakeInstaller(tokens slackmessenger.InstallTokens) *fakeInstaller {
	return &fakeInstaller{tokens: tokens}
}

func (f *fakeInstaller) InstallApp(
	ctx context.Context,
	appID messenger.AppID,
	workspace messenger.WorkspaceRef,
	resolver slackmessenger.InstallParamsResolver,
	sink slackmessenger.InstallTokenSink,
) (messenger.Installation, error) {
	f.mu.Lock()
	f.calls = append(f.calls, recordedInstallCall{appID: appID, workspace: workspace})
	f.mu.Unlock()

	// The real adapter (M4.2.d.2) calls the resolver BEFORE issuing
	// the network request and then invokes the sink AFTER a successful
	// response. The fake mirrors that ordering precisely so wrap chains
	// stay realistic.
	if resolver != nil {
		f.resolverCalls.Add(1)
		params, err := resolver(ctx, appID, workspace)
		if err != nil {
			return messenger.Installation{}, err
		}
		f.mu.Lock()
		f.resolverParams = params
		f.mu.Unlock()
	}

	if f.returnErr != nil {
		return messenger.Installation{}, f.returnErr
	}

	if sink != nil {
		f.sinkCalls.Add(1)
		if err := sink(ctx, f.tokens); err != nil {
			return messenger.Installation{}, err
		}
	}
	return messenger.Installation{AppID: appID, Workspace: workspace}, nil
}

func (f *fakeInstaller) recordedCalls() []recordedInstallCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedInstallCall, len(f.calls))
	copy(out, f.calls)
	return out
}

func (f *fakeInstaller) capturedParams() slackmessenger.InstallParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resolverParams
}

// errEncrypter is a stub [secrets.Encrypter] used for the negative path
// covering an Encrypt-side failure. Decrypt is never called on this
// path — the test fails the install before any decrypt would happen.
type errEncrypter struct {
	err error
}

func (e errEncrypter) Encrypt(_ context.Context, _ []byte) ([]byte, error) {
	return nil, e.err
}

func (e errEncrypter) Decrypt(_ context.Context, _ []byte) ([]byte, error) {
	return nil, e.err
}

// kekStubSecretSource is a minimal [secrets.SecretSource] used to seed the
// real [secrets.AESGCMEncrypter] for the round-trip + ciphertext-not-
// plaintext assertions.
type kekStubSecretSource struct {
	values map[string]string
}

func (s kekStubSecretSource) Get(_ context.Context, key string) (string, error) {
	v, ok := s.values[key]
	if !ok {
		return "", secrets.ErrSecretNotFound
	}
	return v, nil
}

// ────────────────────────────────────────────────────────────────────────
// Helpers
// ────────────────────────────────────────────────────────────────────────

func newOAuthSpawnContext(t *testing.T, watchkeeperID uuid.UUID, code string) saga.SpawnContext {
	t.Helper()
	return saga.SpawnContext{
		ManifestVersionID: uuid.New(),
		AgentID:           watchkeeperID,
		OAuthCode:         code,
		Claim: saga.SpawnClaim{
			OrganizationID:  "org-test",
			AgentID:         "agent-watchmaster",
			AuthorityMatrix: map[string]string{"slack_app_install": "lead_approval"},
		},
	}
}

// newTestEncrypter builds a real AES-256-GCM Encrypter from a random
// 32-byte KEK. Mirrors the helper in the secrets package's own test
// suite so OAuthInstallStep tests round-trip through the production
// crypto path (NOT a stub) for AC6.
func newTestEncrypter(t *testing.T) secrets.Encrypter {
	t.Helper()
	var kek [32]byte
	if _, err := rand.Read(kek[:]); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	src := kekStubSecretSource{values: map[string]string{"KEK": hex.EncodeToString(kek[:])}}
	enc, err := secrets.NewAESGCMEncrypter(context.Background(), src, "KEK")
	if err != nil {
		t.Fatalf("NewAESGCMEncrypter: %v", err)
	}
	return enc
}

// canonicalInstallTokens returns the canned Slack response the fake
// installer surfaces to the sink. Reused across multiple cases.
//
//nolint:gosec // G101: synthetic test placeholders ("xoxb-test-…"), not real tokens.
func canonicalInstallTokens() slackmessenger.InstallTokens {
	return slackmessenger.InstallTokens{
		AppID:           messenger.AppID("test-app-id"),
		TeamID:          "T0123",
		AccessToken:     "xoxb-test-bot-token",
		RefreshToken:    "xoxe-test-refresh-token",
		ExpiresIn:       43200,
		UserAccessToken: "xoxp-test-user-token",
		UserScope:       "chat:write",
	}
}

// seedCredsRow pre-populates the M7.1.c.a creds DAO with the test
// app credentials so the OAuthInstall step's lookup succeeds.
func seedCredsRow(t *testing.T, dao *spawn.MemoryWatchkeeperSlackAppCredsDAO, watchkeeperID uuid.UUID) slackmessenger.CreateAppCredentials {
	t.Helper()
	creds := newTestCreds()
	if err := dao.Put(context.Background(), watchkeeperID, creds); err != nil {
		t.Fatalf("seed Put: %v", err)
	}
	return creds
}

func newOAuthStep(
	t *testing.T,
	installer spawn.SlackAppInstaller,
	dao spawn.WatchkeeperSlackAppCredsDAO,
	enc secrets.Encrypter,
) *spawn.OAuthInstallStep {
	t.Helper()
	return spawn.NewOAuthInstallStep(spawn.OAuthInstallStepDeps{
		Installer:   installer,
		CredsDAO:    dao,
		Encrypter:   enc,
		Workspace:   messenger.WorkspaceRef{ID: "T0123", Name: "Test"},
		RedirectURI: "https://example.com/oauth/callback",
	})
}

// ────────────────────────────────────────────────────────────────────────
// Compile-time + Name() coverage
// ────────────────────────────────────────────────────────────────────────

func TestOAuthInstallStep_Name_ReturnsStableIdentifier(t *testing.T) {
	t.Parallel()

	step := newOAuthStep(
		t,
		newFakeInstaller(canonicalInstallTokens()),
		spawn.NewMemoryWatchkeeperSlackAppCredsDAO(),
		newTestEncrypter(t),
	)
	if got := step.Name(); got != "oauth_install" {
		t.Errorf("Name() = %q, want %q", got, "oauth_install")
	}
	if got := step.Name(); got != spawn.OAuthInstallStepName {
		t.Errorf("Name() = %q, want %q (OAuthInstallStepName)", got, spawn.OAuthInstallStepName)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Constructor panics
// ────────────────────────────────────────────────────────────────────────

func TestNewOAuthInstallStep_PanicsOnRequiredFields(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		mut  func(*spawn.OAuthInstallStepDeps)
	}{
		{"nil Installer", func(d *spawn.OAuthInstallStepDeps) { d.Installer = nil }},
		{"nil CredsDAO", func(d *spawn.OAuthInstallStepDeps) { d.CredsDAO = nil }},
		{"nil Encrypter", func(d *spawn.OAuthInstallStepDeps) { d.Encrypter = nil }},
		{"empty Workspace.ID", func(d *spawn.OAuthInstallStepDeps) { d.Workspace = messenger.WorkspaceRef{} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("NewOAuthInstallStep with %s did not panic", tc.name)
				}
			}()
			deps := spawn.OAuthInstallStepDeps{
				Installer: newFakeInstaller(canonicalInstallTokens()),
				CredsDAO:  spawn.NewMemoryWatchkeeperSlackAppCredsDAO(),
				Encrypter: newTestEncrypter(t),
				Workspace: messenger.WorkspaceRef{ID: "T0123"},
			}
			tc.mut(&deps)
			_ = spawn.NewOAuthInstallStep(deps)
		})
	}
}

// ────────────────────────────────────────────────────────────────────────
// Happy: PutInstallTokens called with ENCRYPTED tokens; step returns nil
// (test-plan §"Happy" first bullet)
// ────────────────────────────────────────────────────────────────────────

func TestOAuthInstallStep_Execute_HappyPath_StoresEncryptedTokens(t *testing.T) {
	t.Parallel()

	tokens := canonicalInstallTokens()
	installer := newFakeInstaller(tokens)
	dao := spawn.NewMemoryWatchkeeperSlackAppCredsDAO()
	enc := newTestEncrypter(t)
	step := newOAuthStep(t, installer, dao, enc)

	watchkeeperID := uuid.New()
	creds := seedCredsRow(t, dao, watchkeeperID)
	ctx := saga.WithSpawnContext(context.Background(), newOAuthSpawnContext(t, watchkeeperID, "code-test"))

	if err := step.Execute(ctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	assertInstallerCallShape(t, installer, creds.AppID)
	assertResolverParams(t, installer, creds, "code-test")
	assertCiphertextsStored(t, dao, watchkeeperID, tokens)
}

// assertInstallerCallShape pins that the installer was called exactly
// once with the appID from the M7.1.c.a creds row and the step-
// configured workspace. Helper-extracted from
// TestOAuthInstallStep_Execute_HappyPath_StoresEncryptedTokens to keep
// gocyclo under the 15-branch lint threshold.
func assertInstallerCallShape(t *testing.T, installer *fakeInstaller, wantAppID messenger.AppID) {
	t.Helper()
	calls := installer.recordedCalls()
	if len(calls) != 1 {
		t.Fatalf("Installer.InstallApp call count = %d, want 1", len(calls))
	}
	if calls[0].appID != wantAppID {
		t.Errorf("call.appID = %q, want %q (from seeded creds)", calls[0].appID, wantAppID)
	}
	if calls[0].workspace.ID != "T0123" {
		t.Errorf("call.workspace.ID = %q, want %q", calls[0].workspace.ID, "T0123")
	}
}

// assertResolverParams pins that the per-call resolver surfaced the
// SpawnContext-derived OAuthCode + the DAO-stored client credentials
// + the step-configured redirect URI to the underlying messenger
// adapter.
func assertResolverParams(t *testing.T, installer *fakeInstaller, creds slackmessenger.CreateAppCredentials, wantCode string) {
	t.Helper()
	params := installer.capturedParams()
	if params.Code != wantCode {
		t.Errorf("resolver.Code = %q, want %q", params.Code, wantCode)
	}
	if params.ClientID != creds.ClientID {
		t.Errorf("resolver.ClientID = %q, want %q", params.ClientID, creds.ClientID)
	}
	if params.ClientSecret != creds.ClientSecret {
		t.Errorf("resolver.ClientSecret = %q, want %q", params.ClientSecret, creds.ClientSecret)
	}
	if params.RedirectURI != "https://example.com/oauth/callback" {
		t.Errorf("resolver.RedirectURI = %q, want configured redirect URI", params.RedirectURI)
	}
}

// assertCiphertextsStored pins AC6: the bot / user / refresh tokens
// stored in the DAO are non-empty ciphertexts (NOT plaintext) and the
// expiry / installed_at timestamps are populated by the step's
// expiry-derivation + the DAO's write-stamp respectively.
func assertCiphertextsStored(
	t *testing.T,
	dao *spawn.MemoryWatchkeeperSlackAppCredsDAO,
	watchkeeperID uuid.UUID,
	tokens slackmessenger.InstallTokens,
) {
	t.Helper()
	botCT, userCT, refreshCT, expiresAt, installedAt, ok := dao.GetInstallTokens(watchkeeperID)
	if !ok {
		t.Fatalf("GetInstallTokens: ok = false, want true (PutInstallTokens never ran)")
	}
	if len(botCT) == 0 {
		t.Errorf("botCT is empty; want non-empty ciphertext")
	}
	if len(userCT) == 0 {
		t.Errorf("userCT is empty; want non-empty ciphertext")
	}
	if len(refreshCT) == 0 {
		t.Errorf("refreshCT is empty; want non-empty ciphertext for non-empty plaintext")
	}
	if expiresAt.IsZero() {
		t.Errorf("expiresAt is zero; want non-zero (canonical tokens carry ExpiresIn)")
	}
	if installedAt.IsZero() {
		t.Errorf("installedAt is zero; DAO must stamp on write")
	}
	// AC6 second pin: ciphertexts NEVER equal plaintext.
	if bytes.Equal(botCT, []byte(tokens.AccessToken)) {
		t.Errorf("botCT == plaintext bot token; encryption short-circuited")
	}
	if bytes.Equal(userCT, []byte(tokens.UserAccessToken)) {
		t.Errorf("userCT == plaintext user token; encryption short-circuited")
	}
	if bytes.Equal(refreshCT, []byte(tokens.RefreshToken)) {
		t.Errorf("refreshCT == plaintext refresh token; encryption short-circuited")
	}
}

// ────────────────────────────────────────────────────────────────────────
// Happy: round-trip Encrypt(token) stored, Decrypt(stored) == plaintext
// (test-plan §"Happy" second bullet)
// ────────────────────────────────────────────────────────────────────────

func TestOAuthInstallStep_Execute_RoundTrip_DecryptedMatchesPlaintext(t *testing.T) {
	t.Parallel()

	tokens := canonicalInstallTokens()
	installer := newFakeInstaller(tokens)
	dao := spawn.NewMemoryWatchkeeperSlackAppCredsDAO()
	enc := newTestEncrypter(t)
	step := newOAuthStep(t, installer, dao, enc)

	watchkeeperID := uuid.New()
	seedCredsRow(t, dao, watchkeeperID)
	ctx := saga.WithSpawnContext(context.Background(), newOAuthSpawnContext(t, watchkeeperID, "code-test"))

	if err := step.Execute(ctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	botCT, userCT, refreshCT, _, _, ok := dao.GetInstallTokens(watchkeeperID)
	if !ok {
		t.Fatalf("GetInstallTokens: ok = false")
	}
	gotBot, err := enc.Decrypt(context.Background(), botCT)
	if err != nil {
		t.Fatalf("Decrypt(botCT): %v", err)
	}
	if string(gotBot) != tokens.AccessToken {
		t.Errorf("Decrypt(botCT) = %q, want %q", gotBot, tokens.AccessToken)
	}
	gotUser, err := enc.Decrypt(context.Background(), userCT)
	if err != nil {
		t.Fatalf("Decrypt(userCT): %v", err)
	}
	if string(gotUser) != tokens.UserAccessToken {
		t.Errorf("Decrypt(userCT) = %q, want %q", gotUser, tokens.UserAccessToken)
	}
	gotRefresh, err := enc.Decrypt(context.Background(), refreshCT)
	if err != nil {
		t.Fatalf("Decrypt(refreshCT): %v", err)
	}
	if string(gotRefresh) != tokens.RefreshToken {
		t.Errorf("Decrypt(refreshCT) = %q, want %q", gotRefresh, tokens.RefreshToken)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Edge: M7.1.c.a creds row missing → wrapped ErrCredsNotFound, no install
// (test-plan §"Edge — creds missing")
// ────────────────────────────────────────────────────────────────────────

func TestOAuthInstallStep_Execute_NoCredsRow_ReturnsErrCredsNotFoundNoInstall(t *testing.T) {
	t.Parallel()

	installer := newFakeInstaller(canonicalInstallTokens())
	dao := spawn.NewMemoryWatchkeeperSlackAppCredsDAO()
	enc := newTestEncrypter(t)
	step := newOAuthStep(t, installer, dao, enc)

	// Do NOT seed the DAO — the lookup MUST surface ErrCredsNotFound.
	ctx := saga.WithSpawnContext(context.Background(), newOAuthSpawnContext(t, uuid.New(), "code-test"))

	err := step.Execute(ctx)
	if err == nil {
		t.Fatalf("Execute: err = nil, want wrapped ErrCredsNotFound")
	}
	if !errors.Is(err, spawn.ErrCredsNotFound) {
		t.Errorf("errors.Is(err, ErrCredsNotFound) = false; got %v", err)
	}
	if got := installer.recordedCalls(); len(got) != 0 {
		t.Errorf("Installer.InstallApp call count = %d, want 0 (fail-fast on missing creds)", len(got))
	}
}

// ────────────────────────────────────────────────────────────────────────
// Edge: empty OAuthCode → wrapped ErrMissingOAuthCode, no install
// (test-plan §"Edge — empty OAuthCode")
// ────────────────────────────────────────────────────────────────────────

func TestOAuthInstallStep_Execute_EmptyOAuthCode_ReturnsErrMissingOAuthCodeNoInstall(t *testing.T) {
	t.Parallel()

	installer := newFakeInstaller(canonicalInstallTokens())
	dao := spawn.NewMemoryWatchkeeperSlackAppCredsDAO()
	enc := newTestEncrypter(t)
	step := newOAuthStep(t, installer, dao, enc)

	watchkeeperID := uuid.New()
	seedCredsRow(t, dao, watchkeeperID)
	ctx := saga.WithSpawnContext(context.Background(), newOAuthSpawnContext(t, watchkeeperID, "")) // empty code

	err := step.Execute(ctx)
	if err == nil {
		t.Fatalf("Execute: err = nil, want wrapped ErrMissingOAuthCode")
	}
	if !errors.Is(err, spawn.ErrMissingOAuthCode) {
		t.Errorf("errors.Is(err, ErrMissingOAuthCode) = false; got %v", err)
	}
	if !strings.HasPrefix(err.Error(), "spawn: oauth_install step:") {
		t.Errorf("err prefix = %q; want %q-prefixed wrap", err.Error(), "spawn: oauth_install step:")
	}
	if got := installer.recordedCalls(); len(got) != 0 {
		t.Errorf("Installer.InstallApp call count = %d, want 0 (resolver short-circuits before installer)", len(got))
	}
}

// ────────────────────────────────────────────────────────────────────────
// Edge: empty RefreshToken → stored refreshCT is nil/zero-length
// (test-plan §"Edge — empty refresh token")
// ────────────────────────────────────────────────────────────────────────

func TestOAuthInstallStep_Execute_EmptyRefreshToken_StoresNilRefreshCT(t *testing.T) {
	t.Parallel()

	tokens := canonicalInstallTokens()
	tokens.RefreshToken = "" // rotation disabled
	installer := newFakeInstaller(tokens)
	dao := spawn.NewMemoryWatchkeeperSlackAppCredsDAO()
	enc := newTestEncrypter(t)
	step := newOAuthStep(t, installer, dao, enc)

	watchkeeperID := uuid.New()
	seedCredsRow(t, dao, watchkeeperID)
	ctx := saga.WithSpawnContext(context.Background(), newOAuthSpawnContext(t, watchkeeperID, "code-test"))

	if err := step.Execute(ctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	_, _, refreshCT, _, _, ok := dao.GetInstallTokens(watchkeeperID)
	if !ok {
		t.Fatalf("GetInstallTokens: ok = false")
	}
	if len(refreshCT) != 0 {
		t.Errorf("refreshCT len = %d, want 0 (empty plaintext must NOT produce a 28-byte ciphertext)", len(refreshCT))
	}
}

// ────────────────────────────────────────────────────────────────────────
// Negative: Installer returns error → step wraps; PutInstallTokens NOT called
// (test-plan §"Negative — Installer error")
// ────────────────────────────────────────────────────────────────────────

func TestOAuthInstallStep_Execute_InstallerError_NoPutInstallTokens(t *testing.T) {
	t.Parallel()

	installer := newFakeInstaller(canonicalInstallTokens())
	installer.returnErr = errors.New("simulated platform error")
	dao := spawn.NewMemoryWatchkeeperSlackAppCredsDAO()
	enc := newTestEncrypter(t)
	step := newOAuthStep(t, installer, dao, enc)

	watchkeeperID := uuid.New()
	seedCredsRow(t, dao, watchkeeperID)
	ctx := saga.WithSpawnContext(context.Background(), newOAuthSpawnContext(t, watchkeeperID, "code-test"))

	err := step.Execute(ctx)
	if err == nil {
		t.Fatalf("Execute: err = nil, want wrapped installer error")
	}
	if !strings.HasPrefix(err.Error(), "spawn: oauth_install step:") {
		t.Errorf("err prefix = %q; want %q-prefixed wrap", err.Error(), "spawn: oauth_install step:")
	}

	// No install token row was written.
	if _, _, _, _, _, ok := dao.GetInstallTokens(watchkeeperID); ok {
		t.Errorf("GetInstallTokens: ok = true, want false (sink must not have fired)")
	}
}

// ────────────────────────────────────────────────────────────────────────
// Negative: Encrypter.Encrypt fails → step wraps; PutInstallTokens NOT called
// (test-plan §"Negative — Encrypter error")
// ────────────────────────────────────────────────────────────────────────

func TestOAuthInstallStep_Execute_EncrypterError_NoPutInstallTokens(t *testing.T) {
	t.Parallel()

	encErr := errors.New("simulated encrypter failure")
	installer := newFakeInstaller(canonicalInstallTokens())
	dao := spawn.NewMemoryWatchkeeperSlackAppCredsDAO()
	enc := errEncrypter{err: encErr}
	step := newOAuthStep(t, installer, dao, enc)

	watchkeeperID := uuid.New()
	seedCredsRow(t, dao, watchkeeperID)
	ctx := saga.WithSpawnContext(context.Background(), newOAuthSpawnContext(t, watchkeeperID, "code-test"))

	err := step.Execute(ctx)
	if err == nil {
		t.Fatalf("Execute: err = nil, want wrapped encrypter error")
	}
	if !errors.Is(err, encErr) {
		t.Errorf("errors.Is(err, encErr) = false; got %v", err)
	}
	if _, _, _, _, _, ok := dao.GetInstallTokens(watchkeeperID); ok {
		t.Errorf("GetInstallTokens: ok = true, want false (DAO write must not have happened)")
	}
}

// ────────────────────────────────────────────────────────────────────────
// Negative: PutInstallTokens fails → step wraps + returns
// (test-plan §"Negative — DAO error")
// ────────────────────────────────────────────────────────────────────────

// putInstallTokensErrDAO is a thin DAO wrapper that delegates Get +
// Put to the embedded memory DAO but injects an error on
// PutInstallTokens. Used to drive the negative-path test for the
// step's wrap chain.
type putInstallTokensErrDAO struct {
	*spawn.MemoryWatchkeeperSlackAppCredsDAO
	err error
}

func (d *putInstallTokensErrDAO) PutInstallTokens(
	_ context.Context,
	_ uuid.UUID,
	_, _, _ []byte,
	_ time.Time,
) error {
	return d.err
}

func TestOAuthInstallStep_Execute_DAOPutInstallTokensError_WrapsAndReturns(t *testing.T) {
	t.Parallel()

	memDAO := spawn.NewMemoryWatchkeeperSlackAppCredsDAO()
	wrapped := &putInstallTokensErrDAO{
		MemoryWatchkeeperSlackAppCredsDAO: memDAO,
		err:                               errors.New("simulated DAO write failure"),
	}

	installer := newFakeInstaller(canonicalInstallTokens())
	enc := newTestEncrypter(t)
	step := newOAuthStep(t, installer, wrapped, enc)

	watchkeeperID := uuid.New()
	seedCredsRow(t, memDAO, watchkeeperID)
	ctx := saga.WithSpawnContext(context.Background(), newOAuthSpawnContext(t, watchkeeperID, "code-test"))

	err := step.Execute(ctx)
	if err == nil {
		t.Fatalf("Execute: err = nil, want wrapped DAO error")
	}
	if !errors.Is(err, wrapped.err) {
		t.Errorf("errors.Is(err, wrapped.err) = false; got %v", err)
	}
	// Document for the reader: the underlying OAuth exchange (Slack
	// `oauth.v2.access`) is server-side complete. M7.1.c.b.b is
	// forward-only per ROADMAP — reconciliation belongs to the future
	// retire saga (M7.2). The test pins the wrap chain only.
}

// ────────────────────────────────────────────────────────────────────────
// Negative: Missing SpawnContext → wrapped error; no install; no DAO call
// (test-plan §"Negative — Missing SpawnContext")
// ────────────────────────────────────────────────────────────────────────

func TestOAuthInstallStep_Execute_MissingSpawnContext_NoInstallNoDAO(t *testing.T) {
	t.Parallel()

	installer := newFakeInstaller(canonicalInstallTokens())
	dao := spawn.NewMemoryWatchkeeperSlackAppCredsDAO()
	enc := newTestEncrypter(t)
	step := newOAuthStep(t, installer, dao, enc)

	err := step.Execute(context.Background())
	if err == nil {
		t.Fatalf("Execute: err = nil, want wrapped ErrMissingSpawnContext")
	}
	if !errors.Is(err, spawn.ErrMissingSpawnContext) {
		t.Errorf("errors.Is(err, ErrMissingSpawnContext) = false; got %v", err)
	}
	if got := installer.recordedCalls(); len(got) != 0 {
		t.Errorf("Installer.InstallApp call count = %d, want 0", len(got))
	}
}

// ────────────────────────────────────────────────────────────────────────
// Edge: AgentID = uuid.Nil → wrapped ErrMissingAgentID; no install
// (symmetry with M7.1.c.a's missing-AgentID guard — uuid.Nil cannot
// be a credential-store key)
// ────────────────────────────────────────────────────────────────────────

func TestOAuthInstallStep_Execute_NilAgentID_NoInstall(t *testing.T) {
	t.Parallel()

	installer := newFakeInstaller(canonicalInstallTokens())
	dao := spawn.NewMemoryWatchkeeperSlackAppCredsDAO()
	enc := newTestEncrypter(t)
	step := newOAuthStep(t, installer, dao, enc)

	sc := saga.SpawnContext{
		ManifestVersionID: uuid.New(),
		AgentID:           uuid.Nil,
		OAuthCode:         "code-test",
	}
	ctx := saga.WithSpawnContext(context.Background(), sc)

	err := step.Execute(ctx)
	if err == nil {
		t.Fatalf("Execute: err = nil, want wrapped ErrMissingAgentID")
	}
	if !errors.Is(err, spawn.ErrMissingAgentID) {
		t.Errorf("errors.Is(err, ErrMissingAgentID) = false; got %v", err)
	}
	if got := installer.recordedCalls(); len(got) != 0 {
		t.Errorf("Installer.InstallApp call count = %d, want 0", len(got))
	}
}

// ────────────────────────────────────────────────────────────────────────
// Security: error strings do NOT leak plaintext OAuth code, plaintext
// tokens, or KEK material — every failure path
// (test-plan §"Security — error redaction")
// ────────────────────────────────────────────────────────────────────────

func TestOAuthInstallStep_Execute_ErrorPaths_DoNotLeakSecrets(t *testing.T) {
	t.Parallel()

	tokens := canonicalInstallTokens()
	creds := newTestCreds()
	secretSubstrings := []string{
		"code-secret-leak", // OAuthCode
		tokens.AccessToken,
		tokens.RefreshToken,
		tokens.UserAccessToken,
		creds.ClientSecret,
		creds.SigningSecret,
		creds.VerificationToken,
	}

	cases := []struct {
		name  string
		setup func() (step *spawn.OAuthInstallStep, ctx context.Context)
	}{
		{
			name: "installer error",
			setup: func() (*spawn.OAuthInstallStep, context.Context) {
				installer := newFakeInstaller(tokens)
				installer.returnErr = errors.New("platform fail")
				dao := spawn.NewMemoryWatchkeeperSlackAppCredsDAO()
				enc := newTestEncrypter(t)
				wkID := uuid.New()
				seedCredsRow(t, dao, wkID)
				return newOAuthStep(t, installer, dao, enc), saga.WithSpawnContext(
					context.Background(), newOAuthSpawnContext(t, wkID, "code-secret-leak"),
				)
			},
		},
		{
			name: "encrypter error",
			setup: func() (*spawn.OAuthInstallStep, context.Context) {
				installer := newFakeInstaller(tokens)
				dao := spawn.NewMemoryWatchkeeperSlackAppCredsDAO()
				enc := errEncrypter{err: errors.New("kek-loading-failure")}
				wkID := uuid.New()
				seedCredsRow(t, dao, wkID)
				return newOAuthStep(t, installer, dao, enc), saga.WithSpawnContext(
					context.Background(), newOAuthSpawnContext(t, wkID, "code-secret-leak"),
				)
			},
		},
		{
			name: "missing creds row",
			setup: func() (*spawn.OAuthInstallStep, context.Context) {
				installer := newFakeInstaller(tokens)
				dao := spawn.NewMemoryWatchkeeperSlackAppCredsDAO()
				enc := newTestEncrypter(t)
				return newOAuthStep(t, installer, dao, enc), saga.WithSpawnContext(
					context.Background(), newOAuthSpawnContext(t, uuid.New(), "code-secret-leak"),
				)
			},
		},
		{
			name: "missing OAuth code",
			setup: func() (*spawn.OAuthInstallStep, context.Context) {
				installer := newFakeInstaller(tokens)
				dao := spawn.NewMemoryWatchkeeperSlackAppCredsDAO()
				enc := newTestEncrypter(t)
				wkID := uuid.New()
				seedCredsRow(t, dao, wkID)
				return newOAuthStep(t, installer, dao, enc), saga.WithSpawnContext(
					context.Background(), newOAuthSpawnContext(t, wkID, ""),
				)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			step, ctx := tc.setup()
			err := step.Execute(ctx)
			if err == nil {
				t.Fatalf("Execute: err = nil, want non-nil for %s", tc.name)
			}
			msg := err.Error()
			for _, secret := range secretSubstrings {
				if secret == "" {
					continue
				}
				if strings.Contains(msg, secret) {
					t.Errorf("error message %q contains secret substring %q (PII leak)", msg, secret)
				}
			}
		})
	}
}

// ────────────────────────────────────────────────────────────────────────
// AC7: PII-safe audit — step source does NOT call keeperslog.Writer.Append
// ────────────────────────────────────────────────────────────────────────

// TestOAuthInstallStep_DoesNotCallKeepersLogAppend mirrors M7.1.c.a's
// source-grep AC pin: it reads oauthinstall_step.go, strips pure
// comment lines, then asserts the non-comment source contains neither
// a "keeperslog." reference nor a ".Append(" call. Stronger than a
// runtime assertion because it catches any future edit that adds a
// keeperslog import or an Append call regardless of test wiring.
func TestOAuthInstallStep_DoesNotCallKeepersLogAppend(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile("oauthinstall_step.go")
	if err != nil {
		t.Fatalf("read oauthinstall_step.go: %v", err)
	}

	var nonComment strings.Builder
	for _, line := range strings.Split(string(src), "\n") {
		if !strings.HasPrefix(strings.TrimLeft(line, " \t"), "//") {
			nonComment.WriteString(line)
			nonComment.WriteByte('\n')
		}
	}
	body := nonComment.String()

	if strings.Contains(body, "keeperslog.") {
		t.Errorf("oauthinstall_step.go references keeperslog in non-comment code — AC7 violated")
	}
	if strings.Contains(body, ".Append(") {
		t.Errorf("oauthinstall_step.go contains '.Append(' in non-comment code — AC7 violated")
	}
}

// ────────────────────────────────────────────────────────────────────────
// Concurrency: 16 goroutines, distinct watchkeeperIDs, race-detector clean
// (test-plan §"Concurrency")
// ────────────────────────────────────────────────────────────────────────

func TestOAuthInstallStep_Execute_Concurrency_DistinctWatchkeepers(t *testing.T) {
	t.Parallel()

	tokens := canonicalInstallTokens()
	installer := newFakeInstaller(tokens)
	dao := spawn.NewMemoryWatchkeeperSlackAppCredsDAO()
	enc := newTestEncrypter(t)
	step := newOAuthStep(t, installer, dao, enc)

	const n = 16
	ids := make([]uuid.UUID, n)
	for i := range ids {
		ids[i] = uuid.New()
		seedCredsRow(t, dao, ids[i])
	}

	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id uuid.UUID) {
			defer wg.Done()
			ctx := saga.WithSpawnContext(context.Background(), newOAuthSpawnContext(t, id, "code-"+id.String()))
			if err := step.Execute(ctx); err != nil {
				t.Errorf("Execute(%v): %v", id, err)
			}
		}(id)
	}
	wg.Wait()

	for _, id := range ids {
		botCT, _, _, _, _, ok := dao.GetInstallTokens(id)
		if !ok {
			t.Errorf("GetInstallTokens(%v): ok = false, want true", id)
			continue
		}
		if len(botCT) == 0 {
			t.Errorf("botCT(%v) is empty after concurrent Execute", id)
		}
	}
}
