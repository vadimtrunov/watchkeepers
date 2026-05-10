package jira

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// recordingTransport is the http.RoundTripper used by the whitelist
// enforcement tests. Every call increments .calls; the response is
// deliberately a static success — but the contract under test is
// that .calls remains zero on the rejection paths (the rejection
// happens BEFORE the network).
type recordingTransport struct {
	calls atomic.Int32
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.calls.Add(1)
	return &http.Response{
		StatusCode: http.StatusNoContent,
		Body:       http.NoBody,
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

func TestUpdateFields_NoWhitelist_RefusesAllWrites(t *testing.T) {
	t.Parallel()
	rt := &recordingTransport{}
	c, _ := newTestClient(
		t,
		func(_ http.ResponseWriter, _ *http.Request) {
			t.Fatalf("HTTP must not be called when no whitelist is configured")
		},
		WithHTTPClient(&http.Client{Transport: rt}),
	)
	err := c.UpdateFields(context.Background(), "PROJ-1", map[string]any{
		"summary": "new title",
	})
	if !errors.Is(err, ErrFieldNotWhitelisted) {
		t.Fatalf("err = %v; want ErrFieldNotWhitelisted (fail-closed default with no whitelist)", err)
	}
	if rt.calls.Load() != 0 {
		t.Errorf("transport called %d times; the rejection must happen BEFORE the network", rt.calls.Load())
	}
}

func TestUpdateFields_NotInWhitelist_RefusedSynchronously(t *testing.T) {
	t.Parallel()
	rt := &recordingTransport{}
	c, _ := newTestClient(
		t,
		func(_ http.ResponseWriter, _ *http.Request) {
			t.Fatalf("HTTP must not be called for non-whitelisted field")
		},
		WithHTTPClient(&http.Client{Transport: rt}),
		WithFieldWhitelist("summary", "labels"),
	)
	err := c.UpdateFields(context.Background(), "PROJ-1", map[string]any{
		"description": "new desc",
	})
	if !errors.Is(err, ErrFieldNotWhitelisted) {
		t.Fatalf("err = %v; want ErrFieldNotWhitelisted", err)
	}
	if !strings.Contains(err.Error(), "description") {
		t.Errorf("err = %q; expected the offending field name to appear", err)
	}
	if rt.calls.Load() != 0 {
		t.Errorf("transport called %d times; rejection must precede the network", rt.calls.Load())
	}
}

func TestUpdateFields_PartiallyAllowed_FailsClosedOnAnyOffender(t *testing.T) {
	t.Parallel()
	rt := &recordingTransport{}
	c, _ := newTestClient(
		t,
		func(_ http.ResponseWriter, _ *http.Request) {
			t.Fatalf("HTTP must not be called when one field is non-whitelisted")
		},
		WithHTTPClient(&http.Client{Transport: rt}),
		WithFieldWhitelist("summary"),
	)
	err := c.UpdateFields(context.Background(), "PROJ-1", map[string]any{
		"summary":     "ok",
		"description": "blocked",
	})
	if !errors.Is(err, ErrFieldNotWhitelisted) {
		t.Fatalf("err = %v; want ErrFieldNotWhitelisted", err)
	}
	if rt.calls.Load() != 0 {
		t.Errorf("transport called %d times; partial-allowed must still be rejected entirely", rt.calls.Load())
	}
}

func TestUpdateFields_NotInWhitelist_AllOffendersSorted(t *testing.T) {
	t.Parallel()
	rt := &recordingTransport{}
	c, _ := newTestClient(
		t,
		func(_ http.ResponseWriter, _ *http.Request) {
			t.Fatalf("HTTP must not be called for any non-whitelisted offender")
		},
		WithHTTPClient(&http.Client{Transport: rt}),
		WithFieldWhitelist("summary"),
	)
	err := c.UpdateFields(context.Background(), "PROJ-1", map[string]any{
		"description": "blocked",
		"assignee":    "someone",
		"labels":      []string{"x"},
	})
	if !errors.Is(err, ErrFieldNotWhitelisted) {
		t.Fatalf("err = %v; want ErrFieldNotWhitelisted", err)
	}
	msg := err.Error()
	for _, want := range []string{"assignee", "description", "labels"} {
		if !strings.Contains(msg, want) {
			t.Errorf("err = %q; expected to mention every offending field %q (sorted)", msg, want)
		}
	}
	// Sorted (alphabetical) ordering pin: assignee < description < labels.
	a := strings.Index(msg, "assignee")
	d := strings.Index(msg, "description")
	l := strings.Index(msg, "labels")
	if a < 0 || a >= d || d >= l {
		t.Errorf("err = %q; expected sorted offender order assignee < description < labels", msg)
	}
}

func TestUpdateFields_RejectsMalformedKey_NoNetwork(t *testing.T) {
	t.Parallel()
	rt := &recordingTransport{}
	c, _ := newTestClient(
		t,
		func(_ http.ResponseWriter, _ *http.Request) {
			t.Fatalf("HTTP must not be called for malformed key")
		},
		WithHTTPClient(&http.Client{Transport: rt}),
		WithFieldWhitelist("summary"),
	)
	err := c.UpdateFields(context.Background(), "../../escape", map[string]any{"summary": "x"})
	if !errors.Is(err, ErrInvalidArgs) {
		t.Fatalf("err = %v; want ErrInvalidArgs", err)
	}
	if rt.calls.Load() != 0 {
		t.Errorf("transport called %d times; malformed-key rejection must precede the network", rt.calls.Load())
	}
}

func TestUpdateFields_AllWhitelisted_HappyPath(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(
		t,
		func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "PUT" {
				t.Errorf("Method = %s; want PUT", r.Method)
			}
			if r.URL.Path != "/rest/api/3/issue/PROJ-1" {
				t.Errorf("Path = %s", r.URL.Path)
			}
			var got map[string]any
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			fields, ok := got["fields"].(map[string]any)
			if !ok {
				t.Fatalf("body.fields is not an object: %v", got)
			}
			if fields["summary"] != "new title" {
				t.Errorf("fields.summary = %v; want \"new title\"", fields["summary"])
			}
			if fields["labels"] == nil {
				t.Errorf("fields.labels missing")
			}
			w.WriteHeader(http.StatusNoContent)
		},
		WithFieldWhitelist("summary", "labels"),
	)
	err := c.UpdateFields(context.Background(), "PROJ-1", map[string]any{
		"summary": "new title",
		"labels":  []string{"alpha", "beta"},
	})
	if err != nil {
		t.Fatalf("UpdateFields: %v", err)
	}
}

func TestUpdateFields_RejectsEmptyKey(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(
		t,
		func(_ http.ResponseWriter, _ *http.Request) {
			t.Fatalf("HTTP must not be called for empty key")
		},
		WithFieldWhitelist("summary"),
	)
	err := c.UpdateFields(context.Background(), "", map[string]any{"summary": "x"})
	if !errors.Is(err, ErrInvalidArgs) {
		t.Fatalf("err = %v; want ErrInvalidArgs", err)
	}
}

func TestUpdateFields_RejectsNilFields(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(
		t,
		func(_ http.ResponseWriter, _ *http.Request) {
			t.Fatalf("HTTP must not be called for nil fields")
		},
		WithFieldWhitelist("summary"),
	)
	err := c.UpdateFields(context.Background(), "PROJ-1", nil)
	if !errors.Is(err, ErrInvalidArgs) {
		t.Fatalf("err = %v; want ErrInvalidArgs", err)
	}
}

func TestUpdateFields_RejectsEmptyFields(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(
		t,
		func(_ http.ResponseWriter, _ *http.Request) {
			t.Fatalf("HTTP must not be called for empty fields map")
		},
		WithFieldWhitelist("summary"),
	)
	err := c.UpdateFields(context.Background(), "PROJ-1", map[string]any{})
	if !errors.Is(err, ErrInvalidArgs) {
		t.Fatalf("err = %v; want ErrInvalidArgs", err)
	}
}

func TestUpdateFields_404Wrapped(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(
		t,
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"errorMessages":["Issue does not exist."]}`)
		},
		WithFieldWhitelist("summary"),
	)
	err := c.UpdateFields(context.Background(), "PROJ-99", map[string]any{"summary": "x"})
	if !errors.Is(err, ErrIssueNotFound) {
		t.Fatalf("err = %v; want ErrIssueNotFound", err)
	}
}

// TestUpdateFields_WhitelistEnforcement_SourceGrep pins the
// "rejection BEFORE network" contract via a source-grep mutation
// test. Per the M7.1.c.a lesson, source-grep beats hollow runtime-
// mock assertions for negative ACs ("does NOT call X"). The
// whitelist check (`fieldWhitelistContains`) MUST appear before the
// `c.do(` call inside [Client.UpdateFields].
//
// Without this AC a future refactor could move the whitelist check
// after `do` and the runtime tests above would still pass on the
// happy paths; the source-grep makes the precondition load-bearing.
func TestUpdateFields_WhitelistEnforcement_SourceGrep(t *testing.T) {
	t.Parallel()
	const target = "update_fields.go"
	src, err := readFile(target)
	if err != nil {
		t.Fatalf("read %s: %v", target, err)
	}
	stripped := stripGoComments(src)
	wlIdx := strings.Index(stripped, "fieldWhitelistContains")
	doIdx := strings.Index(stripped, "c.do(")
	if wlIdx == -1 {
		t.Fatal("update_fields.go does not invoke fieldWhitelistContains")
	}
	if doIdx == -1 {
		t.Fatal("update_fields.go does not invoke c.do — operation must drive transport")
	}
	if wlIdx > doIdx {
		t.Errorf("fieldWhitelistContains (idx=%d) must precede c.do (idx=%d) in source order — whitelist is the BEFORE-network guard", wlIdx, doIdx)
	}
}

// readFile is a thin wrapper to keep the import surface of this test
// file small (we only need os.ReadFile).
func readFile(path string) (string, error) {
	b, err := osReadFile(path)
	return string(b), err
}
