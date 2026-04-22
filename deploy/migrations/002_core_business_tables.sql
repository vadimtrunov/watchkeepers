-- Watchkeeper Keep — core business-domain tables (M2.1.a).
--
-- Six tables under the `watchkeeper` schema (created in 001_init.sql):
-- organization -> human -> manifest -> manifest_version -> watchkeeper -> watch_order.
-- All columns use portable PostgreSQL types so the M2.7 protocol choice
-- (HTTP vs gRPC) stays open. The invariant that
-- `watchkeeper.active_manifest_version_id` references a `manifest_version`
-- whose `manifest_id` matches `watchkeeper.manifest_id` is deferred to
-- Phase 2 (not enforced by a composite FK yet).

-- +goose Up
CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE watchkeeper.organization (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  display_name text NOT NULL,
  timezone text NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE watchkeeper.human (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES watchkeeper.organization (id),
  display_name text NOT NULL,
  email text NULL,
  slack_user_id text NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE watchkeeper.manifest (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  display_name text NOT NULL,
  created_by_human_id uuid NULL REFERENCES watchkeeper.human (id),
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE watchkeeper.manifest_version (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  manifest_id uuid NOT NULL REFERENCES watchkeeper.manifest (id),
  version_no integer NOT NULL CHECK (version_no >= 1),
  system_prompt text NOT NULL,
  tools jsonb NOT NULL DEFAULT '[]'::jsonb,
  authority_matrix jsonb NOT NULL DEFAULT '{}'::jsonb,
  knowledge_sources jsonb NOT NULL DEFAULT '[]'::jsonb,
  personality text NULL,
  language text NULL, -- noqa: RF04
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (manifest_id, version_no)
);

CREATE TABLE watchkeeper.watchkeeper (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  manifest_id uuid NOT NULL REFERENCES watchkeeper.manifest (id),
  lead_human_id uuid NOT NULL REFERENCES watchkeeper.human (id),
  active_manifest_version_id uuid NULL REFERENCES watchkeeper.manifest_version (id),
  status text NOT NULL CHECK (status IN ('pending', 'active', 'retired')),
  spawned_at timestamptz NULL,
  retired_at timestamptz NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE watchkeeper.watch_order (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  watchkeeper_id uuid NOT NULL REFERENCES watchkeeper.watchkeeper (id),
  lead_human_id uuid NOT NULL REFERENCES watchkeeper.human (id),
  content text NOT NULL, -- noqa: RF04
  priority text NOT NULL DEFAULT 'normal' CHECK (priority IN ('low', 'normal', 'high', 'urgent')),
  status text NOT NULL DEFAULT 'pending' CHECK (
    status IN ('pending', 'accepted', 'in_progress', 'completed', 'rejected', 'cancelled')
  ),
  created_at timestamptz NOT NULL DEFAULT now(),
  completed_at timestamptz NULL
);

-- +goose Down
DROP TABLE IF EXISTS watchkeeper.watch_order;
DROP TABLE IF EXISTS watchkeeper.watchkeeper;
DROP TABLE IF EXISTS watchkeeper.manifest_version;
DROP TABLE IF EXISTS watchkeeper.manifest;
DROP TABLE IF EXISTS watchkeeper.human;
DROP TABLE IF EXISTS watchkeeper.organization;
