// Package lifecycle owns the logical Spawn / Retire / Health / List
// bookkeeping for a Watchkeeper agent's database row. It is a thin
// orchestration layer over the Watchkeeper CRUD methods exposed by
// [github.com/vadimtrunov/watchkeepers/core/pkg/keepclient]: Spawn
// inserts a `pending` row and transitions it to `active`, Retire moves
// an `active` row to `retired`, Health reads the row and projects it
// into a slim [Status] view, and List passes through to keepclient's
// listing endpoint with the in-process bound checks mirrored.
//
// # In scope
//
// Logical bookkeeping only. The package adds no SQL, no HTTP, and no
// goroutine of its own; every state-changing call is one (or two,
// in Spawn's case) keepclient round-trip(s). The [Manager] type is
// stateless beyond its immutable client + optional logger + clock.
//
// # Out of scope
//
//   - Process supervision — actual `exec.Command` spawning, child
//     restart loops, and signal forwarding land in M5.3.
//   - Cron-driven lifecycle events — M3.3 wires the cron primitives
//     onto the same keepclient surface; lifecycle stays uninvolved.
//   - Heartbeat publishing / external Health probing — M3.6 owns the
//     liveness side. Health here is a row read, not a probe.
//   - Cross-host distribution / leader election — Phase 1 is single
//     host; multi-host coordination is out of Phase 1.
//   - RLS on the `watchkeeper` table — M3.2.a documented this as a
//     known gap; lifecycle does not paper over it.
//
// # LocalKeepClient interface
//
// The package depends on a local [LocalKeepClient] interface that
// mirrors the four keepclient methods it consumes. Production code
// never imports keepclient directly; only the tests do, for the
// compile-time assertion that `*keepclient.Client` satisfies
// [LocalKeepClient]. This mirrors the notebook+archivestore one-way
// import-cycle break documented in `docs/LESSONS.md` (M2b.6).
//
// # Partial-failure shape
//
// Spawn performs two keepclient calls in sequence: InsertWatchkeeper
// followed by UpdateWatchkeeperStatus("active"). When the Insert
// succeeds but the Update fails the call returns the populated id
// alongside the wrapped error so the caller can retry just the Update
// against the existing `pending` row. This shape mirrors M2b.4 /
// M2b.7 where a multi-step operation surfaces (populated_value, err)
// on a partial failure to keep the retry surface tight.
package lifecycle
