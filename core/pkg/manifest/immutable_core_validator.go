// immutable_core_validator.go ships the Phase 2 §M3.6 self-tuning
// validator surface: the public seam M2's constrained-prompt self-tuner
// will call BEFORE a manifest proposal reaches the approval card.
//
// Layering recap (top-of-stack to bottom):
//
//  1. M3.6 — this file. In-process gate that rejects a self-tune
//     PROPOSAL whose `immutable_core` differs from its parent. Runs
//     before the proposal is persisted, before the approval card is
//     posted to Slack, and BEFORE the M3.2 handler-side parity gate
//     (the M3.2 gate is the defense-in-depth backstop for any code
//     path that bypasses this validator).
//  2. M3.2 — `core/internal/keep/server/handlers_write.go`'s
//     `checkImmutableCoreParity` SQL gate. Same invariant enforced on
//     the keep write path; M3.2 is the last-mile guarantee.
//  3. M3.1 — `core/pkg/manifest/loader.go`'s `decodeImmutableCore` +
//     `runtime.Manifest.ImmutableCore`. Schema-only projection.
//
// Comparison semantics: the validator works on the raw `json.RawMessage`
// bytes (the keepclient wire surface) and treats two payloads as equal
// when their canonical JSON encodings (recursively sorted-keys,
// `json.Marshal` of a `map[string]any` round-trip) are byte-identical.
// This matches Postgres' `jsonb IS DISTINCT FROM` semantics that M3.2
// uses at the SQL layer: key order in the wire payload does NOT trip
// the gate; only structural value differences do. nil / empty / `null`
// RawMessage round-trip as equivalent — a parent with no immutable_core
// declared yet (legacy row predating M3.1) accepts a proposal that
// also omits the field, but flips to "blocked" the moment either side
// has a non-empty payload that the other does not.
//
// Audit discipline: when the validator rejects a proposal it emits a
// single `self_tune_blocked_immutable` keepers_log row carrying the
// proposer, the target field name (or comma-joined list when multiple
// buckets drift), and the correlation_id stamped on the request ctx.
// The event is the ONE keeperslog emission allowed inside the
// `core/pkg/manifest` package; the source-grep AC in
// `immutable_core_validator_test.go` pins that exception.
//
// PII discipline: the payload carries ONLY metadata —
// `event_type=self_tune_blocked_immutable` plus `proposer`,
// `target_field`, and the keeperslog envelope's correlation_id. The
// raw immutable_core jsonb (which may name internal escalation routes,
// cost caps, audit retention policies) is NEVER emitted; the auditor
// only needs to know WHICH bucket the self-tuner tried to mutate and
// who proposed it, not the proposed value.
package manifest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keeperslog"
)

// EventTypeSelfTuneBlockedImmutable is the closed-set keepers_log
// event_type the M3.6 validator emits on every rejection. Hoisted to a
// package constant so a future re-key is a one-line change here that
// downstream consumers (M2 self-tuning audit dashboard, M3.4
// `manifest.history` introspection) pick up via the compiler.
//
// The wire value uses the `self_tune_` prefix mirroring the project's
// `<noun>_<verb>` past-tense event-type convention (see
// `core/pkg/keeperslog/writer.go`'s godoc).
const EventTypeSelfTuneBlockedImmutable = "self_tune_blocked_immutable"

// SelfTunePayloadKey* are the closed-set payload keys the M3.6 rejection
// event carries. Hoisted to constants so a typo in one emit site cannot
// drift from the test's payload-key assertions. The keepers_log envelope
// (event_id, timestamp, correlation_id, trace_id, span_id) is supplied
// by the keeperslog.Writer; these are the only domain-specific keys this
// validator stamps.
const (
	selfTunePayloadKeyProposer    = "proposer"
	selfTunePayloadKeyTargetField = "target_field"
)

// ErrSelfTuneBlockedImmutable is the typed sentinel
// [ValidateImmutableCoreUnchanged] returns when a self-tuning proposal
// touches one or more immutable_core buckets. M2's self-tuner is
// expected to recognise the sentinel via `errors.Is` and surface a
// user-facing "this field cannot be self-tuned" message rather than
// routing the proposal to the approval card.
//
// The sentinel is exported (mirrors the `keepclient.ErrNotFound`
// precedent) because the call site lives in a different package; the
// sentinel is the only stable cross-package identifier for the rejection.
//
// Pattern: re-use this sentinel rather than introducing per-bucket
// sentinels — the audit hook stamps `target_field` so the caller can
// recover the bucket name from the keepers_log row when needed.
var ErrSelfTuneBlockedImmutable = errors.New("manifest: self-tune blocked: immutable_core fields are not self-tunable")

// Appender is the minimal subset of [keeperslog.Writer] the validator's
// audit hook consumes — only the [keeperslog.Writer.Append] method is
// touched. Defined as an interface in this package so unit tests can
// substitute a hand-rolled fake that asserts the audit-row contract
// directly, and so production code never depends on the concrete
// *keeperslog.Writer type at all (mirrors the
// `core/pkg/llm/cost.Appender` import-cycle-break pattern documented
// in `docs/LESSONS.md`).
//
// `*keeperslog.Writer` satisfies this interface as-is; the compile-
// time assertion lives in `immutable_core_validator_test.go`.
type Appender interface {
	Append(ctx context.Context, evt keeperslog.Event) (string, error)
}

// ImmutableCoreValidator is the M3.6 self-tuning gate. Construct via
// [NewImmutableCoreValidator]; the zero value is not usable.
//
// Methods are safe for concurrent use across goroutines: the validator
// holds only the immutable [Appender] reference after construction;
// per-call state lives on the goroutine stack. Mirrors the
// `cost.LoggingProvider` and `keeperslog.Writer` concurrency posture.
type ImmutableCoreValidator struct {
	auditor Appender
}

// NewImmutableCoreValidator constructs a validator with the supplied
// audit sink. A nil `auditor` is allowed and means "validate but do not
// emit a keepers_log row on rejection" — useful for code paths that
// already own the audit emission (e.g. an M2 self-tuner that batches
// its own rejection audit). Production callers should pass a real
// [keeperslog.Writer] so the validator becomes the single audit-emit
// site M3.6 calls for. Nil is NOT a programmer error here (unlike
// [keeperslog.New]'s nil-client panic) because the validator's primary
// contract is the gate decision, not the audit emission.
func NewImmutableCoreValidator(auditor Appender) *ImmutableCoreValidator {
	return &ImmutableCoreValidator{auditor: auditor}
}

// ValidateImmutableCoreUnchanged is the package-level convenience that
// constructs a no-audit validator and runs the gate once. Suitable for
// call sites that already own their audit emission and only need the
// pure boolean gate decision via the [ErrSelfTuneBlockedImmutable]
// sentinel. Mirrors the `manifest.LoadManifest` package-level
// convenience that wraps the loader.
//
// `proposer` MAY be empty when the caller does not yet know who
// proposed the change — the gate decision is independent of the
// proposer label; the field is only used when the audit hook fires
// (and a no-audit caller does not need it). Empty `proposer` round-
// trips through the per-call validator without an audit emission.
func ValidateImmutableCoreUnchanged(ctx context.Context, proposed, parent json.RawMessage, proposer string) error {
	return NewImmutableCoreValidator(nil).Validate(ctx, proposed, parent, proposer)
}

// Validate is the validator's single method. It compares the proposed
// and parent `immutable_core` payloads under canonical-JSON equality
// and returns [ErrSelfTuneBlockedImmutable] on a structural mismatch.
//
// Resolution order:
//
//  1. Canonicalise both sides (`canonicaliseImmutableCore`). Empty /
//     nil / `null` collapse to a single canonical-empty form so the
//     M3.1 "legacy row predating immutable_core" case round-trips
//     cleanly when the proposal also omits the field.
//  2. `bytes.Equal` on the canonicalised forms. Equal ⇒ return nil.
//  3. Mismatch ⇒ derive the comma-joined `target_field` from the diff
//     (`diffImmutableCoreBuckets`) and emit the
//     `self_tune_blocked_immutable` keepers_log row when an [Appender]
//     is configured. The audit-emit error is logged via the wrapped
//     keepers_log error chain but does NOT mask the
//     [ErrSelfTuneBlockedImmutable] sentinel — callers `errors.Is` the
//     sentinel and route deterministically regardless of audit state.
//  4. Return [ErrSelfTuneBlockedImmutable].
//
// Canonicalisation failure (malformed JSON on either side) is a
// distinct error path: the validator returns the wrapped json.Unmarshal
// error rather than the sentinel, so a caller can distinguish "you
// shipped junk" from "you touched a forbidden field". No audit row is
// emitted on the canonicalisation-failure path — the proposer is not
// blocked by a policy decision but by a parse error, which the M2
// self-tuner should treat as a programmer bug.
func (v *ImmutableCoreValidator) Validate(
	ctx context.Context,
	proposed, parent json.RawMessage,
	proposer string,
) error {
	proposedCanon, proposedMap, err := canonicaliseImmutableCore(proposed)
	if err != nil {
		return fmt.Errorf("manifest: validate immutable_core: proposed: %w", err)
	}
	parentCanon, parentMap, err := canonicaliseImmutableCore(parent)
	if err != nil {
		return fmt.Errorf("manifest: validate immutable_core: parent: %w", err)
	}

	if bytes.Equal(proposedCanon, parentCanon) {
		return nil
	}

	targetField := diffImmutableCoreBuckets(proposedMap, parentMap)
	v.emitBlockedAudit(ctx, proposer, targetField)
	return ErrSelfTuneBlockedImmutable
}

// emitBlockedAudit fires a single `self_tune_blocked_immutable`
// keepers_log row when an [Appender] is configured. The payload carries
// ONLY the closed-set keys (proposer + target_field) plus the
// keepers_log envelope (event_id, correlation_id, trace_id, span_id,
// timestamp); the raw immutable_core jsonb is never emitted to honour
// the PII discipline documented on the file godoc.
//
// Audit-emit failures are silently swallowed (no return value) because
// the validator's primary contract is the gate decision via
// [ErrSelfTuneBlockedImmutable]. A keepers_log outage MUST NOT mask the
// "you touched immutable_core" signal to the proposer; the validator
// trades observability for availability here, matching the
// `cost.LoggingProvider` audit-failure precedent (the decorator wraps
// `LogAppend` errors via the optional logger but never propagates them
// to the LLM caller). A future M2 audit dashboard that needs strict
// audit-or-fail semantics should wrap [Appender] with a fail-closed
// decorator at construction time.
func (v *ImmutableCoreValidator) emitBlockedAudit(ctx context.Context, proposer, targetField string) {
	if v.auditor == nil {
		return
	}
	payload := map[string]any{
		selfTunePayloadKeyProposer:    proposer,
		selfTunePayloadKeyTargetField: targetField,
	}
	_, _ = v.auditor.Append(ctx, keeperslog.Event{
		EventType: EventTypeSelfTuneBlockedImmutable,
		Payload:   payload,
	})
}

// canonicaliseImmutableCore decodes the raw `immutable_core` payload
// (typically the wire `keepclient.ManifestVersion.ImmutableCore`) into
// a deterministic canonical form suitable for byte-identical compare
// across the two sides of the gate.
//
// Returns the canonical bytes AND the decoded top-level
// `map[string]json.RawMessage` so the caller can compute the
// per-bucket diff without re-parsing the JSON.
//
// Semantics:
//
//   - Empty input (nil RawMessage, zero length, the literal `null`)
//     canonicalises to the literal `null` bytes. This collapses every
//     "no immutable_core declared" representation onto a single
//     canonical form so a legacy parent vs an omitting proposer pair
//     round-trips as equal.
//   - Non-object top-level (arrays, scalars, malformed JSON) returns
//     a wrapped json.Unmarshal error. The M3.1 schema-only milestone
//     stores top-level objects only (the SQL CHECK
//     `manifest_version_immutable_core_shape` enforces it on the
//     write path), so a non-object on either side is a defense-in-
//     depth failure that the validator surfaces explicitly rather
//     than silently round-tripping.
//   - Object top-level canonicalises via `json.Marshal` of an
//     `any` decode tree. Go's encoding/json sorts map keys
//     alphabetically at marshal time, so the canonical form is
//     deterministic regardless of wire-side key order — the same
//     invariant Postgres' `jsonb` storage relies on. Nested objects
//     are sorted recursively by the standard library; nested arrays
//     preserve element order (semantically meaningful, e.g.
//     `role_boundaries: ["a","b"]` differs from `["b","a"]`).
func canonicaliseImmutableCore(raw json.RawMessage) ([]byte, map[string]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return []byte("null"), nil, nil
	}
	// First pass: decode into map[string]json.RawMessage so we can
	// surface the per-bucket diff in the audit hook without re-parsing.
	// Rejects non-object top-level (arrays, scalars) via the
	// json.Unmarshal type-error path.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil {
		return nil, nil, err
	}
	// Second pass: canonical-bytes via `any` round-trip. encoding/json
	// sorts map keys at marshal time, so the output is deterministic.
	var tree any
	if err := json.Unmarshal(trimmed, &tree); err != nil {
		return nil, nil, err
	}
	canon, err := json.Marshal(tree)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal canonical: %w", err)
	}
	return canon, fields, nil
}

// diffImmutableCoreBuckets enumerates the bucket-level keys that differ
// between the proposed and parent canonical forms. Returns a comma-
// joined list (sorted ascending) suitable for the audit row's
// `target_field` payload key. Each key is either:
//
//   - present on one side and absent on the other (added or removed
//     bucket — typically a wire payload that adds a new key or drops
//     an existing one), OR
//   - present on both sides with structurally different canonical
//     payloads (a self-tuner that mutated a bucket's value).
//
// Two empty / nil maps return an empty string. The caller's contract
// is "this is only called after `bytes.Equal(proposedCanon, parentCanon)`
// returned false", so an empty return there would indicate a logic bug;
// the audit hook tolerates an empty target_field anyway (emitting
// `target_field: ""` with the correlation id is still useful for the
// auditor to recognise "validator rejected this proposal even though
// the bucket-level diff was empty — investigate").
//
// Per-bucket comparison uses canonical re-encoding of each
// RawMessage value so two payloads that differ only in key order
// inside a bucket (e.g. `{"a":1,"b":2}` vs `{"b":2,"a":1}`) do not
// falsely flag the bucket as drifted. Mirrors the top-level
// canonicalisation strategy.
func diffImmutableCoreBuckets(proposed, parent map[string]json.RawMessage) string {
	// Collect the union of keys so an added / removed bucket appears
	// in the diff. Allocating an extra map is cheaper than two-pass
	// scans for the typical 5-bucket payload.
	keys := make(map[string]struct{}, len(proposed)+len(parent))
	for k := range proposed {
		keys[k] = struct{}{}
	}
	for k := range parent {
		keys[k] = struct{}{}
	}
	diffed := make([]string, 0, len(keys))
	for k := range keys {
		if !bucketEqual(proposed[k], parent[k]) {
			diffed = append(diffed, k)
		}
	}
	sort.Strings(diffed)
	// Manual join to avoid allocating a separate strings package
	// dependency for one call site. The slice is short (≤6 entries in
	// practice for the M3.1 5-bucket schema + 1 forward-compat bucket).
	var out bytes.Buffer
	for i, k := range diffed {
		if i > 0 {
			out.WriteByte(',')
		}
		out.WriteString(k)
	}
	return out.String()
}

// bucketEqual reports whether two per-bucket RawMessage payloads are
// structurally identical under canonical-JSON equality. Both-empty
// returns true (a bucket absent on both sides is not a diff). One-empty
// returns false (a bucket added or removed IS a diff). Non-empty values
// are canonicalised via the `any` round-trip and compared byte-wise.
//
// Canonicalisation errors are treated as "different" — if either side
// shipped a payload that does not parse as JSON, the conservative
// posture is to flag the bucket as drifted so the proposer notices.
// The top-level [canonicaliseImmutableCore] already short-circuits on
// a malformed wire payload, so this path only fires on a malformed
// inner-bucket payload (which the M3.1 tolerant-decode contract
// already allows to round-trip through `runtime.ImmutableCore.Extra`).
func bucketEqual(a, b json.RawMessage) bool {
	aEmpty := isEmptyRawMessage(a)
	bEmpty := isEmptyRawMessage(b)
	if aEmpty && bEmpty {
		return true
	}
	if aEmpty != bEmpty {
		return false
	}
	aCanon, errA := canonicaliseRaw(a)
	bCanon, errB := canonicaliseRaw(b)
	if errA != nil || errB != nil {
		return false
	}
	return bytes.Equal(aCanon, bCanon)
}

// isEmptyRawMessage reports whether the wire RawMessage is one of the
// "no payload" sentinels: nil, zero length after whitespace trim, or
// the JSON literal `null`. Mirrors the canonicalisation contract for
// the top-level payload so the empty-on-both-sides case is consistent
// across the bucket and top-level paths.
func isEmptyRawMessage(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

// canonicaliseRaw is the per-bucket canonicaliser used by
// [bucketEqual]. Re-encodes the bucket payload via the `any` round-
// trip so two payloads differing only in key order canonicalise to
// the same bytes. Mirrors the top-level
// [canonicaliseImmutableCore] strategy at the bucket level.
func canonicaliseRaw(raw json.RawMessage) ([]byte, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}
