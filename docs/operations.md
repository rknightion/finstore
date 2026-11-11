# Operating finstore

This is the operator's contract: what to deploy, what each command does, how the data is laid out,
and how to recover. `deploy/` holds a working example of everything named here.

## What runs

One container runs `finstore serve`: an HTTP server with a status page, a scheduler, the Monzo
worker and, when TrueLayer is configured, a worker for each TrueLayer connection. One container runs PostgreSQL 18. A host timer dumps the database to a file for your backup
tool. Nothing else is needed.

The service must not be reachable from the public internet. It expects something in front that
knows who the user is and passes their address in a request header. The example in `deploy/` uses a
Tailscale sidecar for that; a reverse proxy with single sign-on works too.

## Setup

1. Copy `deploy/docker-compose.yaml`, `deploy/ts/tailscale-serve.json` (into `ts/`),
   `deploy/finstore.env.example` (as `finstore.env`), `deploy/owner.env.example` (as `owner.env`)
   and `deploy/ts.env.example` (as `ts.env`). Set every `CHANGE-ME` value. The TrueLayer pair
   (`FINSTORE_TRUELAYER_CLIENT_ID` and `FINSTORE_TRUELAYER_CLIENT_SECRET`) ships empty and may stay
   empty: set both only to enable TrueLayer. Keep the env files at mode 600.
2. Register an OAuth client at <https://developers.monzo.com>: confidential, with the redirect URL
   `<FINSTORE_PUBLIC_URL>/callback/monzo`. Put its id and secret in `finstore.env`. For the
   optional TrueLayer connections, see [Connecting through TrueLayer](#connecting-through-truelayer).
3. Start the database, create the roles and the schemas, then start the service:

   ```sh
   docker compose up -d finstore-db
   docker compose run --rm finstore-admin bootstrap
   docker compose run --rm finstore-admin migrate
   docker compose up -d finstore finstore-ts
   ```

4. Open the status page from a device signed in to your tailnet, and connect Monzo.
5. Install the dump script and its timer (see "Backups").

### The front door: a Tailscale sidecar

The example gives the service its own tailnet node. `finstore-ts` runs Tailscale in userspace mode
with no capabilities, and `tailscale serve` terminates HTTPS for the node's MagicDNS name
(`TS_HOSTNAME` plus your tailnet's DNS suffix) and proxies to `finstore:8080`. Nothing else reaches the service: it publishes no port and shares
no network.

- **Who it is.** For a request from a user's device, `tailscale serve` adds `Tailscale-User-Login`
  and drops any copy the client sent. Set `FINSTORE_AUTH_HEADER=Tailscale-User-Login` and list your
  tailnet login in `FINSTORE_ALLOWED_EMAILS`. A tagged device gets no header, so it gets 404.
- **Who can connect.** Before minting the auth key, add the tag to `tagOwners` in your tailnet
  policy and grant only the users who should reach it, on port 443. Policy tests that deny your
  servers' tags are cheap insurance.
- **The auth key** is used once. After the first login the node's identity is in `data/ts-state`;
  losing that directory needs a new key.
- **Why not a proxy's IP allow-list.** On a host that is itself a Tailscale subnet router or exit
  node, tailscaled masquerades tailnet traffic forwarded into Docker, so a reverse proxy in a
  container sees every tailnet client as the Docker gateway and cannot tell them from the host.

### Or: a sign-in proxy

Any reverse proxy that signs users in also works. Two routes to the service's port 8080, both
restricted to your private network:

| Route | Sign-in | Why |
|---|---|---|
| Everything | Required. The proxy passes the signed-in address in `FINSTORE_AUTH_HEADER` | The pages check that address against `FINSTORE_ALLOWED_EMAILS` |
| Path prefix `/callback/` | None | Monzo's email link may open in a browser with no session, and a login redirect in the middle of the callback can lose the authorisation code. TrueLayer's `/callback/truelayer` is under the same prefix |

The callback acts only on a single-use value from a connect flow that a signed-in user started
within the last hour.

A signed-in user who is not on the allow-list gets 404, not 403, because many sign-in proxies turn
a 403 from the backend into a login redirect, which would loop.

The proxy must strip any copy of the auth header that a client sends. Anything that can reach port
8080 directly can claim to be anyone, so do not publish that port beyond the proxy. The pages show
states, dates and counts only: no transaction, balance or payee.

## Connecting Monzo

Press "Connect Monzo" on the status page. Monzo emails a link; following it returns to the service,
and Monzo then asks for approval in its app.

Three things about Monzo shape this flow:

- **The full history is only available for about five minutes after the email link is followed.**
  The service fetches everything it can in the first 240 seconds after that moment. Approve in the
  app straight away. Ranges it could not finish keep their place, and the next connect continues
  them first.
- **Following the email link replaces the previous connection at once**, before you approve
  anything. If you start a reconnect, finish it. An attempt abandoned before the email link changes
  nothing.
- **A connection ends about 90 days after approval.** The status page and the
  `finstore.consent.expiry.time` metric show when. Reconnecting takes a minute and also refetches
  the full history.

After the first minutes, Monzo serves only the last 90 days. The service syncs the last 7 days every
hour and the last 89 days every night, so a host that was down for less than 90 days catches up by
itself.

## Connecting through TrueLayer

TrueLayer's Data API adds two connections, each its own source with its own consent:

- **`truelayer:ob-lloyds`**: the Lloyds accounts, their transactions, balances, standing orders
  and direct debits. Its accounts are `lloyds-joint`, `lloyds-current` and so on, and are checked
  by running balance (see [How the data is checked](#how-the-data-is-checked)).
- **`truelayer:ob-monzo`**: standing orders and direct debits for the Monzo accounts finstore
  already holds from Monzo's own API. It creates no account: each mandate attaches to the Monzo
  account with the same [bank reference](#bank-references), so connect Monzo first. Pots are
  skipped.

To turn them on, create a TrueLayer Data API client with the redirect URI
`<FINSTORE_PUBLIC_URL>/callback/truelayer` and set `FINSTORE_TRUELAYER_CLIENT_ID` and
`FINSTORE_TRUELAYER_CLIENT_SECRET` (both or neither: one without the other stops the service at
start). Leave `FINSTORE_TRUELAYER_AUTH_URL` and `FINSTORE_TRUELAYER_API_URL` at their defaults
unless you use TrueLayer's sandbox. With neither credential set, the service runs Monzo only and
the status page shows no TrueLayer section.

**Connecting.** Each connection has its own section on the status page. Press "Connect Lloyds
(TrueLayer)" or "Connect Monzo (TrueLayer)", approve at the bank, and the browser comes back to
`/callback/truelayer`. There is no approval wait: the consent is active as soon as you are back, and
the connect-time fetch runs at once (for Lloyds, every transaction since 2024-08-01, then the
balance). If the Lloyds history call fails, the connect run ends `partial` with `history_failed`;
reconnect to fetch it again. Declining at the bank changes nothing.

**Syncing.** Each connection syncs at 06:20 and 18:20 in `FINSTORE_TIMEZONE`, each up to five
minutes late at random: accounts and mandates, and for Lloyds the last 35 days of transactions and
the balance. TrueLayer allows four calls a day to each endpoint for each account. The service counts
its own calls in `raw.api_response` and skips a fifth (the run ends `partial` with `rate_limited`);
a connect day uses three. A `raw.api_response` row with status 0 and no body records a call that got
no whole response (a transport failure): it may still have reached the bank, so it counts toward the
daily limit.

**Reconnecting.** finstore holds three consents, each on its own 90-day clock: Monzo's own and the
two TrueLayer ones. The status page lists them together, so renew all three in one sitting. A
reconnect is a fresh connect: the old consent becomes `replaced` and the new one active, and Lloyds
refetches its full history. A consent ends `lost` when TrueLayer refuses a token refresh or a data
call; reconnect from the status page.

## Accounts that no API reaches

An account fed by statement files is created once:

```sh
docker exec finstore finstore account add --slug bank-joint --type joint --currency GBP \
  --name "Joint account" --expects-imports
```

`--type` is `current`, `joint`, `savings` or `mortgage`. `--expects-imports` makes the account show
"import due" in the coverage view when its newest statement is more than 35 days old.

Then each statement is imported from standard input:

```sh
docker exec -i finstore finstore import --account bank-joint --format halifax-pdf < statement.pdf
```

| Exit code | Meaning |
|---|---|
| 0 | Imported, or "already imported" when this exact file was imported into this account before |
| 2 | Rejected, with the reason. The file is kept in `raw.import_file`; no rows were stored. Giving a rejected file again is rejected again |
| 3 | The account is unknown, or is fed by an API |

Every command exits 1 for any other failure, such as a database it cannot reach, and 64 for a
command line it cannot understand.

The first PDF statement imported into an account ties the account to that statement's sort code
and account number, its bank reference. A later statement with different numbers is rejected, so a
statement cannot land in the wrong account by a slip of `--account`.

A file rejected because the reader misread it is not lost: the bytes are stored. Once the reader
is corrected, `finstore renormalise` reads every stored file again.

Formats:

- **`halifax-pdf`**: the PDF statement the Halifax app produces. The reader checks that the rows
  add up to the statement's totals, that each row's running balance follows from the previous one,
  that the last balance equals the statement's closing balance, that the rows are in date order,
  and that every row is dated inside the statement's printed period. A file that fails any check
  is rejected whole.
- **`manual-csv`**: for anything typed up by hand, such as a mortgage. UTF-8, with the header
  `date,type,amount,balance_after,description`. `date` is `YYYY-MM-DD`; `amount` and
  `balance_after` are decimal pounds; `amount` is signed, and each `balance_after` must equal the
  previous one plus `amount`. Rows are oldest first, and a file whose dates go backwards is
  rejected.

Importing a statement that overlaps one already imported is safe: rows are matched and stored once.
That is how a part-month statement followed by the full month works. Statements can be imported in
any order, oldest last if that is how you find them.

If two statements do not join up, both are still imported, and the gap between them is recorded and
shown in the coverage view until the missing statement arrives. For PDF statements a gap is a break
in the printed periods or in the balance. For `manual-csv` only the balance can show it, so a
skipped stretch in which money in equalled money out goes unnoticed.

### Ending an account

When another source takes over an account from a date (for example a bank API that continues an
account until now fed by statements), end the old account at the last day it should hold:

```sh
docker exec finstore finstore account end --slug bank-joint --last-date 2024-08-21
```

The entry is kept in `raw` like any other operator entry, so a rebuild replays it. The ended
account's transactions and balances after that date stay stored, but `agent.transactions`,
`agent.balances` and `agent.coverage` leave them out, so the days the new account holds are never
shown twice. Its statements that start after the date raise no continuity gap, it is never "import
due", and `agent.accounts.ended_on` shows the date. Statements from before the date can still be
imported into it.

Running the command again with another date replaces the date. `--clear` in place of `--last-date`
removes the end. Record the end before the new source first fetches the account, so the overlap
never shows.

### Bank references

An account's bank reference is its sort code and account number, digits only, joined by a space
(`112233 12345678`). A statement import sets it as above; Monzo's own account list sets it for the
accounts Monzo gives numbers for (current, joint and business accounts). `agent.accounts.bank_ref`
shows it.

Two accounts from different sources may share a bank reference only while one of them has ended.
Any other shared reference means one bank account is held twice: both accounts are reported as a
mismatch in `agent.coverage`, with a gap naming the other account, until one of them is ended.

## Commands

| Command | Database role | Behaviour |
|---|---|---|
| `finstore bootstrap` | superuser | Creates the roles `finstore_owner`, `finstore_app` and `finstore_agent`, sets the two passwords, creates the schemas. Safe to run again |
| `finstore migrate` | owner | Applies the migrations and the grants. Says so when a `renormalise` is required |
| `finstore serve` | app | The HTTP server, the scheduler, the Monzo worker and, when TrueLayer is configured, the two TrueLayer workers |
| `finstore account add`, `finstore account list` | app | Create and list file-fed accounts |
| `finstore account end --slug S --last-date YYYY-MM-DD`, `finstore account end --slug S --clear` | app | Ends an account at a date, or removes its end. See [Ending an account](#ending-an-account) |
| `finstore account accept-difference --slug S --reason TEXT` | app | For an account an API feeds: records that its current balance difference is known and accepted, so it stops counting as a mismatch. A different difference later counts again. A statement-fed account is refused: its gaps close when the missing statement is imported |
| `finstore import` | app | Imports one statement from standard input |
| `finstore load-capture --dir PATH [--dir PATH]...` | app | Loads API responses captured before the service existed. Loading twice loads once, and a capture older than what is already held never replaces newer data |
| `finstore renormalise [--verify]` | owner | Rebuilds `core` from `raw`. With `--verify` it swaps nothing: exit 0 when the rebuilt copy equals the live one, 4 when it differs, listing table, key, column and kind of difference. Either way it first normalises any stored rows the live record has not caught up with |
| `finstore coverage [--json]` | app | Prints the coverage view |
| `finstore healthcheck` | none | Exit 0 when the local server answers `/healthz` with 200, 1 otherwise |
| `finstore version` | none | Version, commit and build date |

Every writer takes one database lock for the whole of "store the raw rows, then normalise them", so
commands are safe to run while the service is running: they wait their turn.

## Configuration

Environment variables only.

| Variable | Needed by | Default | Meaning |
|---|---|---|---|
| `FINSTORE_DATABASE_URL` | `serve`, `import`, `account`, `load-capture`, `coverage` | none | Connection string for `finstore_app` |
| `FINSTORE_OWNER_DATABASE_URL` | `migrate`, `renormalise` | none | Connection string for `finstore_owner` |
| `FINSTORE_SUPERUSER_DATABASE_URL` | `bootstrap` | none | Connection string for the Postgres superuser |
| `FINSTORE_APP_PASSWORD`, `FINSTORE_OWNER_PASSWORD` | `bootstrap` | none | Passwords `bootstrap` sets on the two login roles |
| `FINSTORE_LISTEN_ADDR` | `serve`, `healthcheck` | `:8080` | Address the HTTP server binds |
| `FINSTORE_PUBLIC_URL` | `serve` | none | Base URL users reach the service at |
| `FINSTORE_ALLOWED_EMAILS` | `serve` | none | Comma-separated addresses allowed to use the pages |
| `FINSTORE_AUTH_HEADER` | `serve` | `X-Auth-Request-Email` | Header carrying the signed-in address (`Tailscale-User-Login` with the sidecar) |
| `FINSTORE_MONZO_CLIENT_ID`, `FINSTORE_MONZO_CLIENT_SECRET` | `serve` | none | The OAuth client |
| `FINSTORE_MONZO_API_URL` | `serve` | `https://api.monzo.com` | |
| `FINSTORE_MONZO_AUTH_URL` | `serve` | `https://auth.monzo.com/` | |
| `FINSTORE_MONZO_SYNC_INTERVAL` | `serve` | `1h` | Period of the 7-day sync |
| `FINSTORE_MONZO_NIGHTLY_AT` | `serve` | `03:20` | Local time of the 89-day sync |
| `FINSTORE_MONZO_BACKFILL_CONCURRENCY` | `serve` | `6` | Ranges fetched at once during the full-history fetch |
| `FINSTORE_MONZO_WINDOW` | `serve` | `240s` | How long after the email link the full-history fetch runs |
| `FINSTORE_TRUELAYER_CLIENT_ID`, `FINSTORE_TRUELAYER_CLIENT_SECRET` | `serve` | none | The TrueLayer Data API client. Both or neither: with neither, TrueLayer is off. See [Connecting through TrueLayer](#connecting-through-truelayer) |
| `FINSTORE_TRUELAYER_AUTH_URL` | `serve` | `https://auth.truelayer.com` | TrueLayer's auth server: the auth link and the token call |
| `FINSTORE_TRUELAYER_API_URL` | `serve` | `https://api.truelayer.com` | TrueLayer's Data API |
| `FINSTORE_TIMEZONE` | all | `Europe/London` | Zone for a transaction's day and for the nightly time. Changing it after data is stored needs a `renormalise`, or days are mixed between the two zones |
| `FINSTORE_DUMP_STATUS_FILE` | `serve` | empty | The dump script's status file; the dump metric is off when empty |
| `FINSTORE_OTLP_ENDPOINT`, `FINSTORE_OTLP_INSTANCE_ID`, `FINSTORE_OTLP_TOKEN` | `serve` | empty | OTLP over HTTP with basic auth; export is off when the endpoint is empty |
| `FINSTORE_ENVIRONMENT` | `serve` | `dev` | Value of `deployment.environment.name` |
| `FINSTORE_LOG_LEVEL` | all | `info` | `debug`, `info`, `warn` or `error` |

## The data

Five schemas:

| Schema | Holds | Kept how |
|---|---|---|
| `raw` | Every API response and every imported file, byte for byte, plus what the operator entered | Append-only. Never updated, never deleted from |
| `core` | Accounts, transactions, standing orders and direct debits and every version of each, balances, the periods each completed fetch covered, the reconciliation state | Disposable: `renormalise` rebuilds it from `raw` |
| `sync` | Runs, the consent's history, connect attempts, the full-history fetch's progress | Durable |
| `secret` | The bank tokens | Durable, and left out of dumps |
| `agent` | Read-only views for whatever queries the data | Rebuilt with `core` |

`raw` is the record. Everything in `core` can be thrown away and rebuilt, which is how a parsing bug
is fixed after the fact: correct the code, run `renormalise`, and the stored history is reinterpreted
from the original bytes.

Amounts are integers in minor units (pence). A transaction that the bank later changes gets a new
version; the old one is kept. A transaction that the bank stops returning is marked as disappeared,
not deleted.

### Reading the data

Give a reader the role `finstore_agent`. It has no login and no password of its own: grant it to a
login role you create. It can read the `agent` schema and nothing else, so it cannot see raw
payloads, tokens or the sync tables.

```sql
CREATE ROLE reporting LOGIN PASSWORD '...' IN ROLE finstore_agent;
```

Start with `agent.coverage`: one row per account with the earliest and latest dates held, the
transaction count, whether the stored transactions agree with the bank's balance, and every known
gap with its reason. A reader should check it before trusting a total.

Each account and transaction also carries the source's own record, for anything the named columns
leave out:

- `agent.accounts.detail` is the latest account object Monzo returned for the account (from
  `/accounts`) or the pot (from `/pots`), as JSON. It is null for a file-fed account. It is read
  from the newest successful response for each request, newest by when it was fetched, so a
  capture loaded later with older responses does not replace it. A stored body that cannot be read
  as JSON is skipped. Monzo keeps closed accounts and deleted pots in those lists. The account
  object includes Monzo's identifiers for the account (account number and sort code), its owners'
  names and Monzo user ids, so anyone given `finstore_agent` can read them.
- `agent.transactions.detail` is the latest version of the transaction's source record: the whole
  Monzo transaction object, or the parsed statement row for an imported one. Earlier versions are
  in `agent.transaction_history`.
- `agent.transactions.pot_slug` names the pot a Monzo pot transfer moved money to or from (the pot
  id in the transaction's metadata, when it is a pot finstore holds), whatever the transaction's
  status. It is null for every other transaction.

These columns come after the others, so a query that reads columns by position is unaffected.
`agent.accounts` also ends with `bank_ref` (see [Bank references](#bank-references)) and `ended_on`
(see [Ending an account](#ending-an-account)).

Standing orders and direct debits ("mandates") have a view each, with one row per mandate per
account:

- `agent.standing_orders`: the account, the source that reported it, its bank reference and key,
  the payee, reference, frequency and status, the first, next and final payment dates and amounts
  as the bank gives them, when it was first and last seen, `disappeared_at`, and the full source
  record in `detail`.
- `agent.direct_debits`: the same for a direct debit, with its name, status and the date and
  amount of the previous payment.

A mandate is keyed by the bank's mandate id where it gives one, otherwise by its name (a direct
debit) or its payee, reference and frequency (a standing order), with `#2`, `#3` added to the key of
identical ones (a `#` or `\` in the bank's own text is stored as `\#` or `\\`, so the two never clash). Editing a standing order's reference therefore shows as one mandate ending and
another starting. A mandate the bank's latest complete list leaves out gets `disappeared_at` (it was
cancelled), cleared if it comes back. A mandate whose bank reference no account holds yet has a null
`account_slug` until a later fetch or a `renormalise` attaches it.

Two views follow the money into and out of pots:

- `agent.pot_transactions` has one row per Monzo transaction that moved money into or out of a pot
  finstore holds: the pot, the account the transfer was made on (which can be a joint account, not
  the pot's parent), the date and time, `amount_minor` from the pot's side (positive into the pot),
  the currency, the description, Monzo's `trigger` (such as `coin_jar` for round-ups, `user` or
  `scheduled_payment`; null when Monzo gives none), the status and the full record in `detail`.
  Declined transfers are left out, and so are transactions Monzo stopped returning. Pending
  transfers are kept, because they already move the pot's balance. Some transfers carry the
  trigger `interest_payout`; they are listed and counted like any other transfer. What Monzo does
  not list is the interest it credits inside a savings pot.
- `agent.pot_reconciliation` has one row per pot: its parent account, the pot type Monzo reports
  (`default` is a plain pot), whether it is closed, its latest balance and when that was read, the
  net and count of its transfers as at that reading (the transfers in `agent.pot_transactions`
  made at or before it, so one made since is not counted until the next reading; all of them for a
  pot with no reading), the difference (balance minus transfers) and a `reason`:

  | `reason` | Means |
  |---|---|
  | `matches` | The transfers sum to the balance |
  | `interest_not_listed` | A savings pot whose balance is above its transfers. Monzo pays the interest inside the pot and does not list it. Only the types `instant_access` and `flexible_savings` count as savings pots; a pot of any other type falls to `unexplained` |
  | `unexplained` | Anything else, including a pot with no balance reading (its balance and difference are null) |

### How the data is checked

- **Monzo accounts**: the balance Monzo reports must equal the sum of the stored transactions that
  were not declined. The check runs on every sync, using two balance readings around the fetch so
  that a transaction arriving mid-fetch does not raise a false alarm.
- **File-fed accounts**: each statement must pass its own checks to be imported at all, and each
  must start from the balance the previous one ended on.
- **Accounts with a running balance on every row** (`running_balance`): each day's rows must add
  up, from the previous day's closing balance, to one of that day's running balances. The rows of a
  day carry no time, so only the day's total is checked. A day that does not add up is a mismatch,
  and `agent.coverage.failed_on` names the earliest. The newest closing balance is also compared
  with the bank's balance; because the two are read moments apart, a difference counts only when
  the next reading shows the same one.
- **Bank references**: two accounts from different sources with the same bank reference, neither
  ended, are both a mismatch.
- **Pots** have balances only; Monzo's API does not list a pot's own transactions. The transfers
  in and out of a pot are listed on the account they were made on, and `agent.pot_reconciliation`
  compares them with the pot's balance. That comparison is information, not a check: it raises no
  alarm.

## Backups

`deploy/finstore-dump.sh` writes one uncompressed custom-format dump, checks it can be listed, and
only then replaces the previous one. Point your backup tool at the dump directory and leave the
Postgres data directory out of it: a file-level copy of a running data directory is not a backup.

```sh
install -D -m 755 deploy/finstore-dump.sh /opt/backup-dumps/finstore-dump.sh
install -m 644 deploy/backup-dump-finstore.service deploy/backup-dump-finstore.timer /etc/systemd/system/
install -m 644 deploy/finstore-dump.default.example /etc/default/finstore-dump   # then edit it
systemctl daemon-reload && systemctl enable --now backup-dump-finstore.timer
```

Run the script by hand after a first full-history fetch, after an import, and before `migrate` or
`renormalise`.

The dump leaves out the bank tokens. A dumped token is spent within hours, and a second copy of a
live one is a hazard: used anywhere, it invalidates the one in service.

### Restore

```sh
docker compose up -d finstore-db                       # an empty data directory
docker compose run --rm finstore-admin bootstrap       # roles must exist before the restore
docker exec -i finstore-db pg_restore -U postgres -d finstore --clean --if-exists --exit-on-error < finstore.dump
docker compose run --rm finstore-admin migrate
docker compose up -d finstore
```

Then reconnect Monzo from the status page. The restored database has no tokens, so the service
marks the connection as lost as soon as it starts; reconnecting brings it back. Nothing else is
lost.

## Updating

Before running a new image against real data:

```sh
docker compose run --rm finstore-admin renormalise --verify
```

with the new image. It rebuilds `core` from the real `raw` rows with the new code and reports every
difference without changing anything. No difference means the new code reads your history exactly
as the old code did. A difference is either a fix you were expecting or a regression: read it before
you go on. Then `migrate`, and `renormalise` if `migrate` asks for it.

Stop the service (`docker compose stop finstore`) before either `renormalise`, so that nothing is
written while the record is rebuilt.

When the new image has another normaliser revision (`migrate` says a `renormalise` is required), the
live record was built by the old code and may have another shape. `renormalise --verify` then does not
bring it up to date first, and it cannot compare a reshaped table row by row: it reports the table
as different, and any row the old service had not yet read as present only in the rebuilt copy.
Before the swap, keep what you will want to compare afterwards:

```sh
docker compose run --rm finstore-admin coverage --json > coverage-before.json
docker exec finstore-db psql -U postgres -d finstore -c '\copy core.reconciliation TO STDOUT CSV HEADER' > reconciliation-before.csv
```

After `renormalise`, run both again and compare: transaction counts, dates and reconciliation states
should match apart from what the release notes say changed.

## Telemetry

Metrics carry counts, timestamps and states only. No amount, payee or account number is ever a
metric value, a label or a log field; the log handler drops any field that is not on its list.

| Metric | Attributes | Meaning |
|---|---|---|
| `finstore.sync.last_success.time` | `source` | Unix time of the last successful scheduled sync |
| `finstore.sync.runs` (counter) | `source`, `kind`, `outcome` | Runs finished |
| `finstore.bank.requests` (counter) | `source`, `endpoint`, `status_class` | Bank requests made |
| `finstore.consent.state` | `source`, `state` | 1 for the current state, 0 for the others |
| `finstore.consent.expiry.time` | `source` | Unix time the consent is expected to end |
| `finstore.reconciliation.mismatch` | `account` | 1 while the account's last check failed or it has a statement gap |
| `finstore.import.last.time` | `account` | Unix time of the newest statement date imported |
| `finstore.dump.last_success.time` | none | Unix time from the dump status file |
| `finstore.up` | none | 1, sent every minute |

Alerts worth having: no successful sync for 3 hours (13 hours for a `truelayer:` source, which syncs
twice a day); consent ending within 14 days; consent `lost`
or `expired`; a reconciliation mismatch lasting 90 minutes; newest import older than 35 days; dump
older than 14 hours; `finstore.up` absent for an hour.

## When something goes wrong

| Symptom | Cause | What to do |
|---|---|---|
| Consent `lost` | Monzo ended the consent, a token refresh was rejected, or nobody approved within a day; for a TrueLayer source, TrueLayer refused a refresh or a data call, or the token was missing at start (`token_missing`, after a database restore) | Reconnect from the status page |
| A TrueLayer run ends `partial` with `rate_limited` | A fifth call of the day to one endpoint for one account was skipped, or TrueLayer answered 429 | Nothing: the next day's syncs catch up. Do not reconnect and sync by hand several times in a day |
| A Lloyds connect run ends `partial` with `history_failed` | The full-history call failed, or the service stopped before it ran | Reconnect from the status page |
| Coverage shows "the Monzo window closed before this range was fetched" | The five minutes ran out | Reconnect: unfinished ranges are fetched first |
| Coverage shows "no completed fetch covers this period" | The service was down or disconnected for more than 90 days | Reconnect: the full-history fetch fills it |
| A Monzo account shows `mismatch` | The stored transactions do not add up to the bank's balance | A sync usually clears it. If it lasts, compare `raw` with `core` and run `renormalise --verify`. A closed account with a difference that will never change can be accepted with `account accept-difference` |
| An import exits 2 | The statement failed a check, belongs to another bank account, was given with the wrong `--format`, or its layout changed | The message says which. The file is kept in `raw.import_file` and nothing was stored. A wrong format: give it again with the right one. A changed layout: the reader needs updating, then `renormalise` |
| Coverage shows "no file covers these days" | A statement was skipped | Import the missing statement |
