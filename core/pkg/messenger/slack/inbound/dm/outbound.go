package dm

import (
	"context"
	"fmt"

	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger"
	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger/slack/cards"
)

// SlackSender is the minimal subset of the production
// `*slack.Client.SendMessage` surface the [SlackOutbound] adapter
// consumes. Defined locally so unit tests substitute a hand-rolled
// fake without standing up an HTTP adapter, and so the adapter does
// not import the concrete `*slack.Client` type at all (mirrors the
// keepclient / keeperslog interface-seam pattern documented in
// `docs/LESSONS.md`).
//
// `*slack.Client` satisfies this interface as-is; production wiring
// passes a `*slack.Client` value through.
type SlackSender interface {
	SendMessage(ctx context.Context, channelID string, msg messenger.Message) (messenger.MessageID, error)
}

// SlackOutbound adapts a [SlackSender] to the [Outbound] seam the
// [Dispatcher] consults. The adapter is the M6.3.c production wiring
// for the outbound DM path; tests in this package use a hand-rolled
// fake instead so the dispatcher's behaviour is observable without an
// HTTP round-trip.
//
// LIMITATION: the portable [messenger.Message] surface (M4.2.b) does
// NOT yet carry a `blocks` field; the slack `chat.postMessage`
// adapter's metadata-bag knobs cover only a closed set of scalars
// (mrkdwn, parse, icon_emoji, …). The adapter therefore posts the
// `fallbackText` body only; the [cards.Block] payload is consulted
// only via [SlackOutbound.HasBlocks] for caller diagnostics. A
// follow-up will extend [messenger.Message] with a portable Blocks
// slice; the [Outbound] interface stays stable across the change.
//
// The `blocks` argument is preserved on the wire fallback path:
// every renderer (M6.3.b cards) ALSO produces a plain-text fallback,
// so the admin sees a usable DM even with text-only delivery. The
// approval card's button affordance is the only feature degraded by
// the limitation, and the M6.3.b dispatcher's `block_actions`
// inbound path is exercised by integration once block posting lands.
type SlackOutbound struct {
	sender SlackSender
}

// NewSlackOutbound constructs a [SlackOutbound] that posts via the
// supplied [SlackSender]. Panics on a nil sender — production wiring
// MUST pass a `*slack.Client`.
func NewSlackOutbound(sender SlackSender) *SlackOutbound {
	if sender == nil {
		panic("dm: NewSlackOutbound: SlackSender must not be nil")
	}
	return &SlackOutbound{sender: sender}
}

// Compile-time assertion: [*SlackOutbound] satisfies [Outbound].
var _ Outbound = (*SlackOutbound)(nil)

// Post satisfies [Outbound]. Sends the supplied `fallbackText` body
// through the underlying [SlackSender] (see the package-level
// LIMITATION docblock for the reason `blocks` is not yet posted).
func (s *SlackOutbound) Post(
	ctx context.Context,
	channelID string,
	_ []cards.Block,
	fallbackText string,
) error {
	msg := messenger.Message{Text: fallbackText}
	if _, err := s.sender.SendMessage(ctx, channelID, msg); err != nil {
		return fmt.Errorf("dm: slack send_message: %w", err)
	}
	return nil
}
