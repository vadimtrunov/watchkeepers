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

## Check 3 — `gh` active account matches `origin` owner

Commands:
```bash
owner=$(git remote get-url origin | sed -E 's#.*[:/]([^/]+)/[^/]+(\.git)?$#\1#')
active=$(gh api user --jq .login 2>/dev/null)
echo "owner=$owner active=$active"
```
Pass: `$owner == $active`.
Fail (or `active` empty): stop with:
> `rdd` preflight failed: active `gh` account (`<active>`) is not the owner of `origin` (`<owner>`). Switch with: `gh auth switch --user <owner>`

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

## Stop format

When stopping on any failure, print exactly one message using the format
above. Do not attempt the next check. Do not run any other part of the skill.
