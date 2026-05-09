---
name: ship-roadmap-item
description: Close one atomic ROADMAP sub-item end-to-end (branch → impl → tests → parallel codex+agent review iter-1 → merge). Two reviewers run in parallel; the union of their findings must be applied on the same branch before squash-merging to main.
triggers:
  - "сделай таску M"
  - "do task M"
  - "close M"
  - "ship M"
  - "ralph M"
  - "/ship-roadmap-item"
---

# ship-roadmap-item — Roadmap-driven implementation with mandatory parallel review iter-1

Use this skill when the operator asks to close an atomic `M*` item from
`docs/ROADMAP-phase*.md`. It extends the project's existing `rdd` flow
with a hard requirement: **two reviewers (codex + a code-review agent)
run in parallel on the branch and the union of their findings must be
applied before the PR is merged**. The review iteration happens INSIDE
the same branch — not as a follow-up.

This skill encodes patterns settled across the M7.1.c.* family (CreateApp
→ OAuthInstall → BotProfile saga steps). It is opinionated on:

- saga-step shape (panic-on-nil deps, source-grep AC, PII canary harness)
- defensive copy of reference-typed config
- fail-fast validation before audit/state side effects
- per-call resolver over process-global static for tenant-scoped values

Skip the skill if the task is not an atomic roadmap sub-item, or if the
project is not the watchkeepers repo (the file paths and naming conventions
are project-specific).

## Inputs

| Input | Source |
| --- | --- |
| Roadmap id | User message (`M7.1.c.c`) OR next `[ ]` under the user-named branch |
| API design choice | One focused `AskUserQuestion` when prior art doesn't dictate the answer |
| Codex-fix scope | User answer ("all" / "only Major" / "docblock-only") after iter-1 returns findings |

## Phases

### 1. Discovery (read-only, parallel)

Read in **one** parallel batch:

- `docs/ROADMAP-phase*.md` — locate the target sub-item; check siblings + parent for context.
- `docs/lessons/<family>.md` (e.g. `docs/lessons/M7.md`) — read patterns from the **two most recent** prior-art entries in the same family. They will name reusable sentinels, AC patterns, and seam shapes.
- All Go files under the target package + the immediate seam neighbours + the production-wiring helper (`core/internal/keep/<feature>_wiring/`) + the prior step's `*_test.go`.

Stop when you can list:

- The closed-set name for the new step (e.g. `bot_profile`).
- The seam interface to add (e.g. `BotProfileSetter`).
- The downstream callers that need signature updates.
- The reusable sentinels (`ErrMissingSpawnContext`, `ErrCredsNotFound`, …).

### 2. Design

Pattern-match prior art for sensible defaults. Use **one** focused
`AskUserQuestion` only when an architectural fork affects blast radius
upstream (e.g. interface signature shape). Never ask process questions.

### 3. Branch + task list

```bash
git stash push -m "rdd-codex-pre-branch-stash" <unrelated-paths>  # if needed
git checkout -b rdd/<roadmap-id>-<slug>
```

Open a `TaskCreate` checklist of 4–7 concrete items: implement step,
write tests, extend downstream signatures, update callers/wiring,
build+vet+race, roadmap+lessons. Mark each `in_progress` → `completed`
as you go.

### 4. Implement

Saga-step shape — match the prior `M*.c.*` exactly:

- File: `core/pkg/<feature>/<step>_step.go`. Top-of-file doc-block listing resolution order, audit discipline, PII discipline.
- `<Step>StepName` const, `<Step>Setter` interface (or `*Installer` / `*RPC` per pattern), `<Step>StepDeps` struct, `<Step>Step` struct, `var _ saga.Step = (*<Step>Step)(nil)` compile assertion.
- `New<Step>Step` panics on nil deps with the message `"spawn: New<Step>Step: deps.<Field> must not be nil"`.
- `Execute(ctx)` resolves `saga.SpawnContextFromContext(ctx)`, validates `AgentID != uuid.Nil` (and any other per-call context fields), wraps every error with `fmt.Errorf("spawn: <step_name> step: %w", err)`.
- **Reuse sentinels** from prior steps (`ErrMissingSpawnContext`, `ErrMissingAgentID`, `ErrCredsNotFound`). Don't introduce per-step duplicates.
- **Defensive deep-copy of reference-typed config** (maps + byte slices) in the constructor AND on dispatch. Mirror `cloneBotProfile` pattern.

Test-file shape — match the prior `M*.c.*` exactly:

- Hand-rolled fakes (no mocking lib).
- Tests covering: name, constructor panics, happy path, missing SpawnContext, nil AgentID, downstream-error wrap, ctx-cancel, sentinel passthrough, **source-grep AC** (`os.ReadFile` step file → assert no `keeperslog.` and no `.Append(` outside comments), **PII redaction harness** (canary substrings × every failure path), **defensive-copy assertion** (post-construction caller mutation must not bleed), 16-goroutine concurrency.
- For gosec G101 false-positives on canary constants, annotate with
  `//nolint:gosec // G101: synthetic redaction-harness canaries, not real credentials.`

Downstream cascade:

- Extend the seam interface signature (e.g. `approval.SpawnKickoff`).
- Update the concrete impl + dispatcher + production-wiring helper.
- Update existing tests via a **helper** so call-site changes stay 1-line:
  `kickoffWithDefaults(ctx, k, sagaID, mvID, token)` wraps the new
  signature with sensible defaults.
- Add new tests pinning the new args (forwarding, nil-rejection, error
  surfacing).

Always-on patterns:

- **Fail-fast validation before audit/state side effects.** If a kickoff
  receives `uuid.Nil`, return `ErrInvalidKickoffArgs` BEFORE the audit
  Append + DAO Insert. No half-emitted audit chain.
- **Per-call resolver beats process-global static** for tenant-scoped
  values. Prefer `WithSpawnClaimResolver(func(ctx, mvID) (saga.SpawnClaim, error))`
  over `WithWatchmasterClaim(saga.SpawnClaim)`.

### 5. Verify

```bash
cd core && go build ./... && go vet ./... && go test -race ./... -count=1
```

Run race tests in the background; do not poll. All 30+ packages must
report `ok`.

### 6. Roadmap + lessons

```bash
# Mark the item done
sed -i '' 's/- \[ \] \*\*M<id>\*\*/- [x] **M<id>**/' docs/ROADMAP-phase1.md
```

Append a new entry to `docs/lessons/<family>.md` ABOVE the previous
entry (most-recent-first). Required sections: Context, Pattern (4–7
numbered concrete patterns with names), References. Each pattern names
the file + line range and the reusable lesson. Cross-link to the codex
iter-1 fixes when relevant.

### 7. **Parallel review iter-1 (mandatory before commit)**

Two reviewers run **in parallel** in the same turn. They are mutually
independent — no shared state, no sequential dependency — so they go
into a single message with two tool calls. Their findings overlap by
design; the agent often catches consistency / project-pattern drift the
external model misses, while codex catches subtle correctness +
API-design issues the agent misses (this exact split landed 2 Major
findings on M7.1.c.c that one reviewer alone would have missed).

**Reviewer A — codex via `omc ask`** (Bash, run_in_background=true):

```bash
omc ask codex "Review the M<id> changes on branch rdd/<id>-<slug>.
[1-paragraph diff summary]
[1-paragraph project context: Go module, prior-art reference]
Findings I want from you:
- Subtle correctness issues
- API design concerns
- Missing edge cases
- Regression risk
- Security / PII concerns
- Consistency with prior M<family> patterns
Branch builds + passes go test -race + go vet clean.
Be concrete: cite file:line. Severity-rate (Critical/Major/Minor/Nit).
Don't restate what works."
```

**Reviewer B — `oh-my-claudecode:critic` agent** (Agent tool, parallel):

```
Agent({
  description: "Independent review of M<id> branch",
  subagent_type: "oh-my-claudecode:critic",
  prompt: "<same scope summary as codex prompt> + repo paths to relevant
  files + 'Read docs/lessons/<family>.md before reviewing so you
  do not re-flag settled patterns. Severity-rate Critical/Major/Minor/Nit.
  Cite file:line. Be concrete; do not restate what works.'"
})
```

Notes on reviewer choice:

- `oh-my-claudecode:critic` (Opus, READ-ONLY) is the default — multi-
  perspective review designed for plans + code.
- Substitute `oh-my-claudecode:code-reviewer` for severity-rated code-
  only review when the change is purely implementation (no plan / API
  shape decisions).
- Substitute `oh-my-claudecode:security-reviewer` when the change
  touches secrets, auth, OWASP-relevant surface — run it as a **third**
  parallel reviewer alongside codex + critic, do not replace.

Both reviewers run with `run_in_background=true` (codex via Bash,
agent via Agent tool's background mode). Wait for both notifications
before merging findings. Read codex output from
`core/.omc/artifacts/ask/codex-*.md`; the agent returns its summary as
its tool result.

**Merge findings**:

- Dedup by file:line — agent and codex frequently flag the same line
  with different framing.
- Severity-rate per the **higher** of the two if reviewers disagree.
- Translate to the user as a flat list: `#N | Severity | file:line | one-line summary | suggested fix`.

Ask the user **one** question: scope of fixes (`all` / `only Major+` /
`docblock-only` / `none — defer to follow-up`). Apply the chosen scope.
Re-run verify (step 5). Update the lesson entry to reflect the iter-1
patterns (e.g. "defensive deep copy", "fail-fast precedes audit").

**Then delete the codex artifact** — it's a transient review log, not
a durable record (`rm core/.omc/artifacts/ask/codex-*.md` — markdownlint
will fail otherwise because `.omc/**` is ignored only at repo root).
The agent's tool result lives only in the conversation transcript and
needs no cleanup.

### 8. Commit + push + PR + watch + merge

```bash
git add <each-file-by-name>   # never `git add .` or `-A`
git commit -m "$(cat <<'EOF'
feat(<scope>): <subject> (M<id>)

[paragraph: what this PR adds]

- <bullet 1>
- <bullet 2>
- <bullet 3>

Tests: <count> new tests covering [...]. go test -race ./... clean
across <N> packages.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

Pre-commit hook (lefthook) runs gofumpt + golangci-lint + markdownlint
+ commitlint + prettier + license-scan. Common false-positive: gosec
G101 on synthetic test canaries → annotate with `//nolint:gosec`.

```bash
git push -u origin rdd/<id>-<slug>
gh pr create --title "feat(<scope>): <subject> (M<id>)" --body "$(cat <<'EOF'
## Summary

- <bullet 1>
- <bullet 2>
- <bullet 3>

## Test plan

- [x] go build ./... clean
- [x] go vet ./... clean
- [x] go test -race ./... clean across <N> packages
- [x] golangci-lint clean
- [x] M<id> marked [x] in docs/ROADMAP-phase1.md
- [x] Lesson entry appended to docs/lessons/<family>.md covering N patterns
- [x] codex iter-1 review applied (<S> findings: <breakdown>)

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
gh pr checks <pr-number> --watch    # blocking
gh pr merge <pr-number> --squash --delete-branch
```

Squash matches the project's `(#NNN)` commit-suffix style. The
`--delete-branch` flag removes the branch on origin AND auto-prunes
the local tracking branch on next `git fetch`.

### 9. Local cleanup

```bash
git checkout main
git pull --ff-only origin main
git branch -D rdd/<id>-<slug>      # may already be gone — fine
```

### 10. Mark item shipped (final)

After merge, finalize the lesson entry's metadata so a future reader
can tell at a glance that the item shipped, with what PR, and on what
commit. This is the **last** step — it's the durable "done" mark.

```bash
# Replace the placeholder lines in the lesson entry you appended in step 6:
#
#   **PR**: pending — to be opened in Phase 5b
#   **Merged**: pending — to be merged in Phase 7a
#
# with actual values:
#
#   **PR**: #<pr-number>
#   **Merged**: <merge-commit-sha> on <YYYY-MM-DD>
```

Commit the metadata update with a short follow-up commit on `main`:

```bash
git commit -am "docs(lessons): mark M<id> shipped (#<pr-number>)"
git push origin main
```

This commit is intentionally small and doc-only — it is not gated by
the parallel review iter-1 (the gated work shipped in step 8). The
`docs(lessons)` prefix matches the project's lesson-finalization
convention (see `git log --oneline -- docs/lessons/`).

Confirm to the operator with a one-liner:

```
✅ M<id> shipped — PR #<pr-number> merged as <sha>; roadmap [x], lesson entry finalized.
```

## Success criteria

- [x] PR merged to `main` via **squash** with `(#NNN)` suffix on commit subject.
- [x] All CI checks pass (Go CI, Docker CI, Keep Integration CI, Meta CI, Migrate CI, SQL CI, Security CI, TypeScript CI, CodeRabbit).
- [x] Parallel review iter-1 ran (codex via `omc ask codex` + `oh-my-claudecode:critic` agent in the same turn) and the union of their findings was either applied or explicitly deferred to a follow-up via user choice.
- [x] Roadmap entry marked `[x]`; lesson entry appended with concrete patterns naming files + line ranges.
- [x] Local working tree on `main`, fast-forwarded; feature branch deleted.
- [x] Lesson entry's `**PR**:` and `**Merged**:` placeholders replaced with actual values via a follow-up `docs(lessons): mark M<id> shipped` commit on `main`. The roadmap item is durably "done" only after this final mark.

## Constraints + pitfalls

- **Never claim completion before parallel review iter-1.** The original codex+critic run on M7.1.c.c surfaced 2 Major + 2 Minor findings; without iter-1 they would have shipped to main. The two reviewers MUST run in the same turn (one Bash-background + one Agent-background tool call in one message), not sequentially — sequential runs cost 2× wall-clock and make the agent re-do exploration the codex prompt already covered.
- **Never use `git add .` or `git add -A`.** Stash unrelated work first; stage by-name.
- **Don't commit codex artifacts.** Delete `.omc/artifacts/ask/codex-*.md` after extraction — markdownlint fails on tab-indented JSON-marshalled go-struct dumps inside the artifact.
- **Don't introduce per-step error sentinels** when M-family already has one. Reuse `ErrMissingSpawnContext`, `ErrCredsNotFound`, etc.
- **Don't pin a tenant-scoped value as a process-global static.** Prefer a per-call resolver func — codex iter-1 will catch this and you'll spend a round redoing the API.
- **Don't shallow-copy reference-typed step config.** Maps + byte slices + slices need defensive deep copy in both the constructor AND on dispatch — codex iter-1 will catch this.
- **Don't emit audit BEFORE validating per-call args.** A `uuid.Nil` kickoff with a half-emitted `manifest_approved_for_spawn` row is a PII / observability bug. Fail-fast precedes Append.

## Open questions for future iterations

- The skill currently encodes the watchkeepers Go module's saga-step
  pattern. If a non-Go family lands (TypeScript watchmaster tools), the
  source-grep AC and PII-canary harness need a TS-equivalent codification.
- Codex iter-1 sometimes returns findings that were already lessons in
  prior PRs. Future iteration: feed `docs/lessons/<family>.md` to codex
  in the prompt so it doesn't re-flag settled patterns.
