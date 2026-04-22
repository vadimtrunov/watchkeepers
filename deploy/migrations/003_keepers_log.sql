-- Watchkeeper Keep — append-only audit log (M2.1.b).
--
-- `watchkeeper.keepers_log` captures every Keep-observable event for later
-- inspection (`log_tail`, M2.7) and cross-boundary correlation. The table
-- enforces append-only semantics at the database layer via a trigger that
-- rejects both UPDATE and DELETE with a stable, locale-independent phrase so
-- assertions can match the phrase rather than Postgres-translated text.
-- Secondary indices cover the two expected query shapes: recent-first log
-- tail (`created_at DESC`) and correlation lookups. Per the M2.1.a lesson,
-- per-FK indexing is intentionally deferred to the RLS milestone (M2.1.d).

-- +goose Up
CREATE TABLE watchkeeper.keepers_log (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  event_type text NOT NULL,
  correlation_id uuid NULL,
  actor_watchkeeper_id uuid NULL REFERENCES watchkeeper.watchkeeper (id),
  actor_human_id uuid NULL REFERENCES watchkeeper.human (id),
  payload jsonb NOT NULL DEFAULT '{}'::jsonb,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX keepers_log_created_at_desc_idx
ON watchkeeper.keepers_log (created_at DESC);

CREATE INDEX keepers_log_correlation_id_idx
ON watchkeeper.keepers_log (correlation_id)
WHERE correlation_id IS NOT null;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION watchkeeper.keepers_log_reject_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'keepers_log is append-only';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER keepers_log_no_update
BEFORE UPDATE ON watchkeeper.keepers_log
FOR EACH ROW
EXECUTE FUNCTION watchkeeper.keepers_log_reject_mutation();

CREATE TRIGGER keepers_log_no_delete
BEFORE DELETE ON watchkeeper.keepers_log
FOR EACH ROW
EXECUTE FUNCTION watchkeeper.keepers_log_reject_mutation();

-- +goose Down
DROP TRIGGER IF EXISTS keepers_log_no_delete ON watchkeeper.keepers_log;
DROP TRIGGER IF EXISTS keepers_log_no_update ON watchkeeper.keepers_log;
DROP FUNCTION IF EXISTS watchkeeper.keepers_log_reject_mutation();
DROP TABLE IF EXISTS watchkeeper.keepers_log;
