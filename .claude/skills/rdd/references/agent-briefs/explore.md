# Agent Brief — explore (Phase 2, optional)

The orchestrator optionally dispatches this agent during Phase 2 to gather
repository context for the brainstorm. **Read-only** — the agent must not
write any files or run any mutating commands.

## Preferred `subagent_type`

`oh-my-claudecode:explore` (fallback: `Explore`).

## Input (pass in the agent prompt)

- **TASK scope paragraph** (just the Scope section from the in-progress
  TASK file).
- **Explicit question(s)** the orchestrator wants answered, e.g.:
  - "Where are Go module boundaries currently defined?"
  - "Does any existing file already configure golangci-lint?"
  - "Which files in the repo import from `pgvector`?"
- **Thoroughness level**: `quick` (two or three searches) or `medium` (up
  to ~eight searches).

## Output (exact shape)

```
## Files

- `<path>` — <one-sentence role in the repo>
- ...

## Answer

<two-to-six sentences, directly answering each input question.>

## Uncertain / not found

<optional: bullet list of things that were searched for but not found;
include the search pattern>
```

## Hard rules

- No writes (no `Write`, `Edit`, `NotebookEdit`, no shell commands that
  modify the filesystem).
- No `git` state mutations.
- Keep the answer short. The brainstorm is waiting.
- If a question is out of scope of the repo (asks about external services),
  say so in "Uncertain / not found" rather than guessing.
