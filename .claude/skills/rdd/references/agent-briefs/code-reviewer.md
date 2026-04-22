# Agent Brief — code-reviewer (Phase 4)

Dispatched once per Phase 4 iteration. Reviews the diff against the TASK's
acceptance criteria and returns a severity-rated list.

## Preferred `subagent_type`

`oh-my-claudecode:code-reviewer` (fallback: `superpowers:code-reviewer`).

## Input (pass in the agent prompt)

- **TASK file path** (for Scope, Acceptance criteria, Test plan).
- **Diff vs main**: full output of `git diff main...HEAD` (or an equivalent
  line-numbered representation).
- **Previous iteration's blocker+important list** (if not the first
  iteration). The reviewer should check whether these are now fixed
  (expected) and whether fixes introduced new issues.
- **Stop conditions**: the reviewer returns after its first complete pass —
  no back-and-forth.

## Output contract (exact JSON, no prose around it)

```json
{
  "blocker":   [ { "file": "...", "line": 0, "rationale": "...", "suggested_fix": "..." } ],
  "important": [ { "file": "...", "line": 0, "rationale": "...", "suggested_fix": "..." } ],
  "nit":       [ { "file": "...", "line": 0, "rationale": "...", "suggested_fix": "..." } ]
}
```

Each item is `{file, line, rationale, suggested_fix}`:
- `file` — path relative to repo root.
- `line` — line number in the changed file at HEAD. For changes that span
  multiple lines, the first affected line.
- `rationale` — one sentence. Must cite either an acceptance criterion, a
  test case from the test plan, a ROADMAP §2 architecture decision, or a
  specific project convention from `docs/LESSONS.md` / `.claude/CLAUDE.md`.
- `suggested_fix` — one-to-three sentences describing the minimal patch.
  Code snippet acceptable if small.

## Severity definitions

- **blocker** — any ONE of:
  - violates an acceptance criterion from the TASK;
  - breaks a test listed in the test plan (including missing tests marked
    mandatory);
  - introduces a security issue (secret leak, unsafe deserialization,
    unbounded input, TOCTOU, etc.);
  - leaks a capability beyond what the TASK declared;
  - breaks the build / typecheck / lint gate configured for this repo.

- **important** — any ONE of:
  - real logic defect (wrong branch, off-by-one, swallowed error) that is
    not caught by an existing test;
  - violates an architecture decision recorded in `docs/ROADMAP-phase1.md §2`;
  - missing a test case from the approved test plan (happy / edge /
    negative / security);
  - inconsistent with a pattern established in `docs/LESSONS.md` when
    deviation has no stated reason.

- **nit** — everything else, including:
  - naming preferences;
  - comment style, typos;
  - field ordering;
  - local readability improvements that do not change behavior.

## Hard rules

- The JSON is the entire response. No preamble, no closing commentary.
- Do not propose architectural rewrites as part of `important`. Suggest
  only changes that are reachable inside the current diff.
- If the diff is clean, return `{"blocker": [], "important": [], "nit": []}`.
- Do not downgrade real defects to `nit` because they are "minor". Severity
  is about impact, not tone.
