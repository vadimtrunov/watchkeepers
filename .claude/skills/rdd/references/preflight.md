# Phase 0 — Preflight

Run these checks in order. Any failure stops the skill immediately with the
prescribed message. The skill does not attempt to auto-fix preflight failures.

## Check 1 — ROADMAP file exists

Command:
```bash
ls docs/ROADMAP-*.md 2>/dev/null
```
Pass: at least one matching file printed.
Fail: empty output. Stop with:
> `rdd` preflight failed: no `docs/ROADMAP-*.md` file found. Create a roadmap before running the skill.

## Check 2 — CI is configured

Command:
```bash
ls .github/workflows/*.yml .github/workflows/*.yaml 2>/dev/null | head -1
```
Pass: at least one workflow file printed.
Fail: empty output. Stop with:
> `rdd` preflight failed: no CI workflow in `.github/workflows/`. CI is a hard prerequisite — set up CI before running `rdd`.

## Check 3 — `gh` active account has write access to `origin`

Identity equality (`owner == active`) is too strict for organization
repositories where the owner is an org and the active user is a member with
sufficient rights. Check the active user's permission level instead.

Commands:
```bash
url=$(git remote get-url origin)
owner=$(echo "$url" | sed -E 's#.*[:/]([^/]+)/[^/]+(\.git)?$#\1#')
repo=$(echo  "$url" | sed -E 's#.*[:/][^/]+/([^/]+?)(\.git)?$#\1#')
active=$(gh api user --jq .login 2>/dev/null)
perm=$(gh api "repos/$owner/$repo/collaborators/$active/permission" --jq .permission 2>/dev/null)
echo "owner=$owner repo=$repo active=$active perm=$perm"
```
Pass: `$perm` is one of `admin`, `maintain`, or `write`.
Fail (or `active` / `perm` empty): stop with:
> `rdd` preflight failed: active `gh` account (`<active>`) does not have write access to `<owner>/<repo>` (permission: `<perm>`). Switch with: `gh auth switch --user <user-with-write>`

## Check 4 — Working tree is clean

Command:
```bash
git status --porcelain
```
Pass: empty output.
Fail: non-empty output. Stop with:
> `rdd` preflight failed: working tree is dirty. Commit or stash changes before running.

## Check 5 — Current branch is `main` or a resumable `rdd/*`

Commands:
```bash
branch=$(git rev-parse --abbrev-ref HEAD)
echo "branch=$branch"
```
Pass:
- `$branch == main` — proceed to Phase 1 (fresh run).
- `$branch` matches `rdd/*` AND a corresponding `TASK-*.md` with `Status: in-progress` exists in the working tree — proceed in resume mode.

Fail otherwise. Stop with:
> `rdd` preflight failed: current branch `<branch>` is neither `main` nor a resumable `rdd/*` branch. Checkout `main` (or run `/rdd resume` from the correct `rdd/*` branch).

## Mark rdd active

After Checks 1–5 all pass and before dispatching the Phase 1 planner, set
the active marker:

```bash
mkdir -p .omc/state
touch .omc/state/rdd-active
```

The marker is the canonical signal that an rdd run is in progress. It is
read by the Hard rule 5 reinforcement hooks
(`.claude/skills/rdd/hooks/rdd-{post-agent,stop-check}.sh`) so they only fire during
rdd work — including Phase 1 before any branch or TASK file exists, which
is the exact failure window for the silent-exit incident catalogued in
`FEEDBACK.md` 2026-05-05. The marker is removed in Phase 7b cleanup
(see `SKILL.md` §"Phase map" step 7b) and is gitignored under `.omc/`.

## Stop format

When stopping on any failure, print exactly one message using the format
above. Do not attempt the next check. Do not run any other part of the skill.
