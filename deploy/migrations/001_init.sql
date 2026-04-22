-- Watchkeeper Keep migrations — placeholder.
--
-- Real schema lands in M2 (Keep service) per docs/ROADMAP-phase1.md.
-- Keep holds business knowledge only; no infrastructure metadata.

BEGIN;

CREATE SCHEMA IF NOT EXISTS watchkeeper;

COMMENT ON SCHEMA watchkeeper IS
'Watchkeeper Keep — business knowledge namespace. Schema owner is the keep service role.';

COMMIT;
