-- core: the projection of raw. {{schema}} is replaced with core or core_next before this runs,
-- inside a schema that already exists and has no tables. Nothing here may name another schema.
CREATE TABLE {{schema}}.projection_state (
  only_row boolean PRIMARY KEY DEFAULT true CHECK (only_row),
  last_seq bigint NOT NULL, normaliser_rev int NOT NULL,
  -- the time zone the projection turned instants into days with (FINSTORE_TIMEZONE); set by the
  -- first CatchUp. A projection run with another zone is refused until renormalise.
  zone text);

CREATE TABLE {{schema}}.account (
  id uuid PRIMARY KEY, source text NOT NULL, source_key text NOT NULL, slug text NOT NULL UNIQUE,
  type text NOT NULL,
  feed text NOT NULL CHECK (feed IN ('api','file')),
  currency text NOT NULL, name text NOT NULL, parent_id uuid,
  expects_imports boolean NOT NULL DEFAULT false,
  opened_on date, closed boolean NOT NULL DEFAULT false,
  -- the account's bank reference: sort code and account number, digits only, joined by a space
  -- (ledger.BankRef). An API source sets it from its account list; a file-fed account's first
  -- statement that carries one sets it, and a statement naming another is rejected. Two accounts
  -- of different sources may share it only while one of them has ended_on.
  bank_ref text,
  -- the last day the account has transactions on, set by an account_end operator entry; null
  -- while it has none. Its later rows stay here but the agent views leave them out.
  ended_on date,
  -- the source's whole account object (a TrueLayer account or card, as the bank listed it). Null
  -- when the source gives none: Monzo's is read from raw by agent.accounts instead.
  detail jsonb,
  UNIQUE (source, source_key));
CREATE INDEX account_bank_ref ON {{schema}}.account (bank_ref);

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
  -- running_balance only: the earliest day whose rows do not add up to its running balances.
  failed_on date,
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
  -- first_date and last_date are the printed period (true) or only the row dates (false)
  exact_period boolean NOT NULL DEFAULT false,
  PRIMARY KEY (account_id, raw_seq));

-- The period each completed fetch of one account's transactions proved complete, as instants:
-- raw.fetch's period, or for a source whose history may start later than asked, from the
-- earliest row returned. Disappearance and agent.coverage read it.
CREATE TABLE {{schema}}.fetch_period (
  fetch_id bigint PRIMARY KEY,
  account_id uuid NOT NULL REFERENCES {{schema}}.account(id),
  period_start timestamptz NOT NULL, period_end timestamptz NOT NULL);
CREATE INDEX fetch_period_account ON {{schema}}.fetch_period (account_id);

-- The pending transactions a bank listed for an account, as of the newest response: one
-- pending_snapshot row per account that has had a successful pending read (also when nothing was
-- pending), and that response's rows in pending_transaction, replaced as a whole by every newer
-- response. They are never part of core.transaction: a pending row has another id than the settled
-- row it becomes, so counting both would count the payment twice.
CREATE TABLE {{schema}}.pending_snapshot (
  account_id uuid PRIMARY KEY REFERENCES {{schema}}.account (id),
  as_of      timestamptz NOT NULL,   -- fetched_at of the response
  raw_seq    bigint NOT NULL,        -- that response's seq
  row_count  integer NOT NULL);
CREATE TABLE {{schema}}.pending_transaction (
  account_id     uuid NOT NULL REFERENCES {{schema}}.account (id),
  transaction_id text NOT NULL,
  txn_date       date NOT NULL,      -- date part of the timestamp as the bank wrote it, as settled rows
  amount_minor   bigint NOT NULL,    -- signed as settled rows of the account are
  currency       text NOT NULL,
  description    text NOT NULL,
  merchant_name  text,
  content        jsonb NOT NULL,     -- the whole row
  PRIMARY KEY (account_id, transaction_id));

-- Standing orders and direct debits ("mandates"), one row per mandate per account, versioned
-- like transactions. account_id is null while no account has the mandate's bank reference.
CREATE TABLE {{schema}}.standing_order (
  id uuid PRIMARY KEY, account_id uuid REFERENCES {{schema}}.account(id),
  bank_ref text NOT NULL, source text NOT NULL, mandate_key text NOT NULL,
  payee text, reference text, frequency text, status text,
  first_date date, first_amount_minor bigint, next_date date, next_amount_minor bigint,
  final_date date, final_amount_minor bigint, currency text,
  version_count int NOT NULL, current_hash bytea NOT NULL,
  first_seen_at timestamptz NOT NULL, last_seen_at timestamptz NOT NULL,
  disappeared_at timestamptz,
  UNIQUE (source, bank_ref, mandate_key));

CREATE TABLE {{schema}}.standing_order_version (
  standing_order_id uuid NOT NULL REFERENCES {{schema}}.standing_order(id), version_no int NOT NULL,
  content jsonb NOT NULL, content_hash bytea NOT NULL,
  seen_at timestamptz NOT NULL, raw_seq bigint NOT NULL,
  PRIMARY KEY (standing_order_id, version_no));

CREATE TABLE {{schema}}.direct_debit (
  id uuid PRIMARY KEY, account_id uuid REFERENCES {{schema}}.account(id),
  bank_ref text NOT NULL, source text NOT NULL, mandate_key text NOT NULL,
  name text, status text, previous_date date, previous_amount_minor bigint, currency text,
  version_count int NOT NULL, current_hash bytea NOT NULL,
  first_seen_at timestamptz NOT NULL, last_seen_at timestamptz NOT NULL,
  disappeared_at timestamptz,
  UNIQUE (source, bank_ref, mandate_key));

CREATE TABLE {{schema}}.direct_debit_version (
  direct_debit_id uuid NOT NULL REFERENCES {{schema}}.direct_debit(id), version_no int NOT NULL,
  content jsonb NOT NULL, content_hash bytea NOT NULL,
  seen_at timestamptz NOT NULL, raw_seq bigint NOT NULL,
  PRIMARY KEY (direct_debit_id, version_no));
