package slack

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger"
)

// recordingCredsSink captures CreateAppCredentials callbacks so tests
// assert the sink was invoked with exactly the values returned by
// Slack. Mirrors the recordingTokenSink pattern from
// install_app_test.go (M4.2.d.2 lesson).
type recordingCredsSink struct {
	mu      sync.Mutex
	entries []CreateAppCredentials
}

func (s *recordingCredsSink) sink(_ context.Context, c CreateAppCredentials) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, c)
	return nil
}

func (s *recordingCredsSink) snapshot() []CreateAppCredentials {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]CreateAppCredentials, len(s.entries))
	copy(out, s.entries)
	return out
}

// TestCreateApp_CredsSink_FiresWithFullCredentials asserts the
// happy-path: a successful apps.manifest.create response causes the
// configured sink to receive every credential field Slack returned.
// The Installation-style out-of-band handoff is the M4.2.d.2 design
// pattern applied to CreateApp.
func TestCreateApp_CredsSink_FiresWithFullCredentials(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{
			"ok": true,
			"app_id": "A0123ABCDEF",
			"credentials": {
				"client_id": "0123456789.0987654321",
				"client_secret": "fake-client-secret",
				"verification_token": "fake-verification-token",
				"signing_secret": "fake-signing-secret"
			}
		}`)
	}))
	t.Cleanup(srv.Close)

	sink := &recordingCredsSink{}
	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("xoxe.xoxp-1-test")),
		WithCreateAppCredsSink(sink.sink),
	)
	id, err := c.CreateApp(context.Background(), messenger.AppManifest{
		Name:        "wk",
		Description: "x",
		Scopes:      []string{"chat:write"},
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	if id != messenger.AppID("A0123ABCDEF") {
		t.Errorf("AppID = %q, want A0123ABCDEF", id)
	}

	got := sink.snapshot()
	if len(got) != 1 {
		t.Fatalf("sink invoked %d times, want 1", len(got))
	}
	// #nosec G101 -- test fixtures, not real credentials.
	want := CreateAppCredentials{
		AppID:             messenger.AppID("A0123ABCDEF"),
		ClientID:          "0123456789.0987654321",
		ClientSecret:      "fake-client-secret",
		VerificationToken: "fake-verification-token",
		SigningSecret:     "fake-signing-secret",
	}
	if got[0] != want {
		t.Errorf("sink saw %+v, want %+v", got[0], want)
	}
}

// TestCreateApp_CredsSink_NotConfigured_DiscardsSilently asserts the
// backwards-compatible behaviour: callers that never wire a sink still
// get the AppID back; the credentials are silently discarded. This
// keeps the M4.2.d.1 contract intact for tests / callers that only
// need the AppID.
func TestCreateApp_CredsSink_NotConfigured_DiscardsSilently(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{
			"ok": true,
			"app_id": "A0123ABCDEF",
			"credentials": {
				"client_id": "x",
				"client_secret": "y",
				"verification_token": "z",
				"signing_secret": "w"
			}
		}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("xoxe.xoxp-1-test")),
	)
	id, err := c.CreateApp(context.Background(), messenger.AppManifest{
		Name:        "wk",
		Description: "x",
		Scopes:      []string{"chat:write"},
	})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	if id != messenger.AppID("A0123ABCDEF") {
		t.Errorf("AppID = %q, want A0123ABCDEF", id)
	}
}

// TestCreateApp_CredsSink_Error_PropagatesWrapped asserts that a sink
// that returns an error causes CreateApp to surface it wrapped — the
// caller can match the original sentinel via errors.Is. The adapter
// does NOT roll back the app creation (Slack's manifest.create is
// already server-side complete; reconciliation lives upstream).
func TestCreateApp_CredsSink_Error_PropagatesWrapped(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{
			"ok": true,
			"app_id": "A0123ABCDEF",
			"credentials": {"client_id":"x","client_secret":"y","verification_token":"z","signing_secret":"w"}
		}`)
	}))
	t.Cleanup(srv.Close)

	wantErr := errors.New("vault write failed")
	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("xoxe.xoxp-1-test")),
		WithCreateAppCredsSink(func(context.Context, CreateAppCredentials) error {
			return wantErr
		}),
	)
	_, err := c.CreateApp(context.Background(), messenger.AppManifest{
		Name:        "wk",
		Description: "x",
		Scopes:      []string{"chat:write"},
	})
	if !errors.Is(err, wantErr) {
		t.Errorf("errors.Is(err, wantErr) = false, want true; got %v", err)
	}
}

// TestCreateApp_CredsSink_NilOption_NoOps asserts that
// WithCreateAppCredsSink(nil) is silently ignored — the convention
// across every other functional option in the package (see
// WithInstallTokenSink for the prior art).
func TestCreateApp_CredsSink_NilOption_NoOps(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true,"app_id":"A0123ABCDEF","credentials":{}}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("xoxe.xoxp-1-test")),
		WithCreateAppCredsSink(nil),
	)
	if _, err := c.CreateApp(context.Background(), messenger.AppManifest{
		Name:   "wk",
		Scopes: []string{"chat:write"},
	}); err != nil {
		t.Errorf("CreateApp: %v", err)
	}
}
