package approval

import (
	"errors"
	"testing"
)

func TestTargetSource_ValidateHappyPath(t *testing.T) {
	t.Parallel()
	cases := []TargetSource{TargetSourcePlatform, TargetSourcePrivate}
	for _, ts := range cases {
		if err := ts.Validate(); err != nil {
			t.Errorf("Validate(%q): unexpected err %v", ts, err)
		}
	}
}

func TestTargetSource_ValidateRejectsEmpty(t *testing.T) {
	t.Parallel()
	if err := TargetSource("").Validate(); !errors.Is(err, ErrInvalidTargetSource) {
		t.Errorf("expected ErrInvalidTargetSource for empty, got %v", err)
	}
}

// TestTargetSource_RejectsLocal pins the explicit roadmap invariant
// "local source never offered to the agent": the [TargetSource]
// enum has no `TargetSourceLocal` constant and the validator must
// refuse the literal string `"local"`.
func TestTargetSource_RejectsLocal(t *testing.T) {
	t.Parallel()
	if err := TargetSource("local").Validate(); !errors.Is(err, ErrInvalidTargetSource) {
		t.Errorf("expected ErrInvalidTargetSource for %q, got %v", "local", err)
	}
}

func TestTargetSource_ValidateRejectsUnknown(t *testing.T) {
	t.Parallel()
	cases := []TargetSource{"hosted", "Platform", " platform", "private "}
	for _, ts := range cases {
		if err := ts.Validate(); !errors.Is(err, ErrInvalidTargetSource) {
			t.Errorf("Validate(%q): expected ErrInvalidTargetSource, got %v", ts, err)
		}
	}
}
