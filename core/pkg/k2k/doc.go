// Package k2k provides the Keeper-to-Keeper (K2K) conversation domain
// and persistence surface used by the Phase 2 M1 multi-agent
// communication milestone.
//
// M1.1.a scope — this package ships:
//
//   - The [Conversation] struct: the projection of a single
//     `k2k_conversations` row that the higher layers (Slack adapter
//     wiring in M1.1.b/.c, the K2K tool suite in M1.3, the escalation
//     saga in M1.6) consume.
//   - The [Repository] interface: the unit-test seam between the K2K
//     conversation domain and persistence. Production wiring (M1.1.c)
//     plugs a Postgres-backed [PostgresRepository]; unit tests + dev /
//     smoke loops use [MemoryRepository].
//   - In-memory + Postgres implementations behind [Repository]. The
//     in-memory store is exhaustively tested for lifecycle and
//     concurrency; the Postgres store satisfies the interface as a
//     compile assertion and is exercised end-to-end by the M1.1.c
//     lifecycle wiring + the `scripts/migrate-schema-test.sh` RLS
//     assertions on the matching `029_k2k_conversations.sql` migration.
//
// What this package deliberately does NOT ship at M1.1.a:
//
//   - No Slack adapter calls. The `slack_channel_id` column is populated
//     by the M1.1.c lifecycle wiring once the M1.1.b Slack channel
//     primitives land; the domain stores whatever the caller hands in.
//   - No audit emission. The K2K event taxonomy
//     (`k2k_conversation_opened` / `k2k_conversation_closed` / etc.)
//     lives in M1.4; the [Repository] is a transient state surface, not
//     an audit sink.
//   - No PII redaction harness. The `subject` field is operator-supplied
//     free-text; downstream M1.4 audit emission carries the redaction
//     contract. The repository's only PII discipline at this layer is
//     defensive copy of reference-typed fields (the `participants`
//     slice) so caller-side mutation cannot bleed into the held row.
//   - No token-budget enforcement. M1.5 owns the
//     "budget-exceeded → escalation" semantics; M1.1.a only persists
//     the budget + the running counter and exposes a goroutine-safe
//     [Repository.IncTokens] for the M1.5 enforcement layer to drive.
//
// Concurrency: every [Repository] method is safe for concurrent use
// across goroutines. The in-memory implementation guards the underlying
// map with a [sync.RWMutex]; the Postgres implementation relies on the
// underlying [github.com/jackc/pgx/v5/pgxpool.Pool] for connection
// safety and on row-level locking via `UPDATE ... RETURNING` for the
// [Repository.IncTokens] race.
//
// Per-org RLS: the matching `029_k2k_conversations.sql` migration
// installs the M2.1.d FORCE-RLS pattern keyed off the
// `watchkeeper.org` session GUC (the same GUC the manifest RLS from
// M3.5.a.3.1 consults). The Postgres impl assumes its caller has
// already issued `SET LOCAL ROLE` + `SET LOCAL watchkeeper.org`
// (typically via [core/internal/keep/db.WithScope]) before invoking
// the repository methods; an unset GUC is fail-closed (zero rows
// visible, no INSERT permitted) per the migration's `nullif(..., ”)`
// cast.
//
// See `docs/ROADMAP-phase2.md` §M1 → M1.1 → M1.1.a for the AC and
// `docs/lessons/M1.md` for the patterns settled in this PR.
package k2k
