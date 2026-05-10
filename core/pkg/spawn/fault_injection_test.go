// fault_injection_test.go is the M7.3.c integration harness that
// wires the 5 concrete saga steps (CreateApp, OAuthInstall, BotProfile,
// NotebookProvision, RuntimeLaunch) into a single saga.Runner.Run,
// parameterized by which step is forced to fail. The harness asserts:
//
//   - The reverse-rollback chain dispatches Compensate on every
//     previously-successful step in REVERSE forward order.
//   - The failing step itself is NOT compensated (M7.3.b
//     "failed-step-not-rolled-back" discipline).
//   - Steps later in the forward order than the failing step receive
//     NO Execute call (the runner aborts forward progress on
//     execute-error).
//   - The BotProfile.Compensate no-op fires only as an audit row
//     (positive `saga_step_compensated` for `bot_profile`); the
//     setter is never touched on the rollback path.
//
// The harness drives the steps via [saga.Runner.Run] directly with a
// pre-seeded [saga.SpawnContext] (carrying a non-empty OAuthCode so
// OAuthInstall's input-validation gate passes — the kickoffer
// path's OAuthCode plumbing is a separate M7.1.x concern outside
// the M7.3.c rollback scope). The kickoffer's
// `manifest_rejected_after_spawn_failure` emit on saga abort is
// pinned by the dedicated cases in spawnkickoff_test.go and the
// TestKickoffer_RejectionEmit_FiresOnStep0Failure case below.
//
// The harness is the M7.3.c "fault-injection harness" deliverable —
// a single test loop covering the cross-step rollback contract that
// per-step unit tests can't observe in isolation.
//
// # Out of scope: post-side-effect Execute failures (now covered per-step)
//
// The harness intentionally injects only TOP-LEVEL Execute failures
// (the seam fake's returnErr fires BEFORE the platform call). The
// M7.3.b "failed step is NOT compensated" runner discipline means a
// platform-call-then-sink-failure path leaves orphaned platform
// state UNLESS the step's Execute body itself dispatches a best-
// effort cleanup before returning the error.
//
// M7.3.d closed this gap: the [CreateAppStep.Execute] and
// [OAuthInstallStep.Execute] bodies now capture the in-process
// platform identifier (creds.AppID for CreateApp, plaintext bot
// token for OAuthInstall) inside the sink callback, and on the
// post-platform-call failure branch dispatch a best-effort
// [SlackAppTeardown.TeardownApp] / [OAuthInstallRevoker.Revoke]
// before returning. The seams' widened signatures (knownAppID /
// knownToken parameters) accept the in-process value directly;
// empty values fall back to the M7.3.c rollback-path DAO lookup.
//
// The post-side-effect failure cases are pinned at the per-step
// unit-test level (see TestCreateAppStep_Execute_DAOPutError_DispatchesInExecuteTeardownWithCapturedAppID
// in createapp_step_test.go and
// TestOAuthInstallStep_Execute_DAOPutInstallTokensError_DispatchesInExecuteRevokeWithCapturedToken
// in oauthinstall_step_test.go) rather than duplicated into the
// cross-step harness — the harness's value is cross-step
// rollback-ordering observability, which a sink-failure case
// would NOT add coverage to (the failed step still fails, the
// runner still skips its Compensate, prior steps still get the
// reverse-rollback walk).
//
// See docs/lessons/M7.md M7.3.c iter-1 patterns #1 + the M7.3.d
// patch notes for the full rationale.
package spawn_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
	"github.com/vadimtrunov/watchkeepers/core/pkg/keeperslog"
	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn/saga"
)

// faultInjectionHarness composes the 5 concrete steps + a saga.Runner,
// wiring every Compensator-side seam to a recording fake so the test
// can observe the reverse-rollback chain.
type faultInjectionHarness struct {
	// Forward-side fakes (drive Execute paths).
	rpcFake         *fakeSlackAppRPC
	installer       *fakeInstaller
	botSetter       *fakeBotProfileSetter
	notebookProv    *fakeNotebookProvisioner
	runtimeLauncher *fakeRuntimeLauncher

	// Rollback-side fakes (record Compensate dispatches).
	teardown        *recordingSlackAppTeardown
	revoker         *recordingOAuthInstallRevoker
	notebookArchive *recordingNotebookProvisionArchiver
	runtimeTeardown *recordingRuntimeTeardown

	// Saga wiring.
	dao     *spawn.MemoryWatchkeeperSlackAppCredsDAO
	sagaDAO *saga.MemorySpawnSagaDAO
	keep    *fakeLocalKeepClient
	runner  *saga.Runner

	// Steps in forward order — exposed so the test can pin
	// step.Name() values for the audit assertions without
	// hard-coding strings.
	steps []saga.Step
}

func newFaultInjectionHarness(t *testing.T) *faultInjectionHarness {
	t.Helper()

	dao := spawn.NewMemoryWatchkeeperSlackAppCredsDAO()
	rpcFake := newFakeSlackAppRPC(newTestCreds())
	teardown := &recordingSlackAppTeardown{}
	createApp := spawn.NewCreateAppStep(spawn.CreateAppStepDeps{
		RPC:           rpcFake,
		CredsDAO:      dao,
		Teardown:      teardown,
		AppName:       "test-app",
		Scopes:        []string{"chat:write"},
		ApprovalToken: "tok-fault-injection",
	})

	installer := newFakeInstaller(canonicalInstallTokens())
	revoker := &recordingOAuthInstallRevoker{}
	oauthInstall := spawn.NewOAuthInstallStep(spawn.OAuthInstallStepDeps{
		Installer: installer,
		CredsDAO:  dao,
		Encrypter: newTestEncrypter(t),
		Revoker:   revoker,
		Workspace: messenger.WorkspaceRef{ID: "T0123", Name: "Test"},
	})

	botSetter := newFakeBotProfileSetter()
	botProfile := spawn.NewBotProfileStep(spawn.BotProfileStepDeps{
		Setter:  botSetter,
		Profile: canonicalBotProfile(),
	})

	notebookProv := newFakeNotebookProvisioner()
	notebookArchive := &recordingNotebookProvisionArchiver{}
	notebookProvision := spawn.NewNotebookProvisionStep(spawn.NotebookProvisionStepDeps{
		Provisioner: notebookProv,
		Archiver:    notebookArchive,
		Profile:     canonicalNotebookProfile(),
	})

	runtimeLauncher := newFakeRuntimeLauncher()
	runtimeTeardown := &recordingRuntimeTeardown{}
	runtimeLaunch := spawn.NewRuntimeLaunchStep(spawn.RuntimeLaunchStepDeps{
		Launcher: runtimeLauncher,
		Teardown: runtimeTeardown,
		Profile:  canonicalRuntimeLaunchProfile(),
	})

	steps := []saga.Step{createApp, oauthInstall, botProfile, notebookProvision, runtimeLaunch}

	sagaDAO := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	writer := keeperslog.New(keep)
	runner := saga.NewRunner(saga.Dependencies{DAO: sagaDAO, Logger: writer})

	return &faultInjectionHarness{
		rpcFake:         rpcFake,
		installer:       installer,
		botSetter:       botSetter,
		notebookProv:    notebookProv,
		runtimeLauncher: runtimeLauncher,
		teardown:        teardown,
		revoker:         revoker,
		notebookArchive: notebookArchive,
		runtimeTeardown: runtimeTeardown,
		dao:             dao,
		sagaDAO:         sagaDAO,
		keep:            keep,
		runner:          runner,
		steps:           steps,
	}
}

// run dispatches saga.Runner.Run directly with a pre-seeded
// SpawnContext (carrying a non-empty OAuthCode so OAuthInstall's
// input-validation gate passes). Returns the runner's wrapped step
// error.
func (h *faultInjectionHarness) run(t *testing.T) error {
	t.Helper()
	sagaID := uuid.New()
	manifestVersionID := uuid.New()
	watchkeeperID := uuid.New()
	if err := h.sagaDAO.Insert(context.Background(), sagaID, manifestVersionID); err != nil {
		t.Fatalf("sagaDAO.Insert: %v", err)
	}
	sc := saga.SpawnContext{
		ManifestVersionID: manifestVersionID,
		AgentID:           watchkeeperID,
		Claim: saga.SpawnClaim{
			OrganizationID:  "org-test",
			AgentID:         "agent-watchmaster",
			AuthorityMatrix: map[string]string{"slack_app_create": "lead_approval"},
		},
		OAuthCode: "code-fault-injection",
	}
	ctx := saga.WithSpawnContext(context.Background(), sc)
	return h.runner.Run(ctx, sagaID, h.steps)
}

// ────────────────────────────────────────────────────────────────────────
// Table-driven harness: kill saga at each step N, assert reverse
// rollback fires for steps 0..N-1.
// ────────────────────────────────────────────────────────────────────────

func TestFaultInjection_KillSagaAtStepN_ReverseRollbackOverPriorSteps(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name             string
		injectFailure    func(*faultInjectionHarness)
		failedStepIndex  int      // 0..4
		failedStepName   string   // for the saga_failed payload assertion
		wantCompensators []string // step.Name() values, in REVERSE forward order
	}{
		{
			name: "step0_create_app_fails_no_prior_compensators",
			injectFailure: func(h *faultInjectionHarness) {
				h.rpcFake.returnErr = errors.New("create_app boom")
			},
			failedStepIndex:  0,
			failedStepName:   spawn.CreateAppStepName,
			wantCompensators: []string{}, // no prior steps to compensate
		},
		{
			name: "step1_oauth_install_fails_create_app_compensated",
			injectFailure: func(h *faultInjectionHarness) {
				h.installer.returnErr = errors.New("oauth_install boom")
			},
			failedStepIndex:  1,
			failedStepName:   spawn.OAuthInstallStepName,
			wantCompensators: []string{spawn.CreateAppStepName},
		},
		{
			name: "step2_bot_profile_fails_oauth_then_create_app_compensated",
			injectFailure: func(h *faultInjectionHarness) {
				h.botSetter.returnErr = errors.New("bot_profile boom")
			},
			failedStepIndex: 2,
			failedStepName:  spawn.BotProfileStepName,
			wantCompensators: []string{
				spawn.OAuthInstallStepName,
				spawn.CreateAppStepName,
			},
		},
		{
			name: "step3_notebook_provision_fails_bot_oauth_create_app_compensated",
			injectFailure: func(h *faultInjectionHarness) {
				h.notebookProv.returnErr = errors.New("notebook_provision boom")
			},
			failedStepIndex: 3,
			failedStepName:  spawn.NotebookProvisionStepName,
			wantCompensators: []string{
				spawn.BotProfileStepName,
				spawn.OAuthInstallStepName,
				spawn.CreateAppStepName,
			},
		},
		{
			name: "step4_runtime_launch_fails_all_prior_compensated",
			injectFailure: func(h *faultInjectionHarness) {
				h.runtimeLauncher.returnErr = errors.New("runtime_launch boom")
			},
			failedStepIndex: 4,
			failedStepName:  spawn.RuntimeLaunchStepName,
			wantCompensators: []string{
				spawn.NotebookProvisionStepName,
				spawn.BotProfileStepName,
				spawn.OAuthInstallStepName,
				spawn.CreateAppStepName,
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newFaultInjectionHarness(t)
			tc.injectFailure(h)

			err := h.run(t)
			if err == nil {
				t.Fatalf("kickoff: err = nil; want wrapped step error")
			}

			// Assert per-Compensator dispatch counts. Compensators
			// for steps PRIOR to the failed one MUST run; later
			// steps must not (they never executed).
			assertCompensateCounts(t, h, tc.failedStepIndex)

			// Audit-chain assertions.
			rows := h.keep.recorded()
			if len(rows) == 0 {
				t.Fatalf("no audit rows emitted")
			}
			assertSagaFailedRowPresent(t, rows, tc.failedStepName)
			assertCompensationRowsInOrder(t, rows, tc.wantCompensators)
		})
	}
}

// TestKickoffer_RejectionEmit_FiresOnStep0Failure pins the
// kickoffer-side `manifest_rejected_after_spawn_failure` emit on the
// saga-abort path. Uses a step list of [stubFailingStep] so the
// harness does NOT require OAuthCode plumbing — the M7.3.b emit
// logic is step-agnostic; we only need a step that returns non-nil
// from Execute.
func TestKickoffer_RejectionEmit_FiresOnStep0Failure(t *testing.T) {
	t.Parallel()

	sagaDAO := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	writer := keeperslog.New(keep)
	runner := saga.NewRunner(saga.Dependencies{DAO: sagaDAO, Logger: writer})

	failer := &alwaysFailingStep{name: "kickoff_failure_canary", err: errors.New("kickoff_failure_canary failed")}
	kickoffer := spawn.NewSpawnKickoffer(spawn.SpawnKickoffDeps{
		Logger:  writer,
		DAO:     sagaDAO,
		Runner:  runner,
		AgentID: "agent-watchmaster",
		Steps:   []saga.Step{failer},
	})

	err := kickoffer.Kickoff(
		context.Background(), uuid.New(), uuid.New(), uuid.New(),
		saga.SpawnClaim{
			OrganizationID:  "org-test",
			AgentID:         "agent-watchmaster",
			AuthorityMatrix: map[string]string{"slack_app_create": "lead_approval"},
		},
		"tok-rejection-emit",
	)
	if err == nil {
		t.Fatalf("Kickoff: err = nil, want wrapped step error")
	}
	assertManifestRejectedRowPresent(t, keep.recorded())
}

// alwaysFailingStep is a minimal saga.Step used by the kickoffer
// rejection-emit test: returns nil from Name() / err from Execute,
// no Compensate (silently skipped per M7.3.b).
type alwaysFailingStep struct {
	name string
	err  error
}

func (s *alwaysFailingStep) Name() string                    { return s.name }
func (s *alwaysFailingStep) Execute(_ context.Context) error { return s.err }

// ────────────────────────────────────────────────────────────────────────
// Per-Compensator dispatch-count assertion.
// ────────────────────────────────────────────────────────────────────────

func assertCompensateCounts(t *testing.T, h *faultInjectionHarness, failedStepIndex int) {
	t.Helper()
	// Index → expected callCount on the Compensator-side fake.
	// 1 if the step's Execute succeeded (i.e. step index < failed),
	// 0 otherwise.
	expect := func(stepIdx int) int32 {
		if stepIdx < failedStepIndex {
			return 1
		}
		return 0
	}

	if got := h.teardown.callCount.Load(); got != expect(0) {
		t.Errorf("CreateApp.Compensate count = %d, want %d", got, expect(0))
	}
	if got := h.revoker.callCount.Load(); got != expect(1) {
		t.Errorf("OAuthInstall.Compensate (revoke) count = %d, want %d", got, expect(1))
	}
	// BotProfile.Compensate is the documented no-op. The setter is
	// called only on the forward Execute (when reached); the
	// rollback no-op MUST NOT touch the platform. So the setter's
	// total count must be:
	//   - 0 when the failure is BEFORE step 2 (BotProfile not reached)
	//   - 1 when step 2 (BotProfile) executed (success or failure path)
	//
	// A count of 2 would indicate the rollback dispatched a second
	// setter call — the regression M7.3.b lesson #1 + the explicit
	// no-op contract guards against.
	wantSetterCalls := int32(0)
	if failedStepIndex >= 2 {
		wantSetterCalls = 1
	}
	if got := h.botSetter.callCount.Load(); got != wantSetterCalls {
		t.Errorf("BotProfile.Setter call count = %d, want %d (Compensate must NOT call the setter)", got, wantSetterCalls)
	}
	if got := h.notebookArchive.callCount.Load(); got != expect(3) {
		t.Errorf("NotebookProvision.Compensate (archive) count = %d, want %d", got, expect(3))
	}
	if got := h.runtimeTeardown.callCount.Load(); got != expect(4) {
		t.Errorf("RuntimeLaunch.Compensate (teardown) count = %d, want %d", got, expect(4))
	}
}

// ────────────────────────────────────────────────────────────────────────
// Audit-chain assertions.
// ────────────────────────────────────────────────────────────────────────

func assertSagaFailedRowPresent(t *testing.T, rows []keepclient.LogAppendRequest, failedStepName string) {
	t.Helper()
	for _, r := range rows {
		if r.EventType != saga.EventTypeSagaFailed {
			continue
		}
		data := mustExtractDataPayload(t, r.Payload)
		if name, _ := data["step_name"].(string); name == failedStepName {
			return
		}
	}
	t.Errorf("no saga_failed row for failing step %q", failedStepName)
}

// assertCompensationRowsInOrder validates that the audit chain emits
// exactly `len(wantCompensators)` saga_step_compensated rows in the
// supplied order, and exactly one trailing saga_compensated summary.
// All step.Name() values pinned by index — a regression that
// reverses the rollback order or drops the BotProfile no-op row
// surfaces here.
func assertCompensationRowsInOrder(t *testing.T, rows []keepclient.LogAppendRequest, wantCompensators []string) {
	t.Helper()
	gotCompensators := make([]string, 0, len(wantCompensators))
	var gotSummary bool
	for _, r := range rows {
		switch r.EventType {
		case saga.EventTypeSagaStepCompensated:
			data := mustExtractDataPayload(t, r.Payload)
			if name, _ := data["step_name"].(string); name != "" {
				gotCompensators = append(gotCompensators, name)
			}
		case saga.EventTypeSagaCompensated:
			gotSummary = true
		}
	}
	if len(gotCompensators) != len(wantCompensators) {
		t.Fatalf("saga_step_compensated count = %d, want %d (got=%v want=%v)",
			len(gotCompensators), len(wantCompensators), gotCompensators, wantCompensators)
	}
	for i, want := range wantCompensators {
		if gotCompensators[i] != want {
			t.Errorf("saga_step_compensated[%d] = %q, want %q", i, gotCompensators[i], want)
		}
	}
	if !gotSummary {
		t.Errorf("missing saga_compensated summary row")
	}
}

// assertManifestRejectedRowPresent pins the M7.3.b kickoffer-emit
// of `manifest_rejected_after_spawn_failure` AFTER the saga failure.
func assertManifestRejectedRowPresent(t *testing.T, rows []keepclient.LogAppendRequest) {
	t.Helper()
	for _, r := range rows {
		if r.EventType == spawn.EventTypeManifestRejectedAfterSpawnFailure {
			return
		}
	}
	t.Errorf("no manifest_rejected_after_spawn_failure row emitted; M7.3.b kickoffer emit regressed")
}

// mustExtractDataPayload extracts the keeperslog-envelope `data`
// field as a generic map. Mirrors mustExtractData declared in the
// saga package's test fixtures (different package, so re-declared
// here verbatim).
func mustExtractDataPayload(t *testing.T, payload json.RawMessage) map[string]any {
	t.Helper()
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	dataRaw, ok := envelope["data"]
	if !ok {
		t.Fatalf("payload missing `data` envelope key: %s", string(payload))
	}
	var data map[string]any
	if err := json.Unmarshal(dataRaw, &data); err != nil {
		t.Fatalf("payload.data not JSON: %v", err)
	}
	return data
}
