-- Tenancy, API keys and durable request records (Phase 5).
-- Models and deployments live in the catalog file, so deployment IDs here are
-- the catalog's text IDs. Prompts, outputs and raw keys are never stored.

CREATE TABLE organizations (
  id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  name       TEXT NOT NULL UNIQUE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE projects (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id UUID NOT NULL REFERENCES organizations(id),
  name            TEXT NOT NULL,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (organization_id, name)
);

CREATE TABLE api_keys (
  id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id  UUID NOT NULL REFERENCES projects(id),
  name        TEXT NOT NULL,
  key_prefix  TEXT NOT NULL,              -- shown to people, never enough to authenticate
  key_digest  BYTEA NOT NULL UNIQUE,      -- HMAC-SHA256(pepper, key)
  models      TEXT[] NOT NULL DEFAULT '{}', -- allowed model/alias IDs; empty: any
  rpm         INTEGER NOT NULL DEFAULT 0 CHECK (rpm >= 0),
  tpm         INTEGER NOT NULL DEFAULT 0 CHECK (tpm >= 0),
  concurrency INTEGER NOT NULL DEFAULT 0 CHECK (concurrency >= 0),
  expires_at  TIMESTAMPTZ,
  revoked_at  TIMESTAMPTZ,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX api_keys_project_idx ON api_keys(project_id);

CREATE TABLE inference_requests (
  id              TEXT PRIMARY KEY,         -- the chatcmpl id returned to the client
  project_id      UUID REFERENCES projects(id),   -- null in single-key mode
  api_key_id      UUID REFERENCES api_keys(id),
  requested_model TEXT NOT NULL,
  resolved_model  TEXT,
  deployment_id   TEXT,
  status          TEXT NOT NULL CHECK (status IN ('ok', 'usage_unavailable', 'failed')),
  error_class     TEXT,
  stream          BOOLEAN NOT NULL,
  route           JSONB NOT NULL,           -- the routing decision; holds no prompt text
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX inference_requests_project_idx ON inference_requests(project_id, created_at DESC);

CREATE TABLE provider_attempts (
  inference_request_id TEXT NOT NULL REFERENCES inference_requests(id),
  attempt_number       INTEGER NOT NULL CHECK (attempt_number > 0),
  deployment_id        TEXT NOT NULL,
  class                TEXT,                -- failure class; null on success
  http_status          INTEGER,
  duration_ms          BIGINT NOT NULL CHECK (duration_ms >= 0),
  PRIMARY KEY (inference_request_id, attempt_number)
);

CREATE TABLE request_usage (
  inference_request_id TEXT PRIMARY KEY REFERENCES inference_requests(id),
  input_tokens         BIGINT NOT NULL CHECK (input_tokens >= 0),
  output_tokens        BIGINT NOT NULL CHECK (output_tokens >= 0),
  cached_read_tokens   BIGINT NOT NULL DEFAULT 0 CHECK (cached_read_tokens >= 0),
  cached_write_tokens  BIGINT NOT NULL DEFAULT 0 CHECK (cached_write_tokens >= 0),
  reasoning_tokens     BIGINT NOT NULL DEFAULT 0 CHECK (reasoning_tokens >= 0),
  cost_micros          BIGINT CHECK (cost_micros IS NULL OR cost_micros >= 0), -- null: unpriced, not free
  provider_usage       JSONB NOT NULL DEFAULT '{}'::jsonb
);
