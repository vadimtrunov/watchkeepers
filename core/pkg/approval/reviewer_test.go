package approval

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func newTestReviewer() (*Reviewer, *fakeClock, *fakeIDGenerator, *fakeLogger) {
	clk := newFakeClock(time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC))
	idGen := &fakeIDGenerator{}
	logger := &fakeLogger{}
	r := NewReviewer(ReviewerDeps{Clock: clk, IDGenerator: idGen, Logger: logger})
	return r, clk, idGen, logger
}

func TestReviewer_New_PanicsOnNilDeps(t *testing.T) {
	mk := func(mutate func(*ReviewerDeps)) (panicked bool, msg string) {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
				msg = fmt.Sprintf("%v", r)
			}
		}()
		deps := ReviewerDeps{Clock: newFakeClock(time.Now()), IDGenerator: &fakeIDGenerator{}}
		mutate(&deps)
		_ = NewReviewer(deps)
		return
	}
	tests := []struct{ name, want string }{
		{"Clock", "Clock"},
		{"IDGenerator", "IDGenerator"},
	}
	mutators := map[string]func(*ReviewerDeps){
		"Clock":       func(d *ReviewerDeps) { d.Clock = nil },
		"IDGenerator": func(d *ReviewerDeps) { d.IDGenerator = nil },
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			panicked, msg := mk(mutators[tt.name])
			if !panicked {
				t.Fatalf("expected panic on nil %s", tt.name)
			}
			if !strings.Contains(msg, tt.want) {
				t.Errorf("panic msg: %s (want %s)", msg, tt.want)
			}
		})
	}
}

func TestReviewer_Review_ZeroProposalRejected(t *testing.T) {
	r, _, _, _ := newTestReviewer()
	_, err := r.Review(context.Background(), Proposal{})
	if !errors.Is(err, ErrReviewerNilProposal) {
		t.Errorf("want ErrReviewerNilProposal, got %v", err)
	}
}

func TestReviewer_Review_HappyPath_AllGatesPass(t *testing.T) {
	r, _, _, _ := newTestReviewer()
	p := newTestProposal()
	// Build a draft that passes every gate.
	p.Input.CodeDraft = "export const run = async () => 42;\n" +
		"import { describe, test } from 'vitest';\n" +
		"describe('run', () => { test('returns 42', () => {}); });\n"
	p.Input.Capabilities = []string{"github:read"}
	got, err := r.Review(context.Background(), p)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if len(got.Gates) != 4 {
		t.Fatalf("expected 4 gates, got %d", len(got.Gates))
	}
	for _, g := range got.Gates {
		if g.Severity != SeverityPass {
			t.Errorf("gate %s want pass, got %s (%s)", g.Name, g.Severity, g.Detail)
		}
	}
	if got.Risk != RiskLow {
		t.Errorf("Risk: want low got %s", got.Risk)
	}
	if got.CorrelationID != p.CorrelationID {
		t.Errorf("CorrelationID: want %s got %s", p.CorrelationID, got.CorrelationID)
	}
	if got.ProposalID != p.ID {
		t.Errorf("ProposalID mismatch")
	}
}

func TestReviewer_Review_TypecheckFailures(t *testing.T) {
	cases := []struct {
		name string
		code string
		want Severity
	}{
		{"empty", "", SeverityFail},
		{"whitespace only", "   \n\t", SeverityFail},
		{"no export", "const x = 1;", SeverityFail},
		{"eval usage", "export const x = eval('1');", SeverityFail},
	}
	r, _, _, _ := newTestReviewer()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newTestProposal()
			p.Input.CodeDraft = c.code
			got, err := r.Review(context.Background(), p)
			if err != nil {
				t.Fatalf("Review: %v", err)
			}
			if got.Gates[0].Name != GateTypecheck {
				t.Fatalf("first gate must be typecheck")
			}
			if got.Gates[0].Severity != c.want {
				t.Errorf("typecheck: want %s got %s (%s)", c.want, got.Gates[0].Severity, got.Gates[0].Detail)
			}
		})
	}
}

func TestReviewer_Review_UndeclaredFSNetFails(t *testing.T) {
	r, _, _, _ := newTestReviewer()
	p := newTestProposal()
	p.Input.CodeDraft = `import fs from "fs";` + "\n" + `export const run = () => fs.readFileSync("/etc/passwd");`
	p.Input.Capabilities = []string{"github:read"} // does not declare filesystem:read
	got, err := r.Review(context.Background(), p)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if got.Gates[1].Name != GateUndeclaredFSNet {
		t.Fatalf("second gate must be undeclared_fs_net")
	}
	if got.Gates[1].Severity != SeverityFail {
		t.Errorf("undeclared_fs_net: want fail got %s", got.Gates[1].Severity)
	}
	if !strings.Contains(got.Gates[1].Detail, "filesystem:read") {
		t.Errorf("detail must name the required capability: %s", got.Gates[1].Detail)
	}
	if got.Risk != RiskHigh {
		t.Errorf("any fail must yield RiskHigh, got %s", got.Risk)
	}
}

func TestReviewer_Review_UndeclaredFSNetSuppressed_WhenDeclared(t *testing.T) {
	r, _, _, _ := newTestReviewer()
	p := newTestProposal()
	p.Input.CodeDraft = `import fs from "fs";` + "\n" +
		`import { describe, test } from 'vitest';` + "\n" +
		`describe('x', () => test('y', () => {}));` + "\n" +
		`export const run = () => fs.readFileSync("/etc/passwd");`
	p.Input.Capabilities = []string{"filesystem:read"}
	got, err := r.Review(context.Background(), p)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if got.Gates[1].Severity != SeverityPass {
		t.Errorf("declared fs cap should pass gate, got %s (%s)", got.Gates[1].Severity, got.Gates[1].Detail)
	}
}

func TestReviewer_Review_VitestMissing_Warns(t *testing.T) {
	r, _, _, _ := newTestReviewer()
	p := newTestProposal()
	p.Input.CodeDraft = "export const run = () => 1;"
	p.Input.Capabilities = []string{"github:read"}
	got, err := r.Review(context.Background(), p)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if got.Gates[2].Name != GateVitest {
		t.Fatalf("third gate must be vitest")
	}
	if got.Gates[2].Severity != SeverityWarn {
		t.Errorf("missing vitest must yield warn, got %s", got.Gates[2].Severity)
	}
}

func TestReviewer_Review_CapabilityDeclarationOffGrammar_Warns(t *testing.T) {
	r, _, _, _ := newTestReviewer()
	p := newTestProposal()
	p.Input.CodeDraft = "export const run = () => 1;\nimport {describe, test} from 'vitest';\ndescribe('x', () => test('y', () => {}));"
	p.Input.Capabilities = []string{"BadShape!"}
	got, err := r.Review(context.Background(), p)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if got.Gates[3].Name != GateCapabilityDeclaration {
		t.Fatalf("fourth gate must be capability_declaration")
	}
	if got.Gates[3].Severity != SeverityWarn {
		t.Errorf("off-grammar cap must yield warn, got %s", got.Gates[3].Severity)
	}
}

func TestReviewer_Review_Risk_HighOnAnyFail(t *testing.T) {
	r, _, _, _ := newTestReviewer()
	p := newTestProposal()
	p.Input.CodeDraft = "" // typecheck fails
	got, _ := r.Review(context.Background(), p)
	if got.Risk != RiskHigh {
		t.Errorf("any fail must yield RiskHigh, got %s", got.Risk)
	}
}

func TestReviewer_Review_Risk_MediumOnManyCaps(t *testing.T) {
	r, _, _, _ := newTestReviewer()
	p := newTestProposal()
	p.Input.CodeDraft = "export const run = () => 1;\nimport {describe, test} from 'vitest';\ndescribe('x', () => test('y', () => {}));"
	// 6 caps > threshold of 5; all gate pass.
	p.Input.Capabilities = []string{"a:b", "a:c", "a:d", "a:e", "a:f", "a:g"}
	got, _ := r.Review(context.Background(), p)
	if got.Risk != RiskMedium {
		t.Errorf("> threshold caps must yield RiskMedium, got %s", got.Risk)
	}
}

func TestReviewer_Review_CtxCancelledRefuses(t *testing.T) {
	r, _, _, _ := newTestReviewer()
	p := newTestProposal()
	p.Input.CodeDraft = "export const run = () => 1;"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := r.Review(ctx, p)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("want context.Canceled, got %v", err)
	}
}

func TestReviewer_Review_CorrelationIDFallback_OnEmptyProposalCorrelation(t *testing.T) {
	r, _, idGen, _ := newTestReviewer()
	idGen.next = uuid.UUID{}
	p := newTestProposal()
	p.CorrelationID = ""
	p.Input.CodeDraft = "export const run = () => 1;"
	got, err := r.Review(context.Background(), p)
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if got.CorrelationID == "" {
		t.Errorf("CorrelationID must be filled by IDGenerator fallback")
	}
}

func TestReviewer_Review_IDGeneratorError_OnFallback(t *testing.T) {
	clk := newFakeClock(time.Now())
	idGen := &fakeIDGenerator{err: errors.New("entropy starved")}
	r := NewReviewer(ReviewerDeps{Clock: clk, IDGenerator: idGen})
	p := newTestProposal()
	p.CorrelationID = ""
	p.Input.CodeDraft = "export const run = () => 1;"
	_, err := r.Review(context.Background(), p)
	if err == nil || !strings.Contains(err.Error(), "id generator") {
		t.Errorf("want id generator wrap, got %v", err)
	}
}

func TestReviewer_Review_GateDetailBounded(t *testing.T) {
	if MaxGateDetailLength <= 32 {
		t.Fatalf("MaxGateDetailLength sanity")
	}
	r, _, _, _ := newTestReviewer()
	p := newTestProposal()
	// Build a cap that, after format, produces detail > bound.
	longCap := strings.Repeat("x", MaxGateDetailLength)
	p.Input.CodeDraft = "export const run = () => 1;\nimport {describe, test} from 'vitest';\ndescribe('x', () => test('y', () => {}));"
	p.Input.Capabilities = []string{"BAD!" + longCap}
	got, _ := r.Review(context.Background(), p)
	for _, g := range got.Gates {
		if len(g.Detail) > MaxGateDetailLength {
			t.Errorf("gate %s detail exceeds bound: %d", g.Name, len(g.Detail))
		}
	}
}

func TestReviewer_Review_PIICanary_LoggerRedaction(t *testing.T) {
	const canaryPurpose = "CANARY_PURPOSE_PII_zzzzzz"
	const canaryDesc = "CANARY_DESC_PII_zzzzzz"
	const canaryCode = "CANARY_CODE_PII_zzzzzz"
	r, _, _, logger := newTestReviewer()
	p := newTestProposal()
	p.Input.Purpose = canaryPurpose
	p.Input.PlainLanguageDescription = canaryDesc
	p.Input.CodeDraft = canaryCode // not exported — will fail typecheck, which is fine
	_, _ = r.Review(context.Background(), p)

	for _, e := range logger.snapshot() {
		joined := e.msg
		for _, v := range e.kv {
			joined += "|" + asString(v)
		}
		for _, canary := range []string{canaryPurpose, canaryDesc, canaryCode} {
			if strings.Contains(joined, canary) {
				t.Errorf("logger entry leaked canary %q: %s", canary, joined)
			}
		}
	}
}

func TestReviewer_Review_GatesDeclarationOrderPinned(t *testing.T) {
	r, _, _, _ := newTestReviewer()
	p := newTestProposal()
	p.Input.CodeDraft = "export const run = () => 1;"
	got, _ := r.Review(context.Background(), p)
	want := []GateName{GateTypecheck, GateUndeclaredFSNet, GateVitest, GateCapabilityDeclaration}
	for i, w := range want {
		if got.Gates[i].Name != w {
			t.Errorf("gates[%d]: want %s got %s", i, w, got.Gates[i].Name)
		}
	}
}
