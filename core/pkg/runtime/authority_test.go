package runtime

import (
	"strings"
	"testing"
)

// TestRequiresApproval_TruthTable exercises the full Autonomy ×
// AuthorityMatrix decision matrix described in
// [RequiresApproval]'s godoc. Each row pins both the boolean and the
// reason-string contract: deny reasons are non-empty, allow reasons are
// the empty string so callers can short-circuit on `if required`.
func TestRequiresApproval_TruthTable(t *testing.T) {
	t.Parallel()

	const (
		supervisedReason = "supervised autonomy requires per-action approval"
	)

	cases := []struct {
		name           string
		autonomy       AutonomyLevel
		matrix         map[string]string
		action         string
		wantRequired   bool
		wantReason     string   // exact match when non-empty
		wantContains   []string // substrings when reason is templated
		wantReasonNone bool     // true when reason MUST be empty
	}{
		{
			name:         "empty Autonomy + nil matrix + any action",
			autonomy:     "",
			matrix:       nil,
			action:       "send_message",
			wantRequired: true,
			wantReason:   supervisedReason,
		},
		{
			name:         "empty Autonomy + populated matrix with self",
			autonomy:     "",
			matrix:       map[string]string{"send_message": "self"},
			action:       "send_message",
			wantRequired: true,
			wantReason:   supervisedReason,
		},
		{
			name:         "AutonomySupervised + any action",
			autonomy:     AutonomySupervised,
			matrix:       map[string]string{"send_message": "self"},
			action:       "send_message",
			wantRequired: true,
			wantReason:   supervisedReason,
		},
		{
			name:         "AutonomySupervised + nil matrix",
			autonomy:     AutonomySupervised,
			matrix:       nil,
			action:       "anything",
			wantRequired: true,
			wantReason:   supervisedReason,
		},
		{
			name:           "AutonomyAutonomous + nil matrix + any action",
			autonomy:       AutonomyAutonomous,
			matrix:         nil,
			action:         "send_message",
			wantRequired:   false,
			wantReasonNone: true,
		},
		{
			name:           "AutonomyAutonomous + action absent from matrix",
			autonomy:       AutonomyAutonomous,
			matrix:         map[string]string{"other_action": "lead"},
			action:         "send_message",
			wantRequired:   false,
			wantReasonNone: true,
		},
		{
			name:           "AutonomyAutonomous + action=self",
			autonomy:       AutonomyAutonomous,
			matrix:         map[string]string{"send_message": "self"},
			action:         "send_message",
			wantRequired:   false,
			wantReasonNone: true,
		},
		{
			name:           "AutonomyAutonomous + action=watchmaster",
			autonomy:       AutonomyAutonomous,
			matrix:         map[string]string{"adjust_personality": "watchmaster"},
			action:         "adjust_personality",
			wantRequired:   false,
			wantReasonNone: true,
		},
		{
			name:         "AutonomyAutonomous + action=lead",
			autonomy:     AutonomyAutonomous,
			matrix:       map[string]string{"adjust_personality": "lead"},
			action:       "adjust_personality",
			wantRequired: true,
			wantContains: []string{"lead", "adjust_personality", "authority matrix requires"},
		},
		{
			name:         "AutonomyAutonomous + action=operator",
			autonomy:     AutonomyAutonomous,
			matrix:       map[string]string{"deploy_release": "operator"},
			action:       "deploy_release",
			wantRequired: true,
			wantContains: []string{"operator", "deploy_release", "authority matrix requires"},
		},
		{
			name:         "AutonomyAutonomous + unknown value foo",
			autonomy:     AutonomyAutonomous,
			matrix:       map[string]string{"send_message": "foo"},
			action:       "send_message",
			wantRequired: true,
			wantContains: []string{`"foo"`, "failing closed", "unknown authority value"},
		},
		{
			name:         "AutonomyAutonomous + unknown value with quotes preserved",
			autonomy:     AutonomyAutonomous,
			matrix:       map[string]string{"send_message": "admin"},
			action:       "send_message",
			wantRequired: true,
			wantContains: []string{`"admin"`, "failing closed"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			m := Manifest{
				Autonomy:        tc.autonomy,
				AuthorityMatrix: tc.matrix,
			}
			gotRequired, gotReason := RequiresApproval(m, tc.action)

			if gotRequired != tc.wantRequired {
				t.Fatalf("RequiresApproval required = %v, want %v (reason=%q)",
					gotRequired, tc.wantRequired, gotReason)
			}

			if tc.wantReasonNone {
				if gotReason != "" {
					t.Fatalf("RequiresApproval reason = %q, want empty (allow branch)", gotReason)
				}
				return
			}

			if gotReason == "" {
				t.Fatalf("RequiresApproval reason empty, want non-empty for deny branch")
			}

			if tc.wantReason != "" && gotReason != tc.wantReason {
				t.Fatalf("RequiresApproval reason = %q, want %q", gotReason, tc.wantReason)
			}

			for _, sub := range tc.wantContains {
				if !strings.Contains(gotReason, sub) {
					t.Fatalf("RequiresApproval reason = %q, want substring %q", gotReason, sub)
				}
			}
		})
	}
}

// TestRequiresApproval_AllowReasonsAreEmpty pins the contract that the
// allow branch always returns the empty string so callers can write
// `required, reason := RequiresApproval(m, a); if required { log(reason) }`
// without an extra nil-check.
func TestRequiresApproval_AllowReasonsAreEmpty(t *testing.T) {
	t.Parallel()

	allows := []struct {
		name   string
		m      Manifest
		action string
	}{
		{
			name:   "autonomous + nil matrix",
			m:      Manifest{Autonomy: AutonomyAutonomous},
			action: "x",
		},
		{
			name: "autonomous + self",
			m: Manifest{
				Autonomy:        AutonomyAutonomous,
				AuthorityMatrix: map[string]string{"x": "self"},
			},
			action: "x",
		},
		{
			name: "autonomous + watchmaster",
			m: Manifest{
				Autonomy:        AutonomyAutonomous,
				AuthorityMatrix: map[string]string{"x": "watchmaster"},
			},
			action: "x",
		},
	}
	for _, tc := range allows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			required, reason := RequiresApproval(tc.m, tc.action)
			if required {
				t.Fatalf("required = true, want false")
			}
			if reason != "" {
				t.Fatalf("reason = %q, want empty", reason)
			}
		})
	}
}
