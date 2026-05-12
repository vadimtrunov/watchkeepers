// Doc-block at file head documenting the seam contract.
//
// resolution order: nil-dep check (panic) → input validation
// (ErrReviewerNilProposal) → ctx.Err pass-through → run each gate in
// declaration order (typecheck → undeclared-fs-net → vitest →
// capability-declaration) → compute heuristic risk score from gate
// results + capability count → mint correlation id (UUIDv7 fallback)
// → return ReviewResult to caller.
//
// audit discipline: the reviewer never imports `keeperslog` and never
// calls `.Append(` (see source-grep AC). The audit log entry for the
// review outcome lives in the M9.7 audit subscriber that observes the
// `tool_ai_review_passed` / `tool_ai_review_failed` topics — the
// reviewer itself returns the typed [ReviewResult] for the orchestrator
// to render onto the approval card; event emission is the
// orchestrator's responsibility, not the reviewer's. (M9.4.b ships
// the reviewer + result type; full subscriber wiring follows in M9.7.)
//
// PII discipline: the reviewer ingests [Proposal.Input.CodeDraft] for
// the typecheck + lint gates, but the returned [GateResult] entries
// carry only short bounded-length detail strings (e.g.
// "calls fs.* without filesystem capability") — never the raw code,
// never the `Purpose` body. The optional [Logger] is invoked with
// the proposal id + per-gate name + severity ONLY.
//
// Gate-implementation scope: M9.4.b ships heuristic STUB gates that
// inspect the agent-supplied code as a string with simple substring
// scans. The real CI-grade implementations live in M9.4.d (shared
// workflow template running tsc / eslint / vitest). The stub gates
// give the operator a deterministic, hermetic preview at approval
// time while the real CI runs out-of-band; both flows agree on the
// [GateName] vocabulary so the eventual M9.4.d implementation slots in
// without churning the [CardInput] shape.

package approval

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// GateName identifies one of the four M9.4.b gates. The vocabulary is
// pinned across gate / card / event surfaces; the M9.4.d real CI
// implementation will populate the same names. Closed-set discipline
// from M9.1.a Pattern 7.
type GateName string

const (
	// GateTypecheck heuristically verifies the agent-supplied source
	// has a TS-shaped surface: at least one `export` declaration AND
	// no `eval(` invocations AND non-empty after trimming. The real
	// M9.4.d gate runs `tsc --noEmit`.
	GateTypecheck GateName = "typecheck"

	// GateUndeclaredFSNet flags `fs`/`http`/`fetch` usage when the
	// proposal does not declare the matching capability id. The
	// stub matches a small set of substrings; the M9.4.d gate uses
	// AST-grep / eslint with project-wide rules.
	GateUndeclaredFSNet GateName = "undeclared_fs_net"

	// GateVitest flags absence of `describe(` / `test(` / `it(`
	// blocks in the source — a vitest stub adapter expects each
	// proposed tool to ship at least one test. The real M9.4.d
	// gate runs `vitest --coverage` and enforces a per-tool coverage
	// floor.
	GateVitest GateName = "vitest"

	// GateCapabilityDeclaration validates the per-entry shape of
	// [ProposalInput.Capabilities] (kebab-or-snake style, ≤
	// [MaxCapabilityIDLength] bytes). [ProposalInput.Validate]
	// already enforces the basic shape; this gate adds the
	// downstream-grammar check used by M9.3's dictionary loader.
	GateCapabilityDeclaration GateName = "capability_declaration"
)

// Severity is the per-gate outcome. A `fail` aborts approval at the
// orchestrator boundary; a `warn` surfaces the issue on the approval
// card without blocking the lead.
type Severity string

const (
	// SeverityPass indicates the gate accepted the input.
	SeverityPass Severity = "pass"

	// SeverityWarn indicates the gate flagged a concern but did not
	// reject the input. Renders on the approval card as a yellow
	// marker; the lead may still click `[Approve]`.
	SeverityWarn Severity = "warn"

	// SeverityFail indicates the gate rejected the input. Renders on
	// the approval card as a red marker; the lead SHOULD click
	// `[Reject]` (clicking `[Approve]` is operator-allowed but
	// surfaces on the audit row as "approved despite fail").
	SeverityFail Severity = "fail"
)

// RiskLevel summarises the [ReviewResult.Gates] outcomes into a
// single approval-card badge. The heuristic is deterministic so
// repeated reviews of the same proposal render the same level.
type RiskLevel string

const (
	// RiskLow indicates every gate passed AND the capability count is
	// ≤ [riskLowCapabilityThreshold]. The card shows a green badge.
	RiskLow RiskLevel = "low"

	// RiskMedium indicates at least one [SeverityWarn] OR the
	// capability count is > [riskLowCapabilityThreshold]. The card
	// shows a yellow badge.
	RiskMedium RiskLevel = "medium"

	// RiskHigh indicates at least one [SeverityFail]. The card shows
	// a red badge. The lead's `[Approve]` click is recorded on the
	// audit row as "approved despite fail".
	RiskHigh RiskLevel = "high"
)

// riskLowCapabilityThreshold is the inclusive upper bound on the
// number of capabilities a [RiskLow] tool may declare. Above this, a
// tool is "broad-surface" by definition; render at [RiskMedium] at
// minimum.
const riskLowCapabilityThreshold = 5

// MaxGateDetailLength bounds the [GateResult.Detail] string. The
// detail is rendered on the approval card; bounded length keeps the
// card under Slack's ~3000-char per-section limit even when all four
// gates fail.
const MaxGateDetailLength = 256

// Heuristic-substring regexes consumed by [gateTypecheck] /
// [gateUndeclaredFSNet]. Hoisted as package-level vars so the regex
// compile happens once; the patterns are deliberately permissive
// because the real M9.4.d gates do the rigorous AST parse.
var (
	tsExportPattern    = regexp.MustCompile(`(?m)^\s*export\s+`)
	tsEvalPattern      = regexp.MustCompile(`\beval\s*\(`)
	vitestPattern      = regexp.MustCompile(`\b(describe|test|it)\s*\(`)
	capabilityIDFormat = regexp.MustCompile(`^[a-z][a-z0-9_:.-]*$`)
)

// fsNetPattern names a substring that suggests fs / network access.
// The slice is iterated in order; the first match decides the gate
// outcome. Each entry pairs a needle with the capability id the
// proposal MUST declare to suppress the failure.
var fsNetPattern = []struct {
	Needle     string
	Capability string
}{
	{Needle: `from "fs"`, Capability: "filesystem:read"},
	{Needle: `from 'fs'`, Capability: "filesystem:read"},
	{Needle: `from "node:fs"`, Capability: "filesystem:read"},
	{Needle: `from 'node:fs'`, Capability: "filesystem:read"},
	{Needle: `require("fs")`, Capability: "filesystem:read"},
	{Needle: `require('fs')`, Capability: "filesystem:read"},
	{Needle: `import("fs")`, Capability: "filesystem:read"},
	{Needle: `import('fs')`, Capability: "filesystem:read"},
	{Needle: `fetch(`, Capability: "network:http"},
	{Needle: `from "http"`, Capability: "network:http"},
	{Needle: `from 'http'`, Capability: "network:http"},
	{Needle: `from "node:http"`, Capability: "network:http"},
	{Needle: `from 'node:http'`, Capability: "network:http"},
}

// GateResult is the per-gate outcome the reviewer returns. The
// `Detail` string is bounded by [MaxGateDetailLength] so the approval
// card stays readable even when every gate fails.
type GateResult struct {
	// Name identifies which gate produced the result. The
	// [GateName] vocabulary is closed.
	Name GateName

	// Severity is the per-gate outcome.
	Severity Severity

	// Detail is a short human-readable description of WHY the gate
	// produced this severity. Bounded to [MaxGateDetailLength] bytes
	// at construction time; never carries raw source code. May be
	// empty on [SeverityPass].
	Detail string
}

// ReviewResult is the aggregate outcome the reviewer returns. The
// orchestrator (M9.4.b approval-card renderer) consumes this struct
// verbatim; the M9.7 audit subscriber observes it through the future
// `tool_ai_review_passed` / `tool_ai_review_failed` events.
type ReviewResult struct {
	// ProposalID matches [Proposal.ID]. The reviewer never mutates
	// the proposal; the id is carried for downstream subscribers
	// that join the review back to the proposal store.
	ProposalID uuid.UUID

	// ToolName mirrors [Proposal.Input.Name].
	ToolName string

	// Gates is the per-gate outcome in declaration order
	// (typecheck → undeclared-fs-net → vitest →
	// capability-declaration). The slice is defensively-deep-copied
	// at return time so caller-side mutation does not corrupt the
	// reviewer's internal state.
	Gates []GateResult

	// Risk is the heuristic level computed from `Gates` +
	// `len(Proposal.Input.Capabilities)`.
	Risk RiskLevel

	// ReviewedAt is the wall-clock timestamp captured from the
	// configured [Clock] at review time.
	ReviewedAt time.Time

	// CorrelationID is the canonical string form of [Proposal.ID]
	// when available; otherwise a fresh UUIDv7 minted by
	// [ReviewerDeps.IDGenerator]. Mirrors
	// [Proposal.CorrelationID]'s discipline.
	CorrelationID string
}

// ReviewerDeps bundles the required dependencies for [NewReviewer].
// All non-`Logger` fields are required; nil values panic in
// [NewReviewer] with a named-field message.
type ReviewerDeps struct {
	// Clock stamps [ReviewResult.ReviewedAt]. Required.
	Clock Clock

	// IDGenerator mints the correlation-id fallback. Required.
	IDGenerator IDGenerator

	// Logger receives per-gate diagnostic entries. Optional; a nil
	// [Logger] silently discards entries.
	Logger Logger
}

// Reviewer is the M9.4.b in-process AI reviewer. Construct via
// [NewReviewer]; the zero value is not usable. The reviewer is safe
// for concurrent use across goroutines.
type Reviewer struct {
	deps ReviewerDeps
}

// NewReviewer constructs a [*Reviewer]. Panics with a named-field
// message when any required dependency in `deps` is nil; mirrors
// [New] / [NewWebhook].
func NewReviewer(deps ReviewerDeps) *Reviewer {
	if deps.Clock == nil {
		panic("approval: NewReviewer: deps.Clock must not be nil")
	}
	if deps.IDGenerator == nil {
		panic("approval: NewReviewer: deps.IDGenerator must not be nil")
	}
	return &Reviewer{deps: deps}
}

// Review runs the four M9.4.b stub gates against `proposal` and
// returns the aggregate [ReviewResult]. The resolution order is
// documented at the top of this file.
//
// The reviewer is pure-ish: no I/O, no eventbus emission, no audit
// side-effect. The orchestrator (or the test harness) decides whether
// to render the result onto an approval card, persist it, or emit an
// event.
func (r *Reviewer) Review(ctx context.Context, proposal Proposal) (ReviewResult, error) {
	if proposal.ID == uuid.Nil || strings.TrimSpace(proposal.Input.Name) == "" {
		return ReviewResult{}, ErrReviewerNilProposal
	}
	if err := ctx.Err(); err != nil {
		return ReviewResult{}, err
	}

	gates := make([]GateResult, 0, 4)
	gates = append(gates, gateTypecheck(proposal.Input.CodeDraft))
	gates = append(gates, gateUndeclaredFSNet(proposal.Input.CodeDraft, proposal.Input.Capabilities))
	gates = append(gates, gateVitest(proposal.Input.CodeDraft))
	gates = append(gates, gateCapabilityDeclaration(proposal.Input.Capabilities))

	risk := computeRisk(gates, len(proposal.Input.Capabilities))

	corrID, err := r.newCorrelationID(proposal)
	if err != nil {
		return ReviewResult{}, err
	}

	if r.deps.Logger != nil {
		for _, g := range gates {
			r.deps.Logger.Log(
				ctx, "approval: gate result",
				"proposal_id", proposal.ID,
				"gate", string(g.Name),
				"severity", string(g.Severity),
			)
		}
	}

	return ReviewResult{
		ProposalID:    proposal.ID,
		ToolName:      proposal.Input.Name,
		Gates:         gates,
		Risk:          risk,
		ReviewedAt:    r.deps.Clock.Now(),
		CorrelationID: corrID,
	}, nil
}

// newCorrelationID prefers the proposal's correlation id; falls back
// to a freshly-minted UUIDv7 when empty. Mirrors
// [Webhook.newCorrelationID].
func (r *Reviewer) newCorrelationID(p Proposal) (string, error) {
	if p.CorrelationID != "" {
		return p.CorrelationID, nil
	}
	id, err := r.deps.IDGenerator.NewUUID()
	if err != nil {
		return "", fmt.Errorf("approval: reviewer id generator: %w", err)
	}
	return id.String(), nil
}

// gateTypecheck heuristically validates the agent-supplied source
// against three minimal conditions: non-blank after trim, contains at
// least one `export` declaration, contains no raw `eval(`. The real
// M9.4.d gate runs `tsc --noEmit`; this stub catches the most common
// authoring mistakes without standing up a TS toolchain at runtime.
func gateTypecheck(code string) GateResult {
	if strings.TrimSpace(code) == "" {
		return mkGateResult(GateTypecheck, SeverityFail, "empty source")
	}
	if !tsExportPattern.MatchString(code) {
		return mkGateResult(GateTypecheck, SeverityFail, "no exported declarations")
	}
	if tsEvalPattern.MatchString(code) {
		return mkGateResult(GateTypecheck, SeverityFail, "eval() usage is disallowed")
	}
	return mkGateResult(GateTypecheck, SeverityPass, "")
}

// gateUndeclaredFSNet scans the code for fs / network references and
// flags any that the proposal does not declare. A scan-hit with a
// matching capability id is silently OK; a scan-hit without one is
// [SeverityFail].
func gateUndeclaredFSNet(code string, capabilities []string) GateResult {
	declared := make(map[string]struct{}, len(capabilities))
	for _, c := range capabilities {
		declared[c] = struct{}{}
	}
	for _, p := range fsNetPattern {
		if !strings.Contains(code, p.Needle) {
			continue
		}
		if _, ok := declared[p.Capability]; !ok {
			return mkGateResult(
				GateUndeclaredFSNet,
				SeverityFail,
				fmt.Sprintf("source references %q without declaring %q", p.Needle, p.Capability),
			)
		}
	}
	return mkGateResult(GateUndeclaredFSNet, SeverityPass, "")
}

// gateVitest flags absence of vitest blocks. The real M9.4.d gate
// runs `vitest --coverage`; this stub catches the case where the
// agent forgot to author tests at all. Severity is [SeverityWarn]
// (not [SeverityFail]) because a tool's first iteration may
// legitimately ship without tests when the lead intends to author
// them post-approval.
func gateVitest(code string) GateResult {
	if vitestPattern.MatchString(code) {
		return mkGateResult(GateVitest, SeverityPass, "")
	}
	return mkGateResult(GateVitest, SeverityWarn, "no describe()/test()/it() blocks detected")
}

// gateCapabilityDeclaration enforces the per-entry grammar
// [capabilityIDFormat] expects. The check is in addition to (not in
// place of) [ProposalInput.Validate]: the validator enforces shape +
// bound; this gate enforces the more specific grammar M9.3's
// dictionary loader will consume. The gate severity is
// [SeverityWarn] for off-grammar entries so an early-Phase-1 tool
// shipping an exotic capability id is not blocked at approval time.
func gateCapabilityDeclaration(capabilities []string) GateResult {
	for i, c := range capabilities {
		if !capabilityIDFormat.MatchString(c) {
			return mkGateResult(
				GateCapabilityDeclaration,
				SeverityWarn,
				fmt.Sprintf("capability[%d] %q does not match `^[a-z][a-z0-9_:.-]*$`", i, c),
			)
		}
	}
	return mkGateResult(GateCapabilityDeclaration, SeverityPass, "")
}

// computeRisk maps gate outcomes + capability count onto a
// [RiskLevel]. Deterministic so repeated reviews of the same proposal
// render the same level.
//
//   - Any [SeverityFail] → [RiskHigh].
//   - Otherwise, any [SeverityWarn] OR capCount > threshold → [RiskMedium].
//   - All pass + capCount ≤ threshold → [RiskLow].
func computeRisk(gates []GateResult, capCount int) RiskLevel {
	hasWarn := false
	for _, g := range gates {
		switch g.Severity {
		case SeverityFail:
			return RiskHigh
		case SeverityWarn:
			hasWarn = true
		}
	}
	if hasWarn || capCount > riskLowCapabilityThreshold {
		return RiskMedium
	}
	return RiskLow
}

// mkGateResult builds a [GateResult] with the detail trimmed to
// [MaxGateDetailLength] bytes. Centralised so the bound is enforced
// once per gate construction.
func mkGateResult(name GateName, sev Severity, detail string) GateResult {
	if len(detail) > MaxGateDetailLength {
		detail = detail[:MaxGateDetailLength]
	}
	return GateResult{Name: name, Severity: sev, Detail: detail}
}
