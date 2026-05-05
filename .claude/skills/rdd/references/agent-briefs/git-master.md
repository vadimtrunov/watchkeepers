# Agent Brief — git-master

Two modes:
- **pr** — Phase 5: finalize commit style, push, `gh pr create`.
- **merge** — Phase 7: merge the PR, update ROADMAP checkboxes, push, clean
  up branches.

## Preferred `subagent_type`

`oh-my-claudecode:git-master` (fallback: `general-purpose`).

## Mode — pr (Phase 5)

Input:
- **TASK file path** (for Scope, Acceptance criteria, Test plan — used to
  compose the PR body).
- **Branch name** (`rdd/<slug>`).
- **Base branch** (`main`).
- **Commit-style policy**: follow the style already present in `git log` on
  `main` (heuristic: if ≥ 3 of the last 10 commits use conventional-commits,
  use conventional-commits; otherwise use plain imperative prose).

Actions:
1. Review the commit series on the branch. If commits are coherent, leave
   them. If there is obvious noise (typo fixups, "wip" messages), offer to
   squash — but do not squash without explicit confirmation (report back to
   the orchestrator; orchestrator asks the operator).
2. Push the branch: `git push --set-upstream origin rdd/<slug>`.
3. Open the PR:
   ```bash
   gh pr create --base main --head rdd/<slug> \
     --title "<title>" \
     --body "$(cat <<'EOF'
   ## Summary
   <Scope paragraph from TASK>

   ## Acceptance criteria
   <checklist — use the TASK list as-is>

   ## Test plan
   <checklist — use the TASK list as-is>

   ## Linked ROADMAP item
   docs/ROADMAP-<phase>.md §M# → <roadmap-id>

   🤖 Generated with /rdd
   EOF
   )"
   ```
4. Return the PR URL.

Hard rules:
- Do NOT merge in this mode.
- Do NOT force-push. If the remote branch exists and diverges, stop and
  report.
- PR title ≤ 70 chars. Derived from TASK title (prepend `<roadmap-id>: `).
- `--draft` is off (ready for review).

## Mode — merge (Phase 7b)

Phase 7b is **merge-only**. The lessons append, the FEEDBACK append, and
the ROADMAP checkbox toggle have already been committed to the feature
branch by the `writer` agent in Phase 7a. They reach `main` as part of
the squash commit. There is no follow-up commit on `main`.

Input:
- **PR number**.
- **Feature branch name** (`rdd/<slug>`). If not supplied, derive it
  from the PR head ref via `gh pr view <pr> --json headRefName --jq
  .headRefName` and use that as the branch to delete in step 3.
- **Merge method**: default `squash`; use the repo default if configurable
  via `.github/settings.yml` or `gh api repos/:owner/:repo`.

Actions:
1. Merge the PR:
   ```bash
   gh pr merge <pr> --<method> --delete-branch
   ```
   (`--delete-branch` removes the remote branch.)
2. Pull main locally:
   ```bash
   git checkout main
   git pull --ff-only origin main
   ```
3. Delete the local branch:
   ```bash
   git branch -D rdd/<slug>
   ```

Hard rules:
- Never merge without an explicit instruction from the orchestrator (which
  only sends this brief after Gate 3).
- Do **not** open a follow-up `chore(roadmap)` PR or commit. The
  ROADMAP toggle is already on `main` via the squash commit because
  Phase 7a wrote it on the feature branch.
- Do **not** open a follow-up `docs(lessons)` PR or commit. Same reason.
- Report: merge sha, new `main` sha.
