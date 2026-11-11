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
  and direct debits, their [pending transactions](#pending-transactions), and the Lloyds credit
  cards. Its accounts are `lloyds-joint`, `lloyds-current` and so on, and are checked by running
  balance (see [How the data is checked](#how-the-data-is-checked)). A credit card is an account
  of its own, `lloyds-card-<last four digits>`, of type `credit_card`.
- **`truelayer:ob-monzo`**: standing orders and direct debits for the Monzo accounts finstore
  already holds from Monzo's own API, and the list of Monzo's cards, which is only stored. It
  creates no account: each mandate attaches to the Monzo account with the same
  [bank reference](#bank-references), so connect Monzo first. Pots are skipped. Monzo's own
  connection stays the only source of Monzo's transactions and balances.

Both connections also read `/data/v1/info` once a local day when the consent allows it. That is the
account holder's identity as each bank holds it: name, addresses, date of birth, emails and phones.
It is stored in `raw` and nothing reads it: no view shows it, and `finstore_agent` cannot read
`raw`. It is personal data, and it sits in the nightly dump and the backups like every other bank
body.

**Scopes and the cards reconnect.** A run takes the scopes from the `/me` answer of the consent it
runs under, and calls `/data/v1/cards` and everything under it only when `cards` is among them, and
`/data/v1/info` only when `info` is. A consent made before cards were added lacks `cards` until you
reconnect it, so reconnect Lloyds and Monzo (TrueLayer) once from the status page after updating.
Pending transactions and the account detail need no reconnect. When `/me` cannot be read, a run
treats `cards` and `info` as not granted.

**Refused card reads.** The card reads of a Lloyds run (the card list, each card's transactions and balance) are ledger reads, but a refusal of one (401, 403 or `access_denied`) never ends the consent: a `cards` scope the bank does not honour must not cost the working account feed. The run ends `partial` with `cards_refused`, makes no further card call (not even a pending read), and its account reads are unaffected. On a connect the missing card history does not make the run `history_failed`. A refused token refresh is not a refusal of the card call and still ends the consent, as it does anywhere else.

**Optional reads.** The pending transactions, Monzo's card list and `/info` are optional reads. They
come after every ledger read of a run, and nothing a bank answers to one of them changes the run's
outcome or ends a consent: a refusal (401, 403, `access_denied`) is only counted, a 429 stops the
run's remaining optional reads, and a bank that says an endpoint is unsupported (501, or the codes
`endpoint_not_supported` and `not_supported`) is logged at info level. Each is counted by
`finstore.truelayer.optional_reads` (see [Telemetry](#telemetry)).

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
the balance, and the same for each credit card when the consent has the `cards` scope. The ledger
reads come first; the optional reads (pending transactions, Monzo's card list, `/info`) follow, so
a rate limit on one never costs ledger data. TrueLayer allows four calls a day to each endpoint
for each account, and each card is counted by its own key. The service counts
its own calls in `raw.api_response` and skips a fifth (the run ends `partial` with `rate_limited`);
a connect day uses three. A `raw.api_response` row with status 0 and no body records a call that got
no whole response (a transport failure): it may still have reached the bank, so it counts toward the
daily limit.

**Reconnecting.** finstore holds three consents, each on its own 90-day clock: Monzo's own and the
two TrueLayer ones. The status page lists them together, so renew all three in one sitting. A
reconnect is a fresh connect: the old consent becomes `replaced` and the new one active, and Lloyds
refetches its full history. A consent ends `lost` when TrueLayer refuses a token refresh or a ledger data
call (a refused card read or optional read does not); reconnect from the status page.

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

A credit card has no sort code or account number, so it has no bank reference. Its key is `card:`
and the last four digits of the card's partial number, and that key stands in for the reference on
every per-card call. A card whose partial number does not end in four digits is skipped with a
warning. Two cards in one list that share their last four digits are both skipped for that run, a
warning is logged and the run ends `partial` with `card_key_shared`, since merging two cards into
one account would be worse than having neither.

### Ending a replaced card

A new card number is a new key and so a new account. The old account stops being listed but stays
open with its last balance, so a sum of card balances would count the debt twice (and if TrueLayer's
card resource is the account rather than the plastic, the new key's history repeats the old rows).
finstore warns when a card account that has not ended is missing from a successful card list, and
sets `finstore.truelayer.cards_unlisted` to how many. The warning carries the source and a count
only, since nothing from a bank response goes in a log. Find the card in the data: the open card
accounts that stopped getting new rows are the ones, for example

```sql
SELECT a.slug, c.latest_date
FROM agent.accounts a JOIN agent.coverage c ON c.account_slug = a.slug
WHERE a.type = 'credit_card' AND a.ended_on IS NULL
ORDER BY c.latest_date NULLS FIRST;
```

(`agent.accounts.detail` holds the card object, with its partial number and name). End the old
account the day before the new card's first row:

```sh
docker exec finstore finstore account end --slug lloyds-card-1234 --last-date 2026-11-30
```

The old account's later rows are then left out of the agent views and its balance out of the
current balances.

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
| `FINSTORE_OTLP_ENDPOINT`, `FINSTORE_OTLP_INSTANCE_ID`, `FINSTORE_OTLP_TOKEN` | `serve` and every command except `bootstrap`, `healthcheck`, `version` and `help` | empty | OTLP over HTTP with basic auth, for traces (`/v1/traces`), metrics and logs; export is off when the endpoint is empty. A command whose OTLP settings are unusable runs without telemetry and says so on stderr |
| `FINSTORE_ENVIRONMENT` | `serve` and the same commands | `dev` | Value of `deployment.environment.name` |
| `OTEL_SDK_DISABLED` | `serve` and the same commands | unset | `true` turns every telemetry provider into a no-op, whatever the OTLP settings say |
| `OTEL_RESOURCE_ATTRIBUTES` | `serve` and the same commands | unset | Adds resource attributes. It cannot change `service.name`, `service.version`, `service.instance.id`, `deployment.environment.name` or `finstore.process.role`: finstore's own values win. `OTEL_SERVICE_NAME` is not read |
| `OTEL_TRACES_SAMPLER`, `OTEL_TRACES_SAMPLER_ARG` | `serve` and the same commands | `parentbased_always_on` | The SDK's trace sampler. `always_off` stops traces only |
| `FINSTORE_LOG_LEVEL` | all | `info` | `debug`, `info`, `warn` or `error` |

## The data

Five schemas:

| Schema | Holds | Kept how |
|---|---|---|
| `raw` | Every API response and every imported file, byte for byte, plus what the operator entered | Append-only. Never updated, never deleted from |
| `core` | Accounts, transactions, standing orders and direct debits and every version of each, balances, pending transactions, the periods each completed fetch covered, the reconciliation state | Disposable: `renormalise` rebuilds it from `raw` |
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
gap with its reason. A reader should check it before trusting a total. Its `source` column says where the
account's data comes from: `monzo` for Monzo's own API, `truelayer:<provider>` for an account
TrueLayer feeds, and the file source for a statement-fed account. The status page shows the same
as its Feed column.

Each account and transaction also carries the source's own record, for anything the named columns
leave out:

- `agent.accounts.detail` is the latest account object Monzo returned for the account (from
  `/accounts`) or the pot (from `/pots`), as JSON. It is null for a file-fed account. It is read
  from the newest successful response for each request, newest by when it was fetched, so a
  capture loaded later with older responses does not replace it. A stored body that cannot be read
  as JSON is skipped. Monzo keeps closed accounts and deleted pots in those lists. The account
  object includes Monzo's identifiers for the account (account number and sort code), its owners'
  names and Monzo user ids, so anyone given `finstore_agent` can read them. For a TrueLayer account
  or card it is the account or card object the bank listed newest (`core.account.detail`); an
  account missing from the newest listing keeps the one it was last seen with. A TrueLayer account
  object holds the account number, sort code and IBAN, and a card object the card's partial number
  and the name on it, so the same warning applies.
- `agent.transactions.detail` is the latest version of the transaction's source record: the whole
  Monzo transaction object, or the parsed statement row for an imported one. Earlier versions are
  in `agent.transaction_history`.
- `agent.transactions.pot_slug` names the pot a Monzo pot transfer moved money to or from (the pot
  id in the transaction's metadata, when it is a pot finstore holds), whatever the transaction's
  status. It is null for every other transaction.

These columns come after the others, so a query that reads columns by position is unaffected.
`agent.accounts` also ends with `bank_ref` (see [Bank references](#bank-references)), `ended_on`
(see [Ending an account](#ending-an-account)) and `pending_as_of`: when the newest pending
response for the account was fetched, null when no pending read has succeeded. It tells a reader
how fresh the pending list is even when that list is empty.

### Pending transactions

`agent.pending_transactions` lists the pending transactions the bank gave for each Lloyds account
and card in its newest pending response: `account_slug`, `source`, `as_of` (when that response was
fetched), the transaction id, date, `amount_minor`, currency, description, merchant name and the
whole row in `detail`. An ended account has none. Amounts are signed as the account's settled rows
are: an account's as the bank gave them, a card's from its `transaction_type` (a purchase is
negative).

**These rows are not additive with the ledger.** TrueLayer gives a settled transaction another id
than the pending row it was, so a payment is in this view until the bank stops listing it as
pending and in `agent.transactions` once it settles; a sum over both counts it twice. Monzo is the
other way round: its pending rows are already in `agent.transactions` with status `pending` and in
its sums, because Monzo keeps one id from pending to settled, so this view has none of them and the
Monzo connection through TrueLayer reads no pending transactions.

An account that has had no successful pending read has no rows and a null `pending_as_of`; one
whose last read found nothing pending has no rows and a `pending_as_of` that says when.

### Credit cards

A Lloyds credit card is an account of type `credit_card`, `lloyds-card-<last four digits>`, in
`agent.accounts`, `agent.transactions` and `agent.balances`, with its card object as `detail` and
no bank reference. In the ledger a card is a liability: a purchase is negative, a repayment is
positive, and the balance is minus what is owed. The sign comes from the transaction's
`transaction_type`, not from the sign the bank wrote, and a card transaction whose type is neither
`DEBIT` nor `CREDIT` is rejected as an unexpected body. A card is reconciled `balances_only`:
balances are recorded and nothing is compared, so `agent.coverage` reports no gaps for it and a
wrong sign is caught only by reading its figures against the bank's app. A connect fetches a card's
history from 2021-01-01, and a scheduled run the last 35 days.

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
- **Credit cards** (`balances_only`): balances are recorded and nothing is compared. Nothing
  checks a card's transactions against its balance yet.
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
| `finstore.bank.requests` (counter) | `source`, `endpoint`, `status_class` | Bank requests made. `status_class` is `2xx`, `4xx` and so on, or `none` for a request with no usable response. `none` here and `no_response` on `finstore.bank.request.duration` below mean the same thing; the two names differ because the counter's value is older and its published values do not change |
| `finstore.bank.request.duration` (histogram, seconds) | `source`, `endpoint`, `status_class` | Duration of each bank attempt, retries included, with buckets from 50 ms to 30 s. An attempt with no usable HTTP response has `status_class` `no_response`: none arrived (a transport failure or a timeout), its body could not be read (it was cut off or over the 64 MiB limit) even though a status arrived, or the status was outside 100 to 599. The call then fails with the category `network` or `timeout`, as it does for the counter's `none` |
| `finstore.bank.retries` (counter) | `source`, `endpoint`, `reason` | Bank calls repeated, by `reason`: `rate_limited`, `server_error`, `timeout` or `network`. A token call is never repeated after a transport failure or a 5xx, so it never counts here for those |
| `finstore.truelayer.optional_reads` (counter) | `source`, `endpoint`, `result` | TrueLayer reads whose answer never reaches the ledger (pending transactions, Monzo's card list, `/info`), by result: `ok`, `unsupported`, `refused`, `rate_limited` or `failed` |
| `finstore.truelayer.cards_unlisted` | `source` | How many card accounts that have not ended the newest card list left out |
| `finstore.consent.state` | `source`, `state` | 1 for the current state, 0 for the others |
| `finstore.consent.expiry.time` | `source` | Unix time the consent is expected to end |
| `finstore.reconciliation.mismatch` | `account` | 1 while the account's last check failed or it has a statement gap |
| `finstore.import.last.time` | `account` | Unix time of the newest statement date imported |
| `finstore.dump.last_success.time` | none | Unix time from the dump status file |
| `finstore.up` | none | 1, sent every minute |
| `finstore.process.start.time` | none | Unix time `serve` started. The value changes at each start, so `changes()` over it counts restarts |
| `finstore.scheduler.job.runs` (counter) | `job`, `outcome` | Scheduler job runs, by the job's registered name and outcome: `ok`, `error`, `panic` or `cancelled`. `cancelled` is a job that returned an error after `serve` was told to stop, such as a deploy interrupting a running sync; it is not a failure |
| `finstore.scheduler.job.duration` (histogram) | `job` | Seconds one job run took, with buckets from 1 s to 1 hour |
| `finstore.scheduler.job.last_ok` | `job` | 1 when the job's last run ended `ok`, 0 for `error` or `panic`. A `cancelled` run does not change it. It is held in the `serve` process, not read from the database, so after a restart it is absent until each job has run once. The `status` job runs every minute, so a failing `status` shows within a minute and every other finstore gauge is stale until it recovers |
| `finstore.telemetry.export.failures` (counter) | `signal` | Exports that failed, by `traces`, `metrics` or `logs` |
| `finstore.telemetry.span_attributes.dropped` (counter) | `key` | Span attributes the exporter allow-list removed, by key. A key here is a library that added an attribute; nothing leaked. The Postgres driver's own attributes (`pgx.rows_affected`, a count, and `pgx.prepare_stmt.name`, a hash of constant SQL) are on the list, so a plain query drops nothing: any key here is new |
| `finstore.store.writer_lock.held` (gauge) | `holder` | Seconds the writer lock has been held so far, by who holds it (`monzo.sync`, `monzo.backfill`, `monzo.approval`, `truelayer.sync`, `truelayer.connect`, `truelayer.approve`, `renormalise`, `load-capture`, `account`, `import`). Absent while the lock is free. Every process that can hold the lock reports it: `serve` and the commands that write |
| `db.client.operation.duration` (histogram) | `db.operation.name`, `pgx.operation.type` | Duration of each database operation, in seconds, by the statement's first word (`SELECT`, `INSERT`) and the kind of operation (`query`, `prepare`, `connect`, `acquire`, `batch`, `copy`), with buckets from 1 ms to 10 s |
| `db.client.operation.errors` (counter) | `db.operation.name`, `pgx.operation.type` | Database operations that failed |
| `pgxpool.acquired_connections`, `pgxpool.idle_connections`, `pgxpool.total_connections`, `pgxpool.max_connections` and the other `pgxpool.*` gauges and counters | `db.system.name`, `db.client.connection.pool.name` | The connection pool's statistics. The pool name is the host, port and database of the connection, never the user or a password |
| `http.server.request.duration` | OpenTelemetry HTTP server attributes, including `http.route` | Duration of every request except `GET /healthz`, in seconds, with buckets from 5 ms to 90 s |
| `go.goroutine.count`, `go.memory.used`, `go.memory.allocated`, `go.memory.gc.goal`, `go.processor.limit`, `go.config.gogc` | none | Go runtime metrics |

### Identity

Every process exports one resource: `service.name` `finstore`, `service.version` (the image tag,
such as `main-659f3b6`, or `dev` for an unlabelled build), `deployment.environment.name` and
`finstore.process.role`.

| Process | `service.instance.id` | `finstore.command` |
|---|---|---|
| `serve` | the fixed word `serve`, so its series keep their labels across restarts | absent |
| A command (`migrate`, `renormalise`, `account add`, `account list`, `account accept-difference`, `account end`, `import`, `load-capture`, `coverage`) | a new UUID for each run, so a command never writes `serve`'s series | the command's name |

`bootstrap`, `healthcheck`, `version` and `help` export nothing. `bootstrap` executes `CREATE ROLE`
with a password, and the health check runs every 30 seconds.

### Traces

`serve` makes a server span for every request except `GET /healthz`, named for the route that
answered (`GET /{$}`) and carrying `http.route`, the method, the status and the path, never the
query string or a header. A `traceparent` header on an incoming request becomes a link, never the
parent; finstore sends none to a bank. Each command makes a root span named `finstore <command>`
with `process.exit.code`; a non-zero exit marks it as an error.

Besides the server span, `serve` makes these spans. None carries a code, a state, a query string or
error text: only fixed words.

| Span | When | Attributes |
|---|---|---|
| `connect` | A connect post (`POST /connect/...`) that passed the sign-in; a request answered 404 has its server span only | `finstore.source`, `finstore.connect.outcome` |
| `callback` | A request to a callback route that reached its handler, for Monzo's route and for the TrueLayer route they share | `finstore.source`, `finstore.connect.outcome`, and `error.type` when the outcome is `failed` |
| `scheduler.job` | Each scheduled job run: a new root span, never a child of anything | `finstore.job` (the job's registered name: `status`, `hourly`, `nightly` and one per TrueLayer connection and time of day), `finstore.job.outcome` (`ok`, `error`, `panic` or `cancelled`; only `error` and `panic` mark the span failed) |

`finstore.connect.outcome` says how the request ended:

| Outcome | HTTP status | Meaning |
|---|---|---|
| `started` | 200 | A connect post served the page that moves the browser on to the bank |
| `cross_site` | 400 | A connect post that did not come from the status page |
| `start_failed` | 500 | The attempt could not be started, or gave no usable bank link. The span is marked failed |
| `exchanged` | 303 | The callback was accepted and the bank's code exchanged |
| `invalid_link` | 400 | The callback had no `state` or no `code` |
| `bank_declined` | 400 | The TrueLayer callback came back with the bank's `error` parameter |
| `refused` | 400 | The `state` was unknown, expired, already used or issued for a source that does not use this callback |
| `failed` | 502 or 503 | `error.type` is `exchange` (502, the exchange failed) or `attempt_read` (503, the attempt could not be read). The span is marked failed |

`finstore.source` is the source the route belongs to. On the TrueLayer callback, which both
TrueLayer connections share, it is `unknown` until the attempt the `state` belongs to has been
read, and stays `unknown` when the request ends before that (a missing parameter, the bank's error,
an unusable or unreadable attempt). A `state` issued for another source, such as Monzo's, shows
that source with the outcome `refused`.

Every call to a bank is two levels of span. A call is one `bank {METHOD} {template}` span (for
example `bank GET /accounts`) with `finstore.source`, `url.template`, the method and, when the call
fails, `error.type` (the failure category) and `finstore.bank.code` (the bank's error code, only
when it is a well-formed code string). Each attempt, including the first, is a `{METHOD}
{template}` child span with `server.address`, `server.port`, `http.response.status_code` (absent
when no response came), `error.type` on a failed attempt and `url.full`, which is the scheme, host,
base path and the allow-list's path template with no query. From the second attempt of a call
`http.request.resend_count` is the number of attempts before it, so a 429 then a 200 reads as one
call span, two attempt spans, and one `finstore.bank.retries` count with `reason` `rate_limited`. A
template such as `/data/v1/accounts/{id}/transactions` names the endpoint: a concrete path holds an
id from a bank response, so it never appears in a span, and neither does a query, a header or a
body. finstore sends no trace header to a bank, and a bank sees the same request with telemetry on
or off.

**Postgres.** `serve` and the commands that read or write through the app role (`account add`,
`account list`, `account accept-difference`, `account end`, `import`, `coverage`) and `migrate` open
their database connection with tracing. A query, a prepare, a connect or a pool acquire made while a
span is recording becomes a client span beneath it, named for the statement's first word (`SELECT`)
and carrying `db.query.text` with placeholders only (`$1`), `db.operation.name`, `db.namespace`,
`server.address`, `server.port` and `user.name`. The parameter values are never recorded, and the
database URL, password included, is never a span attribute. `migrate` traces goose's statements too,
since goose connects with the same settings. Queries outside any span make no span. A failed
statement's span carries its SQLSTATE (`pgx.sql_state`) and the status `error`: the error's own text
can quote a value, and the exporter removes it. `bootstrap` never traces: it executes `CREATE ROLE`
with a password. `renormalise` and `load-capture` open their database connection without tracing,
because they run hundreds of thousands of statements; they export their root span and the batch
steps below, and `finstore.store.writer_lock.held` still shows who holds the lock.

**Writer lock.** Where the connection is traced, each hold of the writer lock is a
`store.writer_lock` span from the request to the release, with `finstore.lock.holder` (who asked)
and `finstore.lock.wait_ms` (how long the request waited for the lock). A request that gives up
before it gets the lock, or a hold whose release fails, ends its span with the status
`database_error` (a request that gave up also has no wait). The work done
under the lock is not a child of this span: it stays a child of the span that asked.

**Batch steps.** The ledger makes one span for a catch-up (`ledger.catch_up`, with
`finstore.count` rows applied), one for each step of `renormalise` (`ledger.renormalise.create`,
`.catch_up`, `.diff` with the number of differences, `.swap`) and `load-capture` makes one for each
directory (`capture.directory`) and each transactions fetch (`capture.fetch`), each with the number
of responses. There is never a span for a row. A failed step reads `error`.

One allow-list at the exporter is the only redaction layer for spans. A span attribute whose key is
not on it is dropped and counted (`finstore.telemetry.span_attributes.dropped`); `url.full` is kept
only without a query, fragment or credentials; no span event leaves the process, so an exception
message never does; and a failed span's status text is kept only when it is one of finstore's fixed
category words or a five-character SQLSTATE, otherwise it reads `error`. The list is in
`internal/semconv`.

### Logs and trace ids

Logs go to OTLP and to stderr, from `serve` and from every command that sets telemetry up. A record
written inside a span carries that span's `trace_id` and `span_id`: OTLP records as their trace
fields, and the JSON lines on stderr (what `docker logs` shows) as `trace_id` and `span_id` keys.

Alerts worth having: no successful sync for 3 hours (13 hours for a `truelayer:` source, which syncs
twice a day); consent ending within 14 days; consent `lost`
or `expired`; a reconciliation mismatch lasting 90 minutes; newest import older than 35 days; dump
older than 14 hours; `finstore.up` absent for an hour; `finstore.store.writer_lock.held` above an
hour for any holder (every scheduled sync waits behind it; a `serve` holder past its run deadline
means the release stuck, and a command holder is just a long command).

## When something goes wrong

| Symptom | Cause | What to do |
|---|---|---|
| Consent `lost` | Monzo ended the consent, a token refresh was rejected, or nobody approved within a day; for a TrueLayer source, TrueLayer refused a refresh or a data call, or the token was missing at start (`token_missing`, after a database restore) | Reconnect from the status page |
| A TrueLayer run ends `partial` with `rate_limited` | A fifth call of the day to one endpoint for one account was skipped, or TrueLayer answered 429 | Nothing: the next day's syncs catch up. Do not reconnect and sync by hand several times in a day |
| A Lloyds connect run ends `partial` with `history_failed` | The full-history call of an account or a card failed (a refusal of a card read is `cards_refused` instead), the card list failed, or the service stopped before it ran | Reconnect from the status page |
| A run ends `partial` with `cards_refused` | The bank refused a card read although `/me` lists `cards`. The consent is kept and the run made no further card call | Nothing breaks: accounts sync as usual. Check whether the bank offers cards on this consent; reconnecting may help. Cards simply stay as they were |
| A run ends `partial` with `card_key_shared` | Two cards in one list share their last four digits, so both were skipped | Nothing finstore can do: the cards cannot be told apart by their number. Look at the cards at the bank |
| `finstore.truelayer.optional_reads` has results `refused`, `failed` or `unsupported` for an endpoint | The bank does not honour a scope the consent lists, or the call keeps failing. The run is not marked partial by an optional read | Check the endpoint's results over several days: an endpoint with no `ok` for 48 hours is worth an alert. A consent without `cards` makes no card call: reconnect to add it |
| `finstore.truelayer.cards_unlisted` is above 0 | A card account that has not ended is missing from the newest card list, usually a replaced card | [End the old card's account](#ending-a-replaced-card) |
| Coverage shows "the Monzo window closed before this range was fetched" | The five minutes ran out | Reconnect: unfinished ranges are fetched first |
| Coverage shows "no completed fetch covers this period" | The service was down or disconnected for more than 90 days | Reconnect: the full-history fetch fills it |
| A Monzo account shows `mismatch` | The stored transactions do not add up to the bank's balance | A sync usually clears it. If it lasts, compare `raw` with `core` and run `renormalise --verify`. A closed account with a difference that will never change can be accepted with `account accept-difference` |
| An import exits 2 | The statement failed a check, belongs to another bank account, was given with the wrong `--format`, or its layout changed | The message says which. The file is kept in `raw.import_file` and nothing was stored. A wrong format: give it again with the right one. A changed layout: the reader needs updating, then `renormalise` |
| Coverage shows "no file covers these days" | A statement was skipped | Import the missing statement |
