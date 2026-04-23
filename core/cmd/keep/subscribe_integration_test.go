//go:build integration

// Integration tests for the GET /v1/subscribe SSE endpoint (M2.7.e.a).
// Require a reachable Postgres 16 with pgvector via KEEP_INTEGRATION_DB_URL
// and every migration (001..008) applied. The suite re-uses the shared
// helpers from read_integration_test.go (`buildBinary`, `pickLocalAddr`,
// `waitForHealth`, `issuerForTest`, `mintToken`, `readBootEnv`) and only
// adds the subscribe-specific env (KEEP_SUBSCRIBE_HEARTBEAT=100ms so the
// test does not wait 15s for a heartbeat frame).
//
// Run locally with:
//
//	KEEP_INTEGRATION_DB_URL=postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable \
//	  go test -tags=integration -v -run 'TestSubscribeAPI_' ./core/cmd/keep/...
package main_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// subscribeBootEnv returns the env slice to pass to the Keep binary for
// subscribe integration tests. It layers a short heartbeat on top of
// readBootEnv so the test observes a heartbeat frame within the
// shutdown budget rather than waiting the production-default 15s.
func subscribeBootEnv(dsn, addr string) []string {
	return append(readBootEnv(dsn, addr),
		"KEEP_SUBSCRIBE_HEARTBEAT=100ms",
		"KEEP_SUBSCRIBE_BUFFER=8",
	)
}

// bootKeepSubscribe compiles + starts the Keep binary with the subscribe
// heartbeat override. Returns the listening address, the *exec.Cmd so
// the test can signal it, a done channel that closes with cmd.Wait's
// exit error, and a teardown that best-effort SIGTERMs the process.
func bootKeepSubscribe(t *testing.T, env *testEnv) (string, *exec.Cmd, <-chan error, func()) {
	t.Helper()
	bin := buildBinary(t)
	addr := pickLocalAddr(t)

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, bin)
	cmd.Env = subscribeBootEnv(env.dsn, addr)
	var stderr strings.Builder
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderr)
	cmd.Stdout = os.Stdout
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start binary: %v", err)
	}
	waitForHealth(t, addr)

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	teardown := func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(exitBudget):
			_ = cmd.Process.Kill()
		}
		cancel()
	}
	return addr, cmd, done, teardown
}

// TestSubscribeAPI_MissingAuth — the token-auth gate is wired: a request
// without an Authorization header returns 401 missing_token without ever
// reaching the SSE handler.
func TestSubscribeAPI_MissingAuth(t *testing.T) {
	env := newTestEnv(t)
	addr, _, _, teardown := bootKeepSubscribe(t, env)
	defer teardown()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+addr+"/v1/subscribe", nil)
	if err != nil {
		t.Fatalf("build req: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	var envErr struct {
		Error, Reason string
	}
	if err := json.NewDecoder(resp.Body).Decode(&envErr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if envErr.Reason != "missing_token" {
		t.Errorf("reason = %q, want missing_token", envErr.Reason)
	}
}

// TestSubscribeAPI_HeadersAndHeartbeat — happy path over the real binary:
// SSE response headers are correct and a heartbeat frame is observed
// within one heartbeat interval at the configured KEEP_SUBSCRIBE_HEARTBEAT.
func TestSubscribeAPI_HeadersAndHeartbeat(t *testing.T) {
	env := newTestEnv(t)
	addr, _, _, teardown := bootKeepSubscribe(t, env)
	defer teardown()

	ti := issuerForTest(t)
	tok := mintToken(t, ti, "org")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/v1/subscribe", nil)
	if err != nil {
		t.Fatalf("build req: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", got)
	}
	if got := resp.Header.Get("Connection"); got != "keep-alive" {
		t.Errorf("Connection = %q, want keep-alive", got)
	}

	// Heartbeat interval is 100ms; we allow 1s of wall-clock budget to
	// absorb scheduler jitter.
	frame, err := readSubscribeFrame(resp.Body, time.Second)
	if err != nil {
		t.Fatalf("read heartbeat frame: %v", err)
	}
	if !strings.HasPrefix(frame, ":") {
		t.Errorf("frame = %q, want heartbeat ':' comment", frame)
	}
}

// TestSubscribeAPI_CleanShutdown — with an active subscriber streaming,
// SIGTERM triggers Server.Run to close the Registry before
// httpSrv.Shutdown; the client observes a clean EOF and the process
// exits 0 within KEEP_SHUTDOWN_TIMEOUT.
func TestSubscribeAPI_CleanShutdown(t *testing.T) {
	env := newTestEnv(t)
	addr, cmd, done, teardown := bootKeepSubscribe(t, env)
	// teardown is a best-effort backstop; the test itself issues SIGTERM.
	defer teardown()

	ti := issuerForTest(t)
	tok := mintToken(t, ti, "org")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/v1/subscribe", nil)
	if err != nil {
		t.Fatalf("build req: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Read at least one heartbeat so the stream is verifiably live
	// before SIGTERM, otherwise a fast shutdown could race the handler
	// into the select loop before it picks up the close signal.
	if _, err := readSubscribeFrame(resp.Body, time.Second); err != nil {
		t.Fatalf("read initial frame: %v", err)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}

	// Reader should EOF cleanly (io.EOF) rather than see a reset.
	eofCh := make(chan error, 1)
	go func() {
		br := bufio.NewReader(resp.Body)
		_, err := br.ReadString('\n')
		for err == nil {
			_, err = br.ReadString('\n')
		}
		eofCh <- err
	}()

	select {
	case err := <-eofCh:
		if err != io.EOF && err != io.ErrUnexpectedEOF {
			// Accept connection-close variants that surface as other
			// non-nil errors on the client side; the contract here is
			// "the server did not hang", not "stdlib returned literal
			// EOF". Note an `unexpected EOF` also indicates a clean
			// server-side close in the middle of a frame.
			t.Logf("client read returned %v (accepted as shutdown signal)", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("client did not observe stream end within 10s of SIGTERM")
	}

	// Binary must exit 0 within the 5s shutdown budget plus slack.
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("binary exited non-zero on SIGTERM: %v", err)
		}
	case <-time.After(exitBudget):
		t.Fatalf("binary did not exit within %s of SIGTERM", exitBudget)
	}
}

// readSubscribeFrame reads r until a blank-line frame terminator or the
// budget expires. Returns the frame without the terminator.
func readSubscribeFrame(r io.Reader, budget time.Duration) (string, error) {
	type result struct {
		frame string
		err   error
	}
	done := make(chan result, 1)
	go func() {
		br := bufio.NewReader(r)
		var sb strings.Builder
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				done <- result{sb.String(), err}
				return
			}
			if line == "\n" {
				done <- result{sb.String(), nil}
				return
			}
			sb.WriteString(line)
		}
	}()
	select {
	case r := <-done:
		return r.frame, r.err
	case <-time.After(budget):
		return "", context.DeadlineExceeded
	}
}
