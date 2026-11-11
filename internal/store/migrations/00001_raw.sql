-- +goose Up
-- raw holds what the sources sent. Rows are never updated or deleted. Every table draws its key
-- from raw.seq, which is the single order in which rows are normalised.
CREATE SEQUENCE raw.seq;

CREATE TABLE raw.fetch (
  id            bigint PRIMARY KEY DEFAULT nextval('raw.seq'),
  source        text NOT NULL,
  kind          text NOT NULL,
  account_key   text,
  period_start  timestamptz,
  period_end    timestamptz,
  run_id        bigint NOT NULL,
  started_at    timestamptz NOT NULL);

CREATE TABLE raw.api_response (
  seq           bigint PRIMARY KEY DEFAULT nextval('raw.seq'),
  fetch_id      bigint NOT NULL REFERENCES raw.fetch(id),
  source        text NOT NULL,
  endpoint      text NOT NULL,
  params        jsonb NOT NULL,
  status        int NOT NULL,
  content_type  text NOT NULL,
  fetched_at    timestamptz NOT NULL,
  body          bytea NOT NULL,
  body_sha256   bytea NOT NULL);

CREATE TABLE raw.fetch_complete (
  seq           bigint PRIMARY KEY DEFAULT nextval('raw.seq'),
  fetch_id      bigint NOT NULL UNIQUE REFERENCES raw.fetch(id),
  completed_at  timestamptz NOT NULL,
  pages         int NOT NULL);

CREATE TABLE raw.import_file (
  seq           bigint PRIMARY KEY DEFAULT nextval('raw.seq'),
  account_slug  text NOT NULL,
  format        text NOT NULL,
  file_name     text NOT NULL,
  sha256        bytea NOT NULL,
  size_bytes    int NOT NULL,
  imported_at   timestamptz NOT NULL,
  body          bytea NOT NULL,
  -- The format is part of the key, so a file refused under the wrong --format can still be
  -- imported under the right one.
  UNIQUE (account_slug, format, sha256));

CREATE TABLE raw.operator_entry (
  seq           bigint PRIMARY KEY DEFAULT nextval('raw.seq'),
  kind          text NOT NULL,
  payload       jsonb NOT NULL,
  entered_at    timestamptz NOT NULL);

-- Responses of one fetch (disappearance), idempotent capture loading, the previous balance
-- reading of a run, and the coverage view's join from accounts to their fetches.
CREATE INDEX api_response_fetch ON raw.api_response (fetch_id, seq);
CREATE INDEX api_response_capture_key ON raw.api_response (endpoint, fetched_at, body_sha256);
CREATE INDEX fetch_run ON raw.fetch (run_id, account_key);
CREATE INDEX fetch_account ON raw.fetch (source, account_key);
