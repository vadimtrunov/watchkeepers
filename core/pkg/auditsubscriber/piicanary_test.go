package auditsubscriber

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/approval"
	"github.com/vadimtrunov/watchkeepers/core/pkg/hostedexport"
	"github.com/vadimtrunov/watchkeepers/core/pkg/localpatch"
	"github.com/vadimtrunov/watchkeepers/core/pkg/toolregistry"
	"github.com/vadimtrunov/watchkeepers/core/pkg/toolshare"
)

// Canary substrings stuffed into bus payloads. Synthetic placeholders;
// never real credentials. The `//nolint:gosec` on the const block
// suppresses gosec G101 (hard-coded credential) — these are the test-
// harness PII canaries called out by the M9.5 / M9.6 lesson entries.
//
//nolint:gosec // G101: synthetic redaction-harness canaries, not real credentials.
const (
	canaryFolder    = "wath-canary-folder-001"            // operator-supplied path-shaped value
	canaryAlt       = "wath-canary-alt-002"               // synthetic alternate canary shape
	canaryFilename  = "wath-canary-filename-003"          // synthetic filename leak
	canaryReason    = "canary-reason-004"                 // operator-supplied audit text (Reason IS expected on Append.Payload, but NOT on diagnostic Logger)
	canaryToolName  = "canary-tool-005"                   // identifier — IS expected on Append.Payload
	canaryErrorMsg  = "ssh://user:pat-canary@example.com" // synthetic embedded-creds string a tampered ErrorType could carry
	canaryOperator  = "canary-operator-006"
	canaryProposer  = "canary-proposer-007"
	canarySourceURL = "https://user:pat-canary-008@example.com/repo.git"
)

// TestPIICanary_LoggerOmitsAllPayloadFields_OnAppendFailure asserts
// that when [Writer.Append] fails, the diagnostic [Logger] never
// receives a payload field — only topic, event_type, and err_type.
//
// This is the load-bearing M9.7 redaction discipline: an emitter
// might publish a payload that contains a reason / source-url /
// operator-id (legitimately, per its own PII allowlist), but the
// audit subscriber's DIAGNOSTIC surface must NEVER surface those.
// The keeperslog.Append call IS the audit purpose — its payload
// flows verbatim. The Logger is not the audit purpose.
func TestPIICanary_LoggerOmitsAllPayloadFields_OnAppendFailure(t *testing.T) {
	t.Parallel()
	for _, tc := range piiCanaryCases() {
		tc := tc
		t.Run(tc.eventType, func(t *testing.T) {
			t.Parallel()
			bus := &fakeBus{}
			wr := &fakeWriter{failAlways: true}
			lg := &fakeLogger{}
			s := NewSubscriber(SubscriberDeps{Bus: bus, Writer: wr, Logger: lg})
			if err := s.Start(); err != nil {
				t.Fatalf("Start: %v", err)
			}
			defer func() { _ = s.Stop() }()

			handler := bus.topicHandler(tc.topic)
			if handler == nil {
				t.Fatalf("no handler on topic %q", tc.topic)
			}
			handler(context.Background(), tc.payload)

			logDump := lg.dump()
			for _, banned := range tc.bannedSubstrings {
				if strings.Contains(logDump, banned) {
					t.Errorf("logger leaked %q in entries: %s", banned, logDump)
				}
			}
			// Sanity: the logger DID see the append-failed entry.
			if !strings.Contains(logDump, "append failed") {
				t.Errorf("logger did not record append failure: %s", logDump)
			}
		})
	}
}

// TestPIICanary_LoggerOmitsAllPayloadFields_OnTypeMismatch asserts
// the same redaction discipline on the type-mismatch path. The
// dispatcher logs `topic`, `event_type`, `got_type` (Go type only),
// `err_type` — never the offending event VALUE.
func TestPIICanary_LoggerOmitsAllPayloadFields_OnTypeMismatch(t *testing.T) {
	t.Parallel()
	bus := &fakeBus{}
	wr := &fakeWriter{}
	lg := &fakeLogger{}
	s := NewSubscriber(SubscriberDeps{Bus: bus, Writer: wr, Logger: lg})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = s.Stop() }()

	// Wrong type carrying canary content — the dispatcher must NOT
	// stringify this value into the logger.
	wrongPayload := "secretkey=" + canaryAlt + " path=" + canaryFolder
	handler := bus.topicHandler(toolregistry.TopicSourceSynced)
	handler(context.Background(), wrongPayload)

	logDump := lg.dump()
	for _, banned := range []string{canaryAlt, canaryFolder} {
		if strings.Contains(logDump, banned) {
			t.Errorf("logger leaked %q on type-mismatch path: %s", banned, logDump)
		}
	}
}

// TestPIICanary_PayloadPassedThroughVerbatim asserts the dispatcher
// does NOT mutate / strip / transform the bus payload on its way to
// the writer — the audit row reflects what the emitter published.
//
// Iter-1 critic n5 fix: use [reflect.DeepEqual] (structural equality)
// rather than a `%+v` substring search. DeepEqual catches a future
// "defensively strip a single field" refactor that the substring
// search would miss (a stripped field doesn't remove the canary
// substring from OTHER fields), AND it is stable against time.Time
// `%+v` rendering quirks (`wall=... ext=...`).
func TestPIICanary_PayloadPassedThroughVerbatim(t *testing.T) {
	t.Parallel()
	for _, tc := range piiCanaryCases() {
		tc := tc
		t.Run(tc.eventType, func(t *testing.T) {
			t.Parallel()
			bus := &fakeBus{}
			wr := &fakeWriter{}
			s := NewSubscriber(SubscriberDeps{Bus: bus, Writer: wr})
			if err := s.Start(); err != nil {
				t.Fatalf("Start: %v", err)
			}
			defer func() { _ = s.Stop() }()

			handler := bus.topicHandler(tc.topic)
			handler(context.Background(), tc.payload)

			calls := wr.snapshotEvents()
			if len(calls) != 1 {
				t.Fatalf("Append count: got %d want 1", len(calls))
			}
			if !reflect.DeepEqual(calls[0].event.Payload, tc.payload) {
				t.Errorf("Append payload differs from bus payload (transformation regression):\n got: %#v\nwant: %#v",
					calls[0].event.Payload, tc.payload)
			}
		})
	}
}

// piiCanaryCase is one row of the per-topic PII canary table.
type piiCanaryCase struct {
	topic     string
	eventType string
	payload   any
	// bannedSubstrings MUST NOT appear in the diagnostic [Logger]
	// dump on the failure paths. They are payload values an
	// emitter might carry legitimately (operator id, reason, tool
	// name, an error-type string containing a URL with embedded
	// creds) — none of which the subscriber's diagnostic surface
	// should ever surface.
	bannedSubstrings []string
	// expectedOnPayload MUST appear in the [Writer.Append] payload
	// dump on the happy path. Pins the pass-through contract.
	expectedOnPayload []string
}

// piiCanaryCases returns the per-topic canary table. Each payload
// is stuffed with synthetic canary substrings in DIFFERENT fields
// so a future field rename in one emitter package does not
// retroactively pass a test it should have failed.
func piiCanaryCases() []piiCanaryCase {
	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	pid := uuid.MustParse("00000000-0000-7000-8000-000000000002")
	return []piiCanaryCase{
		{
			topic:     toolregistry.TopicSourceSynced,
			eventType: EventTypeSourceSynced,
			payload: toolregistry.SourceSynced{
				SourceName: canaryToolName,
				SyncedAt:   now,
				LocalPath:  canaryFolder,
				// CorrelationID is set on ctx — also a leak
				// vector, so include in banned list below.
				CorrelationID: "corr-pii-1",
			},
			bannedSubstrings:  []string{canaryFolder},
			expectedOnPayload: []string{canaryToolName, canaryFolder},
		},
		{
			topic:     toolregistry.TopicSourceFailed,
			eventType: EventTypeSourceFailed,
			payload: toolregistry.SourceFailed{
				SourceName:    canaryToolName,
				FailedAt:      now,
				ErrorType:     canaryErrorMsg,
				Phase:         "clone",
				CorrelationID: "corr-pii-2",
			},
			bannedSubstrings:  []string{canaryErrorMsg},
			expectedOnPayload: []string{canaryToolName, canaryErrorMsg},
		},
		{
			topic:     toolregistry.TopicToolShadowed,
			eventType: EventTypeToolShadowed,
			payload: toolregistry.ToolShadowed{
				ToolName:        canaryToolName,
				WinnerSource:    "ws",
				WinnerVersion:   "1.0",
				ShadowedSource:  "ss",
				ShadowedVersion: "0.5",
				Revision:        3,
				BuiltAt:         now,
				CorrelationID:   "corr-pii-3",
			},
			bannedSubstrings:  []string{canaryToolName},
			expectedOnPayload: []string{canaryToolName},
		},
		{
			topic:     approval.TopicToolProposed,
			eventType: EventTypeToolProposed,
			payload: approval.ToolProposed{
				ProposalID:    pid,
				ToolName:      canaryToolName,
				ProposerID:    canaryProposer,
				TargetSource:  approval.TargetSourcePlatform,
				CapabilityIDs: []string{"cap:x"},
				ProposedAt:    now,
				CorrelationID: "corr-pii-4",
			},
			bannedSubstrings:  []string{canaryProposer, canaryToolName},
			expectedOnPayload: []string{canaryProposer, canaryToolName},
		},
		{
			topic:     approval.TopicToolApproved,
			eventType: EventTypeToolApproved,
			payload: approval.ToolApproved{
				ProposalID:    pid,
				ToolName:      canaryToolName,
				ApproverID:    canaryOperator,
				Route:         approval.RouteGitPR,
				TargetSource:  approval.TargetSourcePlatform,
				SourceName:    "platform",
				PRURL:         canarySourceURL,
				MergedSHA:     strings.Repeat("a", 40),
				ApprovedAt:    now,
				CorrelationID: "corr-pii-5",
			},
			bannedSubstrings:  []string{canaryOperator, canarySourceURL},
			expectedOnPayload: []string{canaryOperator, canarySourceURL},
		},
		{
			topic:     approval.TopicToolRejected,
			eventType: EventTypeToolRejected,
			payload: approval.ToolRejected{
				ProposalID:    pid,
				ToolName:      canaryToolName,
				RejecterID:    canaryOperator,
				Route:         approval.RouteSlackNative,
				RejectedAt:    now,
				CorrelationID: "corr-pii-6",
			},
			bannedSubstrings:  []string{canaryOperator, canaryToolName},
			expectedOnPayload: []string{canaryOperator, canaryToolName},
		},
		{
			topic:     approval.TopicDryRunExecuted,
			eventType: EventTypeDryRunExecuted,
			payload: approval.DryRunExecuted{
				ProposalID:       pid,
				ToolName:         canaryToolName,
				Mode:             toolregistry.DryRunModeGhost,
				BrokerKindCounts: map[string]int{canaryFilename: 1},
				InvocationCount:  1,
				ExecutedAt:       now,
				CorrelationID:    "corr-pii-7",
			},
			bannedSubstrings:  []string{canaryFilename, canaryToolName},
			expectedOnPayload: []string{canaryFilename, canaryToolName},
		},
		{
			topic:     localpatch.TopicLocalPatchApplied,
			eventType: EventTypeLocalPatchApplied,
			payload: localpatch.LocalPatchApplied{
				SourceName:    "local",
				ToolName:      canaryToolName,
				ToolVersion:   "1.0",
				OperatorID:    canaryOperator,
				Reason:        canaryReason,
				DiffHash:      strings.Repeat("a", 64),
				Operation:     localpatch.OperationInstall,
				AppliedAt:     now,
				CorrelationID: "corr-pii-8",
			},
			bannedSubstrings:  []string{canaryReason, canaryOperator, canaryToolName},
			expectedOnPayload: []string{canaryReason, canaryOperator, canaryToolName},
		},
		{
			topic:     hostedexport.TopicHostedToolExported,
			eventType: EventTypeHostedToolExported,
			payload: hostedexport.HostedToolExported{
				SourceName:    "hosted",
				ToolName:      canaryToolName,
				ToolVersion:   "1.0",
				OperatorID:    canaryOperator,
				Reason:        canaryReason,
				BundleDigest:  strings.Repeat("b", 64),
				ExportedAt:    now,
				CorrelationID: "corr-pii-9",
			},
			bannedSubstrings:  []string{canaryReason, canaryOperator, canaryToolName},
			expectedOnPayload: []string{canaryReason, canaryOperator, canaryToolName},
		},
		{
			topic:     toolshare.TopicToolShareProposed,
			eventType: EventTypeToolShareProposed,
			payload: toolshare.ToolShareProposed{
				SourceName:    "private",
				ToolName:      canaryToolName,
				ToolVersion:   "1.0",
				ProposerID:    canaryProposer,
				Reason:        canaryReason,
				TargetOwner:   "o",
				TargetRepo:    "r",
				TargetBase:    "main",
				TargetSource:  toolshare.TargetSourcePlatform,
				ProposedAt:    now,
				CorrelationID: "corr-pii-10",
			},
			bannedSubstrings:  []string{canaryReason, canaryProposer, canaryToolName},
			expectedOnPayload: []string{canaryReason, canaryProposer, canaryToolName},
		},
		{
			topic:     toolshare.TopicToolSharePROpened,
			eventType: EventTypeToolSharePROpened,
			payload: toolshare.ToolSharePROpened{
				SourceName:    "private",
				ToolName:      canaryToolName,
				ToolVersion:   "1.0",
				ProposerID:    canaryProposer,
				TargetOwner:   "o",
				TargetRepo:    "r",
				TargetBase:    "main",
				TargetSource:  toolshare.TargetSourcePlatform,
				PRNumber:      42,
				PRHTMLURL:     canarySourceURL,
				OpenedAt:      now,
				CorrelationID: "corr-pii-11",
			},
			bannedSubstrings:  []string{canaryProposer, canarySourceURL, canaryToolName},
			expectedOnPayload: []string{canaryProposer, canarySourceURL, canaryToolName},
		},
	}
}
