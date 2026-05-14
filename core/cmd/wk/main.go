// Command wk is the unified Watchkeeper operator CLI (ROADMAP §M10.2).
//
// Subcommand layout follows the roadmap text verbatim:
//
//	wk spawn
//	wk retire <wk>
//	wk list
//	wk inspect <wk>
//	wk logs <wk>
//	wk tail-keepers-log
//	wk tool {local-install | rollback | hosted {list | show | export} | share | list}
//	wk tools sources {list | status | sync}
//	wk notebook {show | forget | export | import | archive | list-archives} <wk>
//	wk personality {show | set} <wk>
//	wk language {show | set} <wk>
//	wk budget {show | set}
//	wk approvals {pending | inspect}
//
// Every top-level noun-group is mirrored behind `make wk CMD="<group> <args>"`
// in the project Makefile; the CLI is the single supported operator entry
// point. The composite shortcut accepts any flag the binary accepts (the
// Makefile wrapper passes CMD verbatim to ./bin/wk).
//
// M10.2 subsumes the M9.5/M9.6/M9.7 `wk-tool` operator surface — the
// previous `wk-tool` binary is replaced by `wk tool …`. The four migrated
// subcommands (`local-install`, `rollback`, `hosted-export`, `share`) keep
// their flag shapes verbatim; only the invocation prefix changes
// (`wk-tool local-install …` → `wk tool local-install …`).
//
// Exit codes (mirror `core/cmd/spawn-dev-bot/main.go` / `core/cmd/wk-tool/`):
//
//	0 — success
//	1 — runtime failure (HTTP / FS / source-lookup / publish / identity-resolver)
//	2 — usage error (missing required flag, malformed input, unknown subcommand)
//	3 — feature not yet wired: the noun-group exists in the CLI contract but
//	    its backend seam has not landed yet. Exit-3 commands name the M
//	    follow-up that will wire them. This mirrors the M10.1 pattern of
//	    pinning the contract NOW and wiring call sites in follow-up
//	    sub-items.
//
// Required env vars for Keep-backed subcommands (spawn, retire, list,
// inspect, logs, tail-keepers-log, personality, language, budget show):
//
//	WATCHKEEPER_KEEP_BASE_URL — Keep service base URL (e.g. http://keep:8080)
//	WATCHKEEPER_OPERATOR_TOKEN — bearer token minted by the operator's
//	                              identity provider (Keep enforces the
//	                              scope/audience contract).
//
// Required env vars for local-FS subcommands (tool *, notebook *):
//
//	WATCHKEEPER_DATA_DIR — deployment data root (sibling of $DATA_DIR/tools/
//	                       and $DATA_DIR/notebooks/).
//
// PII discipline: secrets (the bearer token, the GitHub PAT consumed by
// `tool share`) reach the binary via ENV ONLY — never argv (which `ps -ef`
// would leak). The `--reason` text and operator/proposer identity flags
// are the only free-form strings on the CLI; both are intentionally part
// of the audit record (the operator's accountability statement IS audit
// content, not a credential).
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
// (M4.3), `core/cmd/keep/main.go` (M2.7.a), and `core/cmd/wk-tool/main.go`
// (M9.5).
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, &osEnvLookup{})
	os.Exit(code)
}

// envLookup is the lookup-only contract used to resolve env-supplied
// defaults and Keep-client config (base URL, bearer token). Abstracted so
// tests inject a deterministic table without `t.Setenv` serialisation.
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
