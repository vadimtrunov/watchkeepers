package github

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestAPIError_NilSafe(t *testing.T) {
	t.Parallel()
	var e *APIError
	if got := e.Error(); got != "github: <nil APIError>" {
		t.Errorf("nil APIError.Error() = %q; want sentinel", got)
	}
	if got := e.Unwrap(); got != nil {
		t.Errorf("nil APIError.Unwrap() = %v; want nil", got)
	}
}

func TestAPIError_Error_FormatIncludesMessageTruncated(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 300)
	e := &APIError{Status: 400, Method: "GET", Endpoint: "/repos/o/r/pulls", Message: long}
	got := e.Error()
	for _, want := range []string{`method="GET"`, `endpoint="/repos/o/r/pulls"`, `status=400`, "msg=", "…"} {
		if !strings.Contains(got, want) {
			t.Errorf("Error() = %q; missing %q", got, want)
		}
	}
}

func TestAPIError_UnwrapMatrix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		status int
		kind   endpointKind
		remain int
		want   error
	}{
		{"401 → ErrInvalidAuth", 401, endpointGeneric, 4999, ErrInvalidAuth},
		{"429 → ErrRateLimited", 429, endpointGeneric, 0, ErrRateLimited},
		{"403 remaining=0 → ErrRateLimited", 403, endpointGeneric, 0, ErrRateLimited},
		{"403 remaining>0 → unmapped", 403, endpointGeneric, 4999, nil},
		{"404 on repo → ErrRepoNotFound", 404, endpointRepo, -1, ErrRepoNotFound},
		{"404 generic → unmapped", 404, endpointGeneric, -1, nil},
		{"500 → unmapped", 500, endpointGeneric, -1, nil},
		{"200 (shouldn't happen) → unmapped", 200, endpointGeneric, -1, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := &APIError{Status: tc.status, kind: tc.kind, RateLimitRemaining: tc.remain}
			got := e.Unwrap()
			if tc.want == nil && got != nil {
				t.Errorf("Unwrap() = %v; want nil", got)
			}
			if tc.want != nil && !errors.Is(got, tc.want) {
				t.Errorf("Unwrap() = %v; want %v", got, tc.want)
			}
		})
	}
}

func TestAPIError_RetryAfterFromHeader(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	target := now.Add(90 * time.Second).Unix()
	got := parseResetHeader(strFromInt64(target), func() time.Time { return now })
	if got < 80*time.Second || got > 100*time.Second {
		t.Errorf("RetryAfter from X-RateLimit-Reset = %v; want ~90s", got)
	}
}
