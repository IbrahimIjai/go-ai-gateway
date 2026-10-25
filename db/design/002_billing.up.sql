-- DESIGN-ONLY migration, dependent on 001_core.up.sql.
-- Journal invariants are enforced by SERIALIZABLE/account locking plus application
-- sum-to-zero checks before transaction commit, and should receive constraint
-- triggers/test coverage before any paid launch.
BEGIN;

CREATE TABLE journal_accounts (
  id UUID PRIMARY KEY,
  organization_id UUID REFERENCES organizations(id),
  kind TEXT NOT NULL CHECK (kind IN ('customer_credit','platform_grants','platform_revenue','provider_payable','adjustment')),
  currency CHAR(3) NOT NULL DEFAULT 'USD' CHECK (currency = 'USD'),
  -- Materialized balance, updated in SAME DB txn as immutable journal entries.
  balance_micros BIGINT NOT NULL DEFAULT 0,
  reserved_micros BIGINT NOT NULL DEFAULT 0 CHECK (reserved_micros >= 0),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (organization_id, kind, currency),
  CHECK (kind <> 'customer_credit' OR balance_micros >= reserved_micros)
);
CREATE UNIQUE INDEX one_platform_journal_account
  ON journal_accounts(kind, currency) WHERE organization_id IS NULL;

CREATE TABLE journal_transactions (
  id UUID PRIMARY KEY,
  inference_request_id UUID REFERENCES inference_requests(id),
  kind TEXT NOT NULL CHECK (kind IN ('credit_grant','credit_purchase','inference_settle','refund','manual_adjustment','reversal')),
  external_reference TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  memo TEXT,
  UNIQUE(inference_request_id, kind),
  UNIQUE(kind, external_reference)
);

CREATE TABLE journal_entries (
  id UUID PRIMARY KEY,
  journal_transaction_id UUID NOT NULL REFERENCES journal_transactions(id),
  journal_account_id UUID NOT NULL REFERENCES journal_accounts(id),
  amount_micros BIGINT NOT NULL CHECK (amount_micros <> 0),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX journal_entries_transaction_idx ON journal_entries(journal_transaction_id);
CREATE INDEX journal_entries_account_idx ON journal_entries(journal_account_id, created_at DESC);

CREATE TABLE credit_reservations (
  id UUID PRIMARY KEY,
  inference_request_id UUID NOT NULL UNIQUE REFERENCES inference_requests(id),
  customer_account_id UUID NOT NULL REFERENCES journal_accounts(id),
  amount_micros BIGINT NOT NULL CHECK (amount_micros > 0),
  status TEXT NOT NULL CHECK (status IN ('active','settled','released','pending_review')),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at TIMESTAMPTZ NOT NULL,
  resolved_at TIMESTAMPTZ,
  CHECK (expires_at > created_at)
);
CREATE INDEX active_reservations_expiry_idx
  ON credit_reservations(expires_at) WHERE status IN ('active','pending_review');

CREATE TABLE reconciliation_issues (
  id UUID PRIMARY KEY,
  inference_request_id UUID REFERENCES inference_requests(id),
  provider_attempt_id UUID REFERENCES provider_attempts(id),
  kind TEXT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('open','investigating','resolved','ignored')),
  details JSONB NOT NULL DEFAULT '{}'::jsonb,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  resolved_at TIMESTAMPTZ
);

CREATE TABLE worker_jobs (
  id UUID PRIMARY KEY,
  kind TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'queued' CHECK (status IN ('queued','running','done','failed')),
  payload JSONB NOT NULL DEFAULT '{}'::jsonb,
  run_after TIMESTAMPTZ NOT NULL DEFAULT now(),
  attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
  locked_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX worker_jobs_pick_idx ON worker_jobs(status, run_after);

COMMIT;
