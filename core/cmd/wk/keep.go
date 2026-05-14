package main

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
)

// Env var keys consulted to build a [keepclient.Client] for Keep-backed
// subcommands. The CLI never accepts the bearer token via argv — leaking
// it to `ps -ef` would defeat the whole purpose. The base URL is passed
// via env to keep CLI invocations short and consistent across the
// noun-group surface.
const (
	keepBaseURLEnvKey     = "WATCHKEEPER_KEEP_BASE_URL"
	operatorTokenEnvKey   = "WATCHKEEPER_OPERATOR_TOKEN"
	watchkeeperDataDirEnv = "WATCHKEEPER_DATA_DIR"
)

// errMissingKeepConfig is returned when one or both Keep env vars are
// unset/blank. The CLI surfaces it to the operator on stderr; the
// process exit code is 2 (usage error). The diagnostic intentionally
// names BOTH env vars so an operator with only half the config gets
// one round-trip instead of N.
var errMissingKeepConfig = errors.New("keep env vars unset — export " + keepBaseURLEnvKey + " and " + operatorTokenEnvKey)

// newKeepClient resolves the Keep base URL + bearer token from env and
// builds a configured [keepclient.Client]. The returned client is safe
// for one-shot CLI use; it owns no goroutines and no caches. The base
// URL must be non-blank AND parseable as an absolute URI; the bearer
// token must be non-blank. Both are intentionally REQUIRED — the CLI
// has no useful unauthenticated calls.
func newKeepClient(env envLookup) (*keepclient.Client, error) {
	base, _ := env.LookupEnv(keepBaseURLEnvKey)
	tok, _ := env.LookupEnv(operatorTokenEnvKey)
	if strings.TrimSpace(base) == "" || strings.TrimSpace(tok) == "" {
		return nil, errMissingKeepConfig
	}
	c := keepclient.NewClient(
		keepclient.WithBaseURL(base),
		keepclient.WithTokenSource(keepclient.StaticToken(tok)),
	)
	return c, nil
}

// withKeepClient is the standard front-matter every Keep-backed
// subcommand uses: build the client, surface a usage error (exit 2)
// when env is missing, otherwise call f with the client. Centralised
// so each subcommand's error path is one line.
func withKeepClient(env envLookup, stderr io.Writer, label string, f func(c *keepclient.Client) int) int {
	c, err := newKeepClient(env)
	if err != nil {
		stderrf(stderr, fmt.Sprintf("wk: %s: %v\n", label, err))
		return 2
	}
	return f(c)
}
