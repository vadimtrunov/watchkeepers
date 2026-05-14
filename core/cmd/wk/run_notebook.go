package main

// run_notebook.go — `wk notebook {show|forget|export|import|archive|list-archives} <wk>`.
//
// notebook DBs are per-agent SQLite files under
// `$WATCHKEEPER_DATA/notebook/<wk-id>.sqlite` (note: the notebook
// package's env var is WATCHKEEPER_DATA, NOT WATCHKEEPER_DATA_DIR — the
// latter is the toolregistry / localpatch root). The CLI does not
// override the path resolution; an operator running `wk notebook show
// <wk>` must export the same WATCHKEEPER_DATA that the watchkeeper
// process uses.
//
// SQLite-side discipline: the per-agent DB file is single-writer. The
// CLI commands open the DB read-write but every command uses ContextCancel
// + Close-on-defer so a concurrent watchkeeper still holding the file
// surfaces as a clean error instead of corruption.
//
// `archive` and `list-archives` are exit-3 stubs (M10.2.b follow-up):
// both depend on an [archivestore.Storer] configured via the operator's
// deployment config, and the CLI has no path to that config today (M9.6
// shipped a per-source config; archivestore config lives in
// `core/internal/keep/config`'s Keep service config, not in the
// CLI-visible env).

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/notebook"
)

func runNotebook(ctx context.Context, args []string, stdout, stderr io.Writer, _ envLookup) int {
	if len(args) == 0 {
		stderrf(stderr, "wk: notebook: missing subcommand (expected one of: show, forget, export, import, archive, list-archives)\n")
		return 2
	}
	switch args[0] {
	case "show":
		return runNotebookShow(ctx, args[1:], stdout, stderr)
	case "forget":
		return runNotebookForget(ctx, args[1:], stdout, stderr)
	case "export":
		return runNotebookExport(ctx, args[1:], stdout, stderr)
	case "import":
		return runNotebookImport(ctx, args[1:], stdout, stderr)
	case "archive":
		return notWiredExit(stderr, "notebook archive", "M10.2.b follow-up — requires archivestore.Storer config exposed via env (today only the Keep service config plumbs it)")
	case "list-archives":
		return notWiredExit(stderr, "notebook list-archives", "M10.2.b follow-up — requires archivestore.Storer config exposed via env")
	default:
		stderrf(stderr, fmt.Sprintf("wk: notebook: unknown subcommand %q (expected one of: show, forget, export, import, archive, list-archives)\n", args[0]))
		return 2
	}
}

// popPositionalNotebook is a small helper that pops <wk-id> off the
// post-flag arg list for notebook subcommands. The signature mirrors
// [popPositional] but uses a notebook-specific label prefix.
//
// Iter-1 critic M8: strict-extra-args check — mirror [popPositional]'s
// `len(rest) > 1 -> exit 2` rejection so `wk notebook show <wk1> <wk2>`
// surfaces a usage error instead of silently dropping `<wk2>`.
func popPositionalNotebook(fs *flag.FlagSet, label string, stderr io.Writer) (string, int) {
	rest := fs.Args()
	if len(rest) == 0 {
		stderrf(stderr, fmt.Sprintf("wk: notebook %s: missing positional <wk-id>\n", label))
		return "", 2
	}
	if len(rest) > 1 {
		stderrf(stderr, fmt.Sprintf("wk: notebook %s: extra positional args after <wk-id>: %v\n", label, rest[1:]))
		return "", 2
	}
	wkID := rest[0]
	if strings.TrimSpace(wkID) == "" {
		stderrf(stderr, fmt.Sprintf("wk: notebook %s: <wk-id> must be non-empty\n", label))
		return "", 2
	}
	return wkID, 0
}

func runNotebookShow(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("wk notebook show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	wkID, code := popPositionalNotebook(fs, "show", stderr)
	if code != 0 {
		return code
	}
	db, err := notebook.Open(ctx, wkID)
	if err != nil {
		stderrf(stderr, fmt.Sprintf("wk: notebook show: open: %v\n", err))
		return 1
	}
	defer func() { _ = db.Close() }()
	stats, err := db.Stats(ctx)
	if err != nil {
		stderrf(stderr, fmt.Sprintf("wk: notebook show: stats: %v\n", err))
		return 1
	}
	data, err := json.MarshalIndent(stats, "", "  ")
	if err != nil {
		stderrf(stderr, fmt.Sprintf("wk: notebook show: marshal: %v\n", err))
		return 1
	}
	if _, err := stdout.Write(append(data, '\n')); err != nil {
		stderrf(stderr, fmt.Sprintf("wk: notebook show: write: %v\n", err))
		return 1
	}
	return 0
}

func runNotebookForget(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("wk notebook forget", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	rest := fs.Args()
	if len(rest) < 2 {
		stderrf(stderr, "wk: notebook forget: missing positional <wk-id> <entry-id>\n")
		return 2
	}
	if len(rest) > 2 {
		stderrf(stderr, fmt.Sprintf("wk: notebook forget: extra positional args: %v\n", rest[2:]))
		return 2
	}
	wkID, entryID := rest[0], rest[1]
	if strings.TrimSpace(wkID) == "" || strings.TrimSpace(entryID) == "" {
		stderrf(stderr, "wk: notebook forget: <wk-id> and <entry-id> must be non-empty\n")
		return 2
	}
	db, err := notebook.Open(ctx, wkID)
	if err != nil {
		stderrf(stderr, fmt.Sprintf("wk: notebook forget: open: %v\n", err))
		return 1
	}
	defer func() { _ = db.Close() }()
	if err := db.Forget(ctx, entryID); err != nil {
		stderrf(stderr, fmt.Sprintf("wk: notebook forget: %v\n", err))
		return 1
	}
	fmt.Fprintf(stdout, "wk: notebook forget ok wk=%s entry=%s\n", wkID, entryID)
	return 0
}

// notebookFileFlags is shared between export and import — both need a
// `--file <abs-path>` to the archive on the operator's local FS.
type notebookFileFlags struct {
	file string
}

func runNotebookExport(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("wk notebook export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var f notebookFileFlags
	fs.StringVar(&f.file, "destination", "", "absolute path of the destination archive file (required; will be CREATED, not overwritten)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	wkID, code := popPositionalNotebook(fs, "export", stderr)
	if code != 0 {
		return code
	}
	if strings.TrimSpace(f.file) == "" {
		stderrf(stderr, "wk: notebook export: missing required flag --destination\n")
		return 2
	}
	// O_EXCL — refuse to overwrite an existing archive. The operator
	// must point at a fresh path; this matches M2b's archivestore PUT
	// semantics where the URI is content-addressed and immutable once
	// written.
	out, err := os.OpenFile(f.file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		stderrf(stderr, fmt.Sprintf("wk: notebook export: create %q: %v\n", f.file, err))
		return 1
	}
	defer func() { _ = out.Close() }()
	db, err := notebook.Open(ctx, wkID)
	if err != nil {
		stderrf(stderr, fmt.Sprintf("wk: notebook export: open: %v\n", err))
		// Best-effort cleanup of the empty destination so a re-run
		// is not blocked by a 0-byte file.
		_ = os.Remove(f.file)
		return 1
	}
	defer func() { _ = db.Close() }()
	if err := db.Archive(ctx, out); err != nil {
		stderrf(stderr, fmt.Sprintf("wk: notebook export: archive: %v\n", err))
		_ = os.Remove(f.file)
		return 1
	}
	if err := out.Sync(); err != nil {
		// Iter-1 critic M7: clean up the partial file on Sync
		// failure so the operator can retry without an O_EXCL
		// collision. A 0-byte or partially-written file lingering
		// from a Sync failure would block any retry.
		_ = os.Remove(f.file)
		stderrf(stderr, fmt.Sprintf("wk: notebook export: sync: %v\n", err))
		return 1
	}
	fmt.Fprintf(stdout, "wk: notebook export ok wk=%s destination=%s\n", wkID, f.file)
	return 0
}

func runNotebookImport(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("wk notebook import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var f notebookFileFlags
	fs.StringVar(&f.file, "archive", "", "absolute path of the source archive file (required; must exist)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	wkID, code := popPositionalNotebook(fs, "import", stderr)
	if code != 0 {
		return code
	}
	if strings.TrimSpace(f.file) == "" {
		stderrf(stderr, "wk: notebook import: missing required flag --archive\n")
		return 2
	}
	in, err := os.Open(f.file)
	if err != nil {
		stderrf(stderr, fmt.Sprintf("wk: notebook import: open %q: %v\n", f.file, err))
		return 1
	}
	defer func() { _ = in.Close() }()
	db, err := notebook.Open(ctx, wkID)
	if err != nil {
		stderrf(stderr, fmt.Sprintf("wk: notebook import: open: %v\n", err))
		return 1
	}
	defer func() { _ = db.Close() }()
	if err := db.Import(ctx, in); err != nil {
		stderrf(stderr, fmt.Sprintf("wk: notebook import: import: %v\n", err))
		return 1
	}
	fmt.Fprintf(stdout, "wk: notebook import ok wk=%s archive=%s at=%s\n", wkID, f.file, time.Now().UTC().Format(time.RFC3339))
	return 0
}
