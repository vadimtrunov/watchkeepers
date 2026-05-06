#!/usr/bin/env bash
# PostToolUse hook for the Agent tool. Mechanically reinforces rdd Hard rule 5
# (FEEDBACK 2026-04-22, 2026-05-05): after every Agent return, the orchestrator
# must emit a verdict text block before any further tool call.
#
# Gated on an active rdd TASK file (TASK-*.md with Status: in-progress) so
# this does not inject reminders during non-rdd work in this repo.
set -euo pipefail

# Drain stdin even if unused (avoid SIGPIPE noise).
cat >/dev/null 2>&1 || true

# Gate: only fire when rdd is active. Active = ANY of:
#   1. `.omc/state/rdd-active` marker file exists (set by Phase 0 preflight,
#      removed by Phase 7b cleanup) — covers Phase 1 before TASK/branch exist.
#   2. An in-progress TASK file in the working tree (covers Phase 2+).
#   3. Current branch matches `rdd/*` (covers Phase 3+ even if TASK was deleted).
shopt -s nullglob
active=0
[[ -f .omc/state/rdd-active ]] && active=1
if [[ ${active} -eq 0 ]]; then
  for f in TASK-*.md; do
    if grep -qE '^\*\*Status\*\*:[[:space:]]*in-progress' "${f}" 2>/dev/null; then
      active=1
      break
    fi
  done
fi
if [[ ${active} -eq 0 ]]; then
  branch="$(git rev-parse --abbrev-ref HEAD 2>/dev/null || true)"
  [[ "${branch}" == rdd/* ]] && active=1
fi
[[ ${active} -eq 0 ]] && exit 0

cat <<'EOF'
{
  "hookSpecificOutput": {
    "hookEventName": "PostToolUse",
    "additionalContext": "rdd Hard rule 5 (mechanical reinforcement): an Agent just returned. The VERY NEXT text segment in this reply MUST be the orchestrator-authored verdict (≤2 lines: agent verdict + next step / phase / gate). Emit text BEFORE any further tool call (TaskUpdate / Edit / next Agent). Silent-exit after Agent return is the #1 rdd failure mode. See .claude/skills/rdd/SKILL.md §Hard rules #5 and FEEDBACK.md 2026-04-22 / 2026-05-05."
  }
}
EOF
