-- +goose Up
CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE IF NOT EXISTS orgs (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS users (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid REFERENCES orgs(id) ON DELETE CASCADE,
  email text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS api_keys (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid REFERENCES orgs(id) ON DELETE CASCADE,
  token text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS inboxes (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid REFERENCES orgs(id) ON DELETE CASCADE,
  address text NOT NULL,
  status text NOT NULL DEFAULT 'active',
  labels text[] NOT NULL DEFAULT '{}',
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS threads (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  inbox_id uuid REFERENCES inboxes(id) ON DELETE CASCADE,
  subject text,
  status text NOT NULL DEFAULT 'open',
  participants_hash text,
  participants jsonb NOT NULL DEFAULT '[]',
  updated_at timestamptz NOT NULL DEFAULT now(),
  sentiment_score real,
  priority_level text,
  provider_thread_id text
);

CREATE TABLE IF NOT EXISTS messages (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  inbox_id uuid REFERENCES inboxes(id) ON DELETE CASCADE,
  thread_id uuid REFERENCES threads(id) ON DELETE CASCADE,
  direction text NOT NULL,
  subject text,
  text text,
  html text,
  raw_ref text,
  created_at timestamptz NOT NULL DEFAULT now(),
  provider_message_id text,
  provider_blob_id text,
  internet_message_id text,
  from_json jsonb,
  to_json jsonb,
  cc_json jsonb
);

CREATE TABLE IF NOT EXISTS attachments (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  message_id uuid REFERENCES messages(id) ON DELETE CASCADE,
  object_ref text,
  mime text,
  size bigint
);

CREATE TABLE IF NOT EXISTS embeddings (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  message_id uuid REFERENCES messages(id) ON DELETE CASCADE,
  chunk_id text NOT NULL,
  vector_metadata jsonb NOT NULL DEFAULT '{}',
  model_name text,
  dim int,
  content_hash text,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS taxonomies (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name text NOT NULL,
  body jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS schemas (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name text NOT NULL,
  body jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS policies (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name text NOT NULL,
  version int NOT NULL DEFAULT 1,
  is_active boolean NOT NULL DEFAULT true,
  body jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS tool_calls (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tool_name text NOT NULL,
  idempotency_key text,
  model_name text,
  prompt_version text,
  latency_ms int,
  correction_text text,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS audit_log (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tool_call_id uuid REFERENCES tool_calls(id) ON DELETE SET NULL,
  actor text,
  inputs_hash text,
  outputs_hash text,
  replay_id text,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS inbox_checkpoints (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  inbox_id uuid REFERENCES inboxes(id) ON DELETE CASCADE,
  provider text NOT NULL,
  last_state text,
  updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_provider ON messages(inbox_id, provider_message_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_threads_provider ON threads(inbox_id, provider_thread_id);
CREATE INDEX IF NOT EXISTS idx_threads_updated ON threads(updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_messages_thread ON messages(thread_id);
CREATE INDEX IF NOT EXISTS idx_messages_text_fts ON messages USING GIN (to_tsvector('simple', coalesce(text,'')));
CREATE UNIQUE INDEX IF NOT EXISTS idx_inbox_checkpoints ON inbox_checkpoints(inbox_id, provider);

-- +goose Down
DROP TABLE IF EXISTS inbox_checkpoints;
DROP TABLE IF EXISTS audit_log;
DROP TABLE IF EXISTS tool_calls;
DROP TABLE IF EXISTS policies;
DROP TABLE IF EXISTS schemas;
DROP TABLE IF EXISTS taxonomies;
DROP TABLE IF EXISTS embeddings;
DROP TABLE IF EXISTS attachments;
DROP TABLE IF EXISTS messages;
DROP TABLE IF EXISTS threads;
DROP TABLE IF EXISTS inboxes;
DROP TABLE IF EXISTS api_keys;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS orgs;
