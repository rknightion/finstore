# Operating finstore

This is the operator's contract: what to deploy, what each command does, how the data is laid out,
and how to recover. `deploy/` holds a working example of everything named here.

## What runs

One container runs `finstore serve`: an HTTP server with a status page, a scheduler, and the Monzo
worker. One container runs PostgreSQL 18. A host timer dumps the database to a file for your backup
tool. Nothing else is needed.

The service must not be reachable from the public internet. It expects a reverse proxy in front that
signs users in and passes the signed-in address in a request header.

## Setup

1. Copy `deploy/docker-compose.yaml`, `deploy/finstore.env.example` (as `finstore.env`) and
   `deploy/owner.env.example` (as `owner.env`). Set every `CHANGE-ME` value. Keep both env files at
   mode 600.
2. Register an OAuth client at <https://developers.monzo.com>: confidential, with the redirect URL
   `<FINSTORE_PUBLIC_URL>/callback/monzo`. Put its id and secret in `finstore.env`.
3. Start the database, create the roles and the schemas, then start the service:

   ```sh
   docker compose up -d finstore-db
   docker compose run --rm finstore-admin bootstrap
   docker compose run --rm finstore-admin migrate
   docker compose up -d finstore
   ```

4. Put the proxy in front (next section), open the status page, and connect Monzo.
5. Install the dump script and its timer (see "Backups").

### The proxy

Two routes to the service's port 8080, both restricted to your private network:

| Route | Sign-in | Why |
|---|---|---|
| Everything | Required. The proxy passes the signed-in address in `FINSTORE_AUTH_HEADER` | The pages check that address against `FINSTORE_ALLOWED_EMAILS` |
| Path prefix `/callback/` | None | Monzo's email link may open in a browser with no session, and a login redirect in the middle of the callback can lose the authorisation code |

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
and account number. A later statement with different numbers is rejected, so a statement cannot
land in the wrong account by a slip of `--account`.

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

## Commands

| Command | Database role | Behaviour |
|---|---|---|
| `finstore bootstrap` | superuser | Creates the roles `finstore_owner`, `finstore_app` and `finstore_agent`, sets the two passwords, creates the schemas. Safe to run again |
| `finstore migrate` | owner | Applies the migrations and the grants. Says so when a `renormalise` is required |
| `finstore serve` | app | The HTTP server, the scheduler and the Monzo worker |
| `finstore account add`, `finstore account list` | app | Create and list file-fed accounts |
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
| `FINSTORE_AUTH_HEADER` | `serve` | `X-Auth-Request-Email` | Header carrying the signed-in address |
| `FINSTORE_MONZO_CLIENT_ID`, `FINSTORE_MONZO_CLIENT_SECRET` | `serve` | none | The OAuth client |
| `FINSTORE_MONZO_API_URL` | `serve` | `https://api.monzo.com` | |
| `FINSTORE_MONZO_AUTH_URL` | `serve` | `https://auth.monzo.com/` | |
| `FINSTORE_MONZO_SYNC_INTERVAL` | `serve` | `1h` | Period of the 7-day sync |
| `FINSTORE_MONZO_NIGHTLY_AT` | `serve` | `03:20` | Local time of the 89-day sync |
| `FINSTORE_MONZO_BACKFILL_CONCURRENCY` | `serve` | `6` | Ranges fetched at once during the full-history fetch |
| `FINSTORE_MONZO_WINDOW` | `serve` | `240s` | How long after the email link the full-history fetch runs |
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
| `core` | Accounts, transactions and every version of each, balances, the reconciliation state | Disposable: `renormalise` rebuilds it from `raw` |
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

### How the data is checked

- **Monzo accounts**: the balance Monzo reports must equal the sum of the stored transactions that
  were not declined. The check runs on every sync, using two balance readings around the fetch so
  that a transaction arriving mid-fetch does not raise a false alarm.
- **File-fed accounts**: each statement must pass its own checks to be imported at all, and each
  must start from the balance the previous one ended on.
- **Pots** have balances only; Monzo's API does not list a pot's own transactions.

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

Alerts worth having: no successful sync for 3 hours; consent ending within 14 days; consent `lost`
or `expired`; a reconciliation mismatch lasting 90 minutes; newest import older than 35 days; dump
older than 14 hours; `finstore.up` absent for an hour.

## When something goes wrong

| Symptom | Cause | What to do |
|---|---|---|
| Consent `lost` | Monzo ended the consent, a token refresh was rejected, or nobody approved within a day | Reconnect from the status page |
| Coverage shows "the Monzo window closed before this range was fetched" | The five minutes ran out | Reconnect: unfinished ranges are fetched first |
| Coverage shows "no completed fetch covers this period" | The service was down or disconnected for more than 90 days | Reconnect: the full-history fetch fills it |
| A Monzo account shows `mismatch` | The stored transactions do not add up to the bank's balance | A sync usually clears it. If it lasts, compare `raw` with `core` and run `renormalise --verify`. A closed account with a difference that will never change can be accepted with `account accept-difference` |
| An import exits 2 | The statement failed a check, belongs to another bank account, was given with the wrong `--format`, or its layout changed | The message says which. The file is kept in `raw.import_file` and nothing was stored. A wrong format: give it again with the right one. A changed layout: the reader needs updating, then `renormalise` |
| Coverage shows "no file covers these days" | A statement was skipped | Import the missing statement |
