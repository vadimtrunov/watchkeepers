// Command wk-tool is the operator-facing CLI surface for the M9.5
// local-patch lifecycle. Two subcommands:
//
//   - `local-install --folder <path> --source <name> --reason <text>
//     --operator <id> [--data-dir <dir>]` — copies the operator-supplied
//     folder into the configured local source's tools directory,
//     snapshotting the prior contents under
//     `<DataDir>/_history/<source>/<tool>/<priorVersion>/` so a later
//     rollback can restore them. Emits `local_patch_applied` with
//     operator identity, content SHA256 (`diff_hash`), reason. Mirror
//     ROADMAP §M9.5: `make tools-local-install <folder>` requires a
//     `--reason` field.
//
//   - `rollback --name <tool> --to <version> [--source <name>]
//     [--data-dir <dir>] --reason <text> --operator <id>` — restores a
//     previously-snapshotted version. Mirror ROADMAP §M9.5:
//     `wk tool rollback <name> --to <version> [--source <source>]`.
//
// Audit channel: BOTH subcommands emit on `localpatch.local_patch_applied`
// (single audit topic, M9.7-listed); the `Operation` field discriminates
// `install` vs `rollback`. The CLI ships a JSONL stdout publisher by
// default — every published event lands as one JSON object per line on
// stdout for the operator's downstream capture path (audit-log
// ingestion, syslog, ELK). Production wiring (M9.7) plugs in the
// durable outbox publisher.
//
// Exit codes (mirror `core/cmd/spawn-dev-bot/main.go` / M2.7.a):
//
//	0 — success
//	1 — runtime failure (filesystem, source-lookup, identity-resolver,
//	    publish)
//	2 — usage error (missing required flag, malformed input)
//
// Operator identity: the `--operator <id>` flag is REQUIRED and passed
// through to the [localpatch.OperatorIdentityResolver] as a hint. The
// CLI's bundled resolver returns the hint verbatim — a hosted
// deployment swaps in a verified-OIDC resolver via the
// localpatch.NewInstaller / NewRollbacker constructor.
//
// PII discipline: secrets must NEVER reach the binary's argv (`ps -ef`
// would surface them). The `--reason` text and `--operator` id are
// the ONLY two free-form-ish strings the CLI accepts; both are
// bounded by the localpatch package and both are intentionally part
// of the audit record (the operator's accountability statement IS
// audit content, not a credential).
package main

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"
)

// main is the os.Exit wrapper so the testable [run] function never
// terminates the test process. Mirror `core/cmd/spawn-dev-bot/main.go`
// (M4.3) and `core/cmd/keep/main.go` (M2.7.a).
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, &osEnvLookup{})
	os.Exit(code)
}

// envLookup is the lookup-only contract used to resolve any future
// env-supplied defaults. Abstracted so tests inject a deterministic
// table without `t.Setenv` serialisation. Currently the CLI consults
// `WATCHKEEPER_DATA_DIR` for the `--data-dir` default.
type envLookup interface {
	LookupEnv(key string) (string, bool)
}

// osEnvLookup is the production [envLookup] implementation.
type osEnvLookup struct{}

// LookupEnv satisfies [envLookup] for [osEnvLookup].
func (osEnvLookup) LookupEnv(key string) (string, bool) {
	return os.LookupEnv(key)
}

// stderrf writes a stable, locale-independent diagnostic to stderr.
// Mirror M2.1.b lesson — error-text assertions in CI must not depend
// on lc_messages.
func stderrf(stderr io.Writer, msg string) {
	_, _ = io.WriteString(stderr, msg)
}
