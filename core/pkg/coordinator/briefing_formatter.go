package coordinator

// briefing_formatter — shared Slack-mrkdwn rendering helper used by
// both `post_daily_briefing` (structured sections) and `nudge_reviewer`
// (templated DM). Centralises the escape discipline and length
// accounting so a future Slack-mrkdwn quirk is a one-file fix.
//
// Why not just use Slack `blocks`? blocks ride on a separate request
// shape and require the M4.2 SendMessage path to grow a `blocks`
// field. M8.2.c keeps the surface narrow by using plain `text` with
// mrkdwn formatting; a follow-up sub-item can upgrade to blocks once
// the metadata bag exposes a block-builder seam.

import (
	"strings"
)

// briefingSection is one heading + bullet group in a daily briefing.
// `Heading` is rendered as bold (`*…*`); `Bullets` render as a
// `•`-prefixed list, one per line. Empty heading is allowed for an
// intro paragraph (no bold prefix). Empty bullets section is skipped
// (no trailing "  •  " line).
type briefingSection struct {
	Heading string
	Bullets []string
}

// formatBriefing renders `title` + `sections` into Slack-mrkdwn text.
// Returns the rendered string plus its rune count so the caller can
// gate against [maxBriefingChars] without re-walking the string. The
// caller is responsible for caps + arg-shape validation; the formatter
// trusts its inputs.
//
// Output shape:
//
//	*Title*
//
//	*Heading 1*
//	• bullet 1
//	• bullet 2
//
//	*Heading 2*
//	• bullet 3
//
// Slack-mrkdwn escape discipline: any `*`, `_`, `~`, “ ` “, or `>`
// embedded in user text would otherwise close the surrounding mrkdwn
// run. The formatter escapes them by wrapping the rune in a
// zero-width-space sandwich (Slack's documented escape — bare
// backslash does NOT work in mrkdwn). For length accounting, the
// zero-width-space contributes one rune per occurrence; callers
// must size their caps with this in mind.
func formatBriefing(title string, sections []briefingSection) (string, int) {
	var b strings.Builder
	if title != "" {
		b.WriteString("*")
		b.WriteString(escapeMrkdwn(title))
		b.WriteString("*")
	}
	for _, sec := range sections {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		if sec.Heading != "" {
			b.WriteString("*")
			b.WriteString(escapeMrkdwn(sec.Heading))
			b.WriteString("*\n")
		}
		for i, bullet := range sec.Bullets {
			if i > 0 {
				b.WriteString("\n")
			}
			b.WriteString("• ")
			b.WriteString(escapeMrkdwn(bullet))
		}
	}
	out := b.String()
	return out, runeLen(out)
}

// escapeMrkdwn wraps Slack-mrkdwn control characters in a
// zero-width-space sandwich so a user-supplied `*great*` reads as
// `*great*` (with the visible asterisks) instead of closing the
// surrounding bold run. The zero-width-space (U+200B) is the
// documented Slack-side escape for mrkdwn — bare backslash does not
// work because Slack does not parse the bare backslash in mrkdwn text.
//
// Control characters wrapped: `*`, `_`, `~`, “ ` “, `>`, `<`. The
// `<` is critical (iter-1 critic Minor #1): Slack parses `<…>` as
// link / mention syntax, so an un-escaped `<@U12345>` in a bullet
// would render as a real @-mention and ping that user. The agent's
// system prompt forbids it, but the formatter MUST not silently
// honour the syntax when it does slip through.
//
// NOT IDEMPOTENT (iter-1 codex Minor #1): running this function twice
// on the same input grows the output. `escapeMrkdwn("*")` produces
// `<ZWS>*<ZWS>`; a second pass produces `<ZWS><ZWS>*<ZWS><ZWS>`. The
// extra ZWS runes collapse to no-ops visually in Slack's renderer
// (they are non-printing), so the rendered output is stable across
// passes — but the structural string and its rune-count are NOT. The
// production call path invokes this function exactly once per
// rendered token via [formatBriefing]; double-escape is not a
// production concern but the docblock must not lie about it. Callers
// SHOULD treat the function as a one-pass projection and rely on
// callers (not the formatter) to avoid pre-escaped input.
func escapeMrkdwn(s string) string {
	if !strings.ContainsAny(s, "*_~`><") {
		return s
	}
	const zws = "\u200b"
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, r := range s {
		switch r {
		case '*', '_', '~', '`', '>', '<':
			b.WriteString(zws)
			b.WriteRune(r)
			b.WriteString(zws)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// runeLen returns the rune count of s. Slack's documented message
// length limits are character-based, not byte-based; byte length would
// undercount multi-byte UTF-8 characters (Cyrillic, emoji, CJK) and
// over-permit short-byte strings.
func runeLen(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}
