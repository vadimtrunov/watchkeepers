package approval

import (
	"errors"
	"strings"
	"testing"
)

func TestProposalInput_ValidateHappyPath(t *testing.T) {
	t.Parallel()
	if err := validInput().Validate(); err != nil {
		t.Errorf("validInput().Validate(): unexpected err %v", err)
	}
}

func TestProposalInput_ValidateMissingName(t *testing.T) {
	t.Parallel()
	cases := []string{"", "   ", "\t\n"}
	for _, name := range cases {
		in := validInput()
		in.Name = name
		if err := in.Validate(); !errors.Is(err, ErrMissingProposalName) {
			t.Errorf("Name=%q: expected ErrMissingProposalName, got %v", name, err)
		}
	}
}

func TestProposalInput_ValidateInvalidName(t *testing.T) {
	t.Parallel()
	cases := []string{
		"CountOpenPRs",   // PascalCase
		"count-open-prs", // kebab-case
		"count.open",     // dot
		"1count",         // leading digit
		"_count",         // leading underscore
		"count$open",     // special char
		"OpenPRs",        // uppercase
		"count open",     // space
	}
	for _, name := range cases {
		in := validInput()
		in.Name = name
		err := in.Validate()
		if !errors.Is(err, ErrInvalidProposalName) {
			t.Errorf("Name=%q: expected ErrInvalidProposalName, got %v", name, err)
		}
	}
}

func TestProposalInput_ValidateNameLowerSnakeAccepted(t *testing.T) {
	t.Parallel()
	cases := []string{
		"a",
		"count_open_prs",
		"find_overdue_tickets",
		"x1",
		"weekly_overdue_digest_v2",
	}
	for _, name := range cases {
		in := validInput()
		in.Name = name
		if err := in.Validate(); err != nil {
			t.Errorf("Name=%q: unexpected err %v", name, err)
		}
	}
}

func TestProposalInput_ValidateNameTooLong(t *testing.T) {
	t.Parallel()
	in := validInput()
	in.Name = strings.Repeat("a", MaxToolNameLength+1)
	err := in.Validate()
	if !errors.Is(err, ErrProposalFieldTooLong) {
		t.Errorf("expected ErrProposalFieldTooLong, got %v", err)
	}
}

func TestProposalInput_ValidateMissingPurpose(t *testing.T) {
	t.Parallel()
	for _, purpose := range []string{"", "   "} {
		in := validInput()
		in.Purpose = purpose
		if err := in.Validate(); !errors.Is(err, ErrMissingProposalPurpose) {
			t.Errorf("Purpose=%q: expected ErrMissingProposalPurpose, got %v", purpose, err)
		}
	}
}

func TestProposalInput_ValidatePurposeTooLong(t *testing.T) {
	t.Parallel()
	in := validInput()
	in.Purpose = strings.Repeat("a", MaxPurposeLength+1)
	if err := in.Validate(); !errors.Is(err, ErrProposalFieldTooLong) {
		t.Errorf("expected ErrProposalFieldTooLong, got %v", err)
	}
}

func TestProposalInput_ValidateMissingPlainLanguageDescription(t *testing.T) {
	t.Parallel()
	for _, pld := range []string{"", "   "} {
		in := validInput()
		in.PlainLanguageDescription = pld
		if err := in.Validate(); !errors.Is(err, ErrMissingPlainLanguageDescription) {
			t.Errorf("PLD=%q: expected ErrMissingPlainLanguageDescription, got %v", pld, err)
		}
	}
}

func TestProposalInput_ValidatePlainLanguageDescriptionTooLong(t *testing.T) {
	t.Parallel()
	in := validInput()
	in.PlainLanguageDescription = strings.Repeat("a", MaxPlainLanguageDescriptionLength+1)
	if err := in.Validate(); !errors.Is(err, ErrProposalFieldTooLong) {
		t.Errorf("expected ErrProposalFieldTooLong, got %v", err)
	}
}

func TestProposalInput_ValidateMissingCodeDraft(t *testing.T) {
	t.Parallel()
	for _, cd := range []string{"", "   ", "\n\n"} {
		in := validInput()
		in.CodeDraft = cd
		if err := in.Validate(); !errors.Is(err, ErrMissingCodeDraft) {
			t.Errorf("CodeDraft=%q: expected ErrMissingCodeDraft, got %v", cd, err)
		}
	}
}

func TestProposalInput_ValidateCodeDraftTooLong(t *testing.T) {
	t.Parallel()
	in := validInput()
	in.CodeDraft = strings.Repeat("a", MaxCodeDraftLength+1)
	if err := in.Validate(); !errors.Is(err, ErrProposalFieldTooLong) {
		t.Errorf("expected ErrProposalFieldTooLong, got %v", err)
	}
}

func TestProposalInput_ValidateEmptyCapabilities(t *testing.T) {
	t.Parallel()
	in := validInput()
	in.Capabilities = nil
	if err := in.Validate(); !errors.Is(err, ErrMissingProposalCapabilities) {
		t.Errorf("nil Capabilities: expected ErrMissingProposalCapabilities, got %v", err)
	}
	in.Capabilities = []string{}
	if err := in.Validate(); !errors.Is(err, ErrMissingProposalCapabilities) {
		t.Errorf("empty Capabilities: expected ErrMissingProposalCapabilities, got %v", err)
	}
}

func TestProposalInput_ValidateTooManyCapabilities(t *testing.T) {
	t.Parallel()
	in := validInput()
	caps := make([]string, MaxCapabilityCount+1)
	for i := range caps {
		caps[i] = "cap"
	}
	in.Capabilities = caps
	if err := in.Validate(); !errors.Is(err, ErrProposalFieldTooLong) {
		t.Errorf("expected ErrProposalFieldTooLong, got %v", err)
	}
}

func TestProposalInput_ValidateBlankCapability(t *testing.T) {
	t.Parallel()
	in := validInput()
	in.Capabilities = []string{"github:read", "   ", "jira:read"}
	if err := in.Validate(); !errors.Is(err, ErrInvalidProposalCapability) {
		t.Errorf("expected ErrInvalidProposalCapability, got %v", err)
	}
}

func TestProposalInput_ValidateCapabilityIDTooLong(t *testing.T) {
	t.Parallel()
	in := validInput()
	in.Capabilities = []string{strings.Repeat("a", MaxCapabilityIDLength+1)}
	if err := in.Validate(); !errors.Is(err, ErrProposalFieldTooLong) {
		t.Errorf("expected ErrProposalFieldTooLong, got %v", err)
	}
}

func TestProposalInput_ValidateInvalidTargetSource(t *testing.T) {
	t.Parallel()
	for _, ts := range []TargetSource{"", "local", "hosted", "Platform"} {
		in := validInput()
		in.TargetSource = ts
		if err := in.Validate(); !errors.Is(err, ErrInvalidTargetSource) {
			t.Errorf("TargetSource=%q: expected ErrInvalidTargetSource, got %v", ts, err)
		}
	}
}

// TestProposalInput_ValidateRejectsLocalTargetExplicitly pins the
// roadmap invariant "local source never offered to the agent". This
// duplicates a slice of TestTargetSource_RejectsLocal but with the
// full ProposalInput context (i.e. all other fields valid).
func TestProposalInput_ValidateRejectsLocalTargetExplicitly(t *testing.T) {
	t.Parallel()
	in := validInput()
	in.TargetSource = TargetSource("local")
	err := in.Validate()
	if !errors.Is(err, ErrInvalidTargetSource) {
		t.Fatalf("expected ErrInvalidTargetSource for target_source=local, got %v", err)
	}
}

func TestProposalInput_ValidateBoundaryLengthsAccepted(t *testing.T) {
	t.Parallel()
	in := ProposalInput{
		Name:                     strings.Repeat("a", MaxToolNameLength),
		Purpose:                  strings.Repeat("p", MaxPurposeLength),
		PlainLanguageDescription: strings.Repeat("d", MaxPlainLanguageDescriptionLength),
		CodeDraft:                strings.Repeat("c", MaxCodeDraftLength),
		Capabilities:             []string{strings.Repeat("k", MaxCapabilityIDLength)},
		TargetSource:             TargetSourcePrivate,
	}
	if err := in.Validate(); err != nil {
		t.Errorf("at boundary lengths: unexpected err %v", err)
	}
}

func TestCloneProposalInput_DeepCopiesCapabilities(t *testing.T) {
	t.Parallel()
	in := validInput()
	in.Capabilities = []string{"a", "b", "c"}
	cp := cloneProposalInput(in)
	// Mutate original; clone must be unaffected.
	in.Capabilities[0] = "mutated"
	in.Capabilities = append(in.Capabilities, "extra")
	if cp.Capabilities[0] != "a" || len(cp.Capabilities) != 3 {
		t.Errorf("clone aliases caller mutation: %v", cp.Capabilities)
	}
}

func TestCloneProposalInput_NilCapabilities(t *testing.T) {
	t.Parallel()
	in := validInput()
	in.Capabilities = nil
	cp := cloneProposalInput(in)
	if cp.Capabilities != nil {
		t.Errorf("expected nil clone, got %v", cp.Capabilities)
	}
}
