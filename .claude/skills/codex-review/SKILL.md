---
name: codex-review
description: Use when you need a fast, severity-rated second-opinion review of the current branch's diff against main from the codex CLI — typically before opening a PR, after a refactor, or when you want an independent reviewer pass without spinning up the full ship-roadmap-item flow. Skip if the change is uncommitted-only (use `--uncommitted` variant inline) or if a full multi-reviewer flow is wanted (use ship-roadmap-item).
triggers:
  - "review with codex"
  - "codex review"
  - "ревью через codex"
  - "second opinion via codex"
  - "/codex-review"
---

# codex-review — Standalone codex-based branch review

Wrap a single `codex review` invocation against the current branch's diff
vs the base branch (default `main`), translate codex's output into a flat
severity-rated list, and return it to the operator. No auto-apply, no
follow-up commits — the operator decides what to fix.

This skill is the lightweight version of the `codex` reviewer used inside
`ship-roadmap-item` iter-1. Use it when you want a fast independent look
at a branch and don't need the parallel agent reviewer, roadmap/lessons
plumbing, or scope-of-fixes prompt.

## When to use

Use this skill when:

- A feature branch is implemented + tests pass, and you want a sanity
  check before opening a PR.
- You finished a refactor and want a fresh pair of eyes on subtle
  correctness, API design, or regression risk.
- A reviewer (human or agent) flagged something and you want a second,
  independent severity rating before deciding to act.

Don't use this skill when:

- The change is an atomic ROADMAP `M*` sub-item — use `ship-roadmap-item`
  instead (it runs codex + critic in parallel, applies findings, marks
  the roadmap + lesson, and merges the PR).
- You need codex to apply fixes — this skill is read-only by design.
- Codex is unavailable (`which codex` is empty) — fall back to
  `Agent({subagent_type: "oh-my-claudecode:code-reviewer"})`.

## Inputs

| Input | Source | Default |
| --- | --- | --- |
| Base branch | Operator (optional) | `main` |
| Extra focus areas | Operator (optional) | none |
| Title for codex summary | Auto-derived | current branch name |

## Procedure

### 1. Verify preconditions

```bash
which codex >/dev/null || { echo "codex CLI not installed"; exit 1; }
git rev-parse --abbrev-ref HEAD                 # current branch
git fetch origin main --quiet                   # ensure base is current
git diff --stat origin/main...HEAD | tail -1    # confirm there IS a diff
```

If the diff is empty or only contains the merge base, stop and tell the
operator — there is nothing to review.

### 2. Build the prompt

Keep the prompt **short and specific**. Codex performs better with a
1-paragraph context summary + an explicit list of findings to focus on
than with a vague "review this code" prompt.

Template:

```text
Review the changes on branch <BRANCH> vs <BASE>.

Context: <1-sentence project summary, e.g. "Go module under
core/ implementing a saga-driven spawn pipeline for keep workers">.
The branch <BRANCH> <1-sentence change summary>.

Findings I want from you:
- Subtle correctness issues (race conditions, nil-deref, shadowed errors)
- API design concerns (signature shape, breaking changes, leaky abstractions)
- Missing edge cases (empty input, ctx cancel, partial failure)
- Regression risk vs prior behavior
- Security / PII concerns
- Consistency with project patterns (cite a sibling file when relevant)

Be concrete: cite file:line. Severity-rate each finding
(Critical / Major / Minor / Nit). Don't restate what works.
```

Construct this prompt in the conversation before invoking codex — do not
hardcode generic phrases like "look for bugs". A weak prompt yields weak
findings.

### 3. Invoke codex review

Run in the background — codex review takes ~1–3 minutes typically and
blocking the turn wastes the operator's time:

```bash
codex review \
  --base main \
  --title "Branch <BRANCH> review" \
  "<PROMPT FROM STEP 2>" \
  > /tmp/codex-review-<BRANCH>.md 2>&1
```

Use `Bash(run_in_background=true)`. You will be notified when it
finishes — do not poll.

If the operator passes a non-default base (`--base feat/X`), substitute
it. If they ask for uncommitted-only, swap `--base main` for
`--uncommitted`.

### 4. Read + translate the output

Once codex finishes, read the artifact (`/tmp/codex-review-<BRANCH>.md`).
Codex's raw output mixes prose, code blocks, and severity ratings — your
job is to compress it into a flat list the operator can scan in 10
seconds.

Translation format:

```text
# Codex review of <BRANCH> (vs <BASE>)

<N> findings: <C> Critical, <M> Major, <m> Minor, <n> Nit

#1 | Major  | core/pkg/foo/bar.go:42  | <one-line summary>
     Fix: <one-line concrete suggested change>

#2 | Minor  | core/pkg/foo/baz.go:88  | <one-line summary>
     Fix: <one-line concrete suggested change>

...
```

Rules:

- Dedup findings that codex stated twice (it sometimes restates a
  Critical finding inside a "summary" section).
- Drop findings that are pure restatements of working code ("the test
  correctly covers X" — codex was told not to do this; if it does
  anyway, drop it).
- Preserve codex's severity rating verbatim; do not re-rate.
- If codex says "no issues found", report exactly that — do not invent
  findings to look productive.

### 5. Hand off to the operator

Return the flat list as your final message. Do NOT:

- Apply any of the fixes (read-only skill — operator decides scope).
- Ask "should I fix these?" — let the operator initiate the next move.
- Open a PR, push, or modify any file.

If the operator then asks to apply specific findings, that is a separate
task — at that point the regular implementation flow applies (edit, run
tests, commit).

### 6. Cleanup

```bash
rm /tmp/codex-review-<BRANCH>.md
```

The artifact in `/tmp/` is transient by definition (cleared on reboot);
removing it explicitly keeps the working tree clean and avoids
accidentally referencing a stale review later in the session.

## Common mistakes

| Mistake | Why it bites | Fix |
| --- | --- | --- |
| Generic prompt ("review this code") | Codex returns vague findings or restates working code | Use the step-2 template with project context + explicit focus list |
| Blocking on codex review | Wastes 1–3 min of operator wall-clock | Always `run_in_background=true`; resume on notification |
| Auto-applying findings | Operator loses the choice; some findings are wrong or out-of-scope | Hand off the list; let the operator pick scope |
| Polling `/tmp/codex-review-*.md` in a sleep loop | The Bash tool notifies on completion — polling burns context | Wait for the harness notification |
| Treating "no issues found" as a failure | Codex is allowed to find nothing; that is a valid result | Report verbatim, do not invent findings |
| Forgetting to fetch origin/main first | Stale base = bogus diff = bogus findings | Always `git fetch origin main` before invoking codex |
| Reviewing uncommitted work with `--base main` | Codex sees the full branch diff, not just the un-staged change | Use `--uncommitted` for working-tree-only review |

## Success criteria

- [x] `codex review` ran against the current branch vs the operator's
      chosen base (default `main`).
- [x] Output was translated into a severity-rated flat list with concrete
      file:line citations and one-line suggested fixes.
- [x] Operator received the list and decides next steps; no files were
      modified by this skill.
- [x] The `/tmp/codex-review-*.md` artifact was removed.
