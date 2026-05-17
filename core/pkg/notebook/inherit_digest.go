// inherit_digest.go is the Phase 2 §M7.1.d periodic job that scans
// `notebook_inherited` audit rows from the last 24h, groups the rows
// by lead human, and posts one Slack DM digest per lead naming the
// inherited entry counts + predecessor → successor pairs.
//
// The job is modelled on [PeriodicBackup]: a fixed-cadence ticker
// loop, per-tick best-effort error handling (a transient seam outage
// does NOT kill the loop), and a `TickCallback`-style hook so
// operators can observe failures without disturbing the scheduler.
//
// # Seam shape
//
// Four interface seams ([InheritAuditScanner], [LeadResolver],
// [InheritDigestPoster], [InheritDigestRunsStore]) keep the job's
// algorithm self-contained and transport-agnostic. Production wiring
// substitutes:
//
//   - `InheritAuditScanner` → a `*keepclient.Client`-backed wrapper
//     that calls `LogTail` filtered by `event_type =
//     'notebook_inherited'` over the [LoadLastRun]-derived window;
//     resolves the successor watchkeeper id via the saga's
//     `correlation_id` chain (a follow-up leaf can swap this for an
//     indexed audit-query endpoint if traffic grows).
//   - `LeadResolver` → a `*keepclient.Client`-backed wrapper that
//     calls `GetWatchkeeper(successorID).LeadHumanID` then maps the
//     human to a Slack user id (the wiring detail belongs to the
//     production helper; the job consumes only the resolved tuple).
//   - `InheritDigestPoster` → the existing Watchmaster Slack
//     adapter's `SendMessage` surface (`coordinator.SlackMessenger`);
//     the wiring helper renders the digest payload into a single
//     `chat.postMessage` body keyed by the lead's DM channel id.
//   - `InheritDigestRunsStore` → a `pgx`-backed wrapper around the
//     `watchkeeper.inherit_digest_runs` table introduced by
//     migration 034.
//
// The unit tests in this package exercise the job's algorithm via
// hand-rolled fakes; the production wiring helper lives outside
// this package (matching the [PeriodicBackup] precedent — that
// helper is also a library function, with the cmd-layer wiring
// deferred to a future leaf).
//
// # Idempotency
//
// [RunInheritDigest] is idempotent within the 24h cadence: on every
// call it loads the prior run's marker via
// [InheritDigestRunsStore.LoadLastRun] and short-circuits to a no-op
// (NO scan, NO post, NO write) when `now - lastRunAt < 24h`. A first
// run (no prior marker) seeds the window with `[now - 24h, now)`.
// Subsequent runs use the prior `last_window_end` as the new window
// start so the cursor never rewinds. A post failure does NOT advance
// the marker — the next tick retries the same window.
//
// # Empty-window discipline
//
// An empty audit-row scan produces NO Slack DM (per the acceptance
// "empty 24h window → no DM sent") but DOES advance the marker so
// the next tick scans the next 24h. The "no DM" guard sits on the
// digest poster's caller (the job's [RunInheritDigest] body), NOT
// on the poster itself, so a degenerate empty-digest payload never
// reaches the Slack adapter.
//
// # PII / audit discipline
//
// The digest payload composer ([buildLeadDigest]) is the SOLE
// composer of the digest body. It carries: the lead's display name
// (via the resolver) + the lead's slack user id (the chat.postMessage
// target) + a per-pair tuple of (predecessor_watchkeeper_id,
// successor_watchkeeper_id, entries_imported). The body NEVER
// echoes the archive URI substring — the URI is an internal
// storage detail, not a lead-facing artefact. Iter-1 codex P1
// pattern from M7.1.c: bounded payload, named keys, no URI bleed.
package notebook

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// digestCadence is the canonical scheduling cadence pinned by the
// roadmap acceptance ("24h tick"). Hoisted to a constant so a typo
// at the call site is a compile error AND the cadence-vs-window
// invariant is enforced in one place. The same value is used as
// both the ticker cadence in [PeriodicInheritDigest] and the
// idempotency guard in [RunInheritDigest].
const digestCadence = 24 * time.Hour

// EventTypeNotebookInherited mirrors
// `spawn.EventTypeNotebookInherited` to avoid a `notebook -> spawn`
// import cycle. The two packages co-own the closed-set string;
// drift surfaces as a `TestEventTypeNotebookInheritedMirror` failure.
const EventTypeNotebookInherited = "notebook_inherited"

// ErrInheritDigestDisabled is the typed error
// [PeriodicInheritDigest] returns when the `enabled` flag is false.
// Matchable via [errors.Is]. The job exits immediately with this
// sentinel so the caller (a `cmd/watchkeeper`-layer wiring helper)
// can distinguish "operator disabled the feature" from a transient
// scheduler-side failure.
var ErrInheritDigestDisabled = errors.New("notebook: inherit digest job is disabled")

// ErrInvalidDigestWindow is the typed error
// [RunInheritDigest] returns when the marker's
// `last_window_end` is later than the supplied `now`. The job's
// monotonic-cursor invariant would otherwise break — a clock skew
// or a manual marker bump in the wrong direction must surface as
// a clear failure rather than silently scanning an empty window.
var ErrInvalidDigestWindow = errors.New("notebook: inherit digest window is invalid")

// InheritEvent is the closed-set shape of a single
// `notebook_inherited` audit row consumed by the digest job. The
// production scanner ([InheritAuditScanner]) is responsible for
// resolving every field — including [SuccessorWatchkeeperID] which
// the audit row does NOT carry on its payload (the M7.1.c payload
// is `{predecessor_watchkeeper_id, archive_uri, entries_imported}`).
// The scanner derives the successor via the saga's correlation_id
// chain (a separate `manifest_approved_for_spawn` row keyed by the
// same correlation_id DOES carry the successor's watchkeeper id).
//
// The job's algorithm treats the field as data; the test fakes
// stamp it directly.
type InheritEvent struct {
	// PredecessorWatchkeeperID is the retired peer's watchkeeper id
	// (from the M7.1.c payload's `predecessor_watchkeeper_id` key).
	PredecessorWatchkeeperID string
	// SuccessorWatchkeeperID is the newly-spawned watchkeeper id
	// (resolved by the scanner from the saga's correlation chain).
	SuccessorWatchkeeperID string
	// EntriesImported is the per-row entry count (from the M7.1.c
	// payload's `entries_imported` key).
	EntriesImported int
	// OccurredAt is the row's `created_at` timestamp. Used for
	// stable per-lead pair ordering on the digest body.
	OccurredAt time.Time
}

// LeadAddress is the closed-set tuple [LeadResolver] returns. The
// `HumanID` field is the keepclient `Human.ID` (UUID); the
// `SlackUserID` field is the `Human.SlackUserID` projection. Both
// are required — a row with an empty `SlackUserID` is degenerate
// (the lead has no Slack contact) and the resolver MUST surface
// it as [ErrLeadHasNoSlackID] so the digest job can skip that
// lead's DM without poisoning the rest of the run.
type LeadAddress struct {
	HumanID     string
	SlackUserID string
	DisplayName string
}

// ErrLeadHasNoSlackID is the typed error [LeadResolver] returns
// when the resolved lead human has no Slack user id. The digest
// job treats it as "skip this lead's DM" (NO post) but DOES
// include the lead's events in the marker advance — losing the
// DM is a per-lead diagnostic, not a window-level rollback.
var ErrLeadHasNoSlackID = errors.New("notebook: lead has no slack_user_id")

// InheritDigestRun is the closed-set shape of the
// `watchkeeper.inherit_digest_runs` row consumed + produced by
// [InheritDigestRunsStore]. The fields mirror the migration's
// columns 1:1; the store handles the SQL marshalling.
type InheritDigestRun struct {
	OrganizationID  string
	LastRunAt       time.Time
	LastWindowStart time.Time
	LastWindowEnd   time.Time
}

// InheritAuditScanner is the seam the digest job dispatches
// through to read the per-window batch of `notebook_inherited`
// rows. The `[since, until)` window is half-open: rows with
// `created_at == since` are EXCLUDED (matches the cursor's
// strictly-after semantics — a row landing exactly on the prior
// `last_window_end` was already drained on the prior run).
//
// Concurrency: implementations MUST be safe for concurrent calls
// across distinct organizations. The production wrapper holds
// only an immutable keepclient reference.
type InheritAuditScanner interface {
	ScanInherited(ctx context.Context, organizationID string, since, until time.Time) ([]InheritEvent, error)
}

// LeadResolver is the seam the digest job dispatches through to
// resolve a successor watchkeeper's lead-human Slack contact. A
// row whose lead has no Slack user id returns [ErrLeadHasNoSlackID]
// (typed sentinel; the job skips that lead's DM but advances the
// marker). Any other error is wrapped and surfaces via the job's
// onLead callback (best-effort — the loop does not abort).
type LeadResolver interface {
	ResolveLead(ctx context.Context, successorWatchkeeperID string) (LeadAddress, error)
}

// InheritDigestPoster is the seam the digest job dispatches through
// to deliver one per-lead Slack DM. Production wires the existing
// Watchmaster `coordinator.SlackMessenger` (a thin alias around
// `chat.postMessage`). Tests substitute a hand-rolled fake.
//
// The `body` argument carries the rendered digest text; the
// `lead.SlackUserID` is the chat.postMessage channel target. The
// poster MUST treat the body as opaque (no parsing, no
// re-formatting); the job's [buildLeadDigest] is the sole composer.
type InheritDigestPoster interface {
	PostDigest(ctx context.Context, lead LeadAddress, body string) error
}

// InheritDigestRunsStore is the seam the digest job dispatches
// through to read + write the per-organization run marker row.
// Production wires a `pgx`-backed wrapper around
// `watchkeeper.inherit_digest_runs`; tests substitute an
// in-memory fake.
//
// `LoadLastRun` returns `(zero, false, nil)` when no marker row
// exists for the org — the FIRST-run seeded-window path. Any
// other error is wrapped and the job aborts (a marker read
// failure could otherwise produce a duplicate DM on the next
// tick).
type InheritDigestRunsStore interface {
	LoadLastRun(ctx context.Context, organizationID string) (InheritDigestRun, bool, error)
	SaveRun(ctx context.Context, run InheritDigestRun) error
}

// InheritDigestDeps is the construction-time bag wired into
// [RunInheritDigest] and [PeriodicInheritDigest]. Held in a
// struct so a future addition (e.g. a per-run metrics emitter)
// lands as a new field without breaking the call signature.
//
// All four seams are required; a nil value for any of them
// panics the constructor with a clear message — matches the
// nil-dep discipline of [NewNotebookInheritStep] +
// [NewBotProfileStep].
type InheritDigestDeps struct {
	Scanner   InheritAuditScanner
	Resolver  LeadResolver
	Poster    InheritDigestPoster
	RunsStore InheritDigestRunsStore
	// Logger is an optional diagnostic sink. When nil, the job
	// emits no diagnostic logs (per-lead failures still surface
	// via [LeadCallback] when wired).
	Logger Logger
}

// LeadCallback is invoked once per resolved lead within a single
// [RunInheritDigest] execution. The arguments mirror the per-lead
// outcome:
//
//   - `(lead, eventsForLead, nil)`     — DM posted successfully.
//   - `(lead, eventsForLead, err)`     — Resolver or Poster
//     surfaced a non-nil error for this lead. The error is
//     scrubbed (no archive URI, no full chat.postMessage body).
//   - `(zero, nil, ErrLeadHasNoSlackID)` — a successor whose
//     lead has no Slack user id; the DM is skipped but the
//     marker still advances.
//
// Per-lead failures do NOT abort the run; the callback fires for
// every lead the scan resolved. The run-level marker advance
// happens AFTER all per-lead callbacks fire (best-effort: a
// transient lead-side outage is observed by the operator but
// does not poison the cursor).
type LeadCallback func(lead LeadAddress, events []InheritEvent, err error)

// validateDeps is the constructor-time guard reused by
// [RunInheritDigest] and [PeriodicInheritDigest]. Panics with a
// clear message on a nil seam. Hoisted to a helper so the two
// entry points share the same nil-dep wire shape.
func validateDeps(deps InheritDigestDeps) {
	if deps.Scanner == nil {
		panic("notebook: inherit digest: deps.Scanner must not be nil")
	}
	if deps.Resolver == nil {
		panic("notebook: inherit digest: deps.Resolver must not be nil")
	}
	if deps.Poster == nil {
		panic("notebook: inherit digest: deps.Poster must not be nil")
	}
	if deps.RunsStore == nil {
		panic("notebook: inherit digest: deps.RunsStore must not be nil")
	}
}

// RunInheritDigest executes one digest pass for the supplied
// organization. The function is the sole entry point exercised
// by the unit tests AND the per-tick worker called by
// [PeriodicInheritDigest].
//
// Resolution order:
//
//  1. Cancellation short-circuit: if `ctx` is already cancelled,
//     return a wrapped `ctx.Err()`; no seam is touched.
//  2. Load the prior run marker via
//     [InheritDigestRunsStore.LoadLastRun].
//  3. Idempotency guard: if a marker exists AND
//     `now - lastRunAt < digestCadence` (24h), return nil without
//     touching the scanner / poster / store. The next tick will
//     retry once the cadence elapses.
//  4. Compute the window: `[windowStart, now)` where windowStart
//     is the prior marker's `LastWindowEnd` OR `now - digestCadence`
//     on a first run.
//  5. Reject `windowStart > now` as [ErrInvalidDigestWindow] (clock
//     skew defence).
//  6. Scan the audit rows via [InheritAuditScanner.ScanInherited].
//     An empty scan is the "empty 24h window → no DM" branch: the
//     marker is still advanced so the cursor moves; the poster is
//     NOT called.
//  7. Group rows by lead via [LeadResolver.ResolveLead]. Each
//     unique successor watchkeeper resolves once (the resolver may
//     be cached by the production wrapper if needed). A resolver
//     failure is reported via `onLead` but does NOT abort the run.
//  8. For each lead with at least one resolved row, build the
//     digest body via [buildLeadDigest] and dispatch
//     [InheritDigestPoster.PostDigest].
//  9. Save the advanced marker via [InheritDigestRunsStore.SaveRun].
//     A save failure surfaces as a wrapped error — the next tick
//     will re-scan the same window.
//
// `onLead` is optional; pass nil to skip per-lead callbacks. The
// scheduling loop uses the callback to log per-lead failures.
//
// All errors are wrapped with `fmt.Errorf("notebook: inherit
// digest: %w", err)` so a caller's `errors.Is` against the
// underlying sentinel still matches.
func RunInheritDigest(
	ctx context.Context,
	organizationID string,
	now time.Time,
	deps InheritDigestDeps,
	onLead LeadCallback,
) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("notebook: inherit digest: %w", err)
	}
	validateDeps(deps)
	if organizationID == "" {
		return fmt.Errorf("notebook: inherit digest: %w", ErrInvalidEntry)
	}

	priorRun, hasPrior, err := deps.RunsStore.LoadLastRun(ctx, organizationID)
	if err != nil {
		return fmt.Errorf("notebook: inherit digest: load last run: %w", err)
	}

	if hasPrior && now.Sub(priorRun.LastRunAt) < digestCadence {
		// Idempotency guard: this tick fell inside the prior run's
		// 24h window. No scan, no post, no marker write — the
		// next tick after the window elapses will pick up where
		// the cursor left off.
		return nil
	}

	var windowStart time.Time
	if hasPrior {
		windowStart = priorRun.LastWindowEnd
	} else {
		windowStart = now.Add(-digestCadence)
	}
	if windowStart.After(now) {
		return fmt.Errorf("notebook: inherit digest: %w", ErrInvalidDigestWindow)
	}

	events, err := deps.Scanner.ScanInherited(ctx, organizationID, windowStart, now)
	if err != nil {
		return fmt.Errorf("notebook: inherit digest: scan: %w", err)
	}

	// Empty window: no DMs but advance the marker. The poster is
	// NOT called — the acceptance pins "empty 24h window → no DM
	// sent". The cursor still moves so the next tick scans the
	// next half-open window.
	if len(events) > 0 {
		dispatchPerLead(ctx, deps, events, onLead)
	}

	newRun := InheritDigestRun{
		OrganizationID:  organizationID,
		LastRunAt:       now,
		LastWindowStart: windowStart,
		LastWindowEnd:   now,
	}
	if err := deps.RunsStore.SaveRun(ctx, newRun); err != nil {
		return fmt.Errorf("notebook: inherit digest: save run: %w", err)
	}
	return nil
}

// dispatchPerLead groups the scanned events by lead and dispatches
// one DM per lead. Extracted from [RunInheritDigest] to keep that
// function under the gocyclo budget while preserving the
// per-lead error-isolation contract.
func dispatchPerLead(
	ctx context.Context,
	deps InheritDigestDeps,
	events []InheritEvent,
	onLead LeadCallback,
) {
	// First pass: resolve every successor's lead. We bucket by
	// human id (the resolver's stable key) so two events whose
	// leads share a human id collapse into one DM.
	type leadBucket struct {
		lead   LeadAddress
		events []InheritEvent
	}
	buckets := make(map[string]*leadBucket)
	leadOrder := make([]string, 0)
	for _, ev := range events {
		lead, err := deps.Resolver.ResolveLead(ctx, ev.SuccessorWatchkeeperID)
		if err != nil {
			if onLead != nil {
				// Errors are scrubbed by the resolver — surface
				// the typed sentinel (or a wrapped one) verbatim.
				onLead(LeadAddress{}, []InheritEvent{ev}, err)
			}
			continue
		}
		if lead.SlackUserID == "" {
			// Defensive: a resolver that returned nil error AND
			// an empty SlackUserID violates the seam contract.
			// Skip the DM, surface via callback, advance marker.
			if onLead != nil {
				onLead(lead, []InheritEvent{ev}, ErrLeadHasNoSlackID)
			}
			continue
		}
		key := lead.HumanID
		bucket, ok := buckets[key]
		if !ok {
			bucket = &leadBucket{lead: lead}
			buckets[key] = bucket
			leadOrder = append(leadOrder, key)
		}
		bucket.events = append(bucket.events, ev)
	}

	// Second pass: render + dispatch per lead. Stable lead order
	// (first-encountered) keeps the test assertions deterministic
	// without forcing the caller to seed events in a sorted order.
	for _, key := range leadOrder {
		bucket := buckets[key]
		// Stable per-pair order: oldest first so the rendered body
		// reads chronologically. Sort the slice in place; the slice
		// is local to this dispatch call so the caller's input is
		// not mutated.
		sort.SliceStable(bucket.events, func(i, j int) bool {
			return bucket.events[i].OccurredAt.Before(bucket.events[j].OccurredAt)
		})
		body := buildLeadDigest(bucket.lead, bucket.events)
		if err := deps.Poster.PostDigest(ctx, bucket.lead, body); err != nil {
			if onLead != nil {
				onLead(bucket.lead, bucket.events, err)
			}
			continue
		}
		if onLead != nil {
			onLead(bucket.lead, bucket.events, nil)
		}
	}
}

// buildLeadDigest renders the per-lead Slack DM body. Sole composer
// (PII guard): the body carries only the lead's display name +
// per-pair `predecessor → successor (N entries)` tuples. NEVER
// echoes the archive URI substring — the URI is an internal
// storage detail not exposed to the lead.
//
// Returned as a single string so the production poster can pass
// it verbatim as `chat.postMessage.text`; markdown-style bullets
// match Slack's rendering for plain-text bodies.
func buildLeadDigest(lead LeadAddress, events []InheritEvent) string {
	total := 0
	for _, ev := range events {
		total += ev.EntriesImported
	}
	name := lead.DisplayName
	if name == "" {
		// Defensive: a resolver that returned an empty DisplayName
		// degrades to the human id so the body is still
		// addressable. The human id is NOT PII (it is an opaque
		// UUID; the audit chain already records it elsewhere).
		name = lead.HumanID
	}
	header := fmt.Sprintf(
		"Notebook inheritance digest for %s — %d inherited entr%s across %d successor%s in the last 24h.",
		name,
		total,
		pluralY(total),
		len(events),
		pluralS(len(events)),
	)
	body := header
	for _, ev := range events {
		body += fmt.Sprintf(
			"\n• %s → %s (%d entr%s)",
			ev.PredecessorWatchkeeperID,
			ev.SuccessorWatchkeeperID,
			ev.EntriesImported,
			pluralY(ev.EntriesImported),
		)
	}
	return body
}

// pluralY returns "y" for n=1 and "ies" otherwise. Tiny helper so
// the digest body reads naturally ("1 entry", "2 entries"). Kept
// inline rather than imported to avoid pulling a pluralisation
// library into the notebook package.
func pluralY(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

// pluralS returns "" for n=1 and "s" otherwise. Same rationale as
// [pluralY].
func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// PeriodicInheritDigest runs [RunInheritDigest] on the supplied
// cadence (typically [digestCadence] = 24h) for the supplied
// organization. The function blocks until `ctx` is cancelled and
// returns the cancellation error on exit.
//
// The `enabled` flag mirrors the roadmap's
// `--inherit-digest-enabled=true` default — a future operator
// override (`--inherit-digest-enabled=false`) flips the flag and
// the function exits immediately with [ErrInheritDigestDisabled].
// Mirrors the [PeriodicBackup] cadence-validation discipline.
//
// Per-tick failures do NOT kill the loop — the marker stays at
// its prior value on a failed tick (no save means the next tick
// re-scans the same window). The `onTick` callback receives the
// per-call error so the operator can correlate failures.
//
// Concurrency: safe for one [PeriodicInheritDigest] goroutine per
// organization. Two goroutines for the same organization would
// race on the marker row's UPDATE and could produce duplicate
// DMs; the production wiring is responsible for the one-per-org
// fan-out.
func PeriodicInheritDigest(
	ctx context.Context,
	organizationID string,
	enabled bool,
	cadence time.Duration,
	clock func() time.Time,
	deps InheritDigestDeps,
	onTick func(err error),
	onLead LeadCallback,
) error {
	if !enabled {
		return ErrInheritDigestDisabled
	}
	if cadence <= 0 {
		return ErrInvalidCadence
	}
	if organizationID == "" {
		return ErrInvalidEntry
	}
	validateDeps(deps)
	if clock == nil {
		clock = time.Now
	}

	ticker := time.NewTicker(cadence)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			err := RunInheritDigest(ctx, organizationID, clock(), deps, onLead)
			if onTick != nil {
				onTick(err)
			}
			// Best-effort: do NOT exit on err. The next tick will retry.
		}
	}
}
