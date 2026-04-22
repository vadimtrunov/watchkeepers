-- Watchkeeper Keep migrations — placeholder.
--
-- Real schema lands in M2 (Keep service) per docs/ROADMAP-phase1.md.
-- Keep holds business knowledge only; no infrastructure metadata.

-- +goose Up
CREATE SCHEMA IF NOT EXISTS watchkeeper;

COMMENT ON SCHEMA watchkeeper IS
'Watchkeeper Keep — business knowledge namespace. Role provisioning and ownership land in M2.';

-- +goose Down
DROP SCHEMA IF EXISTS watchkeeper CASCADE;
