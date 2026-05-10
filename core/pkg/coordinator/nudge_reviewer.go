package coordinator

// nudge_reviewer — Coordinator write tool (M8.2.c).
//
// Resolution order:
//
//  1. ctx.Err() pre-check.
//  2. Reviewer user-id arg → typed string + Slack-user-id shape
//     pre-validation. Refusal text NEVER echoes the raw value.
//  3. Text arg → typed string + non-empty + length cap (mrkdwn-aware
//     rune count).
//  4. Optional title arg → typed string + length cap.
//  5. Compose the message text via [formatBriefing] (title + single
//     bullet for the body) so the Slack-mrkdwn escape discipline is
//     shared with [post_daily_briefing].
//  6. Dispatch via [SlackMessenger.SendMessage] with the reviewer
//     user-id as `channelID` — Slack auto-opens the DM (no
//     conversations.open round-trip needed for chat.postMessage).
//  7. Project the returned MessageID into the success Output.
//
// Audit discipline: handler returns a [agentruntime.ToolResult] only;
// the runtime's tool-result reflection layer (M5.6.b) is the audit
// boundary.
//
// PII discipline: refusal text NEVER echoes the reviewer user-id or
// the message text. The success Output does NOT echo the user-id (PII
// reach surface — see M8.2.b lesson #10). The message text length
// (rune count) is surfaced for the agent to self-audit.

import (
	"context"
	"fmt"

	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger"
	agentruntime "github.com/vadimtrunov/watchkeepers/core/pkg/runtime"
)

// NudgeReviewerName is the manifest tool name the Coordinator
// dispatcher registers this handler under. Mirrors the toolset entry
// in `deploy/migrations/026_coordinator_manifest_v3_seed.sql`.
const NudgeReviewerName = "nudge_reviewer"

// nudgeReviewerRefusalPrefix is the leading namespace for every
// [agentruntime.ToolResult.Error] string this handler surfaces. Per
// the M8.2.b lesson-#10 addendum each Coordinator handler carries a
// per-tool prefix so the package-scoped `refusalPrefix` from M8.2.a
// does NOT collide.
const nudgeReviewerRefusalPrefix = "coordinator: " + NudgeReviewerName + ": "

// nudge_reviewer argument keys.
const (
	// ToolArgReviewerUserID carries the Slack user id of the reviewer
	// (e.g. `"U12345678"`). Required; validated against
	// [slackUserIDPattern]. Slack auto-opens a DM when this id is
	// passed as `chat.postMessage.channel`.
	ToolArgReviewerUserID = "reviewer_user_id"

	// ToolArgText carries the body text of the nudge. Required;
	// non-empty; ≤ [maxNudgeTextChars] rune-count after Slack-mrkdwn
	// escaping. Plain text or Slack-mrkdwn syntax is accepted; the
	// handler does NOT re-template — the agent supplies the final
	// wording (the "templated" framing in the roadmap text refers to
	// the system-prompt template the agent uses, not handler-side
	// substitution).
	ToolArgText = "text"

	// ToolArgTitle is an optional title rendered as the bold preamble
	// of the DM. Empty / absent skips the title line. ≤
	// [maxNudgeTitleChars].
	ToolArgTitle = "title"
)

// Length caps. Slack's documented chat.postMessage `text` limit is
// 40 000 chars; the Coordinator's per-tool caps are lower to keep
// the agent's prompt-window cost predictable.
const (
	// maxNudgeTextChars caps the rune-length of the body text on a
	// single nudge. 2000 covers a paragraph + link + cite while
	// staying well under Slack's 40 000-char chat.postMessage limit.
	maxNudgeTextChars = 2000

	// maxNudgeTitleChars caps the rune-length of the optional title.
	// 200 covers "Reminder: PR #123 review pending" while rejecting
	// agent-paste-prompts that try to smuggle the body into the title.
	maxNudgeTitleChars = 200
)

// SlackMessenger is the single-method interface
// [NewNudgeReviewerHandler] / [NewPostDailyBriefingHandler] consume
// for the chat.postMessage write. Mirrors `slack.Client.SendMessage`'s
// signature exactly so production code passes a `*slack.Client`
// through verbatim; tests inject a hand-rolled fake without touching
// the HTTP client.
type SlackMessenger interface {
	SendMessage(ctx context.Context, channelID string, msg messenger.Message) (messenger.MessageID, error)
}

// NewNudgeReviewerHandler constructs the [agentruntime.ToolHandler]
// the Coordinator dispatcher registers under [NudgeReviewerName].
// Panics on a nil `sender` per the M*.c.* nil-dep discipline.
//
// Args contract:
//
//   - `reviewer_user_id` (string, required): Slack user id matching
//     [slackUserIDPattern].
//   - `text`             (string, required): non-empty; ≤ [maxNudgeTextChars].
//   - `title`            (string, optional): empty or ≤ [maxNudgeTitleChars].
//
// Refusal text NEVER echoes a raw arg value.
//
// Output (success):
//
//   - `message_ts` (string): Slack-assigned message timestamp.
//   - `chars_sent` (int): rune count of the rendered text (post-escape).
//   - `scope`      (object): `{title_present}` only. `reviewer_user_id`
//     is INTENTIONALLY OMITTED (PII discipline).
func NewNudgeReviewerHandler(sender SlackMessenger) agentruntime.ToolHandler {
	if sender == nil {
		panic("coordinator: NewNudgeReviewerHandler: sender must not be nil")
	}
	return func(ctx context.Context, call agentruntime.ToolCall) (agentruntime.ToolResult, error) {
		if err := ctx.Err(); err != nil {
			return agentruntime.ToolResult{}, err
		}

		reviewerID, refusal := readReviewerUserIDArg(call.Arguments)
		if refusal != "" {
			return agentruntime.ToolResult{Error: refusal}, nil
		}

		text, refusal := readNudgeTextArg(call.Arguments)
		if refusal != "" {
			return agentruntime.ToolResult{Error: refusal}, nil
		}

		title, refusal := readNudgeTitleArg(call.Arguments)
		if refusal != "" {
			return agentruntime.ToolResult{Error: refusal}, nil
		}

		rendered, chars := formatBriefing(title, []briefingSection{{Bullets: []string{text}}})

		ts, err := sender.SendMessage(ctx, reviewerID, messenger.Message{Text: rendered})
		if err != nil {
			return agentruntime.ToolResult{}, fmt.Errorf("coordinator: nudge_reviewer: %w", err)
		}

		return agentruntime.ToolResult{
			Output: map[string]any{
				"message_ts": string(ts),
				"chars_sent": chars,
				"scope": map[string]any{
					"title_present": title != "",
				},
			},
		}, nil
	}
}

// readReviewerUserIDArg projects the `reviewer_user_id` arg. Refusal
// text NEVER echoes the raw value.
func readReviewerUserIDArg(args map[string]any) (string, string) {
	raw, present := args[ToolArgReviewerUserID]
	if !present {
		return "", nudgeReviewerRefusalPrefix + "missing required arg: " + ToolArgReviewerUserID
	}
	str, ok := raw.(string)
	if !ok {
		return "", nudgeReviewerRefusalPrefix + ToolArgReviewerUserID + " must be a string"
	}
	if str == "" {
		return "", nudgeReviewerRefusalPrefix + ToolArgReviewerUserID + " must be non-empty"
	}
	if !slackUserIDPattern.MatchString(str) {
		return "", nudgeReviewerRefusalPrefix + ToolArgReviewerUserID +
			" must match Slack user-id shape [UWB][A-Z0-9]{2,}"
	}
	return str, ""
}

// readNudgeTextArg projects the `text` arg. Refusal text NEVER echoes
// the raw value (long body could carry PII / credentials).
func readNudgeTextArg(args map[string]any) (string, string) {
	raw, present := args[ToolArgText]
	if !present {
		return "", nudgeReviewerRefusalPrefix + "missing required arg: " + ToolArgText
	}
	str, ok := raw.(string)
	if !ok {
		return "", nudgeReviewerRefusalPrefix + ToolArgText + " must be a string"
	}
	if str == "" {
		return "", nudgeReviewerRefusalPrefix + ToolArgText + " must be non-empty"
	}
	if runeLen(str) > maxNudgeTextChars {
		return "", nudgeReviewerRefusalPrefix + ToolArgText +
			fmt.Sprintf(" must be ≤ %d characters (rune count)", maxNudgeTextChars)
	}
	return str, ""
}

// readNudgeTitleArg projects the optional `title` arg. Empty / absent
// returns ("", "") — the title is optional and the handler renders no
// preamble when it is empty.
func readNudgeTitleArg(args map[string]any) (string, string) {
	raw, present := args[ToolArgTitle]
	if !present {
		return "", ""
	}
	str, ok := raw.(string)
	if !ok {
		return "", nudgeReviewerRefusalPrefix + ToolArgTitle + " must be a string when present"
	}
	if runeLen(str) > maxNudgeTitleChars {
		return "", nudgeReviewerRefusalPrefix + ToolArgTitle +
			fmt.Sprintf(" must be ≤ %d characters (rune count)", maxNudgeTitleChars)
	}
	return str, ""
}
