package peer_test

import (
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/peer"
	"github.com/vadimtrunov/watchkeepers/core/pkg/toolregistry"
)

func TestBuiltinAskManifest_ShapeAndValidate(t *testing.T) {
	t.Parallel()

	m := peer.BuiltinAskManifest()
	if m.Name != "peer.ask" {
		t.Errorf("Name = %q, want %q", m.Name, "peer.ask")
	}
	if m.Source != peer.BuiltinSourceName {
		t.Errorf("Source = %q, want %q", m.Source, peer.BuiltinSourceName)
	}
	if m.DryRunMode != toolregistry.DryRunModeScoped {
		t.Errorf("DryRunMode = %q, want %q", m.DryRunMode, toolregistry.DryRunModeScoped)
	}
	if len(m.Capabilities) != 1 || m.Capabilities[0] != peer.CapabilityAsk {
		t.Errorf("Capabilities = %v, want [%q]", m.Capabilities, peer.CapabilityAsk)
	}
	// Must round-trip through the toolregistry validator.
	if err := m.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestBuiltinReplyManifest_ShapeAndValidate(t *testing.T) {
	t.Parallel()

	m := peer.BuiltinReplyManifest()
	if m.Name != "peer.reply" {
		t.Errorf("Name = %q, want %q", m.Name, "peer.reply")
	}
	if m.Source != peer.BuiltinSourceName {
		t.Errorf("Source = %q, want %q", m.Source, peer.BuiltinSourceName)
	}
	if m.DryRunMode != toolregistry.DryRunModeScoped {
		t.Errorf("DryRunMode = %q, want %q", m.DryRunMode, toolregistry.DryRunModeScoped)
	}
	if len(m.Capabilities) != 1 || m.Capabilities[0] != peer.CapabilityReply {
		t.Errorf("Capabilities = %v, want [%q]", m.Capabilities, peer.CapabilityReply)
	}
	if err := m.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestBuiltinManifests_DefensiveDeepCopyOfSchema(t *testing.T) {
	t.Parallel()

	// Two consecutive calls must NOT share backing storage for the
	// Schema RawMessage — mutating one must not affect the other.
	a := peer.BuiltinAskManifest()
	b := peer.BuiltinAskManifest()
	if len(a.Schema) == 0 {
		t.Fatal("Schema empty")
	}
	a.Schema[0] = 'X'
	if b.Schema[0] == 'X' {
		t.Error("Schema buffer shared across BuiltinAskManifest calls (defensive copy regressed)")
	}
}
