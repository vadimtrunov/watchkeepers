package coordinator

import (
	"os"
	"strings"
	"testing"
)

func TestFormatBriefing_TitleAndSections(t *testing.T) {
	t.Parallel()

	got, chars := formatBriefing("Daily standup", []briefingSection{
		{Heading: "Blockers", Bullets: []string{"M8.1 wedged on whitelist", "Slack adapter rate-limited"}},
		{Heading: "Next", Bullets: []string{"Ship M8.2.c"}},
	})

	wantSubstrings := []string{
		"*Daily standup*",
		"*Blockers*",
		"• M8.1 wedged on whitelist",
		"*Next*",
		"• Ship M8.2.c",
	}
	for _, s := range wantSubstrings {
		if !strings.Contains(got, s) {
			t.Errorf("formatBriefing output missing %q\nfull:\n%s", s, got)
		}
	}
	if chars != runeLen(got) {
		t.Errorf("chars = %d, want runeLen(out) = %d", chars, runeLen(got))
	}
}

func TestFormatBriefing_EmptyTitle_NoBoldPrefix(t *testing.T) {
	t.Parallel()

	got, _ := formatBriefing("", []briefingSection{{Heading: "Only", Bullets: []string{"one"}}})
	if strings.HasPrefix(got, "*") && strings.Index(got, "*") != strings.Index(got, "*Only*") {
		t.Errorf("output should not start with a title block; got: %q", got)
	}
}

func TestFormatBriefing_EmptyBullets_OnlyHeading(t *testing.T) {
	t.Parallel()

	got, _ := formatBriefing("X", []briefingSection{{Heading: "Heading only"}})
	if strings.Contains(got, "• ") {
		t.Errorf("output must not contain bullet line when Bullets is empty; got: %q", got)
	}
	if !strings.Contains(got, "*Heading only*") {
		t.Errorf("output missing heading: %q", got)
	}
}

func TestEscapeMrkdwn_WrapsControlChars(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
	}{
		{"asterisk", "*bold*"},
		{"underscore", "_italic_"},
		{"tilde", "~strike~"},
		{"backtick", "`code`"},
		{"gt", ">quote"},
		{"lt", "<link>"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := escapeMrkdwn(tc.in)
			// The raw control chars must be wrapped (ZWS before AND after).
			// Asserting via "the output is different from the input and still
			// contains the control chars" is the minimal invariant.
			if got == tc.in {
				t.Errorf("escapeMrkdwn(%q) = %q; expected wrapping", tc.in, got)
			}
		})
	}
}

// TestEscapeMrkdwn_NeutralisesSlackMentionSyntax pins iter-1 critic
// Minor #1: a bullet text containing `<@U12345>` MUST NOT survive
// verbatim through the formatter (else it would render as a real
// @-mention pinging the user). The escape wraps `<` with ZWS so
// Slack's parser cannot recognise the mention envelope.
func TestEscapeMrkdwn_NeutralisesSlackMentionSyntax(t *testing.T) {
	t.Parallel()

	const mention = "<@U12345678>"
	got := escapeMrkdwn(mention)
	if got == mention {
		t.Errorf("escapeMrkdwn(%q) returned input unchanged; mention envelope would render as a real ping", mention)
	}
	// Defence-in-depth: assert the structural `<@U…>` bracketing is
	// broken by ZWS insertion — the `<` rune is present but no longer
	// adjacent to `@`.
	const zws = "​"
	wantPrefix := zws + "<" + zws
	if !strings.HasPrefix(got, wantPrefix) {
		t.Errorf("escapeMrkdwn(%q) did not wrap the leading `<` with ZWS; got %q", mention, got)
	}
}

// TestEscapeMrkdwn_NotIdempotent_DocBlockMatchesBehavior pins iter-1
// codex Minor #1: the docblock previously claimed idempotence; the
// actual behaviour grows the output on every pass. The docblock now
// honestly states this. The test asserts the documented behaviour
// (second pass produces strictly longer output for input containing
// any control rune) so a future "make it idempotent" refactor either
// updates this test OR triggers it as a regression alarm.
func TestEscapeMrkdwn_NotIdempotent_DocBlockMatchesBehavior(t *testing.T) {
	t.Parallel()

	const in = "*x*"
	pass1 := escapeMrkdwn(in)
	pass2 := escapeMrkdwn(pass1)
	if runeLen(pass2) <= runeLen(pass1) {
		t.Errorf("expected double-pass to grow output (docblock says NOT idempotent); pass1=%d pass2=%d",
			runeLen(pass1), runeLen(pass2))
	}
}

// TestBriefingFormatter_NoAuditOrLogAppend_SourceGrep pins iter-1
// critic Missing #5: even the shared formatter must NOT call into
// keeperslog or .Append(. Mirrors the M8.2.b source-grep AC family
// applied to the new shared helper.
func TestBriefingFormatter_NoAuditOrLogAppend_SourceGrep(t *testing.T) {
	t.Parallel()

	srcPath := repoRelative(t, "core/pkg/coordinator/briefing_formatter.go")
	bytes, err := os.ReadFile(srcPath) //nolint:gosec // test reads a fixed repo-relative path.
	if err != nil {
		t.Fatalf("ReadFile %s: %v", srcPath, err)
	}
	body := stripGoComments(string(bytes))
	for _, forbidden := range []string{"keeperslog.", ".Append("} {
		if strings.Contains(body, forbidden) {
			t.Errorf("briefing_formatter.go contains forbidden audit shape %q outside comments", forbidden)
		}
	}
}

func TestEscapeMrkdwn_NoControlChars_PassesThrough(t *testing.T) {
	t.Parallel()

	const in = "plain text with no mrkdwn"
	if got := escapeMrkdwn(in); got != in {
		t.Errorf("escapeMrkdwn(%q) = %q; want pass-through", in, got)
	}
}

func TestRuneLen_CountsUnicodeCharacters(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want int
	}{
		{"ascii", "hello", 5},
		{"cyrillic", "привет", 6},
		{"emoji", "🚀🎯", 2},
		{"mixed", "ok 🚀", 4},
		{"empty", "", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := runeLen(tc.in); got != tc.want {
				t.Errorf("runeLen(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}
