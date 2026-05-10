package coordinator

// post_daily_briefing — Coordinator write tool (M8.2.c).
//
// Resolution order:
//
//  1. ctx.Err() pre-check.
//  2. Channel-id arg → typed string + Slack-channel-id shape
//     pre-validation.
//  3. Title arg → typed string + length cap.
//  4. Sections arg → typed []briefingSection; each section's heading +
//     bullets pass length + count caps.
//  5. Render via [formatBriefing]; reject if the rendered rune-count
//     exceeds [maxBriefingChars].
//  6. Dispatch via [SlackMessenger.SendMessage] with the configured
//     channel id.
//  7. Project the returned MessageID into the success Output.
//
// Audit discipline: handler returns a [agentruntime.ToolResult] only.
// PII discipline: every refusal text uses the [postBriefingRefusalPrefix]
// + constant suffix; raw user-supplied arg values NEVER appear.

import (
	"context"
	"fmt"
	"regexp"

	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger"
	agentruntime "github.com/vadimtrunov/watchkeepers/core/pkg/runtime"
)

// PostDailyBriefingName is the manifest tool name the Coordinator
// dispatcher registers this handler under. Mirrors the toolset entry
// in `deploy/migrations/026_coordinator_manifest_v3_seed.sql`.
const PostDailyBriefingName = "post_daily_briefing"

// postBriefingRefusalPrefix is the leading namespace for every
// refusal text this handler surfaces.
const postBriefingRefusalPrefix = "coordinator: " + PostDailyBriefingName + ": "

// post_daily_briefing argument keys.
const (
	// ToolArgChannelID carries the Slack channel id (C…/G…/D…) the
	// briefing posts to. Required; validated against
	// [slackChannelIDPattern].
	ToolArgChannelID = "channel_id"

	// ToolArgBriefingTitle carries the bold preamble of the briefing.
	// Required; non-empty; ≤ [maxBriefingTitleChars].
	ToolArgBriefingTitle = "title"

	// ToolArgSections carries the structured sections array. Required;
	// non-empty; ≤ [maxBriefingSections] entries. Each section is an
	// object with `heading` (string, required, ≤ [maxBriefingHeadingChars])
	// and `bullets` (array of string, optional, ≤ [maxBriefingBullets]
	// entries, each ≤ [maxBriefingBulletChars]).
	ToolArgSections = "sections"
)

// Length + count caps.
const (
	// maxBriefingChars caps the rune-length of the rendered text
	// (post-escape, post-formatter). 8000 covers a substantial daily
	// briefing while staying under Slack's 40 000-char chat.postMessage
	// limit with headroom.
	maxBriefingChars = 8000

	// maxBriefingTitleChars caps the title rune-length. 200 matches the
	// [maxNudgeTitleChars] cap for consistency.
	maxBriefingTitleChars = 200

	// maxBriefingSections caps the number of sections. 20 covers a
	// fanned-out briefing while rejecting an unbounded array.
	maxBriefingSections = 20

	// maxBriefingHeadingChars caps each section heading.
	maxBriefingHeadingChars = 200

	// maxBriefingBullets caps the bullets per section.
	maxBriefingBullets = 20

	// maxBriefingBulletChars caps each bullet's rune-length.
	maxBriefingBulletChars = 1000
)

// slackChannelIDPattern is the conservative whitelist for Slack
// channel ids. Slack documents `C…` (public channel), `G…` (legacy
// private group / multi-party IM), `D…` (DM). The character set after
// the prefix is uppercase-alphanumeric. Iter-1-style discriminant
// (mandatory `C`/`G`/`D` prefix) rejects token-shaped strings that
// would otherwise satisfy a character-class-only check.
var slackChannelIDPattern = regexp.MustCompile(`^[CGD][A-Z0-9]{2,}$`)

// NewPostDailyBriefingHandler constructs the [agentruntime.ToolHandler]
// the Coordinator dispatcher registers under [PostDailyBriefingName].
// Panics on a nil `sender` per the M*.c.* nil-dep discipline.
//
// Args contract — see the const-block doc above.
//
// Refusal text NEVER echoes raw arg values.
//
// Output (success):
//
//   - `message_ts` (string): Slack-assigned message timestamp.
//   - `chars_sent` (int): rune count of the rendered text.
//   - `scope`      (object): `{section_count, title_present}`. The
//     channel id is INTENTIONALLY OMITTED (deployment-internal
//     identifier; the agent already has it in its call args).
func NewPostDailyBriefingHandler(sender SlackMessenger) agentruntime.ToolHandler {
	if sender == nil {
		panic("coordinator: NewPostDailyBriefingHandler: sender must not be nil")
	}
	return func(ctx context.Context, call agentruntime.ToolCall) (agentruntime.ToolResult, error) {
		if err := ctx.Err(); err != nil {
			return agentruntime.ToolResult{}, err
		}

		channelID, refusal := readBriefingChannelIDArg(call.Arguments)
		if refusal != "" {
			return agentruntime.ToolResult{Error: refusal}, nil
		}

		title, refusal := readBriefingTitleArg(call.Arguments)
		if refusal != "" {
			return agentruntime.ToolResult{Error: refusal}, nil
		}

		sections, refusal := readBriefingSectionsArg(call.Arguments)
		if refusal != "" {
			return agentruntime.ToolResult{Error: refusal}, nil
		}

		rendered, chars := formatBriefing(title, sections)
		if chars > maxBriefingChars {
			return agentruntime.ToolResult{
				Error: postBriefingRefusalPrefix +
					fmt.Sprintf("rendered briefing length %d exceeds cap %d (rune count)", chars, maxBriefingChars),
			}, nil
		}

		ts, err := sender.SendMessage(ctx, channelID, messenger.Message{Text: rendered})
		if err != nil {
			return agentruntime.ToolResult{}, fmt.Errorf("coordinator: post_daily_briefing: %w", err)
		}

		return agentruntime.ToolResult{
			Output: map[string]any{
				"message_ts": string(ts),
				"chars_sent": chars,
				"scope": map[string]any{
					"section_count": len(sections),
					"title_present": title != "",
				},
			},
		}, nil
	}
}

// readBriefingChannelIDArg projects the `channel_id` arg with shape
// pre-validation. Refusal text NEVER echoes the raw value.
func readBriefingChannelIDArg(args map[string]any) (string, string) {
	raw, present := args[ToolArgChannelID]
	if !present {
		return "", postBriefingRefusalPrefix + "missing required arg: " + ToolArgChannelID
	}
	str, ok := raw.(string)
	if !ok {
		return "", postBriefingRefusalPrefix + ToolArgChannelID + " must be a string"
	}
	if str == "" {
		return "", postBriefingRefusalPrefix + ToolArgChannelID + " must be non-empty"
	}
	if !slackChannelIDPattern.MatchString(str) {
		return "", postBriefingRefusalPrefix + ToolArgChannelID +
			" must match Slack channel-id shape [CGD][A-Z0-9]{2,}"
	}
	return str, ""
}

// readBriefingTitleArg projects the `title` arg. Required, non-empty,
// length-capped.
func readBriefingTitleArg(args map[string]any) (string, string) {
	raw, present := args[ToolArgBriefingTitle]
	if !present {
		return "", postBriefingRefusalPrefix + "missing required arg: " + ToolArgBriefingTitle
	}
	str, ok := raw.(string)
	if !ok {
		return "", postBriefingRefusalPrefix + ToolArgBriefingTitle + " must be a string"
	}
	if str == "" {
		return "", postBriefingRefusalPrefix + ToolArgBriefingTitle + " must be non-empty"
	}
	if runeLen(str) > maxBriefingTitleChars {
		return "", postBriefingRefusalPrefix + ToolArgBriefingTitle +
			fmt.Sprintf(" must be ≤ %d characters (rune count)", maxBriefingTitleChars)
	}
	return str, ""
}

// readBriefingSectionsArg projects the `sections` arg into a typed
// []briefingSection. JSON wire shape is `[]any` of `map[string]any`
// per the runtime's [agentruntime.ToolCall.Arguments] decoding. Per-
// section heading + bullets validated against the per-tool caps.
func readBriefingSectionsArg(args map[string]any) ([]briefingSection, string) {
	raw, present := args[ToolArgSections]
	if !present {
		return nil, postBriefingRefusalPrefix + "missing required arg: " + ToolArgSections
	}
	rawSlice, ok := raw.([]any)
	if !ok {
		return nil, postBriefingRefusalPrefix + ToolArgSections + " must be an array"
	}
	if len(rawSlice) == 0 {
		return nil, postBriefingRefusalPrefix + ToolArgSections + " must contain at least one entry"
	}
	if len(rawSlice) > maxBriefingSections {
		return nil, postBriefingRefusalPrefix + ToolArgSections +
			fmt.Sprintf(" must contain ≤ %d entries", maxBriefingSections)
	}
	out := make([]briefingSection, 0, len(rawSlice))
	for i, item := range rawSlice {
		secMap, ok := item.(map[string]any)
		if !ok {
			return nil, postBriefingRefusalPrefix + ToolArgSections +
				fmt.Sprintf(" entry %d must be an object", i)
		}
		sec, refusal := readSection(secMap, i)
		if refusal != "" {
			return nil, refusal
		}
		out = append(out, sec)
	}
	return out, ""
}

// readSection validates one section's heading + bullets. Helper split
// out of [readBriefingSectionsArg] to keep the array-scan loop tight.
func readSection(secMap map[string]any, idx int) (briefingSection, string) {
	var sec briefingSection

	if rawHeading, present := secMap["heading"]; present {
		h, ok := rawHeading.(string)
		if !ok {
			return briefingSection{}, postBriefingRefusalPrefix +
				fmt.Sprintf("section %d heading must be a string", idx)
		}
		if runeLen(h) > maxBriefingHeadingChars {
			return briefingSection{}, postBriefingRefusalPrefix +
				fmt.Sprintf("section %d heading must be ≤ %d characters", idx, maxBriefingHeadingChars)
		}
		sec.Heading = h
	}

	rawBullets, present := secMap["bullets"]
	if !present {
		return sec, ""
	}
	bulletsSlice, ok := rawBullets.([]any)
	if !ok {
		return briefingSection{}, postBriefingRefusalPrefix +
			fmt.Sprintf("section %d bullets must be an array", idx)
	}
	if len(bulletsSlice) > maxBriefingBullets {
		return briefingSection{}, postBriefingRefusalPrefix +
			fmt.Sprintf("section %d must contain ≤ %d bullets", idx, maxBriefingBullets)
	}
	for j, b := range bulletsSlice {
		s, ok := b.(string)
		if !ok {
			return briefingSection{}, postBriefingRefusalPrefix +
				fmt.Sprintf("section %d bullet %d must be a string", idx, j)
		}
		if s == "" {
			return briefingSection{}, postBriefingRefusalPrefix +
				fmt.Sprintf("section %d bullet %d must be non-empty", idx, j)
		}
		if runeLen(s) > maxBriefingBulletChars {
			return briefingSection{}, postBriefingRefusalPrefix +
				fmt.Sprintf("section %d bullet %d must be ≤ %d characters", idx, j, maxBriefingBulletChars)
		}
		sec.Bullets = append(sec.Bullets, s)
	}
	return sec, ""
}
