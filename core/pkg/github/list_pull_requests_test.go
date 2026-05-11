package github

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestListPullRequests_RejectsMalformedOwner(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatalf("HTTP must not be called for malformed owner")
	})
	_, err := c.ListPullRequests(context.Background(), "bad owner", "repo", ListPullRequestsOptions{})
	if !errors.Is(err, ErrInvalidArgs) {
		t.Fatalf("err = %v; want wrapped ErrInvalidArgs", err)
	}
}

func TestListPullRequests_RejectsMalformedRepo(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatalf("HTTP must not be called for malformed repo")
	})
	_, err := c.ListPullRequests(context.Background(), "owner", "bad/repo", ListPullRequestsOptions{})
	if !errors.Is(err, ErrInvalidArgs) {
		t.Fatalf("err = %v; want wrapped ErrInvalidArgs", err)
	}
}

func TestListPullRequests_PathAndQuery(t *testing.T) {
	t.Parallel()
	type capture struct {
		method, path, query string
	}
	var seen capture
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		seen.method = r.Method
		seen.path = r.URL.Path
		seen.query = r.URL.RawQuery
		_, _ = io.WriteString(w, `[]`)
	})
	_, err := c.ListPullRequests(context.Background(), "octocat", "Hello-World", ListPullRequestsOptions{
		State:     "open",
		PerPage:   100,
		Page:      3,
		Sort:      "updated",
		Direction: "asc",
	})
	if err != nil {
		t.Fatalf("ListPullRequests: %v", err)
	}
	if seen.method != "GET" {
		t.Errorf("Method = %s; want GET", seen.method)
	}
	if seen.path != "/repos/octocat/Hello-World/pulls" {
		t.Errorf("Path = %s", seen.path)
	}
	for _, want := range []string{"state=open", "per_page=100", "page=3", "sort=updated", "direction=asc"} {
		if !strings.Contains(seen.query, want) {
			t.Errorf("query missing %q; got %q", want, seen.query)
		}
	}
}

func TestListPullRequests_OmitsEmptyOptionalFields(t *testing.T) {
	t.Parallel()
	var seenQuery string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		seenQuery = r.URL.RawQuery
		_, _ = io.WriteString(w, `[]`)
	})
	_, err := c.ListPullRequests(context.Background(), "owner", "repo", ListPullRequestsOptions{})
	if err != nil {
		t.Fatalf("ListPullRequests: %v", err)
	}
	if seenQuery != "" {
		t.Errorf("query = %q; want empty when all options are zero", seenQuery)
	}
}

func TestListPullRequests_HappyPath_ResponseDecode(t *testing.T) {
	t.Parallel()
	const respJSON = `[
		{
			"number": 42,
			"title": "fix the thing",
			"state": "open",
			"html_url": "https://github.com/o/r/pull/42",
			"created_at": "2024-09-15T14:30:00Z",
			"updated_at": "2024-09-15T15:00:00Z",
			"user": {"login": "alice"},
			"requested_reviewers": [{"login": "bob"}, {"login": "carol"}]
		},
		{
			"number": 43,
			"title": "deleted user PR",
			"state": "open",
			"html_url": "https://github.com/o/r/pull/43",
			"created_at": "2024-09-16T14:30:00Z",
			"updated_at": "2024-09-16T15:00:00Z",
			"user": null,
			"requested_reviewers": []
		}
	]`
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, respJSON)
	})
	res, err := c.ListPullRequests(context.Background(), "o", "r", ListPullRequestsOptions{})
	if err != nil {
		t.Fatalf("ListPullRequests: %v", err)
	}
	if len(res.PullRequests) != 2 {
		t.Fatalf("PullRequests len = %d; want 2", len(res.PullRequests))
	}
	first := res.PullRequests[0]
	wantFirst := struct {
		Number             int
		Title, AuthorLogin string
		Reviewers          []string
	}{42, "fix the thing", "alice", []string{"bob", "carol"}}
	gotFirst := struct {
		Number             int
		Title, AuthorLogin string
		Reviewers          []string
	}{first.Number, first.Title, first.AuthorLogin, first.RequestedReviewers}
	if gotFirst.Number != wantFirst.Number || gotFirst.Title != wantFirst.Title || gotFirst.AuthorLogin != wantFirst.AuthorLogin {
		t.Errorf("first PR scalar fields mismatch: got %+v want %+v", gotFirst, wantFirst)
	}
	if len(gotFirst.Reviewers) != len(wantFirst.Reviewers) || gotFirst.Reviewers[0] != "bob" || gotFirst.Reviewers[1] != "carol" {
		t.Errorf("first PR reviewers = %v; want %v", gotFirst.Reviewers, wantFirst.Reviewers)
	}
	if first.UpdatedAt.IsZero() {
		t.Errorf("first.UpdatedAt should be parsed")
	}
	second := res.PullRequests[1]
	if second.AuthorLogin != "" {
		t.Errorf("second.AuthorLogin = %q; want empty for null user", second.AuthorLogin)
	}
	if len(second.RequestedReviewers) != 0 {
		t.Errorf("second.RequestedReviewers = %v; want empty for empty array", second.RequestedReviewers)
	}
}

func TestListPullRequests_LinkHeaderNextPage(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Link",
			`<https://api.github.com/repos/o/r/pulls?per_page=100&page=2>; rel="next", `+
				`<https://api.github.com/repos/o/r/pulls?per_page=100&page=10>; rel="last"`)
		_, _ = io.WriteString(w, `[]`)
	})
	res, err := c.ListPullRequests(context.Background(), "o", "r", ListPullRequestsOptions{})
	if err != nil {
		t.Fatalf("ListPullRequests: %v", err)
	}
	if res.NextPage != 2 {
		t.Errorf("NextPage = %d; want 2", res.NextPage)
	}
}

func TestListPullRequests_NoLinkHeader_NextPageZero(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[]`)
	})
	res, err := c.ListPullRequests(context.Background(), "o", "r", ListPullRequestsOptions{})
	if err != nil {
		t.Fatalf("ListPullRequests: %v", err)
	}
	if res.NextPage != 0 {
		t.Errorf("NextPage = %d; want 0", res.NextPage)
	}
}

func TestListPullRequests_LinkHeaderOnlyFirstLast_NextPageZero(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Link",
			`<https://api.github.com/repos/o/r/pulls?page=1>; rel="first", `+
				`<https://api.github.com/repos/o/r/pulls?page=5>; rel="last"`)
		_, _ = io.WriteString(w, `[]`)
	})
	res, err := c.ListPullRequests(context.Background(), "o", "r", ListPullRequestsOptions{})
	if err != nil {
		t.Fatalf("ListPullRequests: %v", err)
	}
	if res.NextPage != 0 {
		t.Errorf("NextPage = %d; want 0 (no rel=next)", res.NextPage)
	}
}

func TestListPullRequests_LinkHeaderRelNextWithoutPage_SurfacesError(t *testing.T) {
	t.Parallel()
	// Iter-1 codex Major #1: a rel="next" marker with no parseable
	// page= MUST surface an error rather than silently truncate.
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Link", `<https://api.github.com/repos/o/r/pulls>; rel="next"`)
		_, _ = io.WriteString(w, `[]`)
	})
	_, err := c.ListPullRequests(context.Background(), "o", "r", ListPullRequestsOptions{})
	if !errors.Is(err, ErrInvalidArgs) {
		t.Fatalf("err = %v; want wrapped ErrInvalidArgs (Link rel=\"next\" without parseable page= must fail loudly)", err)
	}
}

func TestListPullRequests_LinkHeaderCaseInsensitiveRelNext(t *testing.T) {
	t.Parallel()
	// Iter-1 critic Major #2: RFC 8288 §3 defines `rel` values as
	// case-insensitive. The parser MUST accept `Next` and `NEXT`.
	for _, hdr := range []string{
		`<https://api.github.com/repos/o/r/pulls?page=2>; rel="Next"`,
		`<https://api.github.com/repos/o/r/pulls?page=3>; rel="NEXT"`,
	} {
		hdr := hdr
		t.Run(hdr, func(t *testing.T) {
			t.Parallel()
			c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Link", hdr)
				_, _ = io.WriteString(w, `[]`)
			})
			res, err := c.ListPullRequests(context.Background(), "o", "r", ListPullRequestsOptions{})
			if err != nil {
				t.Fatalf("ListPullRequests: %v", err)
			}
			if res.NextPage == 0 {
				t.Errorf("NextPage = 0 on case-insensitive rel match; want non-zero")
			}
		})
	}
}

func TestListPullRequests_RejectsNegativePage(t *testing.T) {
	t.Parallel()
	// Iter-1 codex Minor #1: negative Page must reject explicitly
	// rather than silently coerce to GitHub's default first page.
	c, _ := newTestClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatalf("HTTP must not be called for negative Page")
	})
	_, err := c.ListPullRequests(context.Background(), "o", "r", ListPullRequestsOptions{Page: -1})
	if !errors.Is(err, ErrInvalidArgs) {
		t.Errorf("err = %v; want wrapped ErrInvalidArgs", err)
	}
}

func TestListPullRequests_RejectsNegativePerPage(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatalf("HTTP must not be called for negative PerPage")
	})
	_, err := c.ListPullRequests(context.Background(), "o", "r", ListPullRequestsOptions{PerPage: -1})
	if !errors.Is(err, ErrInvalidArgs) {
		t.Errorf("err = %v; want wrapped ErrInvalidArgs", err)
	}
}

func TestListPullRequests_GHESPathPrefixComposedCorrectly(t *testing.T) {
	t.Parallel()
	// Iter-1 critic Missing: confirm the `https://<ghes-host>/api/v3`
	// base URL composes to `/api/v3/repos/o/r/pulls` rather than
	// stripping the `/api/v3` prefix or doubling the slash.
	var seenPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		_, _ = io.WriteString(w, `[]`)
	}))
	t.Cleanup(srv.Close)
	// httptest servers don't normally carry a path prefix; simulate by
	// passing the base URL with the prefix and asserting the
	// composed request lands on `/api/v3/repos/...`.
	c, err := NewClient(
		WithBaseURL(srv.URL+"/api/v3"),
		WithTokenSource(StaticToken{Value: "t"}),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = c.ListPullRequests(context.Background(), "octo", "Hello-World", ListPullRequestsOptions{})
	if err != nil {
		t.Fatalf("ListPullRequests: %v", err)
	}
	if seenPath != "/api/v3/repos/octo/Hello-World/pulls" {
		t.Errorf("seenPath = %q; want /api/v3/repos/octo/Hello-World/pulls (GHES path-prefix composition)", seenPath)
	}
}

func TestListPullRequests_RepoNotFoundOnRepoEndpoint(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found","documentation_url":"https://docs.github.com/..."}`)
	})
	_, err := c.ListPullRequests(context.Background(), "o", "missing", ListPullRequestsOptions{})
	if !errors.Is(err, ErrRepoNotFound) {
		t.Fatalf("err = %v; want wrapped ErrRepoNotFound", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v; want APIError in chain", err)
	}
	if apiErr.DocumentationURL == "" {
		t.Errorf("DocumentationURL = empty; want populated from envelope")
	}
}
