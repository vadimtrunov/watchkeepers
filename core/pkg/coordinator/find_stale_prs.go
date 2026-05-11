package coordinator

// find_stale_prs — Coordinator read tool (M8.2.d).
//
// Resolution order (handler closure body):
//
//  1. ctx.Err() pre-check (no network on cancel).
//  2. Repo owner arg → typed string + GitHub login shape pre-validation.
//     Refusal text NEVER echoes the raw value.
//  3. Repo name arg → typed string + GitHub repository-name shape
//     pre-validation. Refusal text NEVER echoes raw value.
//  4. Reviewer login arg → typed string + GitHub login shape
//     pre-validation. Refusal text NEVER echoes raw value.
//  5. Age threshold arg → typed int (accepts JSON-decoded float64)
//     + range pre-validation. Refusal text NEVER echoes raw value.
//  6. Auto-paginate via M8.2.d [github.Client.ListPullRequests] up to a
//     hard cap ([maxStalePRs] / [maxStalePages]) so the Coordinator
//     does not have to drive cursor loops.
//  7. Filter pulled PRs by (a) requested-reviewer membership of the
//     supplied login AND (b) `UpdatedAt` older than the threshold;
//     surface `truncated=true` when the cap fired.
//
// Audit discipline: handler returns a [agentruntime.ToolResult] only;
// the runtime's tool-result reflection layer (M5.6.b) is the audit
// boundary. NO direct keeperslog.Append from this file (asserted via
// source-grep AC).
//
// PII discipline: every refusal text uses the [findStalePRsRefusalPrefix]
// + constant suffix; raw user-supplied arg values NEVER appear. The
// success Output projection drops author logins (the M8.2.c lesson #10
// extension — repeated identifier echoes in nested entries undo
// root-level omission); only `number` + `title` + `html_url` +
// `updated_at` + `age_days` ride per-PR. The agent has all the inputs
// in its call args; the success path does not need to echo them.

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/github"
	agentruntime "github.com/vadimtrunov/watchkeepers/core/pkg/runtime"
)

// defaultStalePRsNow is the production clock the public factory binds.
// Tests reach for the unexported [newFindStalePRsHandlerWithClock] to
// substitute a fixed `time.Time` so the per-test override stays scoped
// to one handler instance — no package-level mutable shared state,
// no race under -parallel. Mirrors the M8.2.b functional-clock-
// injection precedent.
var defaultStalePRsNow = time.Now

// FindStalePRsName is the manifest tool name the Coordinator
// dispatcher registers this handler under. Mirrors the toolset entry
// in `deploy/migrations/027_coordinator_manifest_v4_seed.sql`. Callers
// use this const rather than the bare string so a future rename is a
// one-line change here.
const FindStalePRsName = "find_stale_prs"

// findStalePRsRefusalPrefix is the leading namespace for every
// [agentruntime.ToolResult.Error] string this handler surfaces. Mirrors
// the M8.2.a/b/c per-tool-prefix convention — `coordinator: <tool>:
// <reason>` — so the agent reads multi-tool refusal text under one
// parsing convention.
const findStalePRsRefusalPrefix = "coordinator: " + FindStalePRsName + ": "

// FindStalePRs argument keys — read from [agentruntime.ToolCall.Arguments].
// Pulled into constants so the future tool schema (M8.2 follow-up)
// references the same identifiers.
const (
	// ToolArgRepoOwner carries the GitHub owner login (user or org
	// handle, e.g. `"vadimtrunov"`). Required; validated against
	// [stalePRsOwnerPattern].
	ToolArgRepoOwner = "repo_owner"

	// ToolArgRepoName carries the GitHub repository name (e.g.
	// `"watchkeepers"`). Required; validated against
	// [stalePRsRepoPattern].
	ToolArgRepoName = "repo_name"

	// ToolArgReviewerLogin carries the GitHub login of the reviewer
	// to filter by (e.g. `"alice"`). Required; validated against
	// [stalePRsOwnerPattern] (the same shape as the owner login —
	// GitHub uses one login namespace for both users + orgs and
	// review-requested principals are always user logins).
	ToolArgReviewerLogin = "reviewer_login"

	// ToolArgStalePRsAgeDays carries a positive integer day count.
	// The handler filters PRs whose `updated_at` is older than
	// `now - <N>d`. Required; validated 1 ≤ N ≤ [maxStalePRsAgeDays].
	ToolArgStalePRsAgeDays = "age_threshold_days"
)

// Pagination + scope caps. Bound the blast radius of a single
// `find_stale_prs` call so a repository with thousands of open PRs
// cannot pull a runaway list into the agent's context window.
const (
	// maxStalePRs caps the total PRs collected across every page
	// before the handler stops paginating and surfaces
	// `truncated=true`. 200 covers an extreme reviewer backlog while
	// keeping the agent's prompt-window cost predictable.
	maxStalePRs = 200

	// maxStalePages caps the number of [github.Client.ListPullRequests]
	// calls the handler makes per dispatch. Defence-in-depth on top of
	// [maxStalePRs] in case the per-PR filter rejects most of each
	// page — at the configured `per_page=100`, 10 pages = 1000 PRs
	// pre-filter, but the PR cap (POST-filter) fires first under
	// typical reviewer-filter selectivity.
	maxStalePages = 10

	// maxStalePRsAgeDays is the upper bound on `age_threshold_days`.
	// 365 covers "any PR with stale review request in the last year"
	// while rejecting nonsensical values.
	maxStalePRsAgeDays = 365

	// stalePRsPageSize is the GitHub `per_page` knob the handler
	// sends per call. 100 is GitHub's documented maximum and
	// minimises page count under M8.2.d's typical repo size.
	stalePRsPageSize = 100
)

// stalePRsOwnerPattern matches the GitHub login shape (user or org):
// alphanumeric + hyphen, no leading/trailing hyphen, max 39 chars.
// Mirrors `core/pkg/github/client.go::ownerPattern` exactly so a
// future drift in the adapter regex requires a matching update here.
var stalePRsOwnerPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9]|-(?:[A-Za-z0-9])){0,38}$`)

// stalePRsRepoPattern matches the GitHub repository-name shape:
// leading character MUST be alphanumeric or `_` (NOT `.` or `-`),
// remainder admits alphanumeric / `_` / `.` / `-`, max 100 chars.
// Mirrors `core/pkg/github/client.go::repoPattern` exactly so a future
// drift in the adapter regex requires a matching update here. The
// leading-char restriction rejects path-traversal shapes (`..`,
// `.foo`, `-foo`) at the handler boundary BEFORE the adapter call —
// defence-in-depth (the adapter ALSO rejects, but rejecting earlier
// keeps the raw value out of the M5.6.b reflector path). Iter-1
// critic Major.
var stalePRsRepoPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.\-]{0,99}$`)

// GitHubPRLister is the single-method interface
// [NewFindStalePRsHandler] consumes for the GitHub read. Mirrors
// `github.Client.ListPullRequests`'s signature exactly so production
// code passes a `*github.Client` through verbatim; tests inject a
// hand-rolled fake without touching the HTTP client. The interface
// lives at the consumer (this package) per the project's "interfaces
// belong to the consumer" convention, mirroring [JiraSearcher] from
// M8.2.b and [JiraFieldUpdater] from M8.2.a.
type GitHubPRLister interface {
	ListPullRequests(
		ctx context.Context,
		owner github.RepoOwner,
		repo github.RepoName,
		opts github.ListPullRequestsOptions,
	) (github.ListPullRequestsResult, error)
}

// Compile-time assertion that the production [*github.Client]
// satisfies the consumer interface. A future signature drift on the
// M8.2.d adapter surface fails build, not production.
var _ GitHubPRLister = (*github.Client)(nil)

// NewFindStalePRsHandler constructs the [agentruntime.ToolHandler] the
// Coordinator dispatcher registers under [FindStalePRsName]. Wraps the
// M8.2.d github read path with the M8.2.d authority discipline
// (validate every arg before the network exchange, refuse token-shaped
// inputs, cap pagination, drop identifier echoes from Output).
//
// Panics on a nil `lister` per the M*.c.* / M8.2.b "panic on nil
// deps" discipline.
//
// Args contract (read from [agentruntime.ToolCall.Arguments]):
//
//   - `repo_owner`         (string, required): GitHub login of the
//     repo owner (user or org). Shape: alphanumeric + hyphen, ≤39
//     chars.
//   - `repo_name`          (string, required): GitHub repository
//     name. Shape: alphanumeric/./_/-, ≤100 chars.
//   - `reviewer_login`     (string, required): GitHub login of the
//     reviewer to filter by. Shape: same as `repo_owner`.
//   - `age_threshold_days` (number, required): integer in
//     [1, [maxStalePRsAgeDays]]. Accepts JSON-decoded `float64` so
//     long as the value is a non-negative whole number.
//
// Refusal contract — returned via [agentruntime.ToolResult.Error]
// (NOT a Go error so the agent can re-plan; mirrors the M8.2.a/b/c
// channel discipline):
//
//   - missing / non-string / empty / shape-violating `repo_owner`
//   - missing / non-string / empty / shape-violating `repo_name`
//   - missing / non-string / empty / shape-violating `reviewer_login`
//   - missing / non-number / out-of-range / non-integer
//     `age_threshold_days`
//
// Refusal text NEVER echoes a raw arg value — the agent already has
// the value in its call args and can re-plan; surfacing it here would
// defeat the pre-network gate.
//
// Output (success) — keys on the returned [agentruntime.ToolResult.Output]:
//
//   - `pull_requests`  (array of object): one entry per stale PR with
//     keys `number`, `title`, `html_url`, `updated_at` (RFC3339 UTC),
//     `age_days` (int — APPROXIMATE; days since the PR's `updated_at`,
//     computed against the closure-captured clock; treat as a hint,
//     not a contract).
//   - `total_returned` (int): `len(pull_requests)`.
//   - `truncated`      (bool): true when the handler stopped
//     paginating because [maxStalePRs] OR [maxStalePages] fired before
//     GitHub reported `NextPage=0` (clean terminus). The agent treats
//     this as "more pages exist; narrow the scope or raise the
//     threshold".
//   - `scope`          (object): the structured scope summary echoed
//     back for the agent's self-audit — keys `repo_owner`, `repo_name`,
//     `age_threshold_days`. `reviewer_login` is INTENTIONALLY OMITTED
//     (M8.2.b lesson #10 / M8.2.c lesson #10 generalisation: PII reach
//     in success output — the M8.2.c iter-1 codex Major #3 finding
//     extended this to per-message echoes, but here a single login at
//     the root would normalise the field across calls — drop it). The
//     agent has `reviewer_login` in its [agentruntime.ToolCall.Arguments].
//     Per-PR projection ALSO drops author login (`pull_requests[].user`
//     is omitted) for the same reason: the success path is a PII reach
//     surface and GitHub logins are subject to the same downstream
//     audit-pipe concerns the M8.2.c iter-1 codex Major #3 raised for
//     Slack user ids.
//
// Forwarded errors — returned as Go `error` (the runtime treats this
// as transport failure and reflects via the M5.6.b auto-reflection):
//
//   - github.Client.ListPullRequests returns wrapped errors (network /
//     API / pagination contract violation) surface verbatim with
//     prefix `"coordinator: find_stale_prs: %w"`.
func NewFindStalePRsHandler(lister GitHubPRLister) agentruntime.ToolHandler {
	return newFindStalePRsHandlerWithClock(lister, defaultStalePRsNow)
}

// newFindStalePRsHandlerWithClock is the test-internal factory that
// lets tests inject a fixed clock without mutating package state. The
// public [NewFindStalePRsHandler] wraps this with [defaultStalePRsNow];
// production code never reaches this surface. Same nil-lister panic
// discipline; clock MUST also be non-nil.
func newFindStalePRsHandlerWithClock(lister GitHubPRLister, clock func() time.Time) agentruntime.ToolHandler {
	if lister == nil {
		panic("coordinator: NewFindStalePRsHandler: lister must not be nil")
	}
	if clock == nil {
		panic("coordinator: NewFindStalePRsHandler: clock must not be nil")
	}
	return func(ctx context.Context, call agentruntime.ToolCall) (agentruntime.ToolResult, error) {
		if err := ctx.Err(); err != nil {
			return agentruntime.ToolResult{}, err
		}

		owner, refusal := readRepoOwnerArg(call.Arguments)
		if refusal != "" {
			return agentruntime.ToolResult{Error: refusal}, nil
		}

		repo, refusal := readRepoNameArg(call.Arguments)
		if refusal != "" {
			return agentruntime.ToolResult{Error: refusal}, nil
		}

		reviewer, refusal := readReviewerLoginArg(call.Arguments)
		if refusal != "" {
			return agentruntime.ToolResult{Error: refusal}, nil
		}

		ageDays, refusal := readStalePRsAgeDaysArg(call.Arguments)
		if refusal != "" {
			return agentruntime.ToolResult{Error: refusal}, nil
		}

		now := clock()
		threshold := now.Add(-time.Duration(ageDays) * 24 * time.Hour)

		prs, truncated, err := paginateStalePRs(ctx, lister, owner, repo, reviewer, threshold)
		if err != nil {
			return agentruntime.ToolResult{}, fmt.Errorf("coordinator: find_stale_prs: %w", err)
		}

		return agentruntime.ToolResult{
			Output: map[string]any{
				"pull_requests":  projectStalePRs(prs, now),
				"total_returned": len(prs),
				"truncated":      truncated,
				"scope": map[string]any{
					"repo_owner":         owner,
					"repo_name":          repo,
					"age_threshold_days": ageDays,
				},
			},
		}, nil
	}
}

// readRepoOwnerArg projects the `repo_owner` arg into a typed string.
// Returns (owner, "") on success; ("", refusalText) on validation
// failure. Refusal text NEVER echoes the raw value (PII discipline).
func readRepoOwnerArg(args map[string]any) (string, string) {
	raw, present := args[ToolArgRepoOwner]
	if !present {
		return "", findStalePRsRefusalPrefix + "missing required arg: " + ToolArgRepoOwner
	}
	str, ok := raw.(string)
	if !ok {
		return "", findStalePRsRefusalPrefix + ToolArgRepoOwner + " must be a string"
	}
	if str == "" {
		return "", findStalePRsRefusalPrefix + ToolArgRepoOwner + " must be non-empty"
	}
	if !stalePRsOwnerPattern.MatchString(str) {
		return "", findStalePRsRefusalPrefix + ToolArgRepoOwner +
			" must match GitHub login shape (alphanumeric/hyphen, no leading/trailing hyphen, ≤39 chars)"
	}
	return str, ""
}

// readRepoNameArg projects the `repo_name` arg into a typed string.
// Refusal text NEVER echoes the raw value.
func readRepoNameArg(args map[string]any) (string, string) {
	raw, present := args[ToolArgRepoName]
	if !present {
		return "", findStalePRsRefusalPrefix + "missing required arg: " + ToolArgRepoName
	}
	str, ok := raw.(string)
	if !ok {
		return "", findStalePRsRefusalPrefix + ToolArgRepoName + " must be a string"
	}
	if str == "" {
		return "", findStalePRsRefusalPrefix + ToolArgRepoName + " must be non-empty"
	}
	if !stalePRsRepoPattern.MatchString(str) {
		return "", findStalePRsRefusalPrefix + ToolArgRepoName +
			" must match GitHub repository-name shape (alphanumeric/./_/-, ≤100 chars)"
	}
	return str, ""
}

// readReviewerLoginArg projects the `reviewer_login` arg into a typed
// string. Refusal text NEVER echoes the raw value.
func readReviewerLoginArg(args map[string]any) (string, string) {
	raw, present := args[ToolArgReviewerLogin]
	if !present {
		return "", findStalePRsRefusalPrefix + "missing required arg: " + ToolArgReviewerLogin
	}
	str, ok := raw.(string)
	if !ok {
		return "", findStalePRsRefusalPrefix + ToolArgReviewerLogin + " must be a string"
	}
	if str == "" {
		return "", findStalePRsRefusalPrefix + ToolArgReviewerLogin + " must be non-empty"
	}
	if !stalePRsOwnerPattern.MatchString(str) {
		return "", findStalePRsRefusalPrefix + ToolArgReviewerLogin +
			" must match GitHub login shape (alphanumeric/hyphen, no leading/trailing hyphen, ≤39 chars)"
	}
	return str, ""
}

// readStalePRsAgeDaysArg projects the `age_threshold_days` arg into a
// typed int. Accepts `int`, `int64`, and JSON-decoded `float64`. The
// runtime's [agentruntime.ToolCall.Arguments] is `map[string]any` and
// JSON numbers decode as `float64`; in-process Go callers may pass
// either of the int forms. Rejects non-integer floats, negative
// values, zero, and values exceeding [maxStalePRsAgeDays]. Refusal
// text NEVER echoes the raw value.
func readStalePRsAgeDaysArg(args map[string]any) (int, string) {
	raw, present := args[ToolArgStalePRsAgeDays]
	if !present {
		return 0, findStalePRsRefusalPrefix + "missing required arg: " + ToolArgStalePRsAgeDays
	}
	var n int
	switch v := raw.(type) {
	case int:
		n = v
	case int64:
		if v < int64(-(1<<31)) || v > int64(1<<31-1) {
			return 0, findStalePRsRefusalPrefix + ToolArgStalePRsAgeDays + " out of range"
		}
		n = int(v)
	case float64:
		if v != float64(int(v)) {
			return 0, findStalePRsRefusalPrefix + ToolArgStalePRsAgeDays + " must be an integer"
		}
		n = int(v)
	default:
		return 0, findStalePRsRefusalPrefix + ToolArgStalePRsAgeDays + " must be a number"
	}
	if n < 1 {
		return 0, findStalePRsRefusalPrefix + ToolArgStalePRsAgeDays + " must be ≥ 1"
	}
	if n > maxStalePRsAgeDays {
		return 0, findStalePRsRefusalPrefix + ToolArgStalePRsAgeDays +
			" must be ≤ " + strconv.Itoa(maxStalePRsAgeDays)
	}
	return n, ""
}

// paginateStalePRs drives the M8.2.d page-number pagination up to
// the [maxStalePRs] / [maxStalePages] caps. Returns the filtered PRs,
// a `truncated` flag, and the underlying ListPullRequests error if
// the M8.2.d layer surfaced one. The handler dispatches the API with
// `state=open`, `sort=updated`, `direction=asc` so the OLDEST stale
// PRs surface first (operationally useful set: the agent gets the
// most-stale tickets even if pagination truncates).
//
// Pagination-stability caveat (iter-1 codex Major):
//
//   - GitHub paginates `/repos/{owner}/{repo}/pulls` with a page-number
//     cursor, and the sort key (`updated`) is MUTABLE. Under concurrent
//     PR activity (someone pushes a commit, someone re-requests a
//     review) a PR's `updated` shifts; the same PR may then appear on
//     two adjacent pages (duplicate in the result set) OR fall off the
//     previous page before this scan reaches the next page (skip).
//   - The handler accepts this tradeoff because (a) the scan runs at
//     human-scale frequency (daily-briefing cycle) so the next run
//     re-surfaces any skipped PR, (b) the agent re-plans on
//     `truncated=true` rather than relying on per-call exhaustiveness,
//     (c) the `asc` ordering biases the scan toward the most-stale
//     PRs, which is the operationally useful set.
//   - A stable-cursor alternative would require GitHub's GraphQL API
//     (REST does not offer cursor pagination on `/pulls`). M9+ may
//     migrate if the M8.x traffic surfaces a real duplicate-rate
//     regression; for M8.2.d the REST + page-number shape is the
//     simpler ship.
//
// Filter sequence per page:
//
//   - Walk every PR in the page; keep it only if (a) it carries
//     `reviewer` in `RequestedReviewers` AND (b) `UpdatedAt` is non-
//     zero AND strictly before `threshold`. (A zero `UpdatedAt` cannot
//     be classified as stale because parse failure is observationally
//     identical to "very recent" — we refuse to classify rather than
//     silently flag.)
//   - When the cumulative collected count reaches [maxStalePRs],
//     surface `truncated=true` immediately. The `more` signal here is
//     defence-in-depth: it MUST be true when more PRs remain on this
//     page OR more pages exist (GitHub `NextPage != 0`) OR a
//     server-contract-violation case (we read more PRs than
//     `stalePRsPageSize` requested — symmetric to the M8.2.b/c
//     "server may overshoot" lesson).
//
// Truncation semantics (matches the M8.2.b/c symmetric defence on
// every pagination signal):
//
//   - Issue cap fires INSIDE a page → `truncated=true` whenever ANY of:
//     more PRs remain on this page OR `NextPage != 0`.
//   - Page cap fires before GitHub reports `NextPage=0` →
//     unconditionally `truncated=true` (the for-loop boundary
//     fall-through).
//   - GitHub returns `NextPage=0` with NO unread PRs → `truncated=false`
//     (clean termination).
func paginateStalePRs(
	ctx context.Context,
	lister GitHubPRLister,
	owner, repo, reviewer string,
	threshold time.Time,
) ([]github.PullRequest, bool, error) {
	collected := make([]github.PullRequest, 0, maxStalePRs)
	page := 1
	for iter := 0; iter < maxStalePages; iter++ {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		res, err := lister.ListPullRequests(ctx, github.RepoOwner(owner), github.RepoName(repo),
			github.ListPullRequestsOptions{
				State:     "open",
				Sort:      "updated",
				Direction: "asc",
				PerPage:   stalePRsPageSize,
				Page:      page,
			})
		if err != nil {
			return nil, false, err
		}
		for i, pr := range res.PullRequests {
			if !matchesStaleFilter(pr, reviewer, threshold) {
				continue
			}
			collected = append(collected, pr)
			if len(collected) >= maxStalePRs {
				// More work on THIS page, OR another page exists ↔
				// truncate. Symmetric defence on every signal source.
				more := res.NextPage != 0 || i+1 < len(res.PullRequests)
				return collected, more, nil
			}
		}
		if res.NextPage == 0 {
			return collected, false, nil
		}
		page = res.NextPage
	}
	// Page cap fired before NextPage went to zero. Truncated.
	return collected, true, nil
}

// matchesStaleFilter reports whether a PR matches the M8.2.d filter:
//
//   - `reviewer` is a member of `pr.RequestedReviewers`.
//   - `pr.UpdatedAt` is non-zero AND strictly before `threshold`.
//
// A zero `UpdatedAt` (parse failure / field absent) is INTENTIONALLY
// excluded from the stale set rather than treated as "infinitely old"
// — a parse-failure PR mis-classified as stale would surface
// false-positive nudges to the agent. Mirrors the M8.2.b
// `parseTime`-zero-guard lesson.
func matchesStaleFilter(pr github.PullRequest, reviewer string, threshold time.Time) bool {
	if pr.UpdatedAt.IsZero() {
		return false
	}
	if !pr.UpdatedAt.Before(threshold) {
		return false
	}
	for _, r := range pr.RequestedReviewers {
		if strings.EqualFold(r, reviewer) {
			return true
		}
	}
	return false
}

// projectStalePRs flattens [github.PullRequest] values into the wire
// shape the agent receives. `age_days` is derived from `UpdatedAt`
// against the supplied `now`; a zero `UpdatedAt` cannot reach this
// projection because [matchesStaleFilter] excludes it pre-filter, but
// the projector still guards defensively to surface `age_days=0` if a
// regression admits one.
//
// PII discipline: author login is NOT echoed (the success-path Output
// is a PII reach surface and GitHub logins are subject to the same
// downstream audit-pipe concerns as Slack user ids — M8.2.c iter-1
// codex Major #3). The agent has `reviewer_login` in its call args and
// does not need any reviewer or author login back in the result. PR
// number + title + html_url are sufficient for the agent to construct a
// nudge or briefing line.
func projectStalePRs(prs []github.PullRequest, now time.Time) []map[string]any {
	out := make([]map[string]any, 0, len(prs))
	for _, pr := range prs {
		entry := map[string]any{
			"number":   pr.Number,
			"title":    pr.Title,
			"html_url": pr.HTMLURL,
		}
		if !pr.UpdatedAt.IsZero() {
			entry["updated_at"] = pr.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z")
			entry["age_days"] = int(now.UTC().Sub(pr.UpdatedAt.UTC()).Hours() / 24)
		} else {
			entry["updated_at"] = ""
			entry["age_days"] = 0
		}
		out = append(out, entry)
	}
	return out
}
