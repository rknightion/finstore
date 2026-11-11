-- The agent views. Applied by migrate and after every core swap, as finstore_owner, after every
-- existing view in agent has been dropped. They read core, sync and raw with the owner's rights.
-- Days are Europe/London days: an instant is converted with AT TIME ZONE 'Europe/London'.

CREATE VIEW agent.accounts AS
SELECT a.slug, a.source, a.type, a.feed, a.currency, a.name,
       p.slug AS parent_slug, a.opened_on, a.closed
FROM core.account a
LEFT JOIN core.account p ON p.id = a.parent_id;

CREATE VIEW agent.transactions AS
SELECT t.id, a.slug AS account_slug, t.txn_date, t.created_at, t.settled_at,
       t.amount_minor, t.currency, t.local_amount_minor, t.local_currency, t.status,
       t.description, t.counterparty, t.merchant_name, t.category, t.scheme, t.notes,
       t.version_count, t.first_seen_at, t.last_seen_at
FROM core.transaction t
JOIN core.account a ON a.id = t.account_id
WHERE t.disappeared_at IS NULL;

CREATE VIEW agent.transaction_history AS
SELECT v.transaction_id, v.version_no, v.seen_at, v.content
FROM core.transaction_version v;

CREATE VIEW agent.balances AS
SELECT a.slug AS account_slug, b.at, b.on_date, b.balance_minor, b.total_balance_minor, b.currency
FROM core.balance_snapshot b
JOIN core.account a ON a.id = b.account_id;

-- coverage: one row per account. A gap is a run of days (from and to inclusive) that nothing
-- covers. For an API account the days are its lifetime minus the days touched by the periods of
-- its completed fetches; the part of that inside an unfinished backfill window is reported as the
-- window closing. The lifetime runs from opened_on (else the first covered day) to yesterday for
-- an open account, or to the last covered day for a closed one. Today is never a gap, so the time
-- between the last hourly fetch and now is not reported. Pots and balances-only accounts have no
-- transaction gaps. For a file account the gaps are core.file_gap (from is the last date of the
-- earlier file, to the first date of the later one) plus "import due" for an account that expects
-- imports, whatever its type, and whose latest statement date (its newest transaction date or
-- balance snapshot date) is more than 35 days old.
CREATE VIEW agent.coverage AS
WITH today AS (
  SELECT (now() AT TIME ZONE 'Europe/London')::date AS d
),
txn AS (
  SELECT account_id, min(txn_date) AS earliest, max(txn_date) AS latest, count(*) AS n
  FROM core.transaction
  WHERE disappeared_at IS NULL
  GROUP BY account_id
),
fetch_days AS (
  SELECT a.id AS account_id,
         (f.period_start AT TIME ZONE 'Europe/London')::date AS first_day,
         ((f.period_end - interval '1 microsecond') AT TIME ZONE 'Europe/London')::date AS last_day
  FROM raw.fetch f
  JOIN raw.fetch_complete fc ON fc.fetch_id = f.id
  JOIN core.account a ON a.source = f.source AND a.source_key = f.account_key
  WHERE f.period_start IS NOT NULL AND f.period_end > f.period_start
),
covered AS (
  SELECT account_id,
         range_agg(daterange(first_day, last_day, '[]')) AS days,
         min(first_day) AS first_day,
         max(last_day) AS last_day
  FROM fetch_days
  GROUP BY account_id
),
unfinished AS (
  SELECT a.id AS account_id,
         range_agg(daterange((w.period_start AT TIME ZONE 'Europe/London')::date,
                             ((w.period_end - interval '1 microsecond') AT TIME ZONE 'Europe/London')::date,
                             '[]')) AS days
  FROM sync.backfill_window w
  JOIN core.account a ON a.feed = 'api' AND a.source_key = w.account_key
  WHERE w.completed_at IS NULL AND w.period_end > w.period_start
  GROUP BY a.id
),
api_life AS (
  SELECT a.id AS account_id,
         coalesce(a.opened_on, c.first_day) AS life_start,
         CASE WHEN a.closed THEN c.last_day + 1 ELSE (SELECT d FROM today) END AS life_end,
         coalesce(c.days, '{}'::datemultirange) AS covered,
         coalesce(u.days, '{}'::datemultirange) AS unfinished
  FROM core.account a
  LEFT JOIN covered c ON c.account_id = a.id
  LEFT JOIN unfinished u ON u.account_id = a.id
  LEFT JOIN core.reconciliation r ON r.account_id = a.id
  WHERE a.feed = 'api' AND a.type <> 'pot' AND coalesce(r.method, '') <> 'balances_only'
),
api_uncovered AS (
  SELECT account_id, unfinished,
         CASE WHEN life_start IS NULL OR life_end IS NULL OR life_start >= life_end
              THEN '{}'::datemultirange
              ELSE datemultirange(daterange(life_start, life_end, '[)')) - covered
         END AS uncovered
  FROM api_life
),
latest_statement AS (
  SELECT a.id AS account_id,
         greatest((SELECT max(t.txn_date) FROM core.transaction t
                   WHERE t.account_id = a.id AND t.disappeared_at IS NULL),
                  (SELECT max(b.on_date) FROM core.balance_snapshot b WHERE b.account_id = a.id)) AS d
  FROM core.account a
  WHERE a.feed = 'file' AND a.expects_imports
),
gap_rows AS (
  SELECT u.account_id, lower(g) AS from_date,
         jsonb_build_object('from', lower(g), 'to', upper(g) - 1,
                            'reason', 'the Monzo window closed before this range was fetched') AS gap
  FROM api_uncovered u, unnest(u.uncovered * u.unfinished) AS g
  UNION ALL
  SELECT u.account_id, lower(g),
         jsonb_build_object('from', lower(g), 'to', upper(g) - 1,
                            'reason', 'no completed fetch covers this period')
  FROM api_uncovered u, unnest(u.uncovered - u.unfinished) AS g
  UNION ALL
  SELECT fg.account_id, fg.after_date,
         jsonb_build_object('from', fg.after_date, 'to', fg.before_date,
                            'reason', 'no file covers these days', 'diff_minor', fg.diff_minor)
  FROM core.file_gap fg
  UNION ALL
  SELECT ls.account_id, ls.d + 1,
         jsonb_build_object('from', ls.d + 1, 'to', (SELECT d FROM today), 'reason', 'import due')
  FROM latest_statement ls
  WHERE ls.d IS NULL OR (SELECT d FROM today) - ls.d > 35
),
gaps AS (
  SELECT account_id, jsonb_agg(gap ORDER BY from_date NULLS FIRST) AS gaps
  FROM gap_rows
  GROUP BY account_id
)
SELECT a.slug AS account_slug,
       a.feed,
       txn.earliest AS earliest_date,
       txn.latest AS latest_date,
       coalesce(txn.n, 0) AS transaction_count,
       CASE
         WHEN a.type = 'pot' OR r.method = 'balances_only' THEN 'balances_only'
         WHEN r.last_check_ok IS FALSE OR coalesce(r.break_count, 0) > 0 THEN 'mismatch'
         WHEN r.last_check_ok IS TRUE THEN 'reconciled'
         ELSE 'not_checked'
       END AS reconciliation_state,
       coalesce(r.reconciled_through, (r.last_pass_at AT TIME ZONE 'Europe/London')::date) AS reconciled_through,
       r.last_diff_minor,
       coalesce(g.gaps, '[]'::jsonb) AS gaps
FROM core.account a
LEFT JOIN txn ON txn.account_id = a.id
LEFT JOIN core.reconciliation r ON r.account_id = a.id
LEFT JOIN gaps g ON g.account_id = a.id;

-- sync_status: one row per source seen in consents, runs or accounts. last_import_at is a JSON
-- object from each file-fed account's slug to the time of its latest raw.import_file row (null
-- when it has none), and is null for a source with no file-fed accounts.
CREATE VIEW agent.sync_status AS
WITH sources AS (
  SELECT source FROM sync.consent
  UNION SELECT source FROM sync.run
  UNION SELECT source FROM core.account
),
cur AS (
  SELECT source, state, approved_at, expected_expiry_at, last_sync_ok_at
  FROM sync.consent
  WHERE is_current
),
last_run AS (
  SELECT DISTINCT ON (source) source, outcome
  FROM sync.run
  WHERE finished_at IS NOT NULL
  ORDER BY source, finished_at DESC, id DESC
),
imports AS (
  SELECT a.source,
         jsonb_object_agg(a.slug, (SELECT max(f.imported_at) FROM raw.import_file f
                                   WHERE f.account_slug = a.slug)) AS obj
  FROM core.account a
  WHERE a.feed = 'file'
  GROUP BY a.source
)
SELECT s.source,
       c.state AS consent_state,
       c.approved_at,
       c.expected_expiry_at,
       CASE WHEN c.expected_expiry_at IS NULL THEN NULL
            ELSE floor(extract(epoch FROM c.expected_expiry_at - now()) / 86400)::int
       END AS days_to_expiry,
       c.last_sync_ok_at,
       lr.outcome AS last_run_outcome,
       i.obj AS last_import_at
FROM sources s
LEFT JOIN cur c ON c.source = s.source
LEFT JOIN last_run lr ON lr.source = s.source
LEFT JOIN imports i ON i.source = s.source;
