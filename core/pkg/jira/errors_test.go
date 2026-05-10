package jira

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestAPIError_Error_Format(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  *APIError
		want string
	}{
		{
			name: "full",
			err:  &APIError{Status: 404, Method: "GET", Endpoint: "/rest/api/3/issue/PROJ-1", Messages: []string{"Issue does not exist."}},
			want: `jira: api error: method="GET" endpoint="/rest/api/3/issue/PROJ-1" status=404 msg=Issue does not exist.`,
		},
		{
			name: "no-messages",
			err:  &APIError{Status: 500, Method: "GET", Endpoint: "/foo"},
			want: `jira: api error: method="GET" endpoint="/foo" status=500`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Errorf("Error() = %q; want %q", got, tc.want)
			}
		})
	}
}

func TestAPIError_Error_TruncatesLongMessage(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 500)
	e := &APIError{Status: 400, Method: "POST", Endpoint: "/x", Messages: []string{long}}
	got := e.Error()
	if len(got) > 400 { // 200-byte truncation + framing chrome
		t.Errorf("Error() length = %d; want bounded around 200 + chrome", len(got))
	}
	if !strings.Contains(got, "…") {
		t.Errorf("Error() = %q; expected ellipsis after truncation", got)
	}
}

func TestAPIError_Error_NilSafe(t *testing.T) {
	t.Parallel()
	var e *APIError
	if got := e.Error(); got != "jira: <nil APIError>" {
		t.Errorf("nil APIError Error() = %q", got)
	}
}

func TestAPIError_Unwrap_Mapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		err    *APIError
		target error
	}{
		{"401 → InvalidAuth", &APIError{Status: 401, kind: endpointGeneric}, ErrInvalidAuth},
		{"403 → InvalidAuth", &APIError{Status: 403, kind: endpointGeneric}, ErrInvalidAuth},
		{"429 → RateLimited", &APIError{Status: 429, kind: endpointGeneric}, ErrRateLimited},
		{"404 issue → IssueNotFound", &APIError{Status: 404, kind: endpointIssue}, ErrIssueNotFound},
		{"400 search w/ jql marker → InvalidJQL", &APIError{Status: 400, kind: endpointSearch, Messages: []string{"The JQL query is invalid: foo"}}, ErrInvalidJQL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !errors.Is(tc.err, tc.target) {
				t.Errorf("errors.Is(%+v, %v) = false; want true", tc.err, tc.target)
			}
		})
	}
}

func TestAPIError_Unwrap_404OnNonIssueEndpoint_NoSentinel(t *testing.T) {
	t.Parallel()
	e := &APIError{Status: 404, kind: endpointGeneric}
	if errors.Is(e, ErrIssueNotFound) {
		t.Errorf("404 on generic endpoint must NOT match ErrIssueNotFound")
	}
}

func TestAPIError_Unwrap_400OnNonSearchEndpoint_NoSentinel(t *testing.T) {
	t.Parallel()
	e := &APIError{Status: 400, kind: endpointGeneric, Messages: []string{"The JQL query is invalid"}}
	if errors.Is(e, ErrInvalidJQL) {
		t.Errorf("400 on generic endpoint must NOT match ErrInvalidJQL even with JQL-like message")
	}
}

func TestAPIError_Unwrap_400OnSearchWithoutJQLMarker_NoSentinel(t *testing.T) {
	t.Parallel()
	e := &APIError{Status: 400, kind: endpointSearch, Messages: []string{"The next page token is invalid"}}
	if errors.Is(e, ErrInvalidJQL) {
		t.Errorf("400 on /search/jql without 'jql' in messages must NOT alias ErrInvalidJQL — distinguishes operator-bad-JQL from cursor/bug")
	}
}

func TestAPIError_Unwrap_NilSafe(t *testing.T) {
	t.Parallel()
	var e *APIError
	if got := e.Unwrap(); got != nil {
		t.Errorf("nil APIError Unwrap() = %v; want nil", got)
	}
}

func TestAPIError_RetryAfter_Independent(t *testing.T) {
	t.Parallel()
	e := &APIError{Status: 429, RetryAfter: 5 * time.Second}
	if !errors.Is(e, ErrRateLimited) {
		t.Errorf("Is(ErrRateLimited) = false")
	}
	if e.RetryAfter != 5*time.Second {
		t.Errorf("RetryAfter = %v", e.RetryAfter)
	}
}
