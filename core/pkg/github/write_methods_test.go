package github

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// ---- GetRef ----

func TestGetRef_RejectsMalformedOwner(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatalf("HTTP must not be called")
	})
	_, err := c.GetRef(context.Background(), "bad owner", "repo", "heads/main")
	if !errors.Is(err, ErrInvalidArgs) {
		t.Fatalf("err=%v want ErrInvalidArgs", err)
	}
}

func TestGetRef_RejectsRefWithPrefix(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatalf("HTTP must not be called")
	})
	_, err := c.GetRef(context.Background(), "owner", "repo", "refs/heads/main")
	if !errors.Is(err, ErrInvalidArgs) {
		t.Fatalf("err=%v want ErrInvalidArgs", err)
	}
}

func TestGetRef_HappyPath_DecodeSHA(t *testing.T) {
	t.Parallel()
	const want = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	var seenPath string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		_, _ = io.WriteString(w, `{"ref":"refs/heads/main","object":{"sha":"`+want+`","type":"commit"}}`)
	})
	got, err := c.GetRef(context.Background(), "octocat", "Hello-World", "heads/main")
	if err != nil {
		t.Fatalf("GetRef: %v", err)
	}
	if got.SHA != want {
		t.Errorf("SHA=%q want %q", got.SHA, want)
	}
	if seenPath != "/repos/octocat/Hello-World/git/ref/heads/main" {
		t.Errorf("path=%q", seenPath)
	}
}

func TestGetRef_404RepoNotFound(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(404)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	})
	_, err := c.GetRef(context.Background(), "owner", "repo", "heads/missing")
	if !errors.Is(err, ErrRepoNotFound) {
		t.Fatalf("err=%v want ErrRepoNotFound", err)
	}
}

// ---- CreateRef ----

func TestCreateRef_RejectsRelativeRef(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatalf("HTTP must not be called")
	})
	_, err := c.CreateRef(context.Background(), CreateRefOptions{
		Owner: "o", Repo: "r", Ref: "heads/branch",
		SHA: strings.Repeat("a", 40),
	})
	if !errors.Is(err, ErrInvalidArgs) {
		t.Fatalf("err=%v want ErrInvalidArgs", err)
	}
}

func TestCreateRef_RejectsShortSHA(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatalf("HTTP must not be called")
	})
	_, err := c.CreateRef(context.Background(), CreateRefOptions{
		Owner: "o", Repo: "r", Ref: "refs/heads/branch", SHA: "abcd",
	})
	if !errors.Is(err, ErrInvalidArgs) {
		t.Fatalf("err=%v want ErrInvalidArgs", err)
	}
}

func TestCreateRef_HappyPath(t *testing.T) {
	t.Parallel()
	const sha = "0123456789abcdef0123456789abcdef01234567"
	var body createRefRequest
	var seenMethod, seenPath string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		seenMethod = r.Method
		seenPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(201)
		_, _ = io.WriteString(w, `{"ref":"refs/heads/share","object":{"sha":"`+sha+`","type":"commit"}}`)
	})
	got, err := c.CreateRef(context.Background(), CreateRefOptions{
		Owner: "o", Repo: "r", Ref: "refs/heads/share", SHA: sha,
	})
	if err != nil {
		t.Fatalf("CreateRef: %v", err)
	}
	if seenMethod != "POST" {
		t.Errorf("method=%q want POST", seenMethod)
	}
	if seenPath != "/repos/o/r/git/refs" {
		t.Errorf("path=%q", seenPath)
	}
	if body.Ref != "refs/heads/share" || body.SHA != sha {
		t.Errorf("body=%+v", body)
	}
	if got.SHA != sha {
		t.Errorf("res.SHA=%q", got.SHA)
	}
}

func TestCreateRef_422AlreadyExists(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(422)
		_, _ = io.WriteString(w, `{"message":"Reference already exists"}`)
	})
	_, err := c.CreateRef(context.Background(), CreateRefOptions{
		Owner: "o", Repo: "r", Ref: "refs/heads/exists", SHA: strings.Repeat("a", 40),
	})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err=%v is not *APIError", err)
	}
	if apiErr.Status != 422 {
		t.Errorf("status=%d want 422", apiErr.Status)
	}
}

// ---- CreateOrUpdateFile ----

func TestCreateOrUpdateFile_RejectsAbsolutePath(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatalf("HTTP must not be called")
	})
	_, err := c.CreateOrUpdateFile(context.Background(), CreateOrUpdateFileOptions{
		Owner: "o", Repo: "r", Path: "/etc/passwd",
		Content: []byte("x"), Message: "m", Branch: "b",
	})
	if !errors.Is(err, ErrInvalidArgs) {
		t.Fatalf("err=%v want ErrInvalidArgs", err)
	}
}

func TestCreateOrUpdateFile_RejectsDotDotSegment(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatalf("HTTP must not be called")
	})
	_, err := c.CreateOrUpdateFile(context.Background(), CreateOrUpdateFileOptions{
		Owner: "o", Repo: "r", Path: "a/../b",
		Content: []byte("x"), Message: "m", Branch: "b",
	})
	if !errors.Is(err, ErrInvalidArgs) {
		t.Fatalf("err=%v want ErrInvalidArgs", err)
	}
}

func TestCreateOrUpdateFile_HappyPath_Base64Encoded(t *testing.T) {
	t.Parallel()
	var body createOrUpdateFileRequest
	var seenMethod, seenPath string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		seenMethod = r.Method
		seenPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(201)
		_, _ = io.WriteString(w, `{"content":{"sha":"file-sha","path":"src/index.ts"},"commit":{"sha":"commit-sha"}}`)
	})
	got, err := c.CreateOrUpdateFile(context.Background(), CreateOrUpdateFileOptions{
		Owner: "o", Repo: "r", Path: "src/index.ts",
		Content: []byte("export default 1;"),
		Message: "add index", Branch: "share-1",
	})
	if err != nil {
		t.Fatalf("CreateOrUpdateFile: %v", err)
	}
	if seenMethod != "PUT" {
		t.Errorf("method=%q want PUT", seenMethod)
	}
	if seenPath != "/repos/o/r/contents/src/index.ts" {
		t.Errorf("path=%q", seenPath)
	}
	// base64-encoded content matches the original.
	if body.Content != "ZXhwb3J0IGRlZmF1bHQgMTs=" {
		t.Errorf("base64 content=%q", body.Content)
	}
	if body.Branch != "share-1" {
		t.Errorf("branch=%q", body.Branch)
	}
	if body.SHA != "" {
		t.Errorf("SHA must be empty on create: %q", body.SHA)
	}
	if got.FileSHA != "file-sha" || got.CommitSHA != "commit-sha" {
		t.Errorf("result=%+v", got)
	}
}

func TestCreateOrUpdateFile_PathSegmentsURLEscaped(t *testing.T) {
	t.Parallel()
	var seenPath string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		w.WriteHeader(201)
		_, _ = io.WriteString(w, `{"content":{"sha":""},"commit":{"sha":""}}`)
	})
	_, err := c.CreateOrUpdateFile(context.Background(), CreateOrUpdateFileOptions{
		Owner: "o", Repo: "r", Path: "src/special name.ts",
		Content: []byte{}, Message: "m", Branch: "b",
	})
	if err != nil {
		t.Fatalf("CreateOrUpdateFile: %v", err)
	}
	// Space inside the segment must be URL-encoded; `/` between
	// segments must be preserved verbatim.
	if !strings.Contains(seenPath, "src/special%20name.ts") {
		t.Errorf("path=%q should contain %q", seenPath, "src/special%20name.ts")
	}
}

func TestCreateOrUpdateFile_401InvalidAuth(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(401)
		_, _ = io.WriteString(w, `{"message":"Bad credentials"}`)
	})
	_, err := c.CreateOrUpdateFile(context.Background(), CreateOrUpdateFileOptions{
		Owner: "o", Repo: "r", Path: "a", Content: []byte{},
		Message: "m", Branch: "b",
	})
	if !errors.Is(err, ErrInvalidAuth) {
		t.Fatalf("err=%v want ErrInvalidAuth", err)
	}
}

// ---- CreatePullRequest ----

func TestCreatePullRequest_RejectsEmptyTitle(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatalf("HTTP must not be called")
	})
	_, err := c.CreatePullRequest(context.Background(), CreatePullRequestOptions{
		Owner: "o", Repo: "r", Title: "", Head: "h", Base: "b",
	})
	if !errors.Is(err, ErrInvalidArgs) {
		t.Fatalf("err=%v want ErrInvalidArgs", err)
	}
}

func TestCreatePullRequest_HappyPath(t *testing.T) {
	t.Parallel()
	var body createPullRequestRequest
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(201)
		_, _ = io.WriteString(w, `{"number":42,"html_url":"https://github.com/o/r/pull/42","node_id":"PR_kwDOAB"}`)
	})
	got, err := c.CreatePullRequest(context.Background(), CreatePullRequestOptions{
		Owner: "o", Repo: "r", Title: "share tool foo",
		Body: "promoting", Head: "share-foo", Base: "main",
	})
	if err != nil {
		t.Fatalf("CreatePullRequest: %v", err)
	}
	if got.Number != 42 {
		t.Errorf("Number=%d want 42", got.Number)
	}
	if got.HTMLURL != "https://github.com/o/r/pull/42" {
		t.Errorf("HTMLURL=%q", got.HTMLURL)
	}
	if body.Title != "share tool foo" || body.Head != "share-foo" || body.Base != "main" {
		t.Errorf("body=%+v", body)
	}
}

func TestCreatePullRequest_422NoDiff(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(422)
		_, _ = io.WriteString(w, `{"message":"No commits between main and share-foo"}`)
	})
	_, err := c.CreatePullRequest(context.Background(), CreatePullRequestOptions{
		Owner: "o", Repo: "r", Title: "t", Head: "share-foo", Base: "main",
	})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err=%v not *APIError", err)
	}
	if apiErr.Status != 422 {
		t.Errorf("status=%d want 422", apiErr.Status)
	}
}

func TestCreatePullRequest_RateLimited(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"message":"too many"}`)
	})
	_, err := c.CreatePullRequest(context.Background(), CreatePullRequestOptions{
		Owner: "o", Repo: "r", Title: "t", Head: "h", Base: "b",
	})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err=%v want ErrRateLimited", err)
	}
}

// ---- helpers exercise ----

func TestValidateSHA_OK(t *testing.T) {
	if err := validateSHA(strings.Repeat("a", 40)); err != nil {
		t.Fatalf("err=%v", err)
	}
}

func TestValidateSHA_UpperRejected(t *testing.T) {
	if err := validateSHA(strings.Repeat("A", 40)); !errors.Is(err, ErrInvalidArgs) {
		t.Fatalf("err=%v want ErrInvalidArgs", err)
	}
}

func TestValidateContentPath_Cases(t *testing.T) {
	cases := []struct {
		path string
		ok   bool
	}{
		{"a", true},
		{"a/b/c.txt", true},
		{"", false},
		{"/a", false},
		{"a//b", false},
		{"a/./b", false},
		{"a/../b", false},
		{strings.Repeat("a/", 600), false},
	}
	for _, c := range cases {
		err := validateContentPath(c.path)
		if c.ok && err != nil {
			t.Errorf("path=%q err=%v want nil", c.path, err)
		}
		if !c.ok && !errors.Is(err, ErrInvalidArgs) {
			t.Errorf("path=%q err=%v want ErrInvalidArgs", c.path, err)
		}
	}
}
