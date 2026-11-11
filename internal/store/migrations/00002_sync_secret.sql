-- +goose Up
-- sync holds operational state; secret holds the OAuth token pair. Neither is part of the
-- projection and both survive a rebuild of core.
CREATE TABLE sync.run (
  id bigserial PRIMARY KEY, source text NOT NULL,
  kind text NOT NULL,
  started_at timestamptz NOT NULL, finished_at timestamptz,
  outcome text CHECK (outcome IN ('ok','partial','failed')),
  requests int NOT NULL DEFAULT 0, rows_seen int NOT NULL DEFAULT 0,
  error_category text);

CREATE TABLE sync.consent (
  id bigserial PRIMARY KEY, source text NOT NULL,
  state text NOT NULL CHECK (state IN ('awaiting_approval','active','replaced','lost','expired')),
  token_issued_at timestamptz NOT NULL, approved_at timestamptz,
  expected_expiry_at timestamptz, ended_at timestamptz, end_reason text,
  is_current boolean NOT NULL DEFAULT false, last_sync_ok_at timestamptz);
CREATE UNIQUE INDEX consent_one_current ON sync.consent (source) WHERE is_current;

CREATE TABLE sync.connect_attempt (
  id bigserial PRIMARY KEY, source text NOT NULL, state_sha256 bytea NOT NULL UNIQUE,
  started_at timestamptz NOT NULL, expires_at timestamptz NOT NULL, used_at timestamptz,
  outcome text CHECK (outcome IN ('exchanged','expired','refused')));

CREATE TABLE sync.backfill_window (
  id bigserial PRIMARY KEY, account_key text NOT NULL,
  period_start timestamptz NOT NULL, period_end timestamptz NOT NULL,
  cursor text, completed_at timestamptz,
  UNIQUE (account_key, period_start, period_end));

CREATE INDEX run_source_finished ON sync.run (source, finished_at);

CREATE TABLE secret.oauth_token (
  source text PRIMARY KEY, access_token text NOT NULL, refresh_token text NOT NULL,
  issued_at timestamptz NOT NULL, expires_at timestamptz NOT NULL, updated_at timestamptz NOT NULL);
