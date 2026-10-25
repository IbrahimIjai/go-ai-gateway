-- Prepaid credit ledger (Phase 6). Amounts are integer micro-USD; every
-- journal transaction's entries sum to zero. Settled history is immutable:
-- corrections are new transactions.

CREATE TABLE journal_accounts (
  id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id     UUID REFERENCES projects(id),          -- null for platform accounts
  kind           TEXT NOT NULL CHECK (kind IN ('customer_credit', 'platform_grants', 'platform_revenue', 'adjustment')),
  balance_micros BIGINT NOT NULL DEFAULT 0,
  reserved_micros BIGINT NOT NULL DEFAULT 0 CHECK (reserved_micros >= 0),
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  -- Holds can never exceed what a customer has: concurrent requests cannot
  -- spend the same funds.
  CHECK (kind <> 'customer_credit' OR balance_micros >= reserved_micros)
);
CREATE UNIQUE INDEX one_customer_account ON journal_accounts(project_id, kind) WHERE project_id IS NOT NULL;
CREATE UNIQUE INDEX one_platform_account ON journal_accounts(kind) WHERE project_id IS NULL;

CREATE TABLE journal_transactions (
  id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  inference_request_id TEXT,                 -- the request a settlement is for
  kind                 TEXT NOT NULL CHECK (kind IN ('credit_grant', 'inference_settle', 'refund', 'manual_adjustment')),
  external_reference   TEXT,                 -- grant or refund reference, for idempotency
  memo                 TEXT,
  created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (inference_request_id, kind),       -- one settlement per request, ever
  UNIQUE (kind, external_reference)
);

CREATE TABLE journal_entries (
  id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  journal_transaction_id UUID NOT NULL REFERENCES journal_transactions(id),
  journal_account_id     UUID NOT NULL REFERENCES journal_accounts(id),
  amount_micros          BIGINT NOT NULL CHECK (amount_micros <> 0),
  created_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX journal_entries_account_idx ON journal_entries(journal_account_id, created_at DESC);

CREATE TABLE credit_reservations (
  inference_request_id TEXT PRIMARY KEY,     -- written before the request record exists
  account_id           UUID NOT NULL REFERENCES journal_accounts(id),
  amount_micros        BIGINT NOT NULL CHECK (amount_micros > 0),
  status               TEXT NOT NULL CHECK (status IN ('active', 'settled', 'released', 'pending_review')),
  settled_micros       BIGINT CHECK (settled_micros IS NULL OR settled_micros >= 0),
  note                 TEXT,
  created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at           TIMESTAMPTZ NOT NULL,
  resolved_at          TIMESTAMPTZ
);
CREATE INDEX credit_reservations_expiry_idx ON credit_reservations(expires_at) WHERE status = 'active';

CREATE TABLE idempotency_keys (
  project_id           UUID NOT NULL REFERENCES projects(id),
  key                  TEXT NOT NULL,
  fingerprint          BYTEA NOT NULL,       -- hash of the request body, never the body
  inference_request_id TEXT NOT NULL,
  created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (project_id, key)
);
