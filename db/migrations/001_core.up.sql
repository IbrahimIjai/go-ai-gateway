-- DESIGN-ONLY migration. Not called by the current in-memory Go server.
-- PostgreSQL 15+. Application creates UUIDs and coordinates transaction state.
BEGIN;

CREATE TABLE organizations (
  id UUID PRIMARY KEY,
  name TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE projects (
  id UUID PRIMARY KEY,
  organization_id UUID NOT NULL REFERENCES organizations(id),
  name TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE(organization_id, name)
);

CREATE TABLE api_keys (
  id UUID PRIMARY KEY,
  project_id UUID NOT NULL REFERENCES projects(id),
  name TEXT NOT NULL,
  key_prefix TEXT NOT NULL,
  key_digest BYTEA NOT NULL UNIQUE,
  monthly_budget_micros BIGINT CHECK (monthly_budget_micros IS NULL OR monthly_budget_micros >= 0),
  expires_at TIMESTAMPTZ,
  revoked_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX api_keys_project_idx ON api_keys(project_id);
CREATE INDEX api_keys_prefix_idx ON api_keys(key_prefix);

CREATE TABLE providers (
  id TEXT PRIMARY KEY,
  label TEXT NOT NULL,
  enabled BOOLEAN NOT NULL DEFAULT true,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE canonical_models (
  id TEXT PRIMARY KEY,
  label TEXT NOT NULL,
  capabilities JSONB NOT NULL DEFAULT '{}'::jsonb,
  context_tokens INTEGER CHECK (context_tokens IS NULL OR context_tokens > 0),
  max_output_tokens INTEGER CHECK (max_output_tokens IS NULL OR max_output_tokens > 0),
  enabled BOOLEAN NOT NULL DEFAULT true
);

CREATE TABLE deployments (
  id UUID PRIMARY KEY,
  canonical_model_id TEXT NOT NULL REFERENCES canonical_models(id),
  provider_id TEXT NOT NULL REFERENCES providers(id),
  upstream_model_id TEXT NOT NULL,
  capabilities_override JSONB NOT NULL DEFAULT '{}'::jsonb,
  enabled BOOLEAN NOT NULL DEFAULT true,
  max_context_tokens INTEGER CHECK (max_context_tokens IS NULL OR max_context_tokens > 0),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE(provider_id, upstream_model_id)
);
CREATE INDEX deployments_model_idx ON deployments(canonical_model_id, enabled);

CREATE TABLE price_versions (
  id UUID PRIMARY KEY,
  deployment_id UUID NOT NULL REFERENCES deployments(id),
  currency CHAR(3) NOT NULL DEFAULT 'USD' CHECK (currency = 'USD'),
  -- rate_card fields: input/output/cache_read/cache_write/etc. in microUSD / million units.
  -- The domain layer must validate rate_card semantics before activation.
  rate_card JSONB NOT NULL,
  markup_basis_points INTEGER NOT NULL DEFAULT 0 CHECK (markup_basis_points >= 0),
  source_url TEXT,
  verified_at TIMESTAMPTZ,
  effective_from TIMESTAMPTZ NOT NULL,
  effective_until TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CHECK (effective_until IS NULL OR effective_until > effective_from)
);
CREATE INDEX price_versions_lookup_idx ON price_versions(deployment_id, effective_from DESC);
-- Enforce non-overlapping effective windows for each deployment in repository logic
-- or with a GiST exclusion constraint after installing btree_gist.

CREATE TABLE inference_requests (
  id UUID PRIMARY KEY,
  project_id UUID NOT NULL REFERENCES projects(id),
  api_key_id UUID NOT NULL REFERENCES api_keys(id),
  -- Idempotency fingerprint is a keyed/hash digest, NEVER raw prompt text.
  idempotency_key TEXT,
  request_fingerprint BYTEA,
  requested_model_id TEXT NOT NULL,
  resolved_model_id TEXT REFERENCES canonical_models(id),
  deployment_id UUID REFERENCES deployments(id),
  price_version_id UUID REFERENCES price_versions(id),
  status TEXT NOT NULL CHECK (status IN ('created','held','in_flight','streaming','settled','released','pending_reconciliation','failed')),
  input_estimate_tokens BIGINT CHECK (input_estimate_tokens IS NULL OR input_estimate_tokens >= 0),
  usage_source TEXT,
  -- Monetary values are microUSD, NULL = unknown (not free).
  upstream_cost_micros BIGINT CHECK (upstream_cost_micros IS NULL OR upstream_cost_micros >= 0),
  customer_cost_micros BIGINT CHECK (customer_cost_micros IS NULL OR customer_cost_micros >= 0),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE(project_id, idempotency_key)
);
CREATE INDEX inference_requests_by_project_idx ON inference_requests(project_id, created_at DESC);
CREATE INDEX inference_requests_by_status_idx ON inference_requests(status, created_at);

CREATE TABLE provider_attempts (
  id UUID PRIMARY KEY,
  inference_request_id UUID NOT NULL REFERENCES inference_requests(id),
  deployment_id UUID NOT NULL REFERENCES deployments(id),
  upstream_request_id TEXT,
  attempt_number INTEGER NOT NULL CHECK (attempt_number > 0),
  status TEXT NOT NULL CHECK (status IN ('started','completed','failed','interrupted')),
  http_status INTEGER,
  error_code TEXT,
  ttft_ms INTEGER CHECK (ttft_ms IS NULL OR ttft_ms >= 0),
  latency_ms INTEGER CHECK (latency_ms IS NULL OR latency_ms >= 0),
  started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  completed_at TIMESTAMPTZ,
  UNIQUE(inference_request_id, attempt_number)
);

CREATE TABLE request_usage (
  inference_request_id UUID PRIMARY KEY REFERENCES inference_requests(id),
  input_tokens BIGINT CHECK (input_tokens IS NULL OR input_tokens >= 0),
  output_tokens BIGINT CHECK (output_tokens IS NULL OR output_tokens >= 0),
  cached_read_tokens BIGINT CHECK (cached_read_tokens IS NULL OR cached_read_tokens >= 0),
  cached_write_tokens BIGINT CHECK (cached_write_tokens IS NULL OR cached_write_tokens >= 0),
  reasoning_tokens BIGINT CHECK (reasoning_tokens IS NULL OR reasoning_tokens >= 0),
  -- Vendor raw usage only. Never store prompts, messages, tool outputs or secrets here.
  provider_usage JSONB NOT NULL DEFAULT '{}'::jsonb,
  provider_usage_source TEXT NOT NULL,
  observed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMIT;
