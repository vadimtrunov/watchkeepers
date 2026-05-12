package toolregistry

import (
	"errors"
	"testing"
)

// TestDryRunMode_ValidateHappyPath pins the closed set documented on
// [DryRunMode].
func TestDryRunMode_ValidateHappyPath(t *testing.T) {
	t.Parallel()
	cases := []DryRunMode{DryRunModeGhost, DryRunModeScoped, DryRunModeNone}
	for _, m := range cases {
		if err := m.Validate(); err != nil {
			t.Errorf("Validate(%q): unexpected err %v", m, err)
		}
	}
}

func TestDryRunMode_ValidateRejectsEmpty(t *testing.T) {
	t.Parallel()
	// Empty string is refused by Validate (the "blank" Manifest case is
	// caught earlier by ErrManifestMissingRequired; this tests the enum
	// validator standalone).
	if err := DryRunMode("").Validate(); !errors.Is(err, ErrInvalidDryRunMode) {
		t.Errorf("expected ErrInvalidDryRunMode for empty, got %v", err)
	}
}

func TestDryRunMode_ValidateRejectsUnknown(t *testing.T) {
	t.Parallel()
	cases := []DryRunMode{"shadow", "Ghost", " ghost", "ghost ", "GHOST"}
	for _, m := range cases {
		if err := m.Validate(); !errors.Is(err, ErrInvalidDryRunMode) {
			t.Errorf("Validate(%q): expected ErrInvalidDryRunMode, got %v", m, err)
		}
	}
}

// TestDecodeManifest_DryRunModeHappyPath pins each of the three
// values against [DecodeManifest] end-to-end.
func TestDecodeManifest_DryRunModeHappyPath(t *testing.T) {
	t.Parallel()
	cases := map[string]DryRunMode{
		`{"name":"x","version":"1","capabilities":["c"],"schema":{},"dry_run_mode":"ghost"}`:  DryRunModeGhost,
		`{"name":"x","version":"1","capabilities":["c"],"schema":{},"dry_run_mode":"scoped"}`: DryRunModeScoped,
		`{"name":"x","version":"1","capabilities":["c"],"schema":{},"dry_run_mode":"none"}`:   DryRunModeNone,
	}
	for raw, want := range cases {
		m, err := DecodeManifest([]byte(raw))
		if err != nil {
			t.Errorf("DecodeManifest(%q): %v", raw, err)
			continue
		}
		if m.DryRunMode != want {
			t.Errorf("DryRunMode: got %q, want %q", m.DryRunMode, want)
		}
	}
}

// TestDecodeManifest_DryRunModeMissingFails covers the
// missing-required surface — a manifest without a `dry_run_mode`
// field is refused with [ErrManifestMissingRequired]. The field is
// load-bearing for the M9.4.c executor's runtime posture.
func TestDecodeManifest_DryRunModeMissingFails(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"name":"x","version":"1","capabilities":["c"],"schema":{}}`)
	_, err := DecodeManifest(raw)
	if !errors.Is(err, ErrManifestMissingRequired) {
		t.Errorf("expected ErrManifestMissingRequired, got %v", err)
	}
}

// TestDecodeManifest_DryRunModeBlankFails — a `dry_run_mode: ""`
// value is refused as missing-required (the validator treats blank
// string as absent for required fields).
func TestDecodeManifest_DryRunModeBlankFails(t *testing.T) {
	t.Parallel()
	cases := []string{
		`{"name":"x","version":"1","capabilities":["c"],"schema":{},"dry_run_mode":""}`,
		`{"name":"x","version":"1","capabilities":["c"],"schema":{},"dry_run_mode":"   "}`,
	}
	for _, raw := range cases {
		_, err := DecodeManifest([]byte(raw))
		if !errors.Is(err, ErrManifestMissingRequired) {
			t.Errorf("%q: expected ErrManifestMissingRequired, got %v", raw, err)
		}
	}
}

// TestDecodeManifest_DryRunModeInvalidFails — a manifest carrying
// an unknown `dry_run_mode` value is refused with
// [ErrInvalidDryRunMode]. Pins the loud-failure discipline.
func TestDecodeManifest_DryRunModeInvalidFails(t *testing.T) {
	t.Parallel()
	cases := []string{
		`{"name":"x","version":"1","capabilities":["c"],"schema":{},"dry_run_mode":"shadow"}`,
		`{"name":"x","version":"1","capabilities":["c"],"schema":{},"dry_run_mode":"Ghost"}`,
		`{"name":"x","version":"1","capabilities":["c"],"schema":{},"dry_run_mode":"GHOST"}`,
	}
	for _, raw := range cases {
		_, err := DecodeManifest([]byte(raw))
		if !errors.Is(err, ErrInvalidDryRunMode) {
			t.Errorf("%q: expected ErrInvalidDryRunMode, got %v", raw, err)
		}
	}
}
