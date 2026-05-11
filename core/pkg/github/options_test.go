package github

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestWithHTTPClient_NilIgnored(t *testing.T) {
	t.Parallel()
	c, err := NewClient(
		WithTokenSource(StaticToken{Value: "t"}),
		WithHTTPClient(nil),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c.cfg.httpClient == nil {
		t.Error("httpClient is nil after WithHTTPClient(nil); expected default")
	}
}

func TestWithLogger_NilIgnored(t *testing.T) {
	t.Parallel()
	_, err := NewClient(
		WithTokenSource(StaticToken{Value: "t"}),
		WithLogger(nil),
	)
	if err != nil {
		t.Fatalf("NewClient with nil logger: %v", err)
	}
}

func TestWithClock_NilIgnored(t *testing.T) {
	t.Parallel()
	c, err := NewClient(
		WithTokenSource(StaticToken{Value: "t"}),
		WithClock(nil),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c.cfg.clock == nil {
		t.Error("clock is nil after WithClock(nil); expected default")
	}
	if got := c.cfg.clock(); got.IsZero() {
		t.Error("default clock returned zero time")
	}
}

func TestWithClock_OverrideRoundTrip(t *testing.T) {
	t.Parallel()
	pinned := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	c, err := NewClient(
		WithTokenSource(StaticToken{Value: "t"}),
		WithClock(func() time.Time { return pinned }),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if got := c.cfg.clock(); !got.Equal(pinned) {
		t.Errorf("clock() = %v; want %v", got, pinned)
	}
}

func TestWithBaseURL_RejectsSchemeless(t *testing.T) {
	t.Parallel()
	_, err := NewClient(
		WithBaseURL("api.github.com"),
		WithTokenSource(StaticToken{Value: "t"}),
	)
	if !errors.Is(err, ErrInvalidBaseURL) {
		t.Errorf("err = %v; want wrapped ErrInvalidBaseURL for schemeless URL", err)
	}
}

func TestWithBaseURL_AcceptsBareHostAndGHES(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"https://api.github.com",
		"https://api.github.com/",
		"https://github.example.com/api/v3",
	} {
		_, err := NewClient(
			WithBaseURL(raw),
			WithTokenSource(StaticToken{Value: "t"}),
		)
		if err != nil {
			t.Errorf("WithBaseURL(%q) = %v; want accepted", raw, err)
		}
	}
}

func TestWithHTTPClient_OverrideUsed(t *testing.T) {
	t.Parallel()
	tuned := &http.Client{Timeout: time.Second}
	c, err := NewClient(
		WithTokenSource(StaticToken{Value: "t"}),
		WithHTTPClient(tuned),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c.cfg.httpClient != tuned {
		t.Error("WithHTTPClient did not replace the default *http.Client")
	}
}
