package main

// run_watchkeeper.go — `wk spawn|retire|list|inspect` subcommands.
//
// Every command is a thin keepclient wrapper that runs against the Keep
// service identified by $WATCHKEEPER_KEEP_BASE_URL with the bearer token
// $WATCHKEEPER_OPERATOR_TOKEN. The CLI does not implement local saga
// orchestration — `spawn` and `retire` write the lifecycle row + status
// transition; the saga's downstream steps (notebook init, runtime boot,
// archive on retire) run in the watchmaster / runtime, not in the CLI.
//
// Iter-1 critic C1 fix: `wk retire` now ALWAYS rides through
// [keepclient.Client.UpdateWatchkeeperRetired] which requires a
// non-empty, RFC-3986-absolute archive_uri. The legacy
// `UpdateWatchkeeperStatus(..., "retired")` path (M6.2.c synchronous
// retire tool) is the only seam that elides the archive URI; exposing
// it through the unified operator CLI would be a one-line escape from
// the M7.2.b/c archive-on-retire invariant. `--archive-uri` is
// therefore REQUIRED on `wk retire`.
//
// Iter-1 critic C2 fix: every write subcommand now REQUIRES `--reason`.
// The M9.5 lesson pinned `--reason` as the operator's accountability
// statement for state-changing calls; the unified CLI now reaches a
// wider write surface (spawn / retire / personality set / language set
// / budget set when wired). Reason is enforced by the CLI as a usage
// gate AND echoed in the operator-facing summary so it lands in shell
// transcripts / operator audit pipelines even when the Keep endpoint
// has no native reason field.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
)

func runSpawn(ctx context.Context, args []string, stdout, stderr io.Writer, env envLookup) int {
	fs := flag.NewFlagSet("wk spawn", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		manifestID    string
		leadHumanID   string
		activeVersion string
		reason        string
		noInherit     bool
	)
	fs.StringVar(&manifestID, "manifest", "", "manifest UUID (required)")
	fs.StringVar(&leadHumanID, "lead", "", "lead-human UUID (required)")
	fs.StringVar(&activeVersion, "active-version", "", "optional pinned manifest_version UUID")
	fs.StringVar(&reason, "reason", "", "operator-supplied audit text (required)")
	// Phase 2 §M7.1.c operator opt-out for the spawn-saga's
	// NotebookInheritStep. When set, the new Watchkeeper's notebook
	// is NOT seeded from the most-recently-retired peer's archive
	// (the spawn flow falls through to a virgin file). The flag is
	// captured at the CLI surface today and reflected in the
	// success message; the full round-trip to
	// `saga.SpawnContext.NoInherit` lands when the Slack-bot binary
	// takes over the spawn-saga kickoff wiring (see the
	// `approval_wiring/wiring.go` DEFERRED WIRING note). The CLI
	// captures the operator's intent so an audit-aware reader of
	// shell transcripts has a record of the choice even though
	// `wk spawn` itself only writes the watchkeeper row today.
	fs.BoolVar(&noInherit, "no-inherit", false, "suppress predecessor-notebook inheritance on spawn (Phase 2 §M7.1.c opt-out)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	missing := []string{}
	if strings.TrimSpace(manifestID) == "" {
		missing = append(missing, "--manifest")
	}
	if strings.TrimSpace(leadHumanID) == "" {
		missing = append(missing, "--lead")
	}
	if strings.TrimSpace(reason) == "" {
		missing = append(missing, "--reason")
	}
	if len(missing) > 0 {
		stderrf(stderr, fmt.Sprintf("wk: spawn: missing required flag(s): %s\n", strings.Join(missing, ", ")))
		return 2
	}
	return withKeepClient(env, stderr, "spawn", func(c *keepclient.Client) int {
		resp, err := c.InsertWatchkeeper(ctx, keepclient.InsertWatchkeeperRequest{
			ManifestID:              manifestID,
			LeadHumanID:             leadHumanID,
			ActiveManifestVersionID: activeVersion,
		})
		if err != nil {
			stderrf(stderr, fmt.Sprintf("wk: spawn: %v\n", err))
			return 1
		}
		fmt.Fprintf(stdout, "wk: spawn ok watchkeeper_id=%s reason=%q no_inherit=%t\n", resp.ID, reason, noInherit)
		return 0
	})
}

func runRetire(ctx context.Context, args []string, stdout, stderr io.Writer, env envLookup) int {
	fs := flag.NewFlagSet("wk retire", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		archiveURI string
		reason     string
	)
	// Iter-1 critic C1: archive-uri is REQUIRED (the only seam that
	// elides it is the legacy M6.2.c synchronous retire tool; exposing
	// that path here would be a one-line escape from the M7.2.c
	// archive-on-retire invariant).
	fs.StringVar(&archiveURI, "archive-uri", "", "archive_uri to record on retire (required; RFC 3986 absolute URI — M7.2.c invariant)")
	fs.StringVar(&reason, "reason", "", "operator-supplied audit text (required)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	rest := fs.Args()
	if len(rest) == 0 {
		stderrf(stderr, "wk: retire: missing positional <wk-id>\n")
		return 2
	}
	if len(rest) > 1 {
		stderrf(stderr, fmt.Sprintf("wk: retire: extra positional args after <wk-id>: %v\n", rest[1:]))
		return 2
	}
	wkID := rest[0]
	missing := []string{}
	if strings.TrimSpace(wkID) == "" {
		stderrf(stderr, "wk: retire: <wk-id> must be non-empty\n")
		return 2
	}
	if strings.TrimSpace(archiveURI) == "" {
		missing = append(missing, "--archive-uri")
	}
	if strings.TrimSpace(reason) == "" {
		missing = append(missing, "--reason")
	}
	if len(missing) > 0 {
		stderrf(stderr, fmt.Sprintf("wk: retire: missing required flag(s): %s\n", strings.Join(missing, ", ")))
		return 2
	}
	return withKeepClient(env, stderr, "retire", func(c *keepclient.Client) int {
		if err := c.UpdateWatchkeeperRetired(ctx, wkID, archiveURI); err != nil {
			stderrf(stderr, fmt.Sprintf("wk: retire: %v\n", err))
			return 1
		}
		fmt.Fprintf(stdout, "wk: retire ok watchkeeper_id=%s archive_uri=%s reason=%q\n", wkID, archiveURI, reason)
		return 0
	})
}

func runList(ctx context.Context, args []string, stdout, stderr io.Writer, env envLookup) int {
	fs := flag.NewFlagSet("wk list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		status string
		limit  int
	)
	fs.StringVar(&status, "status", "", "filter by lifecycle status: pending|active|retired (default: all)")
	fs.IntVar(&limit, "limit", 0, "cap the number of rows returned (server-enforced default + max)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	return withKeepClient(env, stderr, "list", func(c *keepclient.Client) int {
		resp, err := c.ListWatchkeepers(ctx, keepclient.ListWatchkeepersRequest{
			Status: status,
			Limit:  limit,
		})
		if err != nil {
			stderrf(stderr, fmt.Sprintf("wk: list: %v\n", err))
			return 1
		}
		fmt.Fprintln(stdout, "ID\tSTATUS\tMANIFEST_ID\tLEAD\tCREATED_AT")
		for _, w := range resp.Items {
			fmt.Fprintf(
				stdout, "%s\t%s\t%s\t%s\t%s\n",
				w.ID, w.Status, w.ManifestID, w.LeadHumanID, w.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
			)
		}
		return 0
	})
}

func runInspect(ctx context.Context, args []string, stdout, stderr io.Writer, env envLookup) int {
	fs := flag.NewFlagSet("wk inspect", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	rest := fs.Args()
	if len(rest) == 0 {
		stderrf(stderr, "wk: inspect: missing positional <wk-id>\n")
		return 2
	}
	if len(rest) > 1 {
		stderrf(stderr, fmt.Sprintf("wk: inspect: extra positional args after <wk-id>: %v\n", rest[1:]))
		return 2
	}
	wkID := rest[0]
	// Iter-1 critic M9: trim+reject whitespace-only positional. Without
	// this gate a `" "` PathEscapes to `%20` and hits the server, which
	// surfaces as a confusing 4xx; the M9.5 sibling commands already
	// gate this client-side.
	if strings.TrimSpace(wkID) == "" {
		stderrf(stderr, "wk: inspect: <wk-id> must be non-empty\n")
		return 2
	}
	return withKeepClient(env, stderr, "inspect", func(c *keepclient.Client) int {
		w, err := c.GetWatchkeeper(ctx, wkID)
		if err != nil {
			stderrf(stderr, fmt.Sprintf("wk: inspect: %v\n", err))
			return 1
		}
		fmt.Fprintf(stdout, "id              %s\n", w.ID)
		fmt.Fprintf(stdout, "status          %s\n", w.Status)
		fmt.Fprintf(stdout, "manifest_id     %s\n", w.ManifestID)
		fmt.Fprintf(stdout, "lead_human_id   %s\n", w.LeadHumanID)
		if w.ActiveManifestVersionID != nil {
			fmt.Fprintf(stdout, "active_version  %s\n", *w.ActiveManifestVersionID)
		}
		if w.SpawnedAt != nil {
			fmt.Fprintf(stdout, "spawned_at      %s\n", w.SpawnedAt.Format("2006-01-02T15:04:05Z07:00"))
		}
		if w.RetiredAt != nil {
			fmt.Fprintf(stdout, "retired_at      %s\n", w.RetiredAt.Format("2006-01-02T15:04:05Z07:00"))
		}
		if w.ArchiveURI != nil {
			fmt.Fprintf(stdout, "archive_uri     %s\n", *w.ArchiveURI)
		}
		fmt.Fprintf(stdout, "created_at      %s\n", w.CreatedAt.Format("2006-01-02T15:04:05Z07:00"))
		return 0
	})
}
