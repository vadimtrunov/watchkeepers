package main

// run_manifest.go — `wk personality|language|budget {show|set}`.
//
// personality/language are stored as columns on `manifest_versions`
// (migration 010). `show` reads the watchkeeper to find ManifestID, then
// the current ManifestVersion via Keep, and prints the field. `set` reads
// the current version, copies every field into a PutManifestVersionRequest
// with the target field overwritten, bumps VersionNo by one, and issues
// PUT /v1/manifests/{id}/versions. The "read-merge-write" shape mirrors
// the M2.7.b coordinator flow and keeps the surface area of the CLI thin.
//
// `budget show` reads GET /v1/cost-rollups for an `--agent` UUID over an
// `--from .. --to` window with `--grain {daily|weekly}`. `budget set` is
// exit-3 — the manifest carries no budget field today; a follow-up
// sub-item (M10.2.c) will add the wiring once M6's per-watchkeeper
// budget seam is exposed through Keep.
//
// Iter-1 critic fixes:
//
//   - M1: the read-merge-write GET→PUT path is NOT atomic against a
//     concurrent writer. Two `wk personality set` calls racing against
//     the same manifest both compute `mv.VersionNo + 1` and one of them
//     hits `keepclient.ErrConflict` from the server's unique-(manifest_id,
//     version_no) constraint. The CLI now detects the conflict and
//     surfaces a tailored "concurrent edit detected — re-run the command;
//     a competing version landed first" hint so the operator doesn't
//     need to read the keepclient sentinel docs.
//
//   - M2: legacy manifest rows with empty `system_prompt` exist (the
//     non-empty enforcement was added late). The CLI's copy-every-field
//     merge would then ship `SystemPrompt: ""` and trip the keepclient
//     pre-flight + server CHECK. A new optional `--system-prompt` flag
//     overrides the field in the same PUT so the operator can repair
//     the row in one round-trip; the override is intentionally a
//     write-companion (not its own subcommand) because it has no
//     `show`-side meaning.
//
//   - C2: `--reason` is now REQUIRED on personality/language set and
//     echoed in the operator-facing summary. The Keep PUT endpoint has
//     no native `reason` field today; the CLI gate ensures the operator
//     articulates intent for the shell transcript / downstream audit
//     capture.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
)

func runPersonality(ctx context.Context, args []string, stdout, stderr io.Writer, env envLookup) int {
	if len(args) == 0 {
		stderrf(stderr, "wk: personality: missing subcommand (expected one of: show, set)\n")
		return 2
	}
	switch args[0] {
	case "show":
		return runPersonalityShow(ctx, args[1:], stdout, stderr, env)
	case "set":
		return runPersonalitySet(ctx, args[1:], stdout, stderr, env)
	default:
		stderrf(stderr, fmt.Sprintf("wk: personality: unknown subcommand %q (expected one of: show, set)\n", args[0]))
		return 2
	}
}

func runLanguage(ctx context.Context, args []string, stdout, stderr io.Writer, env envLookup) int {
	if len(args) == 0 {
		stderrf(stderr, "wk: language: missing subcommand (expected one of: show, set)\n")
		return 2
	}
	switch args[0] {
	case "show":
		return runLanguageShow(ctx, args[1:], stdout, stderr, env)
	case "set":
		return runLanguageSet(ctx, args[1:], stdout, stderr, env)
	default:
		stderrf(stderr, fmt.Sprintf("wk: language: unknown subcommand %q (expected one of: show, set)\n", args[0]))
		return 2
	}
}

func runBudget(ctx context.Context, args []string, stdout, stderr io.Writer, env envLookup) int {
	if len(args) == 0 {
		stderrf(stderr, "wk: budget: missing subcommand (expected one of: show, set)\n")
		return 2
	}
	switch args[0] {
	case "show":
		return runBudgetShow(ctx, args[1:], stdout, stderr, env)
	case "set":
		return notWiredExit(stderr, "budget set", "M10.2.c follow-up — manifest carries no budget field today; M6 per-watchkeeper budget seam not yet exposed via Keep")
	default:
		stderrf(stderr, fmt.Sprintf("wk: budget: unknown subcommand %q (expected one of: show, set)\n", args[0]))
		return 2
	}
}

func readWKManifestVersion(ctx context.Context, c *keepclient.Client, wkID string) (*keepclient.Watchkeeper, *keepclient.ManifestVersion, error) {
	w, err := c.GetWatchkeeper(ctx, wkID)
	if err != nil {
		return nil, nil, fmt.Errorf("get watchkeeper: %w", err)
	}
	mv, err := c.GetManifest(ctx, w.ManifestID)
	if err != nil {
		return nil, nil, fmt.Errorf("get manifest: %w", err)
	}
	return w, mv, nil
}

func popPositional(fs *flag.FlagSet, label string, stderr io.Writer) (string, int) {
	rest := fs.Args()
	if len(rest) == 0 {
		stderrf(stderr, fmt.Sprintf("wk: %s: missing positional <wk-id>\n", label))
		return "", 2
	}
	if len(rest) > 1 {
		stderrf(stderr, fmt.Sprintf("wk: %s: extra positional args after <wk-id>: %v\n", label, rest[1:]))
		return "", 2
	}
	wkID := rest[0]
	if strings.TrimSpace(wkID) == "" {
		stderrf(stderr, fmt.Sprintf("wk: %s: <wk-id> must be non-empty\n", label))
		return "", 2
	}
	return wkID, 0
}

func runPersonalityShow(ctx context.Context, args []string, stdout, stderr io.Writer, env envLookup) int {
	fs := flag.NewFlagSet("wk personality show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	wkID, code := popPositional(fs, "personality show", stderr)
	if code != 0 {
		return code
	}
	return withKeepClient(env, stderr, "personality show", func(c *keepclient.Client) int {
		_, mv, err := readWKManifestVersion(ctx, c, wkID)
		if err != nil {
			stderrf(stderr, fmt.Sprintf("wk: personality show: %v\n", err))
			return 1
		}
		fmt.Fprintln(stdout, mv.Personality)
		return 0
	})
}

func runLanguageShow(ctx context.Context, args []string, stdout, stderr io.Writer, env envLookup) int {
	fs := flag.NewFlagSet("wk language show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	wkID, code := popPositional(fs, "language show", stderr)
	if code != 0 {
		return code
	}
	return withKeepClient(env, stderr, "language show", func(c *keepclient.Client) int {
		_, mv, err := readWKManifestVersion(ctx, c, wkID)
		if err != nil {
			stderrf(stderr, fmt.Sprintf("wk: language show: %v\n", err))
			return 1
		}
		fmt.Fprintln(stdout, mv.Language)
		return 0
	})
}

func runPersonalitySet(ctx context.Context, args []string, stdout, stderr io.Writer, env envLookup) int {
	return runManifestSet(ctx, args, stdout, stderr, env, "personality set", func(mv *keepclient.ManifestVersion, value string) {
		mv.Personality = value
	})
}

func runLanguageSet(ctx context.Context, args []string, stdout, stderr io.Writer, env envLookup) int {
	return runManifestSet(ctx, args, stdout, stderr, env, "language set", func(mv *keepclient.ManifestVersion, value string) {
		mv.Language = value
	})
}

// runManifestSet centralises the read-merge-write shape used by both
// personality set and language set. The `apply` closure mutates the
// in-memory ManifestVersion before the merged shape lands on
// PutManifestVersionRequest. The server's validators (and the
// keepclient's pre-flight) reject malformed values for either column.
//
// The merge intentionally copies EVERY field of the current
// ManifestVersion into the request: omitting a field would let the
// server's column defaults overwrite a previously-set value (the
// jsonb columns have non-null defaults of `'[]'` / `'{}'`; the text
// columns nullable but populated by the prior version). A
// read-merge-write is required because the Keep server has no patch
// API today.
//
// Iter-1 critic M1: the GET→PUT path is NOT atomic. A concurrent
// `wk * set` (or any other writer racing for `VersionNo+1`) lands
// first; the loser's PUT returns 409 / [keepclient.ErrConflict]. The
// CLI surfaces a tailored "concurrent edit detected" hint instead of
// the raw keepclient error so the operator knows to re-run.
//
// Iter-1 critic M2: legacy manifest rows with empty `system_prompt`
// would trip the keepclient pre-flight after the verbatim copy. The
// optional `--system-prompt` flag overrides the field on the new
// version so the operator can repair the row in the same PUT. The
// override stays scoped to `set` (it has no `show` semantics; the
// rest of the manifest read is unchanged).
func runManifestSet(ctx context.Context, args []string, stdout, stderr io.Writer, env envLookup, label string, apply func(*keepclient.ManifestVersion, string)) int {
	fs := flag.NewFlagSet("wk "+label, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		value        string
		reason       string
		systemPrompt string
	)
	fs.StringVar(&value, "value", "", "new value for the field (required)")
	fs.StringVar(&reason, "reason", "", "operator-supplied audit text (required)")
	fs.StringVar(&systemPrompt, "system-prompt", "", "optional override for SystemPrompt; needed only to repair legacy empty-prompt rows (iter-1 critic M2)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	wkID, code := popPositional(fs, label, stderr)
	if code != 0 {
		return code
	}
	missing := []string{}
	if strings.TrimSpace(value) == "" {
		missing = append(missing, "--value")
	}
	if strings.TrimSpace(reason) == "" {
		missing = append(missing, "--reason")
	}
	if len(missing) > 0 {
		stderrf(stderr, fmt.Sprintf("wk: %s: missing required flag(s): %s\n", label, strings.Join(missing, ", ")))
		return 2
	}
	return withKeepClient(env, stderr, label, func(c *keepclient.Client) int {
		_, mv, err := readWKManifestVersion(ctx, c, wkID)
		if err != nil {
			stderrf(stderr, fmt.Sprintf("wk: %s: %v\n", label, err))
			return 1
		}
		apply(mv, value)
		if systemPrompt != "" {
			mv.SystemPrompt = systemPrompt
		}
		req := keepclient.PutManifestVersionRequest{
			VersionNo:                  mv.VersionNo + 1,
			SystemPrompt:               mv.SystemPrompt,
			Tools:                      mv.Tools,
			AuthorityMatrix:            mv.AuthorityMatrix,
			KnowledgeSources:           mv.KnowledgeSources,
			Personality:                mv.Personality,
			Language:                   mv.Language,
			Model:                      mv.Model,
			Autonomy:                   mv.Autonomy,
			NotebookTopK:               mv.NotebookTopK,
			NotebookRelevanceThreshold: mv.NotebookRelevanceThreshold,
		}
		resp, err := c.PutManifestVersion(ctx, mv.ManifestID, req)
		if err != nil {
			if errors.Is(err, keepclient.ErrConflict) {
				stderrf(stderr, fmt.Sprintf(
					"wk: %s: concurrent edit detected — a competing manifest version landed first; re-run the command to read the new VersionNo and retry\n",
					label,
				))
				return 1
			}
			stderrf(stderr, fmt.Sprintf("wk: %s: %v\n", label, err))
			return 1
		}
		fmt.Fprintf(stdout, "wk: %s ok manifest_id=%s version_no=%d version_id=%s reason=%q\n", label, mv.ManifestID, req.VersionNo, resp.ID, reason)
		return 0
	})
}

func runBudgetShow(ctx context.Context, args []string, stdout, stderr io.Writer, env envLookup) int {
	fs := flag.NewFlagSet("wk budget show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		agentID  string
		fromStr  string
		toStr    string
		grainStr string
	)
	fs.StringVar(&agentID, "agent", "", "watchkeeper UUID to rollup costs for (required)")
	fs.StringVar(&fromStr, "from", "", "inclusive lower bound (RFC3339, required)")
	fs.StringVar(&toStr, "to", "", "exclusive upper bound (RFC3339, required)")
	fs.StringVar(&grainStr, "grain", "daily", "bucket grain: daily|weekly")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	missing := []string{}
	if strings.TrimSpace(agentID) == "" {
		missing = append(missing, "--agent")
	}
	if strings.TrimSpace(fromStr) == "" {
		missing = append(missing, "--from")
	}
	if strings.TrimSpace(toStr) == "" {
		missing = append(missing, "--to")
	}
	if len(missing) > 0 {
		stderrf(stderr, fmt.Sprintf("wk: budget show: missing required flag(s): %s\n", strings.Join(missing, ", ")))
		return 2
	}
	from, err := time.Parse(time.RFC3339, strings.TrimSpace(fromStr))
	if err != nil {
		stderrf(stderr, fmt.Sprintf("wk: budget show: invalid --from: %v\n", err))
		return 2
	}
	to, err := time.Parse(time.RFC3339, strings.TrimSpace(toStr))
	if err != nil {
		stderrf(stderr, fmt.Sprintf("wk: budget show: invalid --to: %v\n", err))
		return 2
	}
	return withKeepClient(env, stderr, "budget show", func(c *keepclient.Client) int {
		resp, err := c.CostRollups(ctx, keepclient.CostRollupsRequest{
			AgentID: agentID,
			From:    from,
			To:      to,
			Grain:   keepclient.CostRollupGrain(grainStr),
		})
		if err != nil {
			stderrf(stderr, fmt.Sprintf("wk: budget show: %v\n", err))
			return 1
		}
		data, mErr := json.MarshalIndent(resp, "", "  ")
		if mErr != nil {
			stderrf(stderr, fmt.Sprintf("wk: budget show: marshal: %v\n", mErr))
			return 1
		}
		if _, wErr := stdout.Write(append(data, '\n')); wErr != nil {
			stderrf(stderr, fmt.Sprintf("wk: budget show: write: %v\n", wErr))
			return 1
		}
		return 0
	})
}
