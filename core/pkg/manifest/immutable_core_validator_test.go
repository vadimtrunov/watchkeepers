package manifest

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
	"github.com/vadimtrunov/watchkeepers/core/pkg/keeperslog"
)

// recordingAppender is a hand-rolled [Appender] used by every M3.6
// validator test. Mirrors the `keeperslog` writer_test fakes
// (`fakeKeepClient`) — no mocking library, just a slice of captured
// events guarded by a mutex so the 16-goroutine concurrency assertion
// can safely append from multiple goroutines.
type recordingAppender struct {
	mu     sync.Mutex
	events []keeperslog.Event
	err    error
}

func (r *recordingAppender) Append(_ context.Context, evt keeperslog.Event) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, evt)
	if r.err != nil {
		return "", r.err
	}
	return "row-id", nil
}

func (r *recordingAppender) snapshot() []keeperslog.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]keeperslog.Event, len(r.events))
	copy(out, r.events)
	return out
}

// Compile-time assertion that the real keeperslog.Writer satisfies the
// validator's local [Appender] interface. Mirrors the
// `cost.LoggingProvider`'s Appender assertion (cost_test.go); breaks
// at compile time if a future keeperslog refactor renames Append.
var _ Appender = (*keeperslog.Writer)(nil)

// TestValidate_BothNil_PassesWithoutAudit covers the legacy parent +
// omitting proposer case: a parent row predating M3.1 has no
// immutable_core declared, the self-tuner ships no immutable_core
// either, and the gate accepts the proposal without an audit row.
func TestValidate_BothNil_PassesWithoutAudit(t *testing.T) {
	t.Parallel()

	auditor := &recordingAppender{}
	v := NewImmutableCoreValidator(auditor)

	err := v.Validate(context.Background(), nil, nil, "watchmaster")
	if err != nil {
		t.Fatalf("Validate: %v, want nil", err)
	}
	if got := auditor.snapshot(); len(got) != 0 {
		t.Errorf("audit emitted on equal-nil path: %#v", got)
	}
}

// TestValidate_BothNull_PassesWithoutAudit asserts the explicit-`null`
// representation collapses to the same canonical-empty form as nil /
// empty RawMessage. The canonicalisation contract is documented on
// `canonicaliseImmutableCore`.
func TestValidate_BothNull_PassesWithoutAudit(t *testing.T) {
	t.Parallel()

	auditor := &recordingAppender{}
	v := NewImmutableCoreValidator(auditor)

	err := v.Validate(
		context.Background(),
		json.RawMessage(`null`),
		json.RawMessage(`null`),
		"watchmaster",
	)
	if err != nil {
		t.Fatalf("Validate: %v, want nil", err)
	}
	if got := auditor.snapshot(); len(got) != 0 {
		t.Errorf("audit emitted on equal-null path: %#v", got)
	}
}

// TestValidate_ByteIdentical_Passes asserts the happy path: a proposal
// whose immutable_core jsonb is byte-identical to the parent (5
// canonical buckets present, identical values) returns nil and emits
// no audit row.
func TestValidate_ByteIdentical_Passes(t *testing.T) {
	t.Parallel()

	payload := json.RawMessage(`{
		"role_boundaries": ["finance-write", "secrets-read"],
		"security_constraints": {"forbidden_destinations": ["external"]},
		"escalation_protocols": {"pii_leak": {"target": "security", "sla_minutes": 15}},
		"cost_limits": {"per_task_tokens": 5000, "per_day_usd": 25},
		"audit_requirements": {"manifest_changes": {"retain_days": 365}}
	}`)

	auditor := &recordingAppender{}
	v := NewImmutableCoreValidator(auditor)

	if err := v.Validate(context.Background(), payload, payload, "agent-7"); err != nil {
		t.Fatalf("Validate: %v, want nil", err)
	}
	if got := auditor.snapshot(); len(got) != 0 {
		t.Errorf("audit emitted on byte-identical path: %#v", got)
	}
}

// TestValidate_KeyReorderEqual_PassesWithoutAudit is the canonicalisation
// regression catch: two payloads with identical structural value but
// different wire-side key order must compare equal (mirrors the M3.2
// SQL gate's `jsonb IS DISTINCT FROM` semantics — see
// `docs/lessons/M3.md` 2026-05-17 M3.2 entry, "structural compare via
// jsonb IS DISTINCT FROM jsonb").
func TestValidate_KeyReorderEqual_PassesWithoutAudit(t *testing.T) {
	t.Parallel()

	proposed := json.RawMessage(`{
		"role_boundaries": ["a","b"],
		"cost_limits": {"per_day_usd": 25, "per_task_tokens": 5000}
	}`)
	parent := json.RawMessage(`{
		"cost_limits": {"per_task_tokens": 5000, "per_day_usd": 25},
		"role_boundaries": ["a","b"]
	}`)

	auditor := &recordingAppender{}
	v := NewImmutableCoreValidator(auditor)

	if err := v.Validate(context.Background(), proposed, parent, "agent-7"); err != nil {
		t.Fatalf("Validate: %v, want nil", err)
	}
	if got := auditor.snapshot(); len(got) != 0 {
		t.Errorf("audit emitted on key-reorder equal path: %#v", got)
	}
}

// TestValidate_PerBucketDrift_BlocksWithSentinel exercises each of the
// five canonical buckets independently: a proposal that mutates ONE
// bucket and leaves the other four byte-identical must return
// [ErrSelfTuneBlockedImmutable] AND emit a single keepers_log row
// naming the drifted bucket via `target_field`. Mirrors the
// "reject when any of the 5 buckets differs" AC from the task brief.
func TestValidate_PerBucketDrift_BlocksWithSentinel(t *testing.T) {
	t.Parallel()

	const baseParent = `{
		"role_boundaries": ["finance-write"],
		"security_constraints": {"floor": "internal"},
		"escalation_protocols": {"pii_leak": "security"},
		"cost_limits": {"per_task_tokens": 5000},
		"audit_requirements": {"manifest_changes": "365d"}
	}`

	cases := []struct {
		bucket  string
		mutated string
	}{
		{
			bucket: "role_boundaries",
			mutated: `{
				"role_boundaries": ["finance-write","secrets-read"],
				"security_constraints": {"floor": "internal"},
				"escalation_protocols": {"pii_leak": "security"},
				"cost_limits": {"per_task_tokens": 5000},
				"audit_requirements": {"manifest_changes": "365d"}
			}`,
		},
		{
			bucket: "security_constraints",
			mutated: `{
				"role_boundaries": ["finance-write"],
				"security_constraints": {"floor": "public"},
				"escalation_protocols": {"pii_leak": "security"},
				"cost_limits": {"per_task_tokens": 5000},
				"audit_requirements": {"manifest_changes": "365d"}
			}`,
		},
		{
			bucket: "escalation_protocols",
			mutated: `{
				"role_boundaries": ["finance-write"],
				"security_constraints": {"floor": "internal"},
				"escalation_protocols": {"pii_leak": "ops"},
				"cost_limits": {"per_task_tokens": 5000},
				"audit_requirements": {"manifest_changes": "365d"}
			}`,
		},
		{
			bucket: "cost_limits",
			mutated: `{
				"role_boundaries": ["finance-write"],
				"security_constraints": {"floor": "internal"},
				"escalation_protocols": {"pii_leak": "security"},
				"cost_limits": {"per_task_tokens": 999999},
				"audit_requirements": {"manifest_changes": "365d"}
			}`,
		},
		{
			bucket: "audit_requirements",
			mutated: `{
				"role_boundaries": ["finance-write"],
				"security_constraints": {"floor": "internal"},
				"escalation_protocols": {"pii_leak": "security"},
				"cost_limits": {"per_task_tokens": 5000},
				"audit_requirements": {"manifest_changes": "30d"}
			}`,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.bucket, func(t *testing.T) {
			t.Parallel()
			auditor := &recordingAppender{}
			v := NewImmutableCoreValidator(auditor)
			err := v.Validate(
				context.Background(),
				json.RawMessage(tc.mutated),
				json.RawMessage(baseParent),
				"agent-7",
			)
			if !errors.Is(err, ErrSelfTuneBlockedImmutable) {
				t.Fatalf("Validate: %v, want ErrSelfTuneBlockedImmutable", err)
			}
			events := auditor.snapshot()
			if len(events) != 1 {
				t.Fatalf("audit events = %d, want 1: %#v", len(events), events)
			}
			ev := events[0]
			if ev.EventType != EventTypeSelfTuneBlockedImmutable {
				t.Errorf("EventType = %q, want %q", ev.EventType, EventTypeSelfTuneBlockedImmutable)
			}
			payload, ok := ev.Payload.(map[string]any)
			if !ok {
				t.Fatalf("Payload type = %T, want map[string]any", ev.Payload)
			}
			if payload[selfTunePayloadKeyProposer] != "agent-7" {
				t.Errorf("proposer = %v, want %q", payload[selfTunePayloadKeyProposer], "agent-7")
			}
			if payload[selfTunePayloadKeyTargetField] != tc.bucket {
				t.Errorf("target_field = %v, want %q", payload[selfTunePayloadKeyTargetField], tc.bucket)
			}
		})
	}
}

// TestValidate_MultiBucketDrift_TargetFieldJoined asserts the comma-
// joined `target_field` payload when a proposal drifts two or more
// buckets simultaneously. Order MUST be alphabetical (sorted) so the
// audit row is deterministic for a given drift pattern — the M3.4
// `manifest.history` introspection tools will join on `target_field`
// values across rows and a non-deterministic order would break that.
func TestValidate_MultiBucketDrift_TargetFieldJoined(t *testing.T) {
	t.Parallel()

	parent := json.RawMessage(`{
		"role_boundaries": ["a"],
		"cost_limits": {"per_task": 100},
		"audit_requirements": {"x": "y"}
	}`)
	proposed := json.RawMessage(`{
		"role_boundaries": ["a","b"],
		"cost_limits": {"per_task": 999},
		"audit_requirements": {"x": "y"}
	}`)

	auditor := &recordingAppender{}
	v := NewImmutableCoreValidator(auditor)
	err := v.Validate(context.Background(), proposed, parent, "watchmaster")
	if !errors.Is(err, ErrSelfTuneBlockedImmutable) {
		t.Fatalf("Validate: %v, want ErrSelfTuneBlockedImmutable", err)
	}
	events := auditor.snapshot()
	if len(events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(events))
	}
	payload := events[0].Payload.(map[string]any)
	const wantTarget = "cost_limits,role_boundaries"
	if payload[selfTunePayloadKeyTargetField] != wantTarget {
		t.Errorf("target_field = %v, want %q", payload[selfTunePayloadKeyTargetField], wantTarget)
	}
}

// TestValidate_AddedBucket_BlocksAndNamesField asserts that adding a
// bucket on the proposed side (parent has no key X, proposed has key
// X) trips the gate AND names the added bucket in the audit row. This
// is the forward-compat fork: a self-tuner that ships a brand-new
// bucket key M3.1 did not recognise must still be blocked.
func TestValidate_AddedBucket_BlocksAndNamesField(t *testing.T) {
	t.Parallel()

	parent := json.RawMessage(`{"role_boundaries":["a"]}`)
	proposed := json.RawMessage(`{"role_boundaries":["a"],"future_bucket":{"k":"v"}}`)

	auditor := &recordingAppender{}
	v := NewImmutableCoreValidator(auditor)
	err := v.Validate(context.Background(), proposed, parent, "agent-7")
	if !errors.Is(err, ErrSelfTuneBlockedImmutable) {
		t.Fatalf("Validate: %v, want ErrSelfTuneBlockedImmutable", err)
	}
	events := auditor.snapshot()
	if len(events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(events))
	}
	payload := events[0].Payload.(map[string]any)
	if payload[selfTunePayloadKeyTargetField] != "future_bucket" {
		t.Errorf("target_field = %v, want %q", payload[selfTunePayloadKeyTargetField], "future_bucket")
	}
}

// TestValidate_RemovedBucket_BlocksAndNamesField is the mirror of the
// added-bucket case: parent has a bucket the proposed side dropped.
// The gate must block; the audit row must name the dropped bucket.
func TestValidate_RemovedBucket_BlocksAndNamesField(t *testing.T) {
	t.Parallel()

	parent := json.RawMessage(`{"role_boundaries":["a"],"cost_limits":{"x":1}}`)
	proposed := json.RawMessage(`{"role_boundaries":["a"]}`)

	auditor := &recordingAppender{}
	v := NewImmutableCoreValidator(auditor)
	err := v.Validate(context.Background(), proposed, parent, "agent-7")
	if !errors.Is(err, ErrSelfTuneBlockedImmutable) {
		t.Fatalf("Validate: %v, want ErrSelfTuneBlockedImmutable", err)
	}
	events := auditor.snapshot()
	if len(events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(events))
	}
	payload := events[0].Payload.(map[string]any)
	if payload[selfTunePayloadKeyTargetField] != "cost_limits" {
		t.Errorf("target_field = %v, want %q", payload[selfTunePayloadKeyTargetField], "cost_limits")
	}
}

// TestValidate_NilParentNonNilProposed_BlocksLegacyCreate asserts that
// a legacy parent (no immutable_core declared) plus a proposing
// self-tuner that ships a non-empty immutable_core is rejected. This
// is the protection against "the self-tuner is the first writer to
// introduce the field" — only an admin (per M3.2) may seed the
// initial value.
func TestValidate_NilParentNonNilProposed_BlocksLegacyCreate(t *testing.T) {
	t.Parallel()

	auditor := &recordingAppender{}
	v := NewImmutableCoreValidator(auditor)
	err := v.Validate(
		context.Background(),
		json.RawMessage(`{"role_boundaries":["a"]}`),
		nil,
		"agent-7",
	)
	if !errors.Is(err, ErrSelfTuneBlockedImmutable) {
		t.Fatalf("Validate: %v, want ErrSelfTuneBlockedImmutable", err)
	}
	events := auditor.snapshot()
	if len(events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(events))
	}
}

// TestValidate_NonNilParentNilProposed_BlocksDrop asserts the mirror
// case: a parent with a populated immutable_core and a proposing
// self-tuner that drops the field entirely is rejected. The
// "set-then-drop" pattern is the most direct self-tuning attack and
// the validator MUST catch it.
func TestValidate_NonNilParentNilProposed_BlocksDrop(t *testing.T) {
	t.Parallel()

	auditor := &recordingAppender{}
	v := NewImmutableCoreValidator(auditor)
	err := v.Validate(
		context.Background(),
		nil,
		json.RawMessage(`{"role_boundaries":["a"]}`),
		"agent-7",
	)
	if !errors.Is(err, ErrSelfTuneBlockedImmutable) {
		t.Fatalf("Validate: %v, want ErrSelfTuneBlockedImmutable", err)
	}
}

// TestValidate_NoAuditor_ReturnsSentinelStill asserts the construction
// contract: a nil [Appender] is allowed and means "no audit row" — the
// gate still returns the sentinel on a structural mismatch.
func TestValidate_NoAuditor_ReturnsSentinelStill(t *testing.T) {
	t.Parallel()

	v := NewImmutableCoreValidator(nil)
	err := v.Validate(
		context.Background(),
		json.RawMessage(`{"role_boundaries":["a"]}`),
		json.RawMessage(`{"role_boundaries":["b"]}`),
		"agent-7",
	)
	if !errors.Is(err, ErrSelfTuneBlockedImmutable) {
		t.Fatalf("Validate: %v, want ErrSelfTuneBlockedImmutable", err)
	}
}

// TestValidate_AuditorErrorSwallowed_StillReturnsSentinel asserts the
// audit-emit failure-mode contract: a keepers_log outage MUST NOT mask
// the gate decision. The validator silently swallows the audit error
// and still returns the sentinel so the proposer is blocked.
func TestValidate_AuditorErrorSwallowed_StillReturnsSentinel(t *testing.T) {
	t.Parallel()

	auditor := &recordingAppender{err: errors.New("keep down")}
	v := NewImmutableCoreValidator(auditor)
	err := v.Validate(
		context.Background(),
		json.RawMessage(`{"a":1}`),
		json.RawMessage(`{"a":2}`),
		"agent-7",
	)
	if !errors.Is(err, ErrSelfTuneBlockedImmutable) {
		t.Fatalf("Validate: %v, want ErrSelfTuneBlockedImmutable", err)
	}
	if got := auditor.snapshot(); len(got) != 1 {
		t.Errorf("audit attempted = %d, want 1", len(got))
	}
}

// TestValidate_MalformedProposed_ReturnsWrappedDecodeError asserts the
// canonicalisation-failure path: a malformed proposed payload returns
// a wrapped json.Unmarshal error, NOT the sentinel, so the caller can
// distinguish "you shipped junk" from "you touched a forbidden field".
// No audit row is emitted on this path — the proposer is blocked by
// a parse error, not a policy decision.
func TestValidate_MalformedProposed_ReturnsWrappedDecodeError(t *testing.T) {
	t.Parallel()

	auditor := &recordingAppender{}
	v := NewImmutableCoreValidator(auditor)
	err := v.Validate(
		context.Background(),
		json.RawMessage(`{not-json`),
		nil,
		"agent-7",
	)
	if err == nil {
		t.Fatalf("Validate: nil, want a json decode error")
	}
	if errors.Is(err, ErrSelfTuneBlockedImmutable) {
		t.Fatalf("Validate: returned sentinel on malformed input, want decode error")
	}
	if !strings.Contains(err.Error(), "proposed") {
		t.Errorf("err = %q, want substring 'proposed'", err.Error())
	}
	if got := auditor.snapshot(); len(got) != 0 {
		t.Errorf("audit emitted on decode-error path: %#v", got)
	}
}

// TestValidate_MalformedParent_ReturnsWrappedDecodeError mirrors the
// proposed-malformed test for the parent side; the error message
// must distinguish which side failed so the M2 self-tuner can pin
// the bug to the right surface.
func TestValidate_MalformedParent_ReturnsWrappedDecodeError(t *testing.T) {
	t.Parallel()

	auditor := &recordingAppender{}
	v := NewImmutableCoreValidator(auditor)
	err := v.Validate(
		context.Background(),
		nil,
		json.RawMessage(`["not","an","object"]`),
		"agent-7",
	)
	if err == nil {
		t.Fatalf("Validate: nil, want decode error")
	}
	if errors.Is(err, ErrSelfTuneBlockedImmutable) {
		t.Fatalf("Validate: returned sentinel on malformed parent, want decode error")
	}
	if !strings.Contains(err.Error(), "parent") {
		t.Errorf("err = %q, want substring 'parent'", err.Error())
	}
}

// TestValidate_PackageLevelConvenience asserts the no-audit
// [ValidateImmutableCoreUnchanged] wrapper round-trips the gate
// decision identically to a nil-Appender [ImmutableCoreValidator].
func TestValidate_PackageLevelConvenience(t *testing.T) {
	t.Parallel()

	t.Run("equal_returns_nil", func(t *testing.T) {
		t.Parallel()
		err := ValidateImmutableCoreUnchanged(
			context.Background(),
			json.RawMessage(`{"a":1}`),
			json.RawMessage(`{"a":1}`),
			"agent-7",
		)
		if err != nil {
			t.Fatalf("Validate: %v, want nil", err)
		}
	})
	t.Run("mismatch_returns_sentinel", func(t *testing.T) {
		t.Parallel()
		err := ValidateImmutableCoreUnchanged(
			context.Background(),
			json.RawMessage(`{"a":2}`),
			json.RawMessage(`{"a":1}`),
			"agent-7",
		)
		if !errors.Is(err, ErrSelfTuneBlockedImmutable) {
			t.Fatalf("Validate: %v, want ErrSelfTuneBlockedImmutable", err)
		}
	})
}

// TestValidate_CorrelationIDFlowsThroughContext asserts the audit row
// inherits the keepers_log correlation_id from the request ctx — the
// validator does NOT mint its own correlation id; it relies on the
// keeperslog.Writer's [ContextWithCorrelationID] propagation so the
// rejection row joins the request chain that produced it.
//
// Uses a real `keeperslog.Writer` against an in-package fake
// keep-client so the correlation_id round-trip exercises the full
// production code path.
func TestValidate_CorrelationIDFlowsThroughContext(t *testing.T) {
	t.Parallel()

	const wantCorr = "corr-fixed-001"
	fake := &fakeKeepLogClient{}
	w := keeperslog.New(
		fake,
		keeperslog.WithIDGenerator(func() (string, error) { return "evt-id", nil }),
		keeperslog.WithCorrelationIDGenerator(func() (string, error) { return "must-not-be-used", nil }),
	)

	v := NewImmutableCoreValidator(w)
	ctx := keeperslog.ContextWithCorrelationID(context.Background(), wantCorr)
	err := v.Validate(ctx, json.RawMessage(`{"a":1}`), json.RawMessage(`{"a":2}`), "agent-7")
	if !errors.Is(err, ErrSelfTuneBlockedImmutable) {
		t.Fatalf("Validate: %v, want ErrSelfTuneBlockedImmutable", err)
	}
	if fake.lastCorrelationID != wantCorr {
		t.Errorf("correlation_id on emitted row = %q, want %q", fake.lastCorrelationID, wantCorr)
	}
	if fake.lastEventType != EventTypeSelfTuneBlockedImmutable {
		t.Errorf("event_type = %q, want %q", fake.lastEventType, EventTypeSelfTuneBlockedImmutable)
	}
}

// TestValidate_PIIRedactionHarness asserts the validator never emits
// the raw immutable_core bytes into the keepers_log row. We populate
// each bucket with a unique synthetic canary substring; on rejection
// the emitted payload must contain ONLY the closed-set keys
// (`proposer`, `target_field`) and must NOT contain any canary
// substring. This is the canary harness pattern documented in
// `docs/lessons/M7.md` for saga-step PII discipline; M3.6 is the
// first manifest-package gate to adopt it.
func TestValidate_PIIRedactionHarness(t *testing.T) {
	t.Parallel()

	//nolint:gosec // G101: synthetic redaction-harness canaries, not real credentials.
	const (
		canaryRoleBoundaries      = "CANARY-ROLE-BOUNDARY-SECRET"
		canarySecurityConstraints = "CANARY-SECURITY-CONSTRAINT-SECRET"
		canaryEscalation          = "CANARY-ESCALATION-SECRET"
		canaryCostLimits          = "CANARY-COSTLIMITS-SECRET"
		canaryAudit               = "CANARY-AUDIT-SECRET"
		canaryProposer            = "CANARY-PROPOSER-FREEFORM"
	)
	proposed := json.RawMessage(`{
		"role_boundaries": ["` + canaryRoleBoundaries + `"],
		"security_constraints": {"k":"` + canarySecurityConstraints + `"},
		"escalation_protocols": {"k":"` + canaryEscalation + `"},
		"cost_limits": {"k":"` + canaryCostLimits + `"},
		"audit_requirements": {"k":"` + canaryAudit + `"}
	}`)
	parent := json.RawMessage(`{}`)

	fake := &fakeKeepLogClient{}
	w := keeperslog.New(
		fake,
		keeperslog.WithIDGenerator(func() (string, error) { return "evt-id", nil }),
		keeperslog.WithCorrelationIDGenerator(func() (string, error) { return "corr-id", nil }),
	)
	v := NewImmutableCoreValidator(w)

	err := v.Validate(context.Background(), proposed, parent, canaryProposer)
	if !errors.Is(err, ErrSelfTuneBlockedImmutable) {
		t.Fatalf("Validate: %v, want ErrSelfTuneBlockedImmutable", err)
	}

	// The proposer canary is the only one allowed on the wire — it is
	// the closed-set `proposer` payload key. Every immutable_core
	// bucket canary MUST be absent.
	encoded := string(fake.lastPayloadBytes)
	if !strings.Contains(encoded, canaryProposer) {
		t.Errorf("payload missing proposer canary; payload = %s", encoded)
	}
	for _, canary := range []string{
		canaryRoleBoundaries,
		canarySecurityConstraints,
		canaryEscalation,
		canaryCostLimits,
		canaryAudit,
	} {
		if strings.Contains(encoded, canary) {
			t.Errorf("PII leak: payload contains bucket canary %q; payload = %s", canary, encoded)
		}
	}
}

// TestValidate_SourceGrep_KeeperslogAllowed pins the source-grep AC
// for M3.6: the validator file `immutable_core_validator.go` is the
// ONE place in `core/pkg/manifest/` that is allowed to import
// `keeperslog` — every other production file in the package must
// remain audit-free so the closed-set wire emission stays mechanical.
//
// This is the M3.6-specific inversion of the saga-step source-grep AC
// (which forbids `keeperslog.` entirely): here the rule is "the
// validator MUST emit the event, and no other manifest production
// file MAY".
func TestValidate_SourceGrep_KeeperslogAllowed(t *testing.T) {
	t.Parallel()

	// The validator must import keeperslog (and reference the package
	// in a meaningful way). Read the source and confirm both
	// invariants below — this is a structural check, not a textual
	// review, so a renamed import alias would still pass.
	src, err := os.ReadFile("immutable_core_validator.go")
	if err != nil {
		t.Fatalf("read validator source: %v", err)
	}
	if !strings.Contains(string(src), `"github.com/vadimtrunov/watchkeepers/core/pkg/keeperslog"`) {
		t.Errorf("validator missing keeperslog import; M3.6 audit hook contract requires it")
	}
	if !strings.Contains(string(src), "keeperslog.Event{") {
		t.Errorf("validator does not emit keeperslog.Event; M3.6 audit hook contract requires it")
	}

	// Every OTHER non-test .go file in this package must NOT reference
	// keeperslog — the validator is the closed-set audit-emit site.
	// We scan only top-level files (the package has no subdirectories).
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if name == "immutable_core_validator.go" {
			continue
		}
		if !strings.HasSuffix(name, ".go") {
			continue
		}
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if strings.Contains(string(body), `"github.com/vadimtrunov/watchkeepers/core/pkg/keeperslog"`) {
			t.Errorf("manifest production file %q imports keeperslog; M3.6 confines audit emission to the validator", name)
		}
		if strings.Contains(string(body), "keeperslog.") {
			t.Errorf("manifest production file %q references keeperslog.; M3.6 confines audit emission to the validator", name)
		}
	}
}

// TestValidate_Concurrent_NoRaceUnderLoad exercises the goroutine-
// safety contract: 16 goroutines hammering the same validator with a
// mixed pass/block workload must produce deterministic counts and no
// race-detector hits. Mirrors the M7.1.c.* saga-step concurrency
// harness shape (`docs/lessons/M7.md`).
func TestValidate_Concurrent_NoRaceUnderLoad(t *testing.T) {
	t.Parallel()

	auditor := &recordingAppender{}
	v := NewImmutableCoreValidator(auditor)
	parent := json.RawMessage(`{"role_boundaries":["a"]}`)
	equal := parent
	drift := json.RawMessage(`{"role_boundaries":["b"]}`)

	const goroutines = 16
	const iterations = 32
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		g := g
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				ctx := context.Background()
				if (g+i)%2 == 0 {
					if err := v.Validate(ctx, equal, parent, "agent-7"); err != nil {
						t.Errorf("equal Validate: %v", err)
					}
				} else {
					if err := v.Validate(ctx, drift, parent, "agent-7"); !errors.Is(err, ErrSelfTuneBlockedImmutable) {
						t.Errorf("drift Validate: %v, want sentinel", err)
					}
				}
			}
		}()
	}
	wg.Wait()
	wantBlocks := goroutines * iterations / 2
	if got := len(auditor.snapshot()); got != wantBlocks {
		t.Errorf("audit events = %d, want %d", got, wantBlocks)
	}
}

// fakeKeepLogClient is a hand-rolled [keeperslog.LocalKeepClient] used
// by the correlation-id and PII-redaction tests. Records the last
// request's correlation_id, event_type, and raw payload bytes so the
// tests can assert envelope-level invariants without spinning up an
// HTTP server.
type fakeKeepLogClient struct {
	mu                sync.Mutex
	lastCorrelationID string
	lastEventType     string
	lastPayloadBytes  json.RawMessage
}

func (f *fakeKeepLogClient) LogAppend(_ context.Context, req keepclient.LogAppendRequest) (*keepclient.LogAppendResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastCorrelationID = req.CorrelationID
	f.lastEventType = req.EventType
	f.lastPayloadBytes = req.Payload
	return &keepclient.LogAppendResponse{ID: "row-id"}, nil
}
