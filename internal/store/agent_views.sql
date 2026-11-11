-- The agent views. Applied by migrate and after every core swap, as finstore_owner, after every
-- existing view in agent has been dropped. They read core, sync and raw with the owner's rights.
-- Days are Europe/London days: an instant is converted with AT TIME ZONE 'Europe/London'.

-- accounts: detail is the latest raw Monzo object for the account (from /accounts) or the pot
-- (from /pots), and null for a file-fed account. For each endpoint and parameter set only the
-- newest readable successful (200) response is read, newest by fetched_at then seq (a capture
-- loaded later but fetched earlier does not win), so the parse stays a handful of bodies however
-- many responses raw holds. A body that is not UTF-8 JSON holding the list is skipped (load-capture
-- stores bodies unchecked), so it can neither shadow a readable response nor fail the read. Monzo
-- lists closed accounts and deleted pots, so that response names every account it ever did. An id
-- listed under more than one parameter set takes the newest response by the same order.
-- New columns are appended so a reader selecting by position is unaffected.
CREATE VIEW agent.accounts AS
WITH request AS (
  SELECT DISTINCT endpoint, params,
         CASE endpoint WHEN '/accounts' THEN 'accounts' ELSE 'pots' END AS list_key
  FROM raw.api_response
  WHERE source = 'monzo' AND endpoint IN ('/accounts', '/pots') AND status = 200
),
-- The newest readable response of each request: newest first, the first whose body is JSON with
-- the list in it. OFFSET 0 keeps the sort below the filter, so LIMIT 1 stops at the first readable
-- body instead of parsing every one.
latest_doc AS (
  SELECT q.endpoint, d.seq, d.fetched_at, raw.json_body(d.body) -> q.list_key AS list
  FROM request q
  CROSS JOIN LATERAL (
    SELECT s.seq, s.fetched_at, s.body
    FROM (SELECT r.seq, r.fetched_at, r.body
          FROM raw.api_response r
          WHERE r.source = 'monzo' AND r.endpoint = q.endpoint AND r.params = q.params AND r.status = 200
          ORDER BY r.fetched_at DESC, r.seq DESC
          OFFSET 0) s
    WHERE jsonb_typeof(raw.json_body(s.body) -> q.list_key) = 'array'
    LIMIT 1
  ) d
),
monzo_object AS (
  SELECT DISTINCT ON (d.endpoint, o.value ->> 'id') d.endpoint, o.value ->> 'id' AS id, o.value AS obj
  FROM latest_doc d
  CROSS JOIN LATERAL jsonb_array_elements(CASE WHEN jsonb_typeof(d.list) = 'array' THEN d.list ELSE '[]'::jsonb END) o
  ORDER BY d.endpoint, o.value ->> 'id', d.fetched_at DESC, d.seq DESC
)
SELECT a.slug, a.source, a.type, a.feed, a.currency, a.name,
       p.slug AS parent_slug, a.opened_on, a.closed,
       m.obj AS detail, a.bank_ref, a.ended_on
FROM core.account a
LEFT JOIN core.account p ON p.id = a.parent_id
LEFT JOIN monzo_object m
  ON a.source = 'monzo' AND a.feed = 'api' AND m.id = a.source_key
 AND m.endpoint = CASE WHEN a.type = 'pot' THEN '/pots' ELSE '/accounts' END;

-- transactions: detail is the latest version's full source record (the Monzo transaction object,
-- or a statement import's parsed row). pot_slug names the pot a Monzo pot transfer moved money to
-- or from (the transaction's metadata.pot_id is a pot finstore holds), whatever its status, and is
-- null for every other transaction. An ended account's transactions dated after its ended_on are
-- left out: the account that continues it holds those days.
CREATE VIEW agent.transactions AS
SELECT t.id, a.slug AS account_slug, t.txn_date, t.created_at, t.settled_at,
       t.amount_minor, t.currency, t.local_amount_minor, t.local_currency, t.status,
       t.description, t.counterparty, t.merchant_name, t.category, t.scheme, t.notes,
       t.version_count, t.first_seen_at, t.last_seen_at,
       v.content AS detail, pot.slug AS pot_slug
FROM core.transaction t
JOIN core.account a ON a.id = t.account_id
LEFT JOIN core.transaction_version v ON v.transaction_id = t.id AND v.version_no = t.version_count
LEFT JOIN core.account pot
  ON t.source = 'monzo' AND pot.source = 'monzo' AND pot.type = 'pot'
 AND pot.source_key = v.content -> 'metadata' ->> 'pot_id'
WHERE t.disappeared_at IS NULL AND (a.ended_on IS NULL OR t.txn_date <= a.ended_on);

-- pot_transactions: one row per Monzo transaction that moved money into or out of a pot finstore
-- holds, matched on the pot id in its metadata (not on the pot's parent: a joint account can pay
-- into a pot). Declined and disappeared transactions are left out; pending ones are kept, since
-- they already move the pot's balance. amount_minor is from the pot's side: positive into the
-- pot, so the negation of the amount on the account the transfer was made on.
CREATE VIEW agent.pot_transactions AS
SELECT t.id AS transaction_id, pot.slug AS pot_slug, a.slug AS account_slug, t.txn_date, t.created_at,
       -t.amount_minor AS amount_minor, t.currency, t.description,
       v.content -> 'metadata' ->> 'trigger' AS trigger, t.status, v.content AS detail
FROM core.transaction t
JOIN core.account a ON a.id = t.account_id
JOIN core.transaction_version v ON v.transaction_id = t.id AND v.version_no = t.version_count
JOIN core.account pot
  ON pot.source = 'monzo' AND pot.type = 'pot' AND pot.source_key = v.content -> 'metadata' ->> 'pot_id'
WHERE t.source = 'monzo' AND t.disappeared_at IS NULL AND t.status <> 'declined';

-- pot_reconciliation: one row per pot. balance is its latest balance snapshot; transfers_net and
-- transfer_count cover its agent.pot_transactions rows created at or before that reading (all of
-- them when it has none); difference is balance minus transfers_net; pot_type is the type in the
-- latest raw /pots object (agent.accounts detail). reason is one of:
--   matches              the transfers sum to the balance
--   interest_not_listed  a savings pot whose balance exceeds its transfers: Monzo pays interest
--                        inside the pot and never lists it. The savings types are exactly
--                        instant_access and flexible_savings; any other type falls to unexplained
--   unexplained          anything else, including a pot with no balance snapshot (balance null)
CREATE VIEW agent.pot_reconciliation AS
WITH latest_balance AS (
  SELECT DISTINCT ON (b.account_id) b.account_id, b.balance_minor, b.at
  FROM core.balance_snapshot b
  JOIN core.account p ON p.id = b.account_id AND p.type = 'pot'
  ORDER BY b.account_id, b.at DESC NULLS LAST, b.raw_seq DESC, b.ordinal DESC
),
-- Transfers as at the balance reading: one made after it is not yet in the balance. A pot with no
-- balance reading counts all of its transfers.
transfers AS (
  SELECT pt.pot_slug, sum(pt.amount_minor)::bigint AS net, count(*) AS n
  FROM agent.pot_transactions pt
  JOIN core.account p ON p.slug = pt.pot_slug
  LEFT JOIN latest_balance b ON b.account_id = p.id
  WHERE b.at IS NULL OR pt.created_at IS NULL OR pt.created_at <= b.at
  GROUP BY pt.pot_slug
),
pots AS (
  SELECT p.slug AS pot_slug, par.slug AS parent_slug, ac.detail ->> 'type' AS pot_type, p.closed,
         b.balance_minor, b.at AS balance_at,
         coalesce(tr.net, 0) AS transfers_net_minor, coalesce(tr.n, 0) AS transfer_count
  FROM core.account p
  LEFT JOIN core.account par ON par.id = p.parent_id
  LEFT JOIN agent.accounts ac ON ac.slug = p.slug
  LEFT JOIN latest_balance b ON b.account_id = p.id
  LEFT JOIN transfers tr ON tr.pot_slug = p.slug
  WHERE p.type = 'pot'
)
SELECT pot_slug, parent_slug, pot_type, closed, balance_minor, balance_at,
       transfers_net_minor, transfer_count,
       balance_minor - transfers_net_minor AS difference_minor,
       CASE
         WHEN balance_minor IS NULL THEN 'unexplained'
         WHEN balance_minor = transfers_net_minor THEN 'matches'
         WHEN balance_minor > transfers_net_minor AND pot_type IN ('instant_access', 'flexible_savings')
           THEN 'interest_not_listed'
         ELSE 'unexplained'
       END AS reason
FROM pots;

CREATE VIEW agent.transaction_history AS
SELECT v.transaction_id, v.version_no, v.seen_at, v.content
FROM core.transaction_version v;

-- balances: an ended account's readings after its ended_on (by on_date, else the day of at) are
-- left out.
CREATE VIEW agent.balances AS
SELECT a.slug AS account_slug, b.at, b.on_date, b.balance_minor, b.total_balance_minor, b.currency
FROM core.balance_snapshot b
JOIN core.account a ON a.id = b.account_id
WHERE a.ended_on IS NULL
   OR coalesce(b.on_date, (b.at AT TIME ZONE 'Europe/London')::date) <= a.ended_on;

-- coverage: one row per account. A gap is a run of days (from and to inclusive) that nothing
-- covers. For an API account the days are its lifetime minus the days touched by the periods of
-- its completed fetches (core.fetch_period, the instants each completed fetch proved complete)
-- and of its completed backfill windows; the part of that inside an unfinished backfill window is
-- reported as the window closing. The lifetime runs from opened_on (else the first covered day) to
-- yesterday for an open account, or to the last covered day for a closed one, and to its
-- ended_on at the latest. Today is never a gap, so the time between the last hourly fetch and now
-- is not reported. Pots and balances-only accounts have no transaction gaps. For a file account
-- the gaps are core.file_gap (from is the last date of the earlier file, to the first date of the
-- later one) plus "import due" for an account that expects imports, whatever its type, has not
-- ended, and whose latest statement date (its newest transaction date or balance snapshot date)
-- is more than 35 days old. An ended account counts only its rows and readings up to ended_on.
-- An account that shares its bank reference with an account of another source, neither ended, is
-- a mismatch with a gap naming the other account. failed_on is a running_balance account's
-- earliest day that does not add up.
CREATE VIEW agent.coverage AS
WITH today AS (
  SELECT (now() AT TIME ZONE 'Europe/London')::date AS d
),
txn AS (
  SELECT t.account_id, min(t.txn_date) AS earliest, max(t.txn_date) AS latest, count(*) AS n
  FROM core.transaction t
  JOIN core.account a ON a.id = t.account_id
  WHERE t.disappeared_at IS NULL AND (a.ended_on IS NULL OR t.txn_date <= a.ended_on)
  GROUP BY t.account_id
),
fetch_days AS (
  SELECT p.account_id,
         (p.period_start AT TIME ZONE 'Europe/London')::date AS first_day,
         ((p.period_end - interval '1 microsecond') AT TIME ZONE 'Europe/London')::date AS last_day
  FROM core.fetch_period p
  WHERE p.period_end > p.period_start
  UNION ALL
  -- A completed full-history range is covered even when it took more than one fetch to finish
  -- (a resumed fetch completes only the part it listed). Its last slot may end in the future, so
  -- it covers no further than the moment it was completed.
  SELECT a.id,
         (w.period_start AT TIME ZONE 'Europe/London')::date,
         ((least(w.period_end, w.completed_at) - interval '1 microsecond') AT TIME ZONE 'Europe/London')::date
  FROM sync.backfill_window w
  JOIN core.account a ON a.feed = 'api' AND a.source_key = w.account_key
  WHERE w.completed_at IS NOT NULL AND least(w.period_end, w.completed_at) > w.period_start
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
         least(CASE WHEN a.closed THEN c.last_day + 1 ELSE (SELECT d FROM today) END, a.ended_on + 1) AS life_end,
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
-- The date the newest statement of every file account ends: its newest transaction date or
-- balance snapshot date, up to its ended_on. latest_statement is the subset the import-due rule
-- applies to.
file_latest AS (
  SELECT a.id AS account_id, a.expects_imports, a.ended_on,
         greatest((SELECT max(t.txn_date) FROM core.transaction t
                   WHERE t.account_id = a.id AND t.disappeared_at IS NULL
                     AND (a.ended_on IS NULL OR t.txn_date <= a.ended_on)),
                  (SELECT max(b.on_date) FROM core.balance_snapshot b
                   WHERE b.account_id = a.id AND (a.ended_on IS NULL OR b.on_date <= a.ended_on))) AS d
  FROM core.account a
  WHERE a.feed = 'file'
),
latest_statement AS (
  SELECT account_id, d FROM file_latest WHERE expects_imports AND ended_on IS NULL
),
shared AS (
  SELECT DISTINCT ON (a.id) a.id AS account_id, b.slug AS other_slug
  FROM core.account a
  JOIN core.account b ON b.bank_ref = a.bank_ref AND b.source <> a.source AND b.ended_on IS NULL
  WHERE a.bank_ref IS NOT NULL AND a.ended_on IS NULL
  ORDER BY a.id, b.slug
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
  UNION ALL
  SELECT s.account_id, NULL::date,
         jsonb_build_object('from', NULL, 'to', (SELECT d FROM today),
                            'reason', 'bank reference shared with ' || s.other_slug)
  FROM shared s
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
         WHEN sh.account_id IS NOT NULL OR r.known_exception = 'shared_bank_ref' THEN 'mismatch'
         WHEN r.last_check_ok IS FALSE OR coalesce(r.break_count, 0) > 0 THEN 'mismatch'
         WHEN r.last_check_ok IS TRUE THEN 'reconciled'
         ELSE 'not_checked'
       END AS reconciliation_state,
       coalesce(r.reconciled_through, (r.last_pass_at AT TIME ZONE 'Europe/London')::date) AS reconciled_through,
       r.last_diff_minor,
       coalesce(g.gaps, '[]'::jsonb) AS gaps,
       fl.d AS latest_statement_date,
       r.failed_on
FROM core.account a
LEFT JOIN txn ON txn.account_id = a.id
LEFT JOIN core.reconciliation r ON r.account_id = a.id
LEFT JOIN gaps g ON g.account_id = a.id
LEFT JOIN file_latest fl ON fl.account_id = a.id
LEFT JOIN shared sh ON sh.account_id = a.id;

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

-- standing_orders and direct_debits: one row per mandate per account, as the bank last listed it.
-- account_slug is null while no account holds the mandate's bank reference (an unresolved
-- mandate). disappeared_at is set when a complete list of that kind for that account left it out
-- (a cancelled mandate), and cleared if it comes back. Dates and amounts are as the bank gives
-- them; detail is the latest version's full source record.
CREATE VIEW agent.standing_orders AS
SELECT s.id, a.slug AS account_slug, s.source, s.bank_ref, s.mandate_key,
       s.payee, s.reference, s.frequency, s.status,
       s.first_date, s.first_amount_minor, s.next_date, s.next_amount_minor,
       s.final_date, s.final_amount_minor, s.currency,
       s.version_count, s.first_seen_at, s.last_seen_at, s.disappeared_at,
       v.content AS detail
FROM core.standing_order s
LEFT JOIN core.account a ON a.id = s.account_id
LEFT JOIN core.standing_order_version v ON v.standing_order_id = s.id AND v.version_no = s.version_count;

CREATE VIEW agent.direct_debits AS
SELECT d.id, a.slug AS account_slug, d.source, d.bank_ref, d.mandate_key,
       d.name, d.status, d.previous_date, d.previous_amount_minor, d.currency,
       d.version_count, d.first_seen_at, d.last_seen_at, d.disappeared_at,
       v.content AS detail
FROM core.direct_debit d
LEFT JOIN core.account a ON a.id = d.account_id
LEFT JOIN core.direct_debit_version v ON v.direct_debit_id = d.id AND v.version_no = d.version_count;
