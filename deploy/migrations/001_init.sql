-- Watchkeeper Keep migrations — placeholder.
--
-- Real schema lands in M2 (Keep service) per docs/ROADMAP-phase1.md.
-- Keep holds business knowledge only; no infrastructure metadata.

begin;

create schema if not exists watchkeeper;

comment on schema watchkeeper is
  'Watchkeeper Keep — business knowledge namespace. Schema owner is the keep service role.';

commit;
