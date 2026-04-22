package server_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/internal/keep/config"
	"github.com/vadimtrunov/watchkeepers/core/internal/keep/server"
)

func TestHealthHandler_OK(t *testing.T) {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()

	server.HealthHandler().ServeHTTP(rec, req)

	resp := rec.Result()
	t.Cleanup(func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close body: %v", err)
		}
	})

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if strings.TrimSpace(string(body)) != `{"status":"ok"}` {
		t.Errorf("body = %q, want %q", string(body), `{"status":"ok"}`)
	}
}

func TestNewRouter_HealthRoute(t *testing.T) {
	h := server.NewRouter()

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("/health status = %d, want 200", rec.Code)
	}
}

// pickLocalAddr reserves an ephemeral loopback port and returns its
// "127.0.0.1:N" string after closing the listener, so the caller can bind to
// it seconds later. This matches net/http's pattern for tests that need a
// real kernel-assigned port.
func pickLocalAddr(t *testing.T) string {
	t.Helper()
	lc := &net.ListenConfig{}
	l, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("close probe listener: %v", err)
	}
	return addr
}

// TestServer_Run_GracefulShutdown exercises the AC4 contract without a real
// database: canceling the context triggers http.Server.Shutdown, in-flight
// requests within the timeout complete, and Run returns nil. A nil pool is
// safe here because no Keep route dereferences it in M2.7.a.
func TestServer_Run_GracefulShutdown(t *testing.T) {
	addr := pickLocalAddr(t)

	cfg := config.Config{
		HTTPAddr:        addr,
		ShutdownTimeout: 2 * time.Second,
	}
	srv := server.New(cfg, nil)

	ctx, cancel := context.WithCancel(context.Background())

	runErr := make(chan error, 1)
	go func() { runErr <- srv.Run(ctx) }()

	// Give ListenAndServe a moment to bind before we hit it.
	deadline := time.Now().Add(2 * time.Second)
	var (
		resp *http.Response
		err  error
	)
	for {
		req, reqErr := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+addr+"/health", nil)
		if reqErr != nil {
			t.Fatalf("build health probe request: %v", reqErr)
		}
		resp, err = http.DefaultClient.Do(req)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("health probe never succeeded: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("health status = %d, want 200", resp.StatusCode)
	}
	if err := resp.Body.Close(); err != nil {
		t.Errorf("close body: %v", err)
	}

	cancel()

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run returned error on graceful shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s after context cancel")
	}
}

// TestServer_Run_SlowHandlerCompletes covers the AC4 edge case: an in-flight
// request that sleeps briefly during SIGTERM completes before the shutdown
// deadline, and Run still returns nil (exit 0). We use a 1s handler delay and
// a 3s shutdown timeout to keep the test fast but well within the AC.
func TestServer_Run_SlowHandlerCompletes(t *testing.T) {
	addr := pickLocalAddr(t)

	mux := http.NewServeMux()
	handlerDone := make(chan struct{})
	mux.HandleFunc("GET /slow", func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(1 * time.Second)
		w.WriteHeader(http.StatusNoContent)
		close(handlerDone)
	})

	cfg := config.Config{HTTPAddr: addr, ShutdownTimeout: 3 * time.Second}
	srv := server.NewWithHandler(cfg, nil, mux)

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- srv.Run(ctx) }()

	// Wait for the listener to be live, then issue the slow request async.
	dialer := &net.Dialer{}
	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, err := dialer.DialContext(context.Background(), "tcp", addr)
		if err == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not bind: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	clientErr := make(chan error, 1)
	go func() {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+addr+"/slow", nil)
		if err != nil {
			clientErr <- err
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			clientErr <- err
			return
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			clientErr <- fmt.Errorf("slow status = %d", resp.StatusCode)
			return
		}
		clientErr <- nil
	}()

	// Give the client 100ms to land in the slow handler, then SIGTERM-equivalent.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run returned error with in-flight slow request: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s of cancel with 1s slow handler")
	}

	select {
	case err := <-clientErr:
		if err != nil {
			t.Fatalf("slow request failed: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("slow request never completed after Run returned")
	}

	select {
	case <-handlerDone:
	default:
		t.Fatal("slow handler did not finish before test exit")
	}
}
