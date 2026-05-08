# Watchmaster manifest

The **Watchmaster** is the orchestrator agent that supervises every
Watchkeeper running under a Watchkeepers deployment. It routes
operator requests, gates privileged actions through lead approval, and
reports token spend on every turn. It is deferential to the lead human
by design; it proposes, the lead approves.

The canonical Watchmaster manifest content is seeded into the Keep
database by `deploy/migrations/017_watchmaster_manifest_seed.sql` (TASK
M6.1.a). The stable identifiers are exposed as Go constants in
`core/pkg/manifest/watchmaster.go` and consumed by every downstream
caller — never hard-code the UUID literals at the call site.

## Stable identifiers

| Constant                          | Role                                 | UUID                                   |
| --------------------------------- | ------------------------------------ | -------------------------------------- |
| `WatchmasterManifestID`           | manifest row                         | `10000000-0000-4000-8000-000000000000` |
| `WatchmasterSystemOrganizationID` | "system" tenant the manifest runs in | `00000000-0000-4000-8000-000000000000` |

## Privilege boundary

The Watchmaster system prompt states **verbatim**:

> Privilege boundary: you NEVER execute Slack App creation directly.
> You ALWAYS go through the privileged RPC tool (M6.1.b) which itself
> runs under lead approval.

Bypassing this boundary is a hard violation. The phrase
`NEVER execute Slack App creation directly` is asserted by
`core/pkg/manifest/watchmaster_seed_test.go`; do not reword it without
also updating the Go test fixture.

## Authority matrix

The seeded `authority_matrix` jsonb declares which privileged actions
the Watchmaster may request, request-with-approval, or is forbidden
from invoking. Values follow the M5.5.b.c.c.b enum
(`"allowed" | "lead_approval" | "forbidden"`):

| Action                  | Disposition     | Notes                                                             |
| ----------------------- | --------------- | ----------------------------------------------------------------- |
| `slack_app_create`      | `lead_approval` | Watchmaster MAY request; M6.1.b RPC executes under lead approval. |
| `watchkeeper_retire`    | `lead_approval` | M6.2 lifecycle gate.                                              |
| `manifest_version_bump` | `lead_approval` | Every manifest update is governed configuration.                  |
| `keepers_log_read`      | `allowed`       | Read-only audit-trail access.                                     |
| `keep_search`           | `allowed`       | Read-only knowledge access.                                       |

`lead_approval` semantics are owned by the runtime / harness ACL gate
landing alongside the M6.1.b RPC.

## Other manifest fields

- **Model**: `claude-haiku-4-5-20251001` — high-frequency low-stakes
  orchestration, not deep reasoning.
- **Autonomy**: `supervised` — every tool call runs under the
  M5.6 reflection lifecycle.
- **Toolset**: empty `[]` placeholder. The real toolset
  (`list_watchkeepers`, `propose_spawn`, etc.) lands in M6.2.
- **Notebook recall**: `top_k = 3`, `relevance_threshold = 0.3` —
  small/loose because the Watchmaster Notebook mostly carries
  org-level operational notes.

## Re-seeding and idempotency

The migration uses `INSERT ... ON CONFLICT (id) DO NOTHING` for every
row. Re-running on a DB that already carries the seed is a no-op. To
roll content forward (e.g., revise the system prompt), follow the
manifest_version_bump workflow under lead approval; do **not** edit
this migration in place — write a new migration that issues an
UPDATE bound to the stable id.
