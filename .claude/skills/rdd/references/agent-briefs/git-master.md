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

## Mode — merge (Phase 7)

Input:
- **PR number**.
- **Feature branch name** (`rdd/<slug>`). If not supplied, derive it
  from the PR head ref via `gh pr view <pr> --json headRefName --jq
  .headRefName` and use that as the branch to delete in step 3.
- **Merge method**: default `squash`; use the repo default if configurable
  via `.github/settings.yml` or `gh api repos/:owner/:repo`.
- **Leaf roadmap id** (e.g. `M1.9.a`) — the one to mark `[x]`.
- **ROADMAP path** (e.g. `docs/ROADMAP-phase1.md`).

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
4. Update the ROADMAP:
   - Mark the leaf `- [ ] **<leaf-id>** ...` → `- [x] **<leaf-id>** ...`.
   - Walk ancestors upward (see `references/roadmap-migration.md` §Cascade).
     For each ancestor whose direct children are all `[x]`, flip the
     ancestor to `[x]`.
5. Commit and push:
   ```bash
   git add docs/ROADMAP-*.md
   git commit -m "chore(roadmap): mark <leaf-id> complete"
   git push origin main
   ```

Hard rules:
- Never merge without an explicit instruction from the orchestrator (which
  only sends this brief after Gate 3).
- If the ROADMAP update commit fails to push (non-fast-forward), do not
  force. Pull, attempt to reapply the checkbox edits on top of the new
  HEAD, commit, push. If conflicts persist, return failure — the merge
  already happened, only the bookkeeping is stuck, and the orchestrator
  will escalate.
- Report: merge sha, new `main` sha, list of ROADMAP ids flipped (leaf plus
  any cascaded ancestors).
