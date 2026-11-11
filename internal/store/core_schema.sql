-- core: the projection of raw. {{schema}} is replaced with core or core_next before this runs,
-- inside a schema that already exists and has no tables. Nothing here may name another schema.
CREATE TABLE {{schema}}.projection_state (
  only_row boolean PRIMARY KEY DEFAULT true CHECK (only_row),
  last_seq bigint NOT NULL, normaliser_rev int NOT NULL);

CREATE TABLE {{schema}}.account (
  id uuid PRIMARY KEY, source text NOT NULL, source_key text NOT NULL, slug text NOT NULL UNIQUE,
  type text NOT NULL,
  feed text NOT NULL CHECK (feed IN ('api','file')),
  currency text NOT NULL, name text NOT NULL, parent_id uuid,
  expects_imports boolean NOT NULL DEFAULT false,
  opened_on date, closed boolean NOT NULL DEFAULT false,
  -- a file-fed account's statement identifier (sort code and account number, digits only),
  -- set by its first import that carries one; a statement naming another is rejected.
  file_ref text,
  UNIQUE (source, source_key));

CREATE TABLE {{schema}}.transaction (
  id uuid PRIMARY KEY, account_id uuid NOT NULL REFERENCES {{schema}}.account(id),
  source text NOT NULL, source_key text NOT NULL,
  amount_minor bigint NOT NULL, currency text NOT NULL,
  local_amount_minor bigint, local_currency text,
  created_at timestamptz, settled_at timestamptz, txn_date date NOT NULL,
  status text NOT NULL CHECK (status IN ('pending','settled','declined')),
  description text NOT NULL, counterparty text, merchant_id text, merchant_name text,
  category text, scheme text, notes text,
  version_count int NOT NULL, current_hash bytea NOT NULL,
  first_seen_at timestamptz NOT NULL, last_seen_at timestamptz NOT NULL,
  disappeared_at timestamptz,
  UNIQUE (account_id, source, source_key));
CREATE INDEX transaction_account_date ON {{schema}}.transaction (account_id, txn_date);
-- Disappearance selects an account's transactions by created_at inside a fetch's period.
CREATE INDEX transaction_account_created ON {{schema}}.transaction (account_id, created_at);

CREATE TABLE {{schema}}.transaction_version (
  transaction_id uuid NOT NULL REFERENCES {{schema}}.transaction(id), version_no int NOT NULL,
  content jsonb NOT NULL, content_hash bytea NOT NULL,
  seen_at timestamptz NOT NULL, raw_seq bigint NOT NULL,
  PRIMARY KEY (transaction_id, version_no));

CREATE TABLE {{schema}}.balance_snapshot (
  account_id uuid NOT NULL REFERENCES {{schema}}.account(id), raw_seq bigint NOT NULL, ordinal int NOT NULL,
  at timestamptz, on_date date, balance_minor bigint NOT NULL, total_balance_minor bigint,
  currency text NOT NULL,
  PRIMARY KEY (account_id, raw_seq, ordinal));

CREATE TABLE {{schema}}.reconciliation (
  account_id uuid PRIMARY KEY REFERENCES {{schema}}.account(id),
  method text NOT NULL,
  running_sum_minor bigint NOT NULL DEFAULT 0,
  last_check_at timestamptz, last_check_ok boolean, last_diff_minor bigint,
  last_pass_at timestamptz, reconciled_through date, break_count int NOT NULL DEFAULT 0,
  known_exception text,
  -- the difference (balance minus running sum) the operator accepted; a check whose difference
  -- equals it passes. Set only by an accept_difference operator entry, with known_exception.
  accepted_diff_minor bigint);

CREATE TABLE {{schema}}.file_gap (
  account_id uuid NOT NULL REFERENCES {{schema}}.account(id), after_date date NOT NULL,
  before_date date NOT NULL, diff_minor bigint NOT NULL, raw_seq bigint NOT NULL,
  PRIMARY KEY (account_id, raw_seq));

-- One row per accepted imported statement. File continuity is computed from the whole set, so
-- it does not depend on the order statements were imported in.
CREATE TABLE {{schema}}.statement (
  account_id uuid NOT NULL REFERENCES {{schema}}.account(id), raw_seq bigint NOT NULL,
  first_date date NOT NULL, last_date date NOT NULL,
  opening_minor bigint NOT NULL, closing_minor bigint NOT NULL,
  PRIMARY KEY (account_id, raw_seq));
