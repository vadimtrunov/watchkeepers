package llm

import (
	"errors"
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/runtime"
)

// validManifest returns a [runtime.Manifest] suitable for happy-path
// builder calls. Tests that need a different shape modify the returned
// value before passing it to the builder.
func validManifest() runtime.Manifest {
	return runtime.Manifest{
		AgentID:      "agent-1",
		SystemPrompt: "You are a test agent.",
		Personality:  "concise",
		Language:     "en-US",
		Model:        "claude-sonnet-4",
		Autonomy:     runtime.AutonomySupervised,
		Toolset:      []string{"echo"},
		AuthorityMatrix: map[string]string{
			"approve_tools": "leader",
		},
		Metadata: map[string]string{
			"persona_id": "p-42",
		},
	}
}

// validMessages returns a 1-message caller slice for happy-path
// builder calls.
func validMessages() []Message {
	return []Message{
		{Role: RoleUser, Content: "hello"},
	}
}

// builderName tags each builder so the parity tests can report which
// variant tripped a regression without per-table boilerplate.
type builderName string

const (
	builderComplete    builderName = "complete"
	builderStream      builderName = "stream"
	builderCountTokens builderName = "count_tokens"
)

// builtRequest is the parity-test view onto the three concrete request
// types. Only the fields shared across all three are projected here so
// the parity assertions compare apples to apples.
type builtRequest struct {
	model    Model
	system   string
	messages []Message
	tools    []ToolDefinition
	metadata map[string]string
}

// runBuilder dispatches to the builder under test and returns the
// shared-field projection plus any builder error. Centralised so the
// parity tests pin all three variants without duplicating the switch.
func runBuilder(
	t *testing.T,
	b builderName,
	m runtime.Manifest,
	msgs []Message,
	opts ...RequestOption,
) (builtRequest, error) {
	t.Helper()
	switch b {
	case builderComplete:
		req, err := BuildCompleteRequest(m, msgs, opts...)
		return builtRequest{
			model:    req.Model,
			system:   req.System,
			messages: req.Messages,
			tools:    req.Tools,
			metadata: req.Metadata,
		}, err
	case builderStream:
		req, err := BuildStreamRequest(m, msgs, opts...)
		return builtRequest{
			model:    req.Model,
			system:   req.System,
			messages: req.Messages,
			tools:    req.Tools,
			metadata: req.Metadata,
		}, err
	case builderCountTokens:
		req, err := BuildCountTokensRequest(m, msgs, opts...)
		return builtRequest{
			model:    req.Model,
			system:   req.System,
			messages: req.Messages,
			tools:    req.Tools,
			metadata: req.Metadata,
		}, err
	default:
		t.Fatalf("unknown builder %q", b)
		return builtRequest{}, nil
	}
}

// allBuilders lists every builder variant for table-driven parity
// assertions. Adding a fourth builder later means appending here.
var allBuilders = []builderName{builderComplete, builderStream, builderCountTokens}

// TestBuildCompleteRequest_HappyPath pins the AC2 mapping rules:
// Model / System / Messages / Metadata are all populated correctly,
// and the language hint rides on the reserved metadata key.
func TestBuildCompleteRequest_HappyPath(t *testing.T) {
	t.Parallel()

	m := validManifest()
	msgs := validMessages()

	req, err := BuildCompleteRequest(m, msgs)
	if err != nil {
		t.Fatalf("BuildCompleteRequest: %v", err)
	}
	if string(req.Model) != m.Model {
		t.Fatalf("req.Model = %q, want %q", req.Model, m.Model)
	}
	if req.System != m.SystemPrompt {
		t.Fatalf("req.System = %q, want %q", req.System, m.SystemPrompt)
	}
	if len(req.Messages) != len(msgs) || req.Messages[0].Content != msgs[0].Content {
		t.Fatalf("req.Messages = %+v, want %+v", req.Messages, msgs)
	}
	if got := req.Metadata[metadataKeyLanguage]; got != m.Language {
		t.Fatalf("req.Metadata[%q] = %q, want %q", metadataKeyLanguage, got, m.Language)
	}
	if got := req.Metadata["persona_id"]; got != "p-42" {
		t.Fatalf("manifest metadata not carried through: %+v", req.Metadata)
	}
	// AC5: Tools default to nil (not empty).
	if req.Tools != nil {
		t.Fatalf("req.Tools = %+v, want nil default", req.Tools)
	}
	// MaxTokens / Temperature default to zero (provider default).
	if req.MaxTokens != 0 || req.Temperature != 0 {
		t.Fatalf("req.MaxTokens=%d Temperature=%v, want zeros", req.MaxTokens, req.Temperature)
	}
}

// TestBuilders_ParityAcrossVariants pins AC6: Stream and CountTokens
// share the same mapping rules as Complete on a valid happy-path input.
func TestBuilders_ParityAcrossVariants(t *testing.T) {
	t.Parallel()

	m := validManifest()
	msgs := validMessages()

	var first builtRequest
	for i, b := range allBuilders {
		got, err := runBuilder(t, b, m, msgs)
		if err != nil {
			t.Fatalf("builder %q: %v", b, err)
		}
		if i == 0 {
			first = got
			continue
		}
		if got.model != first.model {
			t.Errorf("%q model = %q, want %q", b, got.model, first.model)
		}
		if got.system != first.system {
			t.Errorf("%q system = %q, want %q", b, got.system, first.system)
		}
		if len(got.messages) != len(first.messages) {
			t.Errorf("%q messages len = %d, want %d", b, len(got.messages), len(first.messages))
		}
		if got.metadata[metadataKeyLanguage] != first.metadata[metadataKeyLanguage] {
			t.Errorf("%q language hint = %q, want %q", b, got.metadata[metadataKeyLanguage], first.metadata[metadataKeyLanguage])
		}
		if got.metadata["persona_id"] != first.metadata["persona_id"] {
			t.Errorf("%q persona_id = %q, want %q", b, got.metadata["persona_id"], first.metadata["persona_id"])
		}
	}
}

// TestBuilders_ManifestMetadataCarriesThroughVerbatim pins that every
// non-reserved key the manifest carries reaches the projected
// Metadata map unchanged across all three builders.
func TestBuilders_ManifestMetadataCarriesThroughVerbatim(t *testing.T) {
	t.Parallel()

	m := validManifest()
	m.Metadata = map[string]string{
		"persona_id": "p-42",
		"channel":    "slack:c1",
		"trace_id":   "tr-abc",
	}
	msgs := validMessages()

	for _, b := range allBuilders {
		got, err := runBuilder(t, b, m, msgs)
		if err != nil {
			t.Fatalf("%q: %v", b, err)
		}
		for k, v := range m.Metadata {
			if got.metadata[k] != v {
				t.Errorf("%q metadata[%q] = %q, want %q", b, k, got.metadata[k], v)
			}
		}
	}
}

// TestBuilders_LanguageOmittedWhenManifestLanguageEmpty pins the
// negative half of the language-hint contract: no Language ⇒ no
// reserved key in the metadata bag (so providers can detect "not set").
func TestBuilders_LanguageOmittedWhenManifestLanguageEmpty(t *testing.T) {
	t.Parallel()

	m := validManifest()
	m.Language = ""
	m.Metadata = nil
	msgs := validMessages()

	for _, b := range allBuilders {
		got, err := runBuilder(t, b, m, msgs)
		if err != nil {
			t.Fatalf("%q: %v", b, err)
		}
		if got.metadata != nil {
			if _, present := got.metadata[metadataKeyLanguage]; present {
				t.Errorf("%q metadata[%q] present, want absent", b, metadataKeyLanguage)
			}
		}
	}
}

// TestBuilders_OptionOverrides pins AC4 + AC5 across all three builders
// for the option set: WithMaxTokens, WithTemperature, WithTools.
// CountTokens silently drops MaxTokens / Temperature (the request type
// has no such fields); we still pin tools propagate.
func TestBuilders_OptionOverrides(t *testing.T) {
	t.Parallel()

	m := validManifest()
	msgs := validMessages()
	tool := ToolDefinition{
		Name:        "echo",
		Description: "echo back",
		InputSchema: map[string]any{"type": "object"},
	}

	t.Run("complete", func(t *testing.T) {
		t.Parallel()
		req, err := BuildCompleteRequest(
			m, msgs,
			WithMaxTokens(8192),
			WithTemperature(0.7),
			WithTools([]ToolDefinition{tool}),
		)
		if err != nil {
			t.Fatalf("BuildCompleteRequest: %v", err)
		}
		if req.MaxTokens != 8192 {
			t.Errorf("req.MaxTokens = %d, want 8192", req.MaxTokens)
		}
		if req.Temperature != 0.7 {
			t.Errorf("req.Temperature = %v, want 0.7", req.Temperature)
		}
		if len(req.Tools) != 1 || req.Tools[0].Name != "echo" {
			t.Errorf("req.Tools = %+v", req.Tools)
		}
	})

	t.Run("stream", func(t *testing.T) {
		t.Parallel()
		req, err := BuildStreamRequest(
			m, msgs,
			WithMaxTokens(2048),
			WithTemperature(0.2),
			WithTools([]ToolDefinition{tool}),
		)
		if err != nil {
			t.Fatalf("BuildStreamRequest: %v", err)
		}
		if req.MaxTokens != 2048 {
			t.Errorf("req.MaxTokens = %d, want 2048", req.MaxTokens)
		}
		if req.Temperature != 0.2 {
			t.Errorf("req.Temperature = %v, want 0.2", req.Temperature)
		}
		if len(req.Tools) != 1 || req.Tools[0].Name != "echo" {
			t.Errorf("req.Tools = %+v", req.Tools)
		}
	})

	t.Run("count_tokens_carries_tools_only", func(t *testing.T) {
		t.Parallel()
		req, err := BuildCountTokensRequest(
			m, msgs,
			WithMaxTokens(8192),
			WithTemperature(0.7),
			WithTools([]ToolDefinition{tool}),
		)
		if err != nil {
			t.Fatalf("BuildCountTokensRequest: %v", err)
		}
		if len(req.Tools) != 1 || req.Tools[0].Name != "echo" {
			t.Errorf("req.Tools = %+v", req.Tools)
		}
	})
}

// TestBuilders_LastWriteWinsOnRepeatedOptions pins AC4: when the same
// field is set twice, the last option wins (left-to-right composition).
func TestBuilders_LastWriteWinsOnRepeatedOptions(t *testing.T) {
	t.Parallel()

	m := validManifest()
	msgs := validMessages()

	req, err := BuildCompleteRequest(
		m, msgs,
		WithMaxTokens(100),
		WithMaxTokens(200),
		WithTemperature(0.1),
		WithTemperature(0.9),
	)
	if err != nil {
		t.Fatalf("BuildCompleteRequest: %v", err)
	}
	if req.MaxTokens != 200 {
		t.Errorf("req.MaxTokens = %d, want 200 (last write wins)", req.MaxTokens)
	}
	if req.Temperature != 0.9 {
		t.Errorf("req.Temperature = %v, want 0.9 (last write wins)", req.Temperature)
	}
}

// TestBuilders_WithMetadataMergesWithManifestKeys pins AC4: option-side
// WithMetadata keys override manifest-derived keys for the same key,
// and non-conflicting keys merge.
func TestBuilders_WithMetadataMergesWithManifestKeys(t *testing.T) {
	t.Parallel()

	m := validManifest()
	m.Metadata = map[string]string{
		"persona_id": "manifest-value",
		"channel":    "manifest-channel",
	}
	msgs := validMessages()

	req, err := BuildCompleteRequest(
		m, msgs,
		WithMetadata("persona_id", "option-wins"),
		WithMetadata("foo", "bar"),
	)
	if err != nil {
		t.Fatalf("BuildCompleteRequest: %v", err)
	}
	if got := req.Metadata["persona_id"]; got != "option-wins" {
		t.Errorf("persona_id = %q, want option-wins (option overrides manifest)", got)
	}
	if got := req.Metadata["channel"]; got != "manifest-channel" {
		t.Errorf("channel = %q, want manifest-channel (non-conflict carries through)", got)
	}
	if got := req.Metadata["foo"]; got != "bar" {
		t.Errorf("foo = %q, want bar (option-only key)", got)
	}
	// Reserved language key still wins from the manifest unless an
	// explicit option overrides it.
	if got := req.Metadata[metadataKeyLanguage]; got != m.Language {
		t.Errorf("language = %q, want %q", got, m.Language)
	}
}

// TestBuilders_WithMetadataOverridesLanguageHint pins that even the
// reserved language key is override-able when the caller asks (AC4
// last-write-wins semantics: option applies AFTER manifest fold).
func TestBuilders_WithMetadataOverridesLanguageHint(t *testing.T) {
	t.Parallel()

	m := validManifest()
	msgs := validMessages()

	req, err := BuildCompleteRequest(
		m, msgs,
		WithMetadata(metadataKeyLanguage, "ru-RU"),
	)
	if err != nil {
		t.Fatalf("BuildCompleteRequest: %v", err)
	}
	if got := req.Metadata[metadataKeyLanguage]; got != "ru-RU" {
		t.Errorf("language = %q, want ru-RU (option overrides manifest)", got)
	}
}

// TestBuilders_WithToolsDefaultsToNil pins AC5: omitting WithTools
// leaves Tools nil (not an empty slice). Documented contract.
func TestBuilders_WithToolsDefaultsToNil(t *testing.T) {
	t.Parallel()

	m := validManifest()
	msgs := validMessages()

	req, err := BuildCompleteRequest(m, msgs)
	if err != nil {
		t.Fatalf("BuildCompleteRequest: %v", err)
	}
	if req.Tools != nil {
		t.Fatalf("req.Tools = %+v, want nil (default per AC5)", req.Tools)
	}
}

// TestBuilders_NilOptionIsIgnored pins that a nil RequestOption is a
// no-op rather than a panic. Defensive: callers building option lists
// from data may slip a nil through and the projection should soldier on.
func TestBuilders_NilOptionIsIgnored(t *testing.T) {
	t.Parallel()

	m := validManifest()
	msgs := validMessages()

	req, err := BuildCompleteRequest(m, msgs, nil, WithMaxTokens(42), nil)
	if err != nil {
		t.Fatalf("BuildCompleteRequest: %v", err)
	}
	if req.MaxTokens != 42 {
		t.Errorf("req.MaxTokens = %d, want 42 (nil options must not skip later ones)", req.MaxTokens)
	}
}

// TestBuilders_ValidationNegatives pins AC3 across all three builders
// with table-driven cases. Each row mutates the manifest / msgs and
// asserts the expected sentinel surfaces via [errors.Is].
func TestBuilders_ValidationNegatives(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		mut  func(m *runtime.Manifest, msgs *[]Message)
		want error
	}{
		{
			"empty model",
			func(m *runtime.Manifest, _ *[]Message) { m.Model = "" },
			ErrInvalidManifest,
		},
		{
			"empty system prompt",
			func(m *runtime.Manifest, _ *[]Message) { m.SystemPrompt = "" },
			ErrInvalidManifest,
		},
		{
			"empty messages",
			func(_ *runtime.Manifest, msgs *[]Message) { *msgs = nil },
			ErrInvalidPrompt,
		},
		{
			"empty messages slice",
			func(_ *runtime.Manifest, msgs *[]Message) { *msgs = []Message{} },
			ErrInvalidPrompt,
		},
	}

	for _, tc := range cases {
		for _, b := range allBuilders {
			t.Run(string(b)+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				m := validManifest()
				msgs := validMessages()
				tc.mut(&m, &msgs)
				_, err := runBuilder(t, b, m, msgs)
				if !errors.Is(err, tc.want) {
					t.Fatalf("err = %v, want errors.Is %v", err, tc.want)
				}
			})
		}
	}
}

// TestBuilders_NilAuthorityMatrixIsSafe pins the "doesn't panic"
// negative from the test plan: nil Manifest.AuthorityMatrix is a
// non-event for the projection (this layer never consults it).
func TestBuilders_NilAuthorityMatrixIsSafe(t *testing.T) {
	t.Parallel()

	m := validManifest()
	m.AuthorityMatrix = nil
	msgs := validMessages()

	for _, b := range allBuilders {
		if _, err := runBuilder(t, b, m, msgs); err != nil {
			t.Errorf("%q with nil AuthorityMatrix: %v", b, err)
		}
	}
}

// TestBuilders_MessagesSliceIsForwardedVerbatim pins AC2: the
// projected request carries the caller-supplied msgs verbatim, including
// per-message metadata bags.
func TestBuilders_MessagesSliceIsForwardedVerbatim(t *testing.T) {
	t.Parallel()

	m := validManifest()
	msgs := []Message{
		{Role: RoleUser, Content: "hello", Metadata: map[string]string{"channel": "slack:c1"}},
		{Role: RoleAssistant, Content: "hi back"},
		{Role: RoleTool, Content: `{"ok":true}`, Metadata: map[string]string{"tool_call_id": "tc_1"}},
	}

	req, err := BuildCompleteRequest(m, msgs)
	if err != nil {
		t.Fatalf("BuildCompleteRequest: %v", err)
	}
	if len(req.Messages) != len(msgs) {
		t.Fatalf("req.Messages len = %d, want %d", len(req.Messages), len(msgs))
	}
	for i := range msgs {
		if req.Messages[i].Role != msgs[i].Role {
			t.Errorf("Messages[%d].Role = %q, want %q", i, req.Messages[i].Role, msgs[i].Role)
		}
		if req.Messages[i].Content != msgs[i].Content {
			t.Errorf("Messages[%d].Content = %q, want %q", i, req.Messages[i].Content, msgs[i].Content)
		}
		if msgs[i].Metadata != nil && req.Messages[i].Metadata["channel"] != msgs[i].Metadata["channel"] {
			t.Errorf("Messages[%d].Metadata = %+v, want %+v", i, req.Messages[i].Metadata, msgs[i].Metadata)
		}
	}
}

// TestBuilders_WithMetadataInitializesNilMap pins that WithMetadata
// allocates the metadata map when the manifest contributed none (no
// Metadata + no Language). Belt-and-braces against a regression that
// would NPE on the first option write.
func TestBuilders_WithMetadataInitializesNilMap(t *testing.T) {
	t.Parallel()

	m := validManifest()
	m.Metadata = nil
	m.Language = ""
	msgs := validMessages()

	req, err := BuildCompleteRequest(m, msgs, WithMetadata("foo", "bar"))
	if err != nil {
		t.Fatalf("BuildCompleteRequest: %v", err)
	}
	if req.Metadata == nil {
		t.Fatalf("req.Metadata = nil, want allocated map")
	}
	if got := req.Metadata["foo"]; got != "bar" {
		t.Errorf("req.Metadata[foo] = %q, want bar", got)
	}
}

// TestErrInvalidManifest_MatchableViaErrorsIs pins the matchability
// contract for the new sentinel.
func TestErrInvalidManifest_MatchableViaErrorsIs(t *testing.T) {
	t.Parallel()

	if !errors.Is(ErrInvalidManifest, ErrInvalidManifest) {
		t.Fatalf("errors.Is(ErrInvalidManifest, ErrInvalidManifest) = false")
	}
}
