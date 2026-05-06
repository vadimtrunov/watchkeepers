#!/usr/bin/env bash
# Stop hook safety net for rdd Hard rule 5. If the assistant is about to end
# the turn after dispatching an Agent without ever emitting a user-facing
# text block since that Agent return, block the stop and feed back the rule.
#
# Gated on an active rdd TASK file so this only fires during /rdd work.
set -euo pipefail

# Gate: only fire when rdd is active. Active = ANY of:
#   1. `.omc/state/rdd-active` marker file (Phase 0 preflight → Phase 7b cleanup).
#   2. An in-progress TASK file in the working tree.
#   3. Current branch matches `rdd/*`.
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

input="$(cat 2>/dev/null || true)"

if [[ ${active} -eq 0 ]]; then
  exit 0
fi
transcript="$(printf '%s' "${input}" | jq -r '.transcript_path // empty' 2>/dev/null || true)"
if [[ -z "${transcript}" || ! -f "${transcript}" ]]; then
  exit 0
fi

# Walk transcript JSONL: locate the most recent assistant block that is a
# tool_use named "Agent". If found, scan all subsequent assistant blocks for
# a text block with non-whitespace content. If none, signal a violation.
verdict="$(jq -rs '
  def is_text_block: .type == "text" and ((.text // "") | test("\\S"));
  def is_agent_call: .type == "tool_use" and .name == "Agent";

  # Build a flat list of (record_index, content_block) pairs across assistant records.
  [ to_entries[]
    | select(.value.type == "assistant")
    | .key as $i
    | (.value.message.content // [])[]
    | {i: $i, b: .}
  ] as $blocks
  | (last(($blocks | to_entries[] | select(.value.b | is_agent_call)) // empty) | .key) as $last_agent_idx
  | if $last_agent_idx == null then "ok"
    else
      ($blocks[($last_agent_idx + 1):]
        | map(select(.b | is_text_block))
        | length) as $text_after
      | if $text_after == 0 then "block" else "ok" end
    end
' "${transcript}" 2>/dev/null || echo ok)"

if [[ "${verdict}" == "block" ]]; then
  cat <<'EOF'
{
  "decision": "block",
  "reason": "rdd Hard rule 5 violation: this turn called the Agent tool but emitted no user-facing text block afterwards. Add the orchestrator verdict (≤2 lines: agent verdict + next step / phase / gate) NOW, before stopping. Silent-exit after Agent return is the #1 rdd failure mode (see FEEDBACK.md 2026-04-22, 2026-05-05)."
}
EOF
fi
