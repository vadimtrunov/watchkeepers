package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger"
	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger/slack"
)

// credentialsFileMode is the umask-friendly mode the credentials file
// is written with. 0o600 (rw owner only) prevents accidental
// world-readable secrets — operator-grade discipline. Mirrors the
// `~/.netrc` convention.
const credentialsFileMode os.FileMode = 0o600

// flags is the parsed flag bundle. Hoisted to a struct so [run] can
// validate after flag.Parse without juggling six locals.
type flags struct {
	manifestPath   string
	credentialsOut string
	configTokenKey string
	dryRun         bool
	baseURL        string // optional; tests inject the httptest URL
}

// parseFlags decodes the supplied argv into a [flags] bundle. Returns
// the bundle plus any flag.ErrHelp / flag.ErrInvalidArgs surfaced by
// the FlagSet. Errors other than help cause [run] to exit with code 2
// (the conventional "usage error" code matching `flag.ExitOnError`,
// but expressed via return values so tests can assert without trapping
// os.Exit).
func parseFlags(args []string, stderr io.Writer) (flags, error) {
	fs := flag.NewFlagSet("spawn-dev-bot", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var f flags
	fs.StringVar(&f.manifestPath, "manifest", "", "path to YAML manifest describing the parent app (required)")
	fs.StringVar(&f.credentialsOut, "credentials-out", "", "path to write the JSON-encoded app credentials (required; mode 0600)")
	fs.StringVar(&f.configTokenKey, "config-token-key", "WATCHKEEPER_SLACK_CONFIG_TOKEN", "secrets key holding the Slack `xoxe-*` configuration token")
	fs.BoolVar(&f.dryRun, "dry-run", false, "parse + validate the manifest, print the resolved Slack body, do NOT contact Slack or read secrets")
	fs.StringVar(&f.baseURL, "base-url", "", "override Slack base URL (testing hook; default https://slack.com/api)")

	if err := fs.Parse(args); err != nil {
		return flags{}, err
	}
	return f, nil
}

// run is the testable entrypoint. The signature mirrors
// core/cmd/keep/main.go (M2.7.a) so test wiring stays consistent
// across the cmd-tree:
//
//   - ctx threads through to the slack.Client.CreateApp call;
//   - stdout / stderr are explicit so tests capture without trapping
//     os.Stdout / os.Stderr;
//   - env / fs are abstracted so tests inject deterministic fixtures
//     without mutating process state.
//
// Exit codes:
//
//	0 — success (or --dry-run success)
//	1 — runtime failure (manifest read, secret resolve, Slack call,
//	    credentials sink write)
//	2 — usage error (missing required flags, malformed YAML)
func run(ctx context.Context, args []string, stdout, stderr io.Writer, env envLookup, fs fileSystem) int {
	f, err := parseFlags(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if err := validateFlags(f); err != nil {
		stderrf(stderr, fmt.Sprintf("spawn-dev-bot: %v\n", err))
		return 2
	}

	manifestBytes, err := fs.ReadFile(f.manifestPath)
	if err != nil {
		stderrf(stderr, fmt.Sprintf("spawn-dev-bot: read manifest: %v\n", err))
		return 1
	}

	manifest, err := parseManifest(manifestBytes)
	if err != nil {
		stderrf(stderr, fmt.Sprintf("spawn-dev-bot: %v\n", err))
		return 2
	}

	if f.dryRun {
		return runDryRun(manifest, stdout)
	}

	return runLive(ctx, f, manifest, stdout, stderr, env, fs)
}

// validateFlags checks the required-flag invariants. Hoisted out of
// [run] so the validation table stays scannable and the error chain
// preserved for tests asserting via errors.Is.
func validateFlags(f flags) error {
	if f.manifestPath == "" {
		return ErrManifestEmpty
	}
	if f.credentialsOut == "" {
		return ErrCredentialsOutEmpty
	}
	if f.configTokenKey == "" {
		return ErrConfigTokenKeyEmpty
	}
	return nil
}

// runDryRun parses + validates the manifest, prints the resolved
// Slack manifest body to stdout, and returns 0 without contacting
// Slack or reading any secret. Useful as a CI gate against malformed
// manifests + a developer affordance for inspecting the wire shape.
//
// The output is JSON-encoded (not YAML) so downstream tooling — jq,
// `make` recipes, lint scripts — can pipe through without a YAML
// parser.
func runDryRun(m Manifest, stdout io.Writer) int {
	wire := messengerManifestPreview(m.toMessengerManifest())
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(wire); err != nil {
		// json.Encode on a map[string]any with primitives + slices +
		// nested maps cannot fail in practice; a defensive return keeps
		// the code path explicit.
		return 1
	}
	return 0
}

// messengerManifestPreview re-shapes the portable
// [messenger.AppManifest] into the JSON map the Slack adapter would
// actually send. Re-implements the subset of buildManifestBody (in
// core/pkg/messenger/slack/create_app.go) the dry-run preview needs;
// duplicated by design so the test surface stays purely public-API
// without leaking the slack package's internals.
func messengerManifestPreview(m messenger.AppManifest) map[string]any {
	display := map[string]any{"name": m.Name}
	if m.Description != "" {
		display["description"] = m.Description
	}
	out := map[string]any{"display_information": display}
	if len(m.Scopes) > 0 {
		out["oauth_config"] = map[string]any{
			"scopes": map[string]any{"bot": m.Scopes},
		}
		// Mirror the features.bot_user the adapter emits alongside bot
		// scopes — Slack rejects bot scopes without it, so a dry-run
		// preview that hides it lies about the real request.
		out["features"] = map[string]any{
			"bot_user": map[string]any{
				"display_name":  m.Name,
				"always_online": false,
			},
		}
	}
	if len(m.Metadata) > 0 {
		preview := make(map[string]string, len(m.Metadata))
		for k, v := range m.Metadata {
			preview[k] = v
		}
		out["metadata_extensions"] = preview
	}
	return out
}

// runLive resolves the configuration token via the secrets-interface
// stand-in (env lookup wrapped in [secrets.SecretSource] semantics —
// empty values surface as "not set"), constructs the slack.Client
// with a credentials-sink wired to write the --credentials-out file,
// calls CreateApp, and prints the redacted summary.
//
// The function is deliberately small — every step has a single
// responsibility so failures map onto a unique exit-code branch.
func runLive(
	ctx context.Context,
	f flags,
	manifest Manifest,
	stdout, stderr io.Writer,
	env envLookup,
	fs fileSystem,
) int {
	configToken, err := lookupConfigToken(env, f.configTokenKey)
	if err != nil {
		stderrf(stderr, fmt.Sprintf("spawn-dev-bot: resolve config token: %v\n", err))
		return 1
	}

	sink, captured := newRecordingSink()
	clientOpts := []slack.ClientOption{
		slack.WithTokenSource(slack.StaticToken(configToken)),
		slack.WithCreateAppCredsSink(sink),
	}
	if f.baseURL != "" {
		clientOpts = append(clientOpts, slack.WithBaseURL(f.baseURL))
	}
	client := slack.NewClient(clientOpts...)

	appID, err := client.CreateApp(ctx, manifest.toMessengerManifest())
	if err != nil {
		stderrf(stderr, fmt.Sprintf("spawn-dev-bot: create app: %v\n", err))
		return 1
	}

	creds := captured.value()
	if creds.AppID == "" {
		// Defensive: CreateApp succeeded but the sink never fired —
		// indicates an adapter regression. Bail rather than write a
		// half-populated credentials file.
		stderrf(stderr, "spawn-dev-bot: credentials sink did not fire\n")
		return 1
	}

	if err := writeCredentialsFile(fs, f.credentialsOut, creds); err != nil {
		stderrf(stderr, fmt.Sprintf("spawn-dev-bot: write credentials: %v\n", err))
		return 1
	}

	printRedactedSummary(stdout, appID, f.credentialsOut, manifest.Name)
	return 0
}

// lookupConfigToken resolves the configuration-token value from the
// supplied [envLookup]. Returns [secrets.ErrSecretNotFound]-shaped
// behaviour: empty / missing values surface as a sentinel-wrapped
// error rather than an empty string (mirrors the EnvSource contract
// in core/pkg/secrets/env.go).
//
// The script does not import core/pkg/secrets directly — the secrets
// package's [secrets.SecretSource.Get] threads ctx for HTTP-backed
// future implementations, but this script's only Phase-1 backend is
// env (already non-blocking). Inlining the empty-string-as-not-set
// rule here keeps the dependency surface tight without losing the
// contract semantics.
func lookupConfigToken(env envLookup, key string) (string, error) {
	v, ok := env.LookupEnv(key)
	if !ok || v == "" {
		return "", fmt.Errorf("%w: %s", errConfigTokenNotFound, key)
	}
	return v, nil
}

// errConfigTokenNotFound is the local sentinel mirroring
// secrets.ErrSecretNotFound semantics for the config-token resolution
// path. Stable phrase — CI assertions stay locale-independent.
var errConfigTokenNotFound = errors.New("config token not found")

// recordingSink is the minimal in-memory holder the script wires to
// [slack.WithCreateAppCredsSink]. Captures the credentials, exposes
// them via [recordingSink.value]. Concurrency: CreateApp invokes the
// sink once, in-line, before returning; no mutex is required.
type recordingSink struct {
	creds slack.CreateAppCredentials
	fired bool
}

// newRecordingSink returns the [slack.CreateAppCredsSink] callback
// plus the holder that captures the credentials. Returning the holder
// (rather than a bare callback) lets [runLive] read the credentials
// AFTER CreateApp returns without resorting to a closure-mutating
// pointer dance.
func newRecordingSink() (slack.CreateAppCredsSink, *recordingSink) {
	r := &recordingSink{}
	cb := func(_ context.Context, c slack.CreateAppCredentials) error {
		r.creds = c
		r.fired = true
		return nil
	}
	return cb, r
}

// value returns the captured credentials. The receiver is the holder
// the [newRecordingSink] returns; calling value before the sink fired
// returns a zero CreateAppCredentials.
func (r *recordingSink) value() slack.CreateAppCredentials {
	return r.creds
}

// credentialsFile is the JSON shape written to --credentials-out.
// Designed for direct ingestion into the operator's secrets store
// (vault, AWS SSM, 1Password CLI). Field names mirror Slack's API
// schema so cross-referencing the docs is one less translation step.
type credentialsFile struct {
	AppID             string `json:"app_id"`
	ClientID          string `json:"client_id"`
	ClientSecret      string `json:"client_secret"`
	SigningSecret     string `json:"signing_secret"`
	VerificationToken string `json:"verification_token"`
}

// writeCredentialsFile encodes `creds` as indented JSON and writes it
// to `path` with mode 0o600. The encoder is buffered so a partial
// write never lands on disk: either the whole file is committed, or
// the error surfaces before any bytes hit the path.
func writeCredentialsFile(fs fileSystem, path string, creds slack.CreateAppCredentials) error {
	out := credentialsFile{
		AppID:             string(creds.AppID),
		ClientID:          creds.ClientID,
		ClientSecret:      creds.ClientSecret,
		SigningSecret:     creds.SigningSecret,
		VerificationToken: creds.VerificationToken,
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	// #nosec G117 -- this struct's whole purpose is to serialise the
	// just-issued Slack app credentials to a 0o600 file the operator
	// ingests into their secrets store; the marshal is intentional.
	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("encode credentials: %w", err)
	}
	if err := fs.WriteFile(path, buf.Bytes(), credentialsFileMode); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// printRedactedSummary writes the operator-facing summary to stdout.
// REDACTION DISCIPLINE: the credentials never appear here — only the
// AppID, the file path, and the manifest name. The file at
// `credentialsPath` is the single observation point for the secrets.
func printRedactedSummary(stdout io.Writer, appID messenger.AppID, credentialsPath, manifestName string) {
	fmt.Fprintf(stdout, "spawn-dev-bot: created Slack app\n")
	fmt.Fprintf(stdout, "  app_id:           %s\n", appID)
	fmt.Fprintf(stdout, "  manifest_name:    %s\n", manifestName)
	fmt.Fprintf(stdout, "  credentials_file: %s (mode 0600)\n", credentialsPath)
	fmt.Fprintf(stdout, "spawn-dev-bot: next: ingest the credentials file into the secrets store, then run the install flow.\n")
}

// bytesReader wraps `data` in a *bytes.Reader. Hoisted as a helper so
// the manifest decoder construction in manifest.go reads as a one-liner.
func bytesReader(data []byte) *bytes.Reader {
	return bytes.NewReader(data)
}
