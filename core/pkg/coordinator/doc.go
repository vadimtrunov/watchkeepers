// Package coordinator implements the runtime.ToolHandler-shaped
// implementations of the Coordinator manifest's toolset. The package
// owns ONE concrete handler per manifest tool name; the M8.2.a sub-
// item ships [NewUpdateTicketFieldHandler] (the first Jira-write
// tool consuming the M8.1 jira.Client whitelist), and M8.2.b/c/d
// extend the package with `find_overdue_tickets`,
// `fetch_watch_orders` / `nudge_reviewer` / `post_daily_briefing`,
// and `find_stale_prs` respectively.
//
// Wiring discipline: the package exposes only handler factories. The
// caller (the M8.3 boot path or a test) constructs a
// [runtime.ToolDispatcher] and calls Register on each handler. The
// package does NOT own the dispatcher, the manifest loader, or the
// runtime — it is a leaf consumer of all three.
//
// Authority discipline: every handler MUST refuse out-of-authority
// arguments BEFORE it reaches the underlying adapter call.
// `update_ticket_field` refuses any args containing the `assignee`
// key (see [AssigneeFieldKey]) — dual-defense alongside the M8.1
// jira whitelist that excludes `assignee` BEFORE the network call.
// The Coordinator's authority matrix maps the bare tool name to
// `"self"` so the runtime gate dispatches the call; per-arg
// authority discrimination happens INSIDE the handler.
//
// PII discipline: handlers MUST NEVER log API tokens, OAuth bot
// tokens, Slack workspace credentials, or Jira basic-auth values.
// Adapter clients (jira, slack) own their own redaction discipline;
// handlers contribute by passing only manifest-declared call
// arguments through to the adapter and by NOT echoing call.Arguments
// values in error returns.
package coordinator
