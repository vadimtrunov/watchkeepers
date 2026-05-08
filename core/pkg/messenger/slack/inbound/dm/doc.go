// Package dm implements the M6.3.c [inbound.EventDispatcher] that
// turns admin Slack DMs into Watchmaster tool invocations. The
// resolution order on [Dispatcher.DispatchEvent]:
//
//  1. Decode the inner Slack `message` event from
//     [inbound.Event.Inner]. Non-message types are ACKed silently
//     (no audit row).
//
//  2. Filter: only `channel_type == "im"` is in scope; channel posts,
//     app_mentions, and threads are ACKed silently.
//
//  3. Filter: only human user messages (no `subtype` set, non-empty
//     `user`, no `bot_id`); empty `text` short-circuits silently.
//
//  4. Emit `slack_dm_received` audit row (no body content).
//
//  5. Parse the text via the injected [intent.Parser].
//
//  6. Branch:
//
//     - `list_watchkeepers` / `report_cost` / `report_health` →
//     call [ReadToolDispatcher]; render the result; post outbound
//     DM; emit `slack_dm_dispatched_read_only` (or `slack_dm_failed`
//     on tool error).
//     - `propose_spawn` → call [ProposeSpawnInvoker]; render the
//     approval card via [cards.RenderProposeSpawn]; insert a row
//     via [spawn.PendingApprovalDAO]; post outbound DM; emit
//     `slack_dm_dispatched_manifest_bump` (or `slack_dm_failed` on
//     any leg).
//     - `unknown` → post the help DM; emit `slack_dm_unknown_intent`.
//
// PII discipline: NO audit row carries the raw message text, the
// rendered DM body, or the tool-side request params. Only closed-set
// vocabulary (intent name, channel id, error class) lands on the
// keepers_log payload.
//
// Test seams:
//
//   - [intent.Parser]      — closed-set classifier (M6.3.c step 1)
//   - [ReadToolDispatcher] — read-only tool fan-out (analogous to
//     M6.3.b's `Replayer` seam)
//   - [ProposeSpawnInvoker] — manifest-bump tool fan-out
//   - [Outbound]           — outbound DM poster (composes
//     `*slack.Client.SendMessage` in production)
//   - [spawn.PendingApprovalDAO] — same DAO M6.3.b owns
//   - [AuditAppender]      — same shape as the M6.3.a/b dispatchers
package dm
