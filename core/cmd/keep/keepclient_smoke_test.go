//go:build integration

// Smoke contract test for the keepclient package against the real Keep
// binary. This test reuses the same boot harness (buildBinary +
// pickLocalAddr + waitForHealth) as integration_test.go and is gated by the
// shared KEEP_INTEGRATION_DB_URL env var, so it participates in `make
// keep-integration-test` alongside the rest of the integration suite.
package main_test

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
)

// TestKeepClient_Smoke_Health boots the compiled keep binary against CI
// Postgres, constructs a keepclient against the bound address, and asserts
// that Health(ctx) succeeds. Acts as an end-to-end exerciser of the
// transport plumbing landed in M2.8.a (AC7) — every later business
// endpoint inherits the same wiring.
func TestKeepClient_Smoke_Health(t *testing.T) {
	dsn := requireDBURL(t)
	bin := buildBinary(t)
	addr := pickLocalAddr(t)

	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()
	cmd := exec.CommandContext(runCtx, bin)
	cmd.Env = append(os.Environ(),
		"KEEP_DATABASE_URL="+dsn,
		"KEEP_HTTP_ADDR="+addr,
		"KEEP_SHUTDOWN_TIMEOUT=5s",
		"KEEP_TOKEN_SIGNING_KEY="+testTokenSigningKeyB64,
		"KEEP_TOKEN_ISSUER="+testTokenIssuer,
	)
	var stderr strings.Builder
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderr)
	cmd.Stdout = os.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatalf("start binary: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	// Wait for /health via raw HTTP first so a slow boot does not blame
	// the keepclient transport for a binary that is not yet ready.
	waitForHealth(t, addr)

	// Construct the client against the bound address (no token source —
	// /health is open).
	c := keepclient.NewClient(keepclient.WithBaseURL("http://" + addr))

	healthCtx, healthCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer healthCancel()
	if err := c.Health(healthCtx); err != nil {
		t.Errorf("keepclient.Health: %v", err)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("binary exited non-zero after SIGTERM: %v; stderr:\n%s", err, stderr.String())
		}
	case <-time.After(exitBudget):
		_ = cmd.Process.Kill()
		t.Fatalf("binary did not exit within %s of SIGTERM; stderr:\n%s", exitBudget, stderr.String())
	}
}
