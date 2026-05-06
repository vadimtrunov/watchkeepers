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
		AgentID:         "agent-1",
		SystemPrompt:    "You are a test agent.",
		Personality:     "concise",
		Language:        "en-US",
		Model:           "claude-sonnet-4",
		Autonomy:        runtime.AutonomySupervised,
		Toolset:         []string{"echo"},
		AuthorityMatrix: map[string]string{"approve_tools": "leader"},
		Metadata:        map[string]string{"persona_id": "p-42"},
	}
}

// validMessages returns a 1-message caller slice for happy-path
// builder calls.
func validMessages() []Message {
	return []Message{{Role: RoleUser, Content: "hello"}}
}

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

// builderFn is the dispatch shape every builder reduces to for the
// table-driven tests. Centralised so the parity assertions iterate over
// `allBuilders` instead of duplicating the switch per test.
type builderFn struct {
	name string
	run  func(runtime.Manifest, []Message, ...RequestOption) (builtRequest, error)
}

// allBuilders lists every builder variant for table-driven parity
// assertions. Adding a fourth builder later means appending here.
var allBuilders = []builderFn{
	{"complete", func(m runtime.Manifest, msgs []Message, opts ...RequestOption) (builtRequest, error) {
		r, err := BuildCompleteRequest(m, msgs, opts...)
		return builtRequest{r.Model, r.System, r.Messages, r.Tools, r.Metadata}, err
	}},
	{"stream", func(m runtime.Manifest, msgs []Message, opts ...RequestOption) (builtRequest, error) {
		r, err := BuildStreamRequest(m, msgs, opts...)
		return builtRequest{r.Model, r.System, r.Messages, r.Tools, r.Metadata}, err
	}},
	{"count_tokens", func(m runtime.Manifest, msgs []Message, opts ...RequestOption) (builtRequest, error) {
		r, err := BuildCountTokensRequest(m, msgs, opts...)
		return builtRequest{r.Model, r.System, r.Messages, r.Tools, r.Metadata}, err
	}},
}

// TestBuilders_HappyPathAndParity pins AC2 + AC6: every builder maps
// Model / System / Messages / Metadata correctly and the language hint
// rides on the reserved metadata key. Manifest.Metadata keys carry
// through verbatim across all three variants.
func TestBuilders_HappyPathAndParity(t *testing.T) {
	t.Parallel()

	m := validManifest()
	msgs := validMessages()

	for _, b := range allBuilders {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			got, err := b.run(m, msgs)
			if err != nil {
				t.Fatalf("%s: %v", b.name, err)
			}
			if string(got.model) != m.Model {
				t.Errorf("model = %q, want %q", got.model, m.Model)
			}
			if got.system != m.SystemPrompt {
				t.Errorf("system = %q, want %q", got.system, m.SystemPrompt)
			}
			if len(got.messages) != len(msgs) || got.messages[0].Content != msgs[0].Content {
				t.Errorf("messages = %+v, want %+v", got.messages, msgs)
			}
			if got.metadata[metadataKeyLanguage] != m.Language {
				t.Errorf("language = %q, want %q", got.metadata[metadataKeyLanguage], m.Language)
			}
			if got.metadata["persona_id"] != "p-42" {
				t.Errorf("persona_id not carried: %+v", got.metadata)
			}
			if got.tools != nil {
				t.Errorf("tools = %+v, want nil default (AC5)", got.tools)
			}
		})
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
	for _, b := range allBuilders {
		got, err := b.run(m, validMessages())
		if err != nil {
			t.Fatalf("%s: %v", b.name, err)
		}
		if _, present := got.metadata[metadataKeyLanguage]; present {
			t.Errorf("%s: language key present, want absent", b.name)
		}
	}
}

// TestBuilders_OptionOverrides pins AC4 + AC5 across the three concrete
// request types. CountTokens silently drops MaxTokens / Temperature
// (the request type has no such fields); we still pin tools propagate.
func TestBuilders_OptionOverrides(t *testing.T) {
	t.Parallel()

	m := validManifest()
	msgs := validMessages()
	tool := ToolDefinition{
		Name:        "echo",
		Description: "echo back",
		InputSchema: map[string]any{"type": "object"},
	}

	c, err := BuildCompleteRequest(m, msgs, WithMaxTokens(8192), WithTemperature(0.7), WithTools([]ToolDefinition{tool}))
	if err != nil {
		t.Fatalf("BuildCompleteRequest: %v", err)
	}
	if c.MaxTokens != 8192 || c.Temperature != 0.7 {
		t.Errorf("complete knobs = (%d, %v), want (8192, 0.7)", c.MaxTokens, c.Temperature)
	}
	if len(c.Tools) != 1 || c.Tools[0].Name != "echo" {
		t.Errorf("complete tools = %+v", c.Tools)
	}

	s, err := BuildStreamRequest(m, msgs, WithMaxTokens(2048), WithTemperature(0.2), WithTools([]ToolDefinition{tool}))
	if err != nil {
		t.Fatalf("BuildStreamRequest: %v", err)
	}
	if s.MaxTokens != 2048 || s.Temperature != 0.2 {
		t.Errorf("stream knobs = (%d, %v), want (2048, 0.2)", s.MaxTokens, s.Temperature)
	}
	if len(s.Tools) != 1 {
		t.Errorf("stream tools len = %d, want 1", len(s.Tools))
	}

	ct, err := BuildCountTokensRequest(m, msgs, WithMaxTokens(8192), WithTemperature(0.7), WithTools([]ToolDefinition{tool}))
	if err != nil {
		t.Fatalf("BuildCountTokensRequest: %v", err)
	}
	if len(ct.Tools) != 1 || ct.Tools[0].Name != "echo" {
		t.Errorf("count_tokens tools = %+v", ct.Tools)
	}
}

// TestBuilders_OptionComposition pins AC4 last-write-wins for repeated
// options and AC4 metadata merge precedence (option keys override
// manifest keys for the same key, including the reserved language hint).
// Also exercises the nil-RequestOption no-op and the nil-metadata-init
// branch of WithMetadata in one shot.
func TestBuilders_OptionComposition(t *testing.T) {
	t.Parallel()

	m := validManifest()
	m.Metadata = map[string]string{"persona_id": "manifest-value", "channel": "manifest-channel"}
	msgs := validMessages()

	req, err := BuildCompleteRequest(
		m, msgs,
		nil,                                    // skipped without panic
		WithMaxTokens(100), WithMaxTokens(200), // last-write-wins
		WithTemperature(0.1), WithTemperature(0.9),
		WithMetadata("persona_id", "option-wins"),  // overrides manifest
		WithMetadata("foo", "bar"),                 // option-only key
		WithMetadata(metadataKeyLanguage, "ru-RU"), // overrides reserved
	)
	if err != nil {
		t.Fatalf("BuildCompleteRequest: %v", err)
	}
	if req.MaxTokens != 200 || req.Temperature != 0.9 {
		t.Errorf("(MaxTokens, Temperature) = (%d, %v), want (200, 0.9)", req.MaxTokens, req.Temperature)
	}
	wantMeta := map[string]string{
		"persona_id":        "option-wins",
		"channel":           "manifest-channel",
		"foo":               "bar",
		metadataKeyLanguage: "ru-RU",
	}
	for k, want := range wantMeta {
		if got := req.Metadata[k]; got != want {
			t.Errorf("metadata[%q] = %q, want %q", k, got, want)
		}
	}

	// Nil-metadata-init branch: manifest contributes none, option still
	// allocates the bag.
	m2 := validManifest()
	m2.Metadata = nil
	m2.Language = ""
	r2, err := BuildCompleteRequest(m2, msgs, WithMetadata("foo", "bar"))
	if err != nil {
		t.Fatalf("BuildCompleteRequest empty-meta: %v", err)
	}
	if r2.Metadata == nil || r2.Metadata["foo"] != "bar" {
		t.Errorf("nil-init metadata = %+v, want {foo: bar}", r2.Metadata)
	}
}

// TestBuilders_MessagesSliceForwardedVerbatim pins AC2: the projected
// request carries the caller-supplied msgs verbatim, including
// per-message metadata bags.
func TestBuilders_MessagesSliceForwardedVerbatim(t *testing.T) {
	t.Parallel()

	msgs := []Message{
		{Role: RoleUser, Content: "hello", Metadata: map[string]string{"channel": "slack:c1"}},
		{Role: RoleAssistant, Content: "hi back"},
		{Role: RoleTool, Content: `{"ok":true}`, Metadata: map[string]string{"tool_call_id": "tc_1"}},
	}
	req, err := BuildCompleteRequest(validManifest(), msgs)
	if err != nil {
		t.Fatalf("BuildCompleteRequest: %v", err)
	}
	if len(req.Messages) != len(msgs) {
		t.Fatalf("messages len = %d, want %d", len(req.Messages), len(msgs))
	}
	for i := range msgs {
		if req.Messages[i].Role != msgs[i].Role || req.Messages[i].Content != msgs[i].Content {
			t.Errorf("messages[%d] = %+v, want %+v", i, req.Messages[i], msgs[i])
		}
		if msgs[i].Metadata != nil && req.Messages[i].Metadata["channel"] != msgs[i].Metadata["channel"] {
			t.Errorf("messages[%d].Metadata = %+v, want %+v", i, req.Messages[i].Metadata, msgs[i].Metadata)
		}
	}
}

// TestBuilders_ValidationNegatives pins AC3 across all three builders
// with table-driven cases, including the "nil AuthorityMatrix doesn't
// panic" negative from the test plan (encoded as a happy case).
func TestBuilders_ValidationNegatives(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		mut  func(m *runtime.Manifest, msgs *[]Message)
		want error // nil ⇒ happy
	}{
		{"empty model", func(m *runtime.Manifest, _ *[]Message) { m.Model = "" }, ErrInvalidManifest},
		{"empty system", func(m *runtime.Manifest, _ *[]Message) { m.SystemPrompt = "" }, ErrInvalidManifest},
		{"nil messages", func(_ *runtime.Manifest, msgs *[]Message) { *msgs = nil }, ErrInvalidPrompt},
		{"empty messages slice", func(_ *runtime.Manifest, msgs *[]Message) { *msgs = []Message{} }, ErrInvalidPrompt},
		{"nil authority matrix", func(m *runtime.Manifest, _ *[]Message) { m.AuthorityMatrix = nil }, nil},
	}
	for _, tc := range cases {
		for _, b := range allBuilders {
			t.Run(b.name+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				m := validManifest()
				msgs := validMessages()
				tc.mut(&m, &msgs)
				_, err := b.run(m, msgs)
				if tc.want == nil {
					if err != nil {
						t.Fatalf("err = %v, want nil", err)
					}
					return
				}
				if !errors.Is(err, tc.want) {
					t.Fatalf("err = %v, want errors.Is %v", err, tc.want)
				}
			})
		}
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
