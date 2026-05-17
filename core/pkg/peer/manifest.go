// manifest.go ships the M1.3.a tool-registry built-in entries for
// `peer.ask` and `peer.reply`. The roadmap M1.3.a AC pins a
// "Tool-registry entry under `built-in` source with capability
// `peer:ask` / `peer:reply`"; this file provides the matching
// [toolregistry.Manifest] values so a future runtime-side loader
// can register them without duplicating the literals.
//
// The `built-in` source name is reserved across the toolregistry for
// platform-owned, statically-shipped tools (the M8.1 / M8.2 milestones
// will introduce the matching signing + verification at boot). M1.3.a
// only ships the manifest values; M1.3.b / .c / .d will append their
// own entries (`peer.close`, `peer.subscribe`, `peer.broadcast`).
//
// The manifests are deliberately constructed via a helper rather than
// embedded JSON because the [toolregistry.DecodeManifest] strict-decode
// path rejects unknown fields AND demands a `dry_run_mode` value;
// stamping the fields in Go keeps the M9.4.a contract honoured without
// dragging a JSON fixture into the package. Future M9.3 signing wires
// the `Signature` field at build time; M1.3.a leaves it empty per the
// `NoopSignatureVerifier` contract.

package peer

import (
	"encoding/json"

	"github.com/vadimtrunov/watchkeepers/core/pkg/toolregistry"
)

// BuiltinSourceName is the reserved [toolregistry.Manifest.Source]
// name for every peer-family built-in. Hoisted to a constant so a
// future loader can match against it without hard-coding the literal.
// Mirrors the M8.2 "built-in tools signed at core-binary build time"
// trust posture documented in the Phase 2 ROADMAP.
const BuiltinSourceName = "built-in"

// peerAskSchema is the zod-compatible JSON-schema fragment describing
// [Tool.Ask]'s argument shape. The M9.4.a strict-decode path requires
// a non-null schema; this schema is the minimum viable surface (no
// per-argument constraints beyond type) so M1.3.a does not duplicate
// validation already enforced by [AskParams]. A future M9.3 schema
// linter will tighten the constraints.
var peerAskSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "target": { "type": "string", "description": "watchkeeper id or role name" },
    "subject": { "type": "string", "description": "operator-facing free-text label" },
    "body": { "type": "string", "description": "request payload" },
    "timeout_ms": { "type": "integer", "minimum": 1, "description": "wait timeout in milliseconds" }
  },
  "required": ["target", "subject", "body", "timeout_ms"]
}`)

// peerReplySchema is the zod-compatible JSON-schema fragment describing
// [Tool.Reply]'s argument shape. Same minimum-viable-surface posture as
// [peerAskSchema].
var peerReplySchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "conversation_id": { "type": "string", "format": "uuid", "description": "k2k conversation id" },
    "body": { "type": "string", "description": "reply payload" }
  },
  "required": ["conversation_id", "body"]
}`)

// peerCloseSchema is the zod-compatible JSON-schema fragment describing
// [Tool.Close]'s argument shape. Same minimum-viable-surface posture as
// [peerAskSchema] / [peerReplySchema]. `summary` is optional (an empty
// or omitted summary records an empty close_summary column) so the
// schema does not list it under `required`.
var peerCloseSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "conversation_id": { "type": "string", "format": "uuid", "description": "k2k conversation id" },
    "summary": { "type": "string", "description": "one-line operator-facing close summary" }
  },
  "required": ["conversation_id"]
}`)

// peerBroadcastSchema is the zod-compatible JSON-schema fragment
// describing [Tool.Broadcast]'s argument shape. Same minimum-viable-
// surface posture as the other peer.* schemas. `roles` / `languages` /
// `capabilities` are optional individually but at least one MUST be
// non-empty for the call to admit (enforced inside
// [Tool.Broadcast] via [ErrPeerRoleFilterEmpty]); the JSON schema
// surface keeps them all optional and lets the Go validator do the
// cross-field check (M9.3's schema linter will tighten this).
var peerBroadcastSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "subject": { "type": "string", "description": "operator-facing free-text label" },
    "body": { "type": "string", "description": "request payload broadcast verbatim to every target" },
    "per_target_timeout_ms": { "type": "integer", "minimum": 1, "description": "per-target wait timeout in milliseconds" },
    "concurrency": { "type": "integer", "minimum": 0, "description": "worker-pool bound; 0 = default" },
    "filter": {
      "type": "object",
      "properties": {
        "roles": { "type": "array", "items": { "type": "string" }, "description": "closed-set role filter" },
        "languages": { "type": "array", "items": { "type": "string" }, "description": "closed-set language filter" },
        "capabilities": { "type": "array", "items": { "type": "string" }, "description": "set-superset capability filter" },
        "exclude_self": { "type": "boolean", "description": "drop the acting watchkeeper from the resolved set" }
      }
    }
  },
  "required": ["subject", "body", "per_target_timeout_ms", "filter"]
}`)

// peerSubscribeSchema is the zod-compatible JSON-schema fragment
// describing [Tool.Subscribe]'s argument shape. Same minimum-viable-
// surface posture as [peerAskSchema] / [peerReplySchema] /
// [peerCloseSchema]. Both `target` and `event_types` are optional —
// empty / omitted values broaden the subscription to "every event in
// the tenant" / "every event type" respectively.
var peerSubscribeSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "target": { "type": "string", "description": "watchkeeper id filter; empty subscribes to every watchkeeper in the tenant" },
    "event_types": {
      "type": "array",
      "items": { "type": "string" },
      "description": "closed-set event-type filter; empty subscribes to every event type"
    }
  }
}`)

// BuiltinAskManifest returns the [toolregistry.Manifest] entry for
// `peer.ask`. Stamped with [BuiltinSourceName], capability
// [CapabilityAsk], and the [toolregistry.DryRunModeScoped] mode (a
// dry-run ask reroutes the request to the lead's DM under M9.4.c).
// The returned value is a fresh struct per call; defensive deep-copy
// of the schema RawMessage protects callers that mutate the returned
// value.
func BuiltinAskManifest() toolregistry.Manifest {
	return toolregistry.Manifest{
		Name:         "peer.ask",
		Version:      "1.0.0",
		Capabilities: []string{CapabilityAsk},
		Schema:       cloneBytes(peerAskSchema),
		Source:       BuiltinSourceName,
		DryRunMode:   toolregistry.DryRunModeScoped,
	}
}

// BuiltinReplyManifest returns the [toolregistry.Manifest] entry for
// `peer.reply`. Same shape as [BuiltinAskManifest] but with the
// [CapabilityReply] capability id and the `peer.reply` name. Dry-run
// mode is [toolregistry.DryRunModeScoped] — a dry-run reply reroutes
// the reply to the lead's DM under M9.4.c.
func BuiltinReplyManifest() toolregistry.Manifest {
	return toolregistry.Manifest{
		Name:         "peer.reply",
		Version:      "1.0.0",
		Capabilities: []string{CapabilityReply},
		Schema:       cloneBytes(peerReplySchema),
		Source:       BuiltinSourceName,
		DryRunMode:   toolregistry.DryRunModeScoped,
	}
}

// BuiltinCloseManifest returns the [toolregistry.Manifest] entry for
// `peer.close`. Same shape as [BuiltinAskManifest] / [BuiltinReplyManifest]
// but with the [CapabilityClose] capability id and the `peer.close`
// name. Dry-run mode is [toolregistry.DryRunModeScoped] — a dry-run
// close reroutes the close notification to the lead's DM under M9.4.c.
// The returned value is a fresh struct per call; defensive deep-copy of
// the schema RawMessage protects callers that mutate the returned value.
func BuiltinCloseManifest() toolregistry.Manifest {
	return toolregistry.Manifest{
		Name:         "peer.close",
		Version:      "1.0.0",
		Capabilities: []string{CapabilityClose},
		Schema:       cloneBytes(peerCloseSchema),
		Source:       BuiltinSourceName,
		DryRunMode:   toolregistry.DryRunModeScoped,
	}
}

// BuiltinSubscribeManifest returns the [toolregistry.Manifest] entry for
// `peer.subscribe`. Same shape as [BuiltinAskManifest] /
// [BuiltinReplyManifest] / [BuiltinCloseManifest] but with the
// [CapabilitySubscribe] capability id and the `peer.subscribe` name.
// Dry-run mode is [toolregistry.DryRunModeScoped] — a dry-run subscribe
// reroutes the delivered events to a dev-loop sink under M9.4.c. The
// returned value is a fresh struct per call; defensive deep-copy of the
// schema RawMessage protects callers that mutate the returned value.
func BuiltinSubscribeManifest() toolregistry.Manifest {
	return toolregistry.Manifest{
		Name:         "peer.subscribe",
		Version:      "1.0.0",
		Capabilities: []string{CapabilitySubscribe},
		Schema:       cloneBytes(peerSubscribeSchema),
		Source:       BuiltinSourceName,
		DryRunMode:   toolregistry.DryRunModeScoped,
	}
}

// BuiltinBroadcastManifest returns the [toolregistry.Manifest] entry
// for `peer.broadcast`. Same shape as [BuiltinAskManifest] /
// [BuiltinReplyManifest] / [BuiltinCloseManifest] /
// [BuiltinSubscribeManifest] but with the [CapabilityBroadcast]
// capability id and the `peer.broadcast` name. Dry-run mode is
// [toolregistry.DryRunModeScoped] — a dry-run broadcast reroutes the
// fan-out to the lead's DM under M9.4.c. The returned value is a
// fresh struct per call; defensive deep-copy of the schema RawMessage
// protects callers that mutate the returned value.
func BuiltinBroadcastManifest() toolregistry.Manifest {
	return toolregistry.Manifest{
		Name:         "peer.broadcast",
		Version:      "1.0.0",
		Capabilities: []string{CapabilityBroadcast},
		Schema:       cloneBytes(peerBroadcastSchema),
		Source:       BuiltinSourceName,
		DryRunMode:   toolregistry.DryRunModeScoped,
	}
}

// cloneBytes returns a defensive deep-copy of `in`. Hoisted here to
// keep the manifest helpers self-contained; mirrors the same helper
// in `core/pkg/toolregistry/manifest.go`.
func cloneBytes(in []byte) []byte {
	if in == nil {
		return nil
	}
	out := make([]byte, len(in))
	copy(out, in)
	return out
}
