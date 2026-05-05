// Command spawn-dev-bot bootstraps a parent Slack app for the
// Watchkeeper dev workspace from a YAML manifest. ROADMAP §M4 → M4.3.
//
// The script reads a manifest file, resolves the configuration token
// from the secrets interface (env via [secrets.EnvSource] in Phase 1),
// calls Slack's `apps.manifest.create`, and writes the returned
// credentials (client_id, client_secret, signing_secret,
// verification_token) to a structured JSON file the operator ingests
// into their secrets store. The credentials never appear in stdout,
// stderr, or the script's logs — only the AppID and a redacted summary
// land on the operator's terminal.
//
// Boot sequence (mirrors the Keep service's run-loop discipline):
//
//  1. Parse flags (--manifest, --credentials-out, --config-token-key,
//     --dry-run, --workspace-id).
//  2. Load manifest YAML, validate required fields.
//  3. Resolve the configuration token from the secrets interface
//     (skipped under --dry-run).
//  4. Construct a [slack.Client] with a configuration-token
//     [slack.TokenSource] and the credentials-sink wired to the
//     supplied --credentials-out path.
//  5. Call [slack.Client.CreateApp]; the sink writes the credentials
//     file before CreateApp returns.
//  6. Print a redacted summary (AppID + credential file path) to
//     stdout.
//
// Under --dry-run the binary parses + validates the manifest, prints
// the resolved Slack manifest body it WOULD send (with no token
// resolution and no Slack round-trip), and exits 0. Useful as a CI
// gate against malformed manifests.
//
// Verification (M4.3):
//
//   - `make spawn-dev-bot` invokes this binary with sensible defaults
//     against a real dev workspace (operator-handled provisioning).
//   - The companion binary-grep test
//     (core/cmd/spawn-dev-bot/secrets_grep_test.go) builds the binary
//     and asserts no `xoxb-* / xoxp-* / xoxe-* / xapp-*` token
//     prefixes appear in the resulting bytes — verification bullet 279.
package main

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"
)

// main is the os.Exit wrapper so the testable [run] function never
// terminates the test process. Mirrors the discipline from
// core/cmd/keep/main.go (M2.7.a).
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, &osEnvLookup{}, &osFileSystem{})
	os.Exit(code)
}

// envLookup is the lookup-only contract the script uses to find the
// configuration-token env var. Abstracted (rather than calling
// os.LookupEnv directly) so tests inject a deterministic table without
// having to mutate process env across goroutines (t.Setenv requires
// per-test serialisation; an injected lookup is concurrency-safe).
type envLookup interface {
	LookupEnv(key string) (string, bool)
}

// osEnvLookup is the production [envLookup] implementation: a thin
// pass-through to os.LookupEnv.
type osEnvLookup struct{}

// LookupEnv satisfies [envLookup] for [osEnvLookup].
func (osEnvLookup) LookupEnv(key string) (string, bool) {
	return os.LookupEnv(key)
}

// fileSystem is the read+write contract the script uses for the
// manifest read and the credentials-out write. Abstracted so tests
// inject an in-memory map; production wiring uses [osFileSystem].
type fileSystem interface {
	ReadFile(path string) ([]byte, error)
	WriteFile(path string, data []byte, perm os.FileMode) error
}

// osFileSystem is the production [fileSystem] implementation:
// pass-through to the stdlib os helpers.
type osFileSystem struct{}

// ReadFile satisfies [fileSystem.ReadFile] for [osFileSystem].
func (osFileSystem) ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// WriteFile satisfies [fileSystem.WriteFile] for [osFileSystem]. The
// credentials file is written with mode 0o600 by the caller; this
// helper does not impose its own perms beyond what the caller passes.
func (osFileSystem) WriteFile(path string, data []byte, perm os.FileMode) error {
	return os.WriteFile(path, data, perm)
}

// stderrf writes a stable, locale-independent diagnostic to stderr.
// Mirrors LESSON M2.1.b — error-text assertions in CI must not depend
// on lc_messages.
func stderrf(stderr io.Writer, msg string) {
	_, _ = io.WriteString(stderr, msg)
}
