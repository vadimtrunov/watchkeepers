package jira

import (
	"errors"
	"testing"
	"time"
)

func TestWithFieldWhitelist_DeduplicatesAndDropsEmpty(t *testing.T) {
	t.Parallel()
	c, err := NewClient(
		WithBaseURL("https://example.atlassian.net"),
		WithBasicAuth(StaticBasicAuth{Email: "a", Token: "b"}),
		WithFieldWhitelist("summary", "summary", "", "labels"),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	wl := c.cfg.fieldWhitelist
	if len(wl) != 2 {
		t.Fatalf("whitelist size = %d; want 2 (summary, labels)", len(wl))
	}
	for _, k := range []string{"summary", "labels"} {
		if _, ok := wl[k]; !ok {
			t.Errorf("whitelist missing %q", k)
		}
	}
	if _, ok := wl[""]; ok {
		t.Errorf("whitelist contains empty string; should be silently dropped")
	}
}

func TestWithFieldWhitelist_NoArgsResetsToFailClosed(t *testing.T) {
	t.Parallel()
	c, err := NewClient(
		WithBaseURL("https://example.atlassian.net"),
		WithBasicAuth(StaticBasicAuth{Email: "a", Token: "b"}),
		WithFieldWhitelist("summary"),
		WithFieldWhitelist(),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if len(c.cfg.fieldWhitelist) != 0 {
		t.Errorf("whitelist size = %d; want 0 — empty WithFieldWhitelist() must RESET (not preserve) any earlier non-empty config", len(c.cfg.fieldWhitelist))
	}
}

func TestWithFieldWhitelist_LastCallReplacesPrior(t *testing.T) {
	t.Parallel()
	c, err := NewClient(
		WithBaseURL("https://example.atlassian.net"),
		WithBasicAuth(StaticBasicAuth{Email: "a", Token: "b"}),
		WithFieldWhitelist("summary", "description", "labels"),
		WithFieldWhitelist("labels"),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if len(c.cfg.fieldWhitelist) != 1 {
		t.Fatalf("whitelist size = %d; want 1 — last WithFieldWhitelist call must REPLACE (not union with) prior", len(c.cfg.fieldWhitelist))
	}
	if _, ok := c.cfg.fieldWhitelist["labels"]; !ok {
		t.Error("expected only 'labels' after replacement")
	}
	if _, ok := c.cfg.fieldWhitelist["summary"]; ok {
		t.Error("'summary' from earlier call must NOT survive replacement (tamper-resistance)")
	}
}

func TestWithHTTPClient_NilIgnored(t *testing.T) {
	t.Parallel()
	c, err := NewClient(
		WithBaseURL("https://example.atlassian.net"),
		WithBasicAuth(StaticBasicAuth{Email: "a", Token: "b"}),
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
		WithBaseURL("https://example.atlassian.net"),
		WithBasicAuth(StaticBasicAuth{Email: "a", Token: "b"}),
		WithLogger(nil),
	)
	if err != nil {
		t.Fatalf("NewClient with nil logger: %v", err)
	}
}

func TestWithClock_NilIgnored(t *testing.T) {
	t.Parallel()
	c, err := NewClient(
		WithBaseURL("https://example.atlassian.net"),
		WithBasicAuth(StaticBasicAuth{Email: "a", Token: "b"}),
		WithClock(nil),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c.cfg.clock == nil {
		t.Error("clock is nil after WithClock(nil); expected default")
	}
	got := c.cfg.clock()
	if got.IsZero() {
		t.Error("default clock returned zero time")
	}
}

func TestWithClock_OverrideRoundTrip(t *testing.T) {
	t.Parallel()
	pinned := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	c, err := NewClient(
		WithBaseURL("https://example.atlassian.net"),
		WithBasicAuth(StaticBasicAuth{Email: "a", Token: "b"}),
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
		WithBaseURL("example.atlassian.net"),
		WithBasicAuth(StaticBasicAuth{Email: "a", Token: "b"}),
	)
	if !errors.Is(err, ErrInvalidBaseURL) {
		t.Errorf("err = %v; want wrapped ErrInvalidBaseURL for schemeless URL (distinguishes invalid from missing)", err)
	}
}

func TestWithBaseURL_RejectsPathPrefix(t *testing.T) {
	t.Parallel()
	_, err := NewClient(
		WithBaseURL("https://example.atlassian.net/wiki"),
		WithBasicAuth(StaticBasicAuth{Email: "a", Token: "b"}),
	)
	if !errors.Is(err, ErrInvalidBaseURL) {
		t.Errorf("err = %v; want wrapped ErrInvalidBaseURL for URL with path prefix — would prepend to every REST call and silently route wrong (Confluence subpath, LB rewrite, …)", err)
	}
}

func TestWithBaseURL_AcceptsBareHostAndTrailingSlash(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"https://example.atlassian.net", "https://example.atlassian.net/"} {
		_, err := NewClient(
			WithBaseURL(raw),
			WithBasicAuth(StaticBasicAuth{Email: "a", Token: "b"}),
		)
		if err != nil {
			t.Errorf("WithBaseURL(%q) = %v; want accepted", raw, err)
		}
	}
}
