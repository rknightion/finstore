# Payees, recurring payments and cash flow in finstore

*   **Author:** finstore maintainers
*   **Status:** Proposed
*   **Date:** 2026-11-25

--------------------------------------------------------------------------------

## Overview

finstore keeps every transaction a bank reports, but it does not say what a transaction is: a
subscription, a bill, a transfer between two of the owner's accounts, a salary or a card
repayment. An agent that wants to answer "what am I subscribed to", "what went up in price" or "how
much did I spend last month" has to work that out from raw rows on every question. Today's
`spending_summary` also counts a transfer between two of the owner's accounts as spending.

This design adds a derived layer over the existing transactions:

1.  **Payees.** Every transaction gets a payee: one identity for a merchant, company or person
    across the name variations finstore can recognise, with totals per payee and per year.
2.  **Flow classes.** Every transaction that was not declined gets one class: `internal`,
    `scope_transfer`, `own_external`, `investment`, `debt_repayment`, `borrowing`, `income`,
    `refund` or `spending`, so totals stop counting the owner's own money moving between accounts as spending
    or income.
3.  **Recurring payments.** Each payee's payments are grouped into series with a cadence, a typical
    amount, price changes, a next expected date and a status.
4.  **Cash flow and profile.** Monthly income, spending and savings by account scope, category
    trends, upcoming payments, unusual transactions and a short profile of the owner's finances.
5.  **Corrections.** The operator corrects any of the above with `finstore payee`, `finstore
    category` and `finstore account scope` commands. Each correction is an append-only operator
    entry, applies to all past and future transactions, and survives `renormalise`.
6.  **MCP tools.** Four new read-only tools return these results in one call each.
7.  **A changed default.** `spending_summary` stops counting the owner's own money: its default
    totals cover spending only (or income and refunds), not transfers between held accounts,
    investments or debt repayments, and settled rows only unless pending rows are asked for.

Everything derived is computed in SQL views at read time. Only the corrections are stored.

## How finstore works today

*   **`raw`** is the append-only record of every source payload and operator entry, in one sequence
    (`raw.seq`). **`core`** is a disposable projection of it, rebuilt by `finstore renormalise`, and
    **`agent`** is a set of read-only views over `core`. See "The data" in
    [the operator's contract](../operations.md).
*   **Operator entries** (`raw.operator_entry`, with a `kind` and a JSON `payload`) record facts the
    operator enters by command, such as `account_add`, `account_end`, `accept_gap` and
    `accept_difference`. The ledger normalises each entry into `core` in sequence order, live and on
    replay. An entry the ledger cannot apply changes nothing; the ledger counts it and logs its
    category, and stores no record of it.
*   **Rebuild and its checks.** `NormaliserRev` is a constant in the ledger. Raising it marks every
    stored `core` as out of date, so `migrate` then requires a full `renormalise`. `DiffCore` is
    the comparison `renormalise --verify` runs between a rebuilt `core` and the live one.
*   **`agent.transactions`** has one row per live transaction with `status` `settled`, `pending` or
    `declined`, a signed `amount_minor`, `scheme`, `category`, `merchant_name`, `counterparty`,
    `description`, `pot_slug` and `detail`, the latest source record as JSON. Every Monzo field this
    design reads that is not a column (`metadata.*`, `merchant.group_id`, `counterparty.*`,
    `local_currency`, `fees`) comes from `detail`.
*   **Accounts.** `core.account` has a `type`, a `parent_id` (a pot's parent account), a `source_key`
    (for Monzo, the Monzo account id) and a `bank_ref`: sort code and account number, digits only,
    joined by one space (see [Bank references](../operations.md#bank-references)). A credit card is
    an account of type `credit_card` with no `bank_ref`; a purchase is negative and a repayment
    positive ([Credit cards](../operations.md#credit-cards)).
*   **Mandates.** `agent.direct_debits` and `agent.standing_orders` list the mandates the Open
    Banking connections report, each with `disappeared_at` once the bank stops listing it; a
    standing order has `next_date` and `next_amount_minor`.
*   **The MCP endpoint** offers read-only tools: `overview`, `search_transactions`,
    `spending_summary`, `describe_schema`, `query_sql`, `portfolio` and `pension_summary`.
    `spending_summary` groups money in or out by month, category, merchant, counterparty or account,
    leaves out declined rows and pot transfers, and counts pending rows.
*   **Reader logins** (`finstore_agent` members, such as a Grafana datasource) read the `agent`
    schema only. `finstore_mcp`, the login the MCP tools use, reads `agent`, `core`, `raw` and
    `sync`. New `agent` views reach both through the existing grant on all tables in `agent`.

## Problem Statement

The properties below were observed on a real dataset of tens of thousands of transactions over
several years, almost all from Monzo with a small Open Banking feed. They are properties of that
dataset, not guarantees from any bank, and they shape the design.

1.  **Merchant names vary, and Monzo mostly groups them.** Monzo card transactions carry a merchant
    object with an `id` and a `group_id`. Many merchant ids collapse into fewer groups, and a group
    almost never carries more than one name. But some names belong to two or three groups, so one
    company can appear as several payees.
2.  **Bank transfer payees vary more.** A faster payment or direct debit has a counterparty name and
    bank details. The same bank details can carry several names, either because the name was
    written differently (with references, numbers or truncation appended) or because the bank
    details belong to a payment collector that takes payments for many unrelated merchants. Bank
    details alone are therefore not a payee identity.
3.  **The Open Banking feed has descriptions only.** A row has a description with references and
    dates appended to the name, and a provider category (`TRANSFER`, `DIRECT_DEBIT`, `CREDIT`). It
    has no counterparty bank details, so finstore cannot see from that side alone which account
    the money went to.
4.  **The bank declares some recurrence, in its own ids.** Monzo marks a direct debit with a mandate
    id (`metadata.bacs_direct_debit_instruction_id`), a standing order with `metadata.series_id`, and
    some recurring card payments and direct debits with `metadata.subscription_id`. Most recurring
    card payments carry none of these. These ids do not match the keys of `agent.direct_debits`
    and `agent.standing_orders`, which come from the Open Banking connections: in the dataset no
    Monzo mandate id equals a stored mandate key, and a standing order's key is its payee,
    reference and frequency.
5.  **One merchant can hold several subscriptions.** Charges from one merchant group fall into
    separate amount bands that each recur on their own schedule, and card verification charges of a
    few pence sit among them.
6.  **Prices change.** A subscription steps up in price once or twice a year; a utility bill varies
    every month around a level.
7.  **Much of the money is the owner's own.** Pot transfers, transfers between two held accounts,
    Monzo Flex repayments and moves of a purchase to Flex, transfers to the owner's accounts at
    other banks, to a company the owner runs, to investment and crypto platforms, and credit card
    repayments together exceed real spending. finstore can recognise some of these on its own:
    *   pots, by `pot_slug`;
    *   Flex, by `metadata.flex_transaction_type` (`repayment`, `transaction_moved_to_flex_*`,
        `flex_move_money_transfer_*`);
    *   transfers between held accounts, from the side that carries the counterparty's bank
        details or Monzo account id; the other side is recognisable only by pairing it with the
        first (Problem 3).

    The rest need the operator to say which payee is which.
8.  **Monzo's self-transfer flag crosses owners.** `metadata.p2p_is_to_self` is set on transfers
    between a personal and a joint account and between a personal and a business account, so it
    says "the owner is on both sides", not "the same household of money".
9.  **Monzo's `include_in_spending` flag is not usable as the spending rule.** It is true on both
    sides of a Flex repayment, so summing by it counts the repayment as spending.
10. **Monzo custom categories have no names in the API.** Categories such as `category_0000...`
    carry a small share of transactions and need a name from the operator.
11. **Accounts belong to different households of money.** The dataset holds personal, joint and
    business accounts. Joint accounts receive money from another person, and business accounts
    belong to a company. Monzo's account type says which is which; the Open Banking feed names a
    joint account only when its display name is exactly "joint", and a statement-fed account has
    no such type at all.

## Goals

1.  Give every transaction a payee that is stable across the name variations finstore recognises,
    and give the operator a way to merge, separate, rename and classify payees.
2.  Classify every transaction that was not declined into exactly one flow class, so that spending
    and income totals exclude the owner's own money moving between accounts.
3.  List recurring payments with cadence, typical amount, price history, next expected date, status,
    annual cost and evidence, including the ones the bank does not declare.
4.  Give monthly cash flow (income, spending, debt repayments, investment flows, savings rate) by
    account scope, category trends, upcoming payments and unusual transactions.
5.  Make every correction append-only, retroactive and permanent across `renormalise`, and show any
    correction that has stopped applying.
6.  Expose the results through MCP tools that answer the common questions in one call, and through
    `agent` views that reader logins such as Grafana can chart.
7.  Check the classification on the live dataset with arithmetic checks that can fail (pairs that
    must cancel, an independent recomputation), as well as by review with the operator.

## Non-Goals

*   **Budgets, goals and alerts.** finstore stays a store, not a budgeting app.
*   **Model-driven categorisation inside finstore.** Classification is deterministic SQL. An agent may
    interpret the data through the MCP tools and recommend corrections, but the operator applies them.
*   **An MCP write tool.** The MCP endpoint stays read-only (see Alternative F).
*   **Cancelling or changing anything at a bank or merchant.** finstore stays read-only toward banks.
*   **Investment performance.** `portfolio` and `pension_summary` cover it. Transfers to investment
    accounts appear here only as the `investment` flow class.
*   **Currency conversion.** Sums are per currency, as everywhere else in finstore.

## Critical User Journeys (CUJs)

### CUJ 1: What am I subscribed to?

An MCP client calls `recurring_payments`. It gets every active series: payee, cadence, typical and
latest amount, annual cost, next expected date, evidence (mandate, standing order, provider
subscription or inferred) and price changes, with the total annual cost of every matching series.
Series the operator ignored are left out unless asked for.

### CUJ 2: What went up in price?

The same tool, with `changed_only` and sorted by `change_12m`, lists each series whose typical
amount differs from a year earlier, with the date and amounts of each change.

### CUJ 3: How much did I spend last month?

An MCP client calls `cash_flow` for a month range. Spending leaves out pot moves, transfers between
held accounts, Flex repayments and moves to Flex, transfers to the owner's accounts at other banks,
investments and debt repayments; each is its own figure, per account scope. The client sees
category trends and the savings rate without writing SQL.

### CUJ 4: The operator corrects a payee

The operator runs `finstore payee list --query <text>` and sees two payees for one company. They run
`finstore payee merge <key> <into-key>`, and every view and tool shows one payee immediately, for
every past transaction and every new one. A later `finstore renormalise` rebuilds the same result
from `raw`.

### CUJ 5: The operator names the owner's money

The operator marks the counterparty that is their account at another bank as `own_external`, a
broker as `investment`, a credit card company whose card finstore does not hold as
`debt_repayment`, and their employer as `income` with `--employer`. They set the scope of an Open
Banking joint account that finstore took for personal. Monthly spending no longer includes those
payments.

### CUJ 6: A Grafana dashboard

A reader login charts `agent.monthly_flows` by month and scope, `agent.category_months` by category,
and `agent.recurring_payments` as a table, without new grants.

### CUJ 7: Was I charged twice?

An MCP client reads `agent.anomalies` (through `query_sql`) or the anomaly counts in
`finance_profile`, and finds a payment taken twice within two days, a charge far above what a payee
usually takes, a large first payment to a new payee, or a foreign-currency fee. The owner checks
each with the bank or merchant.

### CUJ 8: An agent interprets the data

An agent calls `finance_profile`, then `payees` and `recurring_payments`, and reports to the
operator: "these three inferred series look like one gym, and this payee looks like your
partner". The operator applies what they agree with by CLI.

## Proposed Architecture

```text
raw.operator_entry (payee_rule, category_name, account_scope)
        │  ledger normalises, in sequence order
        ▼
core.payee_alias, core.payee_setting, core.series_setting,
core.category_name, core.account.scope
        │
        ▼
agent.transactions ──► agent.cash_flows ──► agent.payees, agent.payee_years
core.account ────────►  (payee + class)  ├─► agent.recurring_payments ──┐
                                         ├─► agent.monthly_flows        │
                                         ├─► agent.category_months      ▼
                                         ├─► agent.anomalies     agent.upcoming_payments
                                         └─► agent.payee_rule_status     ▲
agent.standing_orders ───────────────────────────────────────────────────┘

MCP tools: payees, recurring_payments, cash_flow, finance_profile
```

The only stored state is the operator's corrections: four `core` tables and one `core.account`
column, built from operator entries. Everything else is a view, so a correction applies retroactively by construction and no
derived row can go stale.

### Why views

Recurrence and payee totals depend on a payee's whole history: one new transaction can change a
series' cadence, status and next date. The ledger normalises incrementally, one `raw` row at a
time, so stored series would need recomputing on every transaction and every rule (Alternative A).
A view computes them on read.

Prototypes on the live dataset set the shape of the views:

*   Classifying every transaction (payee key with name normalisation, held-account matching, class)
    took about half a second when held accounts are matched by equality joins on `bank_ref` and
    `source_key`. The same matching written as one lateral lookup with an `OR` took about four
    seconds, so the views must use equality joins.
*   Assigning undeclared payments to amount bands, the one sequential step, took about half a
    second over all payees as a recursive query.

The MCP tools select from these views and never restate their logic. A tool that returns several
arrays (`finance_profile`, `cash_flow`) uses the pattern `portfolio` and `pension_summary` already
use: one statement that `UNION ALL`s rows tagged by array, read in one read-only transaction, so
the arrays share one snapshot and the existing per-array limits. Each branch recomputes
`agent.cash_flows`, which the budgets under Non-Functional Requirements allow for. If a tool misses its budget, the remedy is
Alternative B, never a copy of a view body in tool SQL.

### Payee keys

Each transaction gets a **payee key** from the first rule that applies. The grammar is
`^(mg|mu|n|x):.+$`.

| Key order | Source | Key |
|---|---|---|
| 1 | Monzo merchant object with a `group_id` | `mg:<group_id>` |
| 2 | Monzo counterparty with a Monzo `user_id` (person to person) | `mu:<user_id>` |
| 3 | Any other transaction with a counterparty or description name | `n:<normalised name>` |
| 4 | None of the above | `x:<scheme>` |

Bank details are not part of a key, because a payment collector shares them across merchants
(Problem 2). They are used only to recognise held accounts. `x:` payees are catch-alls: they never
form recurring series or anomalies.

**Name normalisation** (key order 3 and automatic merging) is one SQL function,
`agent.normalise_payee_name(text)`, so the views and the CLI agree. The initial rules are below;
they are tuning values, recorded in the function's comment and tests, and may change (see Changing
the normaliser):

1.  Lowercase, and replace every run of characters other than letters, digits, `&` and `'` with
    one space.
2.  Drop reference tokens: any token of six or more characters containing a digit, any date token
    (`01nov26`, `2026-11-01`, `01/11/2026`), and a trailing run of digits.
3.  Drop a leading collector prefix of the form `<word> re ` (as in `<collector> re <merchant>`),
    keeping the merchant.
4.  Drop trailing company suffixes: `ltd`, `limited`, `plc`, `llp`, `inc`, `uk`.
5.  Trim. An empty result falls through to key order 4.

**Automatic merging.** Two `mg:` keys merge into one payee when the most frequent normalised
merchant name of each (ties broken by the lowest name) is the same (Problem 1). No other keys merge
automatically: equal normalised names already give one `n:` key, an `mg:` key never merges with an
`n:` key, and an `mu:` key never merges, because a person's name is not an identity.

**Canonical key.** A payee is identified by its **canonical key** (`payee_id`), resolved at read
time in three steps:

1.  **Groups.** Every key forms an automatic group with the keys it merges with automatically. A
    key with an operator `separate` setting leaves its group and forms a group of its own.
2.  **Group identity.** A group's identity is the member with the earliest first transaction, ties
    broken by the lowest key. A key that appears later joins under the existing identity, so the
    identity moves only if an earlier transaction is imported for a different member.
3.  **Merges.** An operator `merge` on a key moves the group that key is in after step 1 into the
    payee its target key belongs to (the target is itself resolved by these steps). For a key still
    in an automatic group, that is the whole group, including members that join later; for a
    separated key, it is the key alone. If two members of one group carry different merges, the one
    with the higher `entry_seq` wins.

    The view follows merge targets at most ten steps and stops at a key it has already visited.
    Such a loop leaves the group unmerged and is listed in `agent.payee_rule_status`.

The canonical key is the identity of the final group. Operator settings always win over automatic
merging. To move one key out of an automatic group into another payee, the operator sets both
`separate` and `merge` on it.

The **display name** of a payee is the operator's rename if there is one, else the most frequent raw
name (merchant name, counterparty name or description) among its transactions over the last two
years, else over all time, ties broken by the lowest name.

**Changing the normaliser.** An `n:` key is the normaliser's output, so a rule stored against an
`n:` key would stop matching when the function changes. A rule about an `n:` payee therefore stores
the raw name the operator named (`name_sample`), and the views resolve its key as
`agent.normalise_payee_name(name_sample)` at read time, so the rule follows any change to the
function. The only case left is a change that splits one key into two; a pull request that changes
the function includes a test that reports which fixture keys change. The function is created with `CREATE OR REPLACE` in the views file after the views are dropped, and
`EXECUTE` on it is granted to `finstore_app`, `finstore_agent` and `finstore_mcp` in the same
file, after the function (not in `grants.sql`, which `migrate` also runs on its own before any
function exists). Postgres checks a function a view calls against the user reading the view; the default
grant to `PUBLIC` covers that today, and the explicit grant keeps it working if that default is
ever revoked.

### Operator rules

The operator's corrections are operator entries of three new kinds, `payee_rule`, `category_name`
and `account_scope`. The ledger normalises each into **effective state** in `core`, the way
`account_end` already works (`account_scope` sets `core.account.scope` through the same read and
upsert path as `account_end`): a later entry replaces an earlier one, so a view
reads current state with a plain join and never replays history.

```sql
-- Merge and separate settings, one row per key named. Targets are stored as named and resolved by
-- the views (see Canonical key), because automatic groups change as transactions arrive.
CREATE TABLE core.payee_alias (
  payee_key     text PRIMARY KEY,
  separate      boolean NOT NULL DEFAULT false,
  into_key      text,                      -- merge target, or null
  name_sample   text,                      -- raw name seen when the key is n:
  entry_seq     bigint NOT NULL,           -- the raw.operator_entry.seq that last changed the row
  entered_at    timestamptz NOT NULL,
  CHECK (separate OR into_key IS NOT NULL));

-- Settings named on a payee key. A null column has no setting. The CLI writes settings against the
-- payee's canonical key at the time, so a payee's settings normally sit on one row.
CREATE TABLE core.payee_setting (
  payee_key     text PRIMARY KEY,
  name          text,
  flow_class    text CHECK (flow_class IN ('internal', 'scope_transfer', 'own_external',
                                           'investment', 'debt_repayment', 'borrowing', 'income',
                                           'refund', 'spending')),
  employer      boolean,                   -- only with flow_class income
  category      text,
  name_sample   text,
  entry_seq     bigint NOT NULL,           -- the entry that last changed the row
  entered_at    timestamptz NOT NULL);

-- Operator decisions about a recurring series.
CREATE TABLE core.series_setting (
  series_key  text PRIMARY KEY,
  recurring   text NOT NULL CHECK (recurring IN ('confirm', 'ignore', 'not_recurring')),
  entry_seq   bigint NOT NULL,
  entered_at  timestamptz NOT NULL);

CREATE TABLE core.category_name (
  category    text PRIMARY KEY,            -- as agent.transactions.category
  name        text NOT NULL,
  entry_seq   bigint NOT NULL,
  entered_at  timestamptz NOT NULL);

-- An account's operator-set scope, written by the existing account upsert path the way
-- account_end writes ended_on. Null means no setting. (In core_schema.sql every table name is
-- written {{schema}}.name, because the same DDL builds core_next.)
ALTER TABLE core.account ADD COLUMN scope text
  CHECK (scope IN ('personal', 'joint', 'business'));
```

(In `core_schema.sql` the column is part of `CREATE TABLE core.account`; the `ALTER` form only
shows the change.)

Entry payloads (`raw.operator_entry.payload`), with only the fields the action uses:

| Kind | Payload fields |
|---|---|
| `payee_rule` | `action`: one of `merge`, `separate`, `rename`, `class`, `category`, `recurring`, `clear`; `payee_key`; `name_sample`; and per action: `into_key` (merge), `name` (rename), `flow_class` and `employer` (class), `category` (category), `series_key` and `recurring` (recurring), `clears` (clear) |
| `category_name` | `category`; `name`, where null clears it |
| `account_scope` | `slug`; `scope`, where null clears it |

**Clearing.** A `clear` names what it removes in `clears`:

*   `merge`: nulls `into_key`, and deletes the `payee_alias` row if the key is not separated;
*   `separate`: sets `separate` false, and deletes the row if it has no `into_key`;
*   `name`, `class` or `category`: nulls that setting (`class` nulls `employer` too), and deletes
    the `payee_setting` row when every setting is null;
*   `recurring`, with `series_key`: deletes the `series_setting` row.

**Validation**, in the ledger, so a rebuild applies the same rules as the live run:

*   `payee_key` and `into_key` match the payee key grammar, and `series_key` matches the series key
    grammar (`^(dd|so|sub|inf):.+$`). Neither needs to exist yet: a rule may name a payee before
    its first transaction.
*   `flow_class` is one of the classes, and `employer` is accepted only with `income`.
*   `account_scope` names an existing account that is not a pot (a pot takes its parent's scope).

The CLI validates an entry with the same exported validation functions the ledger uses, before it
writes anything, as the existing operator commands do: an invalid entry is refused with its reason
and exits non-zero, and nothing reaches `raw`. A valid entry is inserted and normalised in the same
transaction. The ledger's own validation is the backstop for entries written another way: it skips
them as unparseable (counted and logged), as it does any operator entry it cannot apply.
Merge loops are not a ledger check, because automatic groups change as data arrives: the CLI
refuses a merge that would loop through the current payees, and the views stop any loop that
arises later (see Canonical key).

**Which payee a setting applies to.** A setting named on any key applies to the canonical payee
that key belongs to now. The CLI names the canonical key, so two rows for one payee arise only after
a later merge; then, for each setting, the non-null value on the row with the higher `entry_seq`
wins. A series decision applies to its series key wherever that series
now belongs.

**Retroactivity.** Views join the effective state on every query, so a rule changes every past and
future transaction immediately. `renormalise` rebuilds the four tables and the column from `raw`
like the rest of `core`, so `--verify` compares them too.

**Orphaned rules.** A rule can stop matching anything: its key may never have had transactions,
the normaliser may have changed, or an inferred series may have gained an earlier first
transaction. `agent.payee_rule_status` has one row per row of the four tables:

| Column | Meaning |
|---|---|
| `rule_table` | `payee_alias`, `payee_setting`, `series_setting` or `category_name` |
| `key` | the payee key (for an `n:` rule, as its `name_sample` normalises now), series key or category |
| `detail` | the row's settings as JSON |
| `entered_at` | when it was last set |
| `matches` | true when the key currently has at least one transaction |
| `last_match_date` | date of the newest matching transaction |
| `problem` | null, `no_match` or `merge_loop` |

`finance_profile` reports the count of rows with a `problem`, and `finstore payee show` prints a
key's rules with their status.

### Account scope

Each account has a **scope**: `personal`, `joint` or `business`. It is, in order:

1.  the operator's setting in `core.account.scope`, if any;
2.  for a pot, its parent account's scope;
3.  from the account type: `joint` gives `joint`, `business` gives `business`;
4.  otherwise `personal`.

`agent.account_scopes` shows the result: `slug`, `scope` and `scope_source` (`setting`, `parent`,
`type` or `default`). Step 3 suits Monzo, whose types are reliable. An Open Banking joint account whose display name is
not exactly "joint", and any statement-fed joint or business account, needs an operator setting
(`finstore account scope <slug> <scope>`); `finance_profile` lists the accounts on the default.

### Flow classes

`agent.cash_flows` gives each transaction that was not declined one class. The first rule that
applies wins:

| Rule | Condition | Class | `flow_reason` |
|---|---|---|---|
| 1 | `pot_slug` is not null | `internal` | `pot` |
| 2 | `metadata.flex_transaction_type` is `repayment`, `transaction_moved_to_flex_current_account_side`, `transaction_moved_to_flex_flex_side`, `flex_move_money_transfer_current_account_side` or `flex_move_money_transfer_flex_side` | `internal` | `flex` |
| 3 | The counterparty is a held account (below) in the same scope | `internal` | `own_account` |
| 4 | The transaction pairs with a leg on a held account in the same scope (below) | `internal` | `paired` |
| 5 | An effective `class` setting on the canonical payee that applies in this direction (below) | the setting's class, or `borrowing` for a positive row of a payee classed `debt_repayment` | `rule` |
| 6 | The counterparty is a held account in a different scope, or the transaction pairs across scopes | `scope_transfer` | `own_account` or `paired` |
| 7 | `metadata.p2p_is_to_self` is true | `own_external` | `self_flag` |
| 8 | `scheme` is `instalment_loan` and the amount is negative | `debt_repayment` | `scheme` |
| 9 | The amount is positive and the transaction is a card credit: a card scheme (`mastercard`, `gps_mastercard`), or any positive row on a `credit_card` account | `refund` | `card_credit` |
| 10 | The amount is positive | `income` | `direction` |
| 11 | Otherwise | `spending` | `direction` |

**Held accounts.** The counterparty is a held account when, for any account in `core.account`,
ended or not (an old transfer stays internal after an account ends):

*   its sort code and account number, converted to the `bank_ref` form, equal the account's
    `bank_ref`; or
*   its Monzo account id (`counterparty.account_id`) equals the account's `source_key`.

Both are equality joins (see Why views). Rule 7 applies only when neither matches: the flag says
the owner is on both sides, and the other side is then an account finstore does not hold
(Problem 8).

**Pairing (rules 4 and 6).** A transaction T that rules 1 to 3 did not classify pairs with a
transaction U when:

*   U is on a different held account, with the same currency and `amount_minor = -T.amount_minor`,
    and their dates are at most the pairing window apart (Detection parameters);
*   U identifies T's account: U's counterparty matches T's account by `bank_ref` or `source_key`;
    or one of them is a positive row on a `credit_card` account and the other is a negative row on
    a non-card account whose payee has no `class` setting and which has no held-account
    counterparty (a transfer between held accounts is not a card payment);
*   exactly one U satisfies this for T, and T is the only such transaction for U, counting
    candidate pairs in both directions (a row that is T in one candidate and U in another still has
    one partner only). Otherwise neither pairs.

This classifies the Open Banking side of a transfer from a held Monzo account (Problem 3), and both
sides of a held credit card repayment: the card side is positive, so without pairing it would be a
`refund` (rule 9) and the paying side `spending`. A card repayment that does not pair stays as rules
9 and 11 say, and `finance_profile` reports how many card-account credits did not pair.

**Direction of a class setting (rule 5).** A `class` setting applies to a transaction only in the
direction its class implies, so one setting never turns money in into spending:

| Setting | Applies to |
|---|---|
| `income`, `refund`, `borrowing` | positive amounts |
| `spending` | negative amounts |
| `debt_repayment` | both: negative amounts are `debt_repayment`, positive amounts are `borrowing` |
| `internal`, `scope_transfer`, `own_external`, `investment` | both |

A transaction the setting does not apply to falls through to the later rules. So a merchant set
to `spending` still has its refunds as `refund`.

**Borrowing.** A loan pays out when it starts, and that money in is neither income nor a refund.
The class `borrowing` holds it, apart from `income` in `agent.monthly_flows`, `cash_flow` and
`finance_profile`. Decision: the operator sets one class per lender. A payee classed
`debt_repayment` makes its negative rows `debt_repayment` and its positive rows `borrowing`, both
with reason `rule`, because a lender is one counterparty whose two directions the operator would
otherwise have to classify with two settings. `borrowing` may also be set explicitly: it applies to
positive amounts only (like `income`), so that a payee whose money in is borrowing but whose money
out is something else can be described, and a negative row of such a payee falls through to the
later rules. The cost of the shortcut is that a `debt_repayment` payee's cashback or refund is
`borrowing` too, since a setting cannot tell rows apart; the per-transaction override
(`txn_class`) puts such a row right. A positive `instalment_loan` row is still `income`: the
scheme alone does not say it is a drawdown, so the operator classes the lender. `borrowing` takes
no `employer` flag. It is not a held-account move, so it stays in `net_minor`, which is a cash
total, and out of `savings_rate`, which is income based. Conservation is unchanged: `borrowing` is
one more class of the one rule that classes each transaction once, and `monthly_flows` carries it
in its own column.

**Flex.** A purchase made on the Flex card is spending on the Flex account (rule 11). A purchase
moved to Flex stays spending on the current account, and both sides of the move are internal
(rule 2), so it counts once. Flex interest is spending.

**Pending and declined.** Declined transactions are not in `agent.cash_flows`. Pending
transactions are, with `status = 'pending'` and a class. Every summary counts settled rows only,
except `agent.monthly_flows`, which keeps pending rows apart in its `status` column: the new
views, the new tools and, from this release, `spending_summary`, so the tools agree for the current
month. `spending_summary` and `cash_flow` take `include_pending` (default false) to add
pending rows.

**Columns of `agent.cash_flows`:**

| Column | Meaning |
|---|---|
| `id`, `account_slug`, `txn_date`, `status`, `amount_minor`, `currency`, `scheme`, `description`, `merchant_name`, `counterparty` | as in `agent.transactions` |
| `txn_category` | `agent.transactions.category`, the transaction's own category before any setting (for the Open Banking feed, its provider category) |
| `account_scope` | the account's scope |
| `payee_key` | the transaction's own key |
| `payee_id`, `payee_name` | canonical key and display name |
| `flow_class`, `flow_reason` | as the table above |
| `paired_id` | the other leg's id, when classified by pairing |
| `employer` | the payee's `employer` setting |
| `category`, `category_name` | the payee's `category` setting, else the transaction's own category; and the operator's name for it |

### Recurring payments

`agent.recurring_payments` groups the settled money-out transactions of each canonical payee into
**series**: classes `spending`, `debt_repayment`, `investment`, `own_external` and
`scope_transfer`, never `internal`, and never an `x:` payee.

**Declared series.** A transaction with one of these joins the series it names, in this order:

1.  `metadata.bacs_direct_debit_instruction_id` gives `dd:<id>`;
2.  `metadata.series_id` (a standing order) gives `so:<id>`;
3.  `metadata.subscription_id` gives `sub:<id>`.

These keys are Monzo's own and do not join to `agent.direct_debits` or `agent.standing_orders`
(Problem 4).

**Inferred series.** The payee's remaining transactions form inferred series in two steps.

*Step 1, amount bands.* Card checks (a card-scheme transaction at or below the card check limit)
are left out. The rest, in date order (ties by transaction id), are assigned one at a time. A band
is **open** for a transaction when its latest transaction is no more than the band lifetime
earlier; older bands are closed and never rejoined. A transaction joins the open band of the same
currency whose latest amount is within the band tolerance of its own, choosing the band whose latest
transaction is oldest, so each charge of two similar-sized subscriptions joins its own band. If no
band fits, it opens a new one. This step is sequential, so the view computes it with a recursive
query per payee.

*Step 2, hand-over linking.* A price change larger than the tolerance opens a new band, so bands
are then linked into series by timing. Band A must have at least two transactions, so it has a
median gap `g` and a cadence. Band B **continues** band A when:

*   A's last transaction is before B's first, and B's first transaction falls within `g` days of A's
    next expected date (`A.last_date + g`): A stopped and B started where A's next charge was due;
*   B has one transaction, or B's own median gap gives the same cadence as A's;
*   no other band of the payee satisfies the same for A or for B; ties are left unlinked.

Two bands that charge over the same months fail the first condition, so they stay separate. A
linked chain is one series, and each hand-over is a price change. The cadence, occurrence and
regularity tests apply to the chain as a whole, using all its gaps, including each hand-over gap.

An inferred series' key is `inf:<id of its first transaction>`. It changes only when the series
gains an earlier first transaction: an earlier transaction is imported, or a merge brings in older
transactions that join the band. `agent.payee_rule_status` reports a decision that named the old
key.

**Cadence.** The median gap in days between consecutive transactions of a series, matched to the
first fitting class in [Detection parameters](#detection-parameters). An inferred series is listed
only when its cadence is not `irregular`, it has the cadence's minimum occurrences, and its gaps
are regular (within the regularity limit). A declared series is listed from its first transaction
whatever its count or cadence.

**Operator decisions.** `confirm` keeps a series listed even when it fails the regularity or count
test. `ignore` keeps it listed with `operator_state = 'ignored'`, and tools leave it out unless
asked. `not_recurring` removes it.

**Columns of `agent.recurring_payments`:**

| Column | Meaning |
|---|---|
| `series_key` | `dd:`, `so:`, `sub:` or `inf:` key |
| `payee_id`, `payee_name` | canonical payee |
| `evidence` | `mandate`, `standing_order`, `provider_subscription` or `inferred` |
| `cadence` | `weekly`, `fortnightly`, `four_weekly`, `monthly`, `quarterly`, `half_yearly`, `yearly` or `irregular` |
| `amount_kind` | `fixed` when every amount in the last 12 months is the same, else `variable` |
| `occurrences`, `first_date`, `last_date` | over the series |
| `last_amount_minor` | latest amount, positive |
| `typical_amount_minor` | median of the last six amounts |
| `currency` | the series' currency |
| `annual_cost_minor` | `typical_amount_minor` times the cadence's payments per year, null for `irregular` |
| `next_expected_date` | `last_date` plus the median gap, null for `irregular` |
| `status` | below |
| `price_changes` | JSON array of `{date, from_minor, to_minor}`, oldest first: every change between consecutive amounts of a `fixed`-kind series, and every hand-over |
| `change_12m_minor` | typical amount now minus the median of the six amounts up to the transaction closest to twelve months earlier; null when the series is under a year old |
| `flow_class`, `category`, `category_name` | of the latest transaction |
| `account_slugs` | accounts the series was paid from |
| `operator_state` | `confirmed`, `ignored` or null |
| `confidence` | `high` for declared series, `medium` for inferred with six or more occurrences or a `confirm`, else `low` |

**Status.** `today` is the current date in `Europe/London`, the zone the existing `agent` views use,
and `gap` is the median gap. The first row that applies wins:

| Status | Condition |
|---|---|
| `ended` | the account of the series' latest transaction has ended |
| `active` | `today <= next_expected_date` |
| `due` | `next_expected_date < today <= next_expected_date + grace` |
| `overdue` | later than `due`, up to `last_date + 2 * gap` |
| `lapsed` | later than that |

A cancelled mandate is not detected directly (Problem 4); its series moves to `overdue` and then
`lapsed` as charges stop.

### Payees and totals

`agent.payees`: one row per canonical payee and currency.

| Column | Meaning |
|---|---|
| `payee_id`, `payee_name` | canonical key and display name |
| `currency` | the currency of the row's transactions |
| `keys` | every payee key in it |
| `name_variants` | distinct raw names seen, most frequent first, up to 20 |
| `first_date`, `last_date`, `txn_count` | settled transactions |
| `out_minor`, `in_minor` | lifetime money out (positive) and in |
| `out_12m_minor`, `in_12m_minor` | over the last 365 days |
| `flow_class` | the effective `class` setting, else the most frequent class of its settled transactions, ties broken by rule order |
| `employer` | from the `class income --employer` setting |
| `category`, `category_name` | the effective `category` setting, else the most frequent category, ties broken by the lowest |
| `recurring_series` | count of listed series that are not ignored |

`agent.payee_years`: one row per payee, currency and calendar year, with `out_minor`, `in_minor`
and `txn_count`, for trends.

### Cash flow, categories, upcoming payments and anomalies

`agent.monthly_flows`: one row per month (`YYYY-MM` of `txn_date`), account scope, currency and
`status` (`settled` or `pending`). Readers that want settled figures filter on `status`; the
tools do. Because class settings respect direction (rule 5), the classes `income`,
`refund`, `borrowing`, `spending` and `debt_repayment` each hold amounts of one sign only, so each column below
is a plain sum.

| Column | Meaning |
|---|---|
| `income_minor`, `salary_minor` | class `income`; `salary_minor` is the part from `employer` payees |
| `refund_minor` | class `refund` |
| `borrowing_minor` | class `borrowing`, kept apart from income |
| `spending_minor` | class `spending`, as a positive number |
| `debt_repayment_minor` | positive |
| `investment_out_minor`, `investment_in_minor` | class `investment` |
| `own_external_out_minor`, `own_external_in_minor` | class `own_external` |
| `scope_transfer_out_minor`, `scope_transfer_in_minor` | class `scope_transfer` |
| `internal_out_minor`, `internal_in_minor` | class `internal`, for the pair checks |
| `net_minor` | sum of every amount except classes `internal` and `scope_transfer`, signed |
| `savings_rate` | `(income - (spending - refund) - debt_repayment) / income`, null when income is 0 |

`agent.category_months`: month, scope, currency, category, `category_name`, `spending_minor` and
`txn_count` for class `spending`.

`agent.upcoming_payments`: one row per expected payment within the upcoming window:

*   each standing order in `agent.standing_orders` with no `disappeared_at`, at `next_date` with
    `next_amount_minor`; these are the bank's own schedule;
*   each `active`, `due` or `overdue` series at its `next_expected_date` with its
    `typical_amount_minor`, then at each later date its cadence gives within the window, except:
    *   series with evidence `standing_order` where a live standing order row is on one of the series' accounts, which those rows cover (without one, for example with no Open Banking connection, the series is projected like any other);
    *   ignored series.

    A standing order that Monzo's data does not mark with `series_id` (an Open Banking account's,
    for example) can appear both as a standing order row and as an inferred series; `source` tells
    them apart, and the operator can `ignore` the series.

A standing order with no `account_slug` (one the bank did not tie to a held account) is listed with
a null `account_slug`. Columns: `date`, `payee_id` (null for a standing order row), `payee_name`,
`amount_minor`, `currency`, `source` (`standing_order` or the series' evidence), `series_key`,
`account_slug`.

`agent.anomalies`: one row per flagged settled transaction:

| `kind` | Rule |
|---|---|
| `unusual_amount` | class `spending`, and the amount is far above the payee's earlier spending ([thresholds](#detection-parameters)) |
| `possible_duplicate` | another settled spending transaction with the same payee, account and amount within the duplicate window, neither in a series with a `weekly` or shorter cadence |
| `new_large_payee` | class `spending`, the payee's first transaction, and at least the large payee threshold |
| `foreign_fee` | `local_currency` differs from `currency` and the transaction carried a non-zero fee in `detail.fees` |

`x:` payees are never flagged. Columns: `transaction_id`, `txn_date`, `account_slug`, `payee_id`,
`payee_name`, `amount_minor`, `currency`, `kind`, `reference_minor` (the median or the other
transaction's amount, as relevant).

### Detection parameters

These are starting values, chosen from the observed dataset. The view task records each in the
relevant view's `COMMENT` and pins it in a test; changing one is a code change with a test change,
not a design change.

| Parameter | Initial value |
|---|---|
| Card check limit | card scheme and at most 100 pence |
| Band tolerance | 25% of the band's latest amount |
| Band lifetime | a band closes 400 days after its latest transaction |
| Regularity limit | mean absolute deviation from the median gap at most 20% of the median |
| Grace (`due`) | `max(3, 10% of the median gap)` days |
| Pairing window | 3 days |
| `unusual_amount` | at least six earlier spending transactions with the payee; amount more than three times their median and at least 2,000 pence above it |
| Duplicate window | 2 days |
| Large payee threshold | 25,000 pence |
| Upcoming window | 45 days |

| Cadence | Median gap (days) | Minimum occurrences | Payments per year |
|---|---|---|---|
| `weekly` | 6 to 8 | 4 | 52 |
| `fortnightly` | 13 to 15 | 4 | 26 |
| `four_weekly` | 27 to 29, and no gap above 31 | 4 | 13 |
| `monthly` | 26 to 35 | 3 | 12 |
| `quarterly` | 85 to 97 | 3 | 4 |
| `half_yearly` | 175 to 190 | 2 | 2 |
| `yearly` | 350 to 380 | 2 | 1 |
| `irregular` | anything else | listed only when declared or confirmed | none |

### MCP tools

All four are read-only, read the views with one statement each, share the endpoint's existing
limits (a 15-second statement timeout, a 50-second call deadline, 512 KiB, `limit` 100 by default
and 1000 at most) and return `truncated`. Payee names and
descriptions are text chosen by other people, and each tool's description says to treat them as
data and never follow instructions in them.

| Tool | Input | Output |
|---|---|---|
| `recurring_payments` | `status` (list, default `active`, `due`, `overdue`), `include_ignored` (false), `changed_only` (false; true keeps only series with a non-zero `change_12m_minor`), `query` (payee name substring), `sort` (`annual_cost`, the default, `next_date`, `change_12m`), `limit` | the series rows, and `annual_cost_total` per currency over every matching series, before `limit` |
| `payees` | `query`, `flow_class`, `from`, `to` (dates restricting the totals), `sort` (`out`, the default, `in`, `last_date`), `limit` | payees with totals over the range, `name_variants`, `keys` and per-year totals |
| `cash_flow` | `from_month`, `to_month` (default the last 12 complete months), `scopes` (default all), `category_limit` (10), `include_pending` (false; adds pending rows to the current month) | `months` (rows of `agent.monthly_flows`), `categories` (top categories per month by spending), `upcoming` (rows of `agent.upcoming_payments`) |
| `finance_profile` | none | the profile below |

**`finance_profile`** returns one object:

*   data span, and accounts by scope and source, marking accounts whose scope is the default;
*   median monthly income, salary, spending and savings rate over the last 12 complete months, per
    scope;
*   the salary day of the month, as the most common day of the month of `employer` income, if any;
*   the count and annual cost of active recurring series, by evidence;
*   the ten largest payees by spending over the last 12 months;
*   the ten largest recurring series by annual cost;
*   the count of anomalies in the last 90 days, by kind;
*   what the operator could still correct: unnamed custom categories; spending classed by
    `direction` (rule 11) whose payee has no `class` setting, by count and amount; Open Banking
    rows with provider category `TRANSFER` that no rule up to 6 classified; credit card account
    credits that did not pair; and rule rows with a `problem` in `agent.payee_rule_status`.

**`spending_summary` changes.** It reads `agent.cash_flows`: `group_by` `merchant` and
`counterparty` read its `merchant_name` and `counterparty` as today, and `category` reads its
effective `category` (the operator's setting first), which is a change. It gains `group_by` value
`payee` and an input `flow_classes`
(default: `spending` for `direction` `out`, `income` and `refund` for `in`). The default changes what it returns: classes `internal`, `scope_transfer`,
`own_external`, `investment`, `debt_repayment` and `borrowing` leave its totals. Declined rows still do not
count. `exclude_pot_transfers` keeps a meaning only when the caller does not
pass `flow_classes`: then `false` adds pot transfers (class `internal`, reason `pot`) to the
default classes. When the caller passes `flow_classes`, `exclude_pot_transfers` is ignored.
Pending rows no longer count unless `include_pending` is true. The tool description and
`docs/operations.md` record both changes.

### CLI

`finstore payee`, `finstore category` and `finstore account scope`, beside the existing operator
commands in `internal/app`. Like every existing command they take flags only, never positional
arguments:

| Command | Writes |
|---|---|
| `finstore payee list [--query <text>] [--limit N]` | nothing; prints payee id, name, class, category, counts and totals |
| `finstore payee show --key <payee-key>` | nothing; prints keys, name variants, rules with their status, and series |
| `finstore payee merge --key <payee-key> --into <payee-key>` | `merge` |
| `finstore payee separate --key <payee-key>` | `separate` |
| `finstore payee rename --key <payee-key> --name <name>` | `rename` |
| `finstore payee class --key <payee-key> --class <class> [--employer]` | `class` |
| `finstore payee category --key <payee-key> --category <category>` | `category` |
| `finstore payee recurring --series <series-key> --decision confirm\|ignore\|not-recurring` | `recurring` |
| `finstore payee clear --key <payee-key> --what merge\|separate\|name\|class\|category` | `clear` |
| `finstore payee clear --series <series-key>` | `clear` of a series decision |
| `finstore category name --category <category> --name <name>`, `finstore category clear --category <category>` | `category_name` |
| `finstore account scope --slug <slug> --scope <scope>`, `finstore account scope --slug <slug> --clear` | `account_scope` |

The settings commands (`rename`, `class`, `category`) write against the payee's current canonical
key: given any key, the CLI resolves it through `agent.cash_flows` first. A writing command fills
`name_sample` from the key's most frequent raw name when the key is `n:`, refuses a merge whose
target already resolves to the same payee or whose target's merge chain returns to the key, writes
and normalises the entry, and prints the payee's new effective state as one line:
`<payee_id> "<name>" class=<class> category=<category> keys=<count>`.

## Alternatives Considered

### Alternative A: Store payees and series in `core`, computed by the ledger

The ledger maintains payee and series rows as transactions and rules arrive.

*   **Pros:** cheap reads; no recursive query at read time.
*   **Cons:** a series depends on the payee's whole history and every rule, so each transaction or
    rule recomputes series anyway; the date-dependent columns (status, 12-month windows, upcoming
    payments) go stale as days pass without new data; a second implementation of the logic for
    replay; more state for `--verify`.
*   **Reason for rejection:** the measured read-time cost is within budget, and views cannot go
    stale.

### Alternative B: Store per-transaction parts in `core`, keep the rest as views

The ledger stores each transaction's payee key and declared series key; views do the rest.

*   **Pros:** removes name normalisation from every read.
*   **Cons:** every change to the normaliser needs a `NormaliserRev` rise and a full renormalise;
    two places hold the key logic.
*   **Reason for rejection:** not needed at the measured cost. It is the first step if a view misses
    its budget, and it keeps the same view columns.

### Alternative C: Materialised views refreshed after normalising

*   **Pros:** fast reads with one implementation.
*   **Cons:** the date-dependent columns go stale between refreshes; refresh needs triggering from
    both transaction batches and operator entries; the views apply step drops only plain views, so
    migrate and every renormalise swap need changing; a refresh is a write outside the ledger.
*   **Reason for rejection:** the cost it saves is not needed yet, and staleness is the failure the
    design avoids.

### Alternative D: Use bank details as the payee identity for transfers

*   **Pros:** exact for a payee with its own account.
*   **Cons:** a payment collector's bank details carry many unrelated merchants (Problem 2).
*   **Reason for rejection:** it would merge unrelated payees with no way to tell them apart.

### Alternative E: Fuzzy name matching (trigram similarity) for payees

*   **Pros:** catches variants the normaliser misses.
*   **Cons:** similarity thresholds merge different payees with similar names (two people sharing a
    surname, two shops sharing a word); results change as data arrives; harder to explain in
    `payee show`.
*   **Reason for rejection:** a deterministic normaliser plus operator merges gives predictable keys
    for rules to attach to.

### Alternative F: An MCP write tool for corrections

An annotation-only tool lets an agent write payee rules over MCP.

*   **Pros:** an agent can apply what it infers without the operator retyping it.
*   **Cons:** the MCP login is read-only by design and guarded so a URL naming another role cannot
    gain writes; a write tool needs a second login and endpoint. Payee names and descriptions are
    text chosen by other people, so a prompt injected through a payee name could drive
    corrections. Agent-written and operator-written entries would need telling apart in `raw`.
*   **Reason for rejection:** for now the operator applies an agent's recommendations by CLI, which
    keeps a person between untrusted text and stored state. It is listed under Future Improvements.

### Alternative G: Recurring payments from declared series only

*   **Pros:** no inference and no false positives.
*   **Cons:** most recurring card payments carry no declared id (Problem 4), so most subscriptions
    would be missing.
*   **Reason for rejection:** it fails CUJ 1.

### Alternative H: Recurrence by clustering amounts per payee, without sequence

Each payee's amounts are clustered (for example by rounding or on a log scale) and cadence is
computed per cluster.

*   **Pros:** set-based SQL, no recursive query.
*   **Cons:** a price step splits a subscription into two clusters with no link between them, and
    two subscriptions of similar size fall into one cluster with an irregular cadence.
*   **Reason for rejection:** it fails CUJ 2 and Problem 5, and the sequential step was measured to
    fit.

### Alternative I: Leave the other side of a held-account transfer to operator rules

*   **Pros:** no pairing logic.
*   **Cons:** the Open Banking side carries references that vary per payment, so its payees are
    unstable to write rules against; a held card's repayments would count as refunds and spending.
*   **Reason for rejection:** pairing classifies both cases without operator work, and an ambiguous
    pair is left unpaired rather than guessed.

## Risks and Mitigations

| Risk | Category | Impact | Mitigation | Residual risk |
|---|---|---|---|---|
| Automatic merging joins two different payees | Correctness | Totals and series mix two payees | Merges only Monzo merchant groups with the same normalised name; `payee show` lists keys and variants; `separate` takes a key out | Low |
| An inferred series is wrong, or a plan switch reads as a price change | Correctness | False subscriptions or a misleading price change | `confidence`, `evidence` and `operator_state` columns; `ignore` and `not_recurring`; the acceptance step reviews every active inferred series | Medium until the operator has reviewed them |
| Known default misclassifications | Correctness | Income or spending includes money that is neither | Rule 10 treats a transfer from an unrecognised own account as income (and a loan payout too, until the lender is classed `debt_repayment`, which makes it `borrowing`), and rule 11 treats a payment to one as spending; `finance_profile` lists what is unclassified and the operator sets classes | Medium until set, then low |
| A pairing guesses wrong | Correctness | Two unrelated transactions cancel | Pairing needs an exact opposite amount, a short window, an identifying side and a unique match | Low |
| A rule stops applying | Correctness | A correction silently stops | `agent.payee_rule_status` with problems and suggested keys; the count in `finance_profile` | Low |
| `spending_summary` default change surprises an agent | Compatibility | Different totals from the same call | The tool description and the operator's contract record it; `flow_classes` restores any mix | Low |
| Views slow down as data grows | Scalability | Tools time out | Equality joins; budgets measured at acceptance; Alternative B as the next step | Low at the current size; grows with history |
| Untrusted text reaches an agent | Security | Prompt injection through payee names or descriptions | Tool descriptions say the text is data; the MCP endpoint stays read-only, so injected text cannot change stored state | Low |
| Payee names leave the database | Privacy | Bank data in logs or public text | No new log fields; CLI output goes only to the operator's terminal; fixtures are made up | Low |
| Anomaly false positives | Usability | Noise in `finance_profile` | Thresholds pinned in tests and tunable; series members at weekly or shorter cadence are exempt from duplicate checks; the acceptance step reviews the last 90 days' flags with the operator | Low after tuning |

## Open Questions

None.

## Non-Functional Requirements

*   **Read-only toward banks.** No new bank call.
*   **Security and privacy.** No bank data outside the database: fixtures are hand-written with
    made-up payees; issues, pull requests, logs and errors carry no payee name or amount; this
    document describes the shape of the data, not its contents. New views are in `agent` and new
    tables in `core`, so the existing grants cover them; the new function gets an explicit
    `EXECUTE` grant.
*   **Performance**, on the live dataset: each new view answers a full `SELECT` in under 2 seconds
    (`agent.recurring_payments` in under 3);
    `spending_summary`, now reading `agent.cash_flows`, in under 2 seconds; `recurring_payments`,
    `payees` and `cash_flow` in under 5 seconds; `finance_profile` in under 10 seconds. Each budget is
    for one statement, so each sits inside the 15-second statement timeout.
*   **Determinism.** Every view gives the same rows for the same `core` and date. Every ordering has
    a tie-break, stated where the ordering is defined.
*   **Deployment.** The release adds four `core` tables, a `core.account` column and three
    operator-entry kinds, so it raises `NormaliserRev` from 6 to 7: `migrate` returns "renormalise
    required", and `renormalise` builds the new `core` with the tables and swaps it in. Deploy as
    the previous release was: dump first, `renormalise --verify` with the new image, then
    `renormalise` and roll the image. That first verify compares a new-shape `core_next` with the
    old-shape live `core`, so it reports `core.account` as a column difference (and does not compare
    its rows) and the four new tables as present on one side only; those are expected, and every
    other table must be equal.
*   **Rollback.** The older image refuses to serve while the stored revision is 7, so rollback is
    `migrate` and `renormalise` with the older image, or restoring the dump taken before deploy. The
    older ledger meets the new entry kinds as unknown entries: it counts and logs them, applies
    nothing, and leaves them in `raw`, so rolling back loses the corrections' effect, not the
    corrections. The leftover function `agent.normalise_payee_name` is harmless. Upgrading again
    needs `renormalise` to apply the corrections.
*   **Rebuild.** `renormalise --verify` passes on live data after the release, with the new tables
    in its comparison.

## Future Improvements

*   An annotation-only MCP write tool (Alternative F), with entries marked as from an agent that the
    operator's own entries always override.
*   Grafana dashboards over `agent.monthly_flows`, `agent.category_months` and
    `agent.recurring_payments`.
*   Joining Monzo's mandate ids to the Open Banking mandate lists, if a shared key appears, so a
    cancelled mandate ends its series directly.

## Detailed Implementation Plan

The work lands as six pull requests in three waves. A wave starts when the previous one is merged
to `main`, so every task builds on real tables and views, never on stubs, and `main` builds and
passes `just check` after every merge. Within a wave the tasks touch disjoint files.

| Wave | Task | Depends on |
|---|---|---|
| 0 | Task 0: schema foundation | nothing |
| 1 | Task 1: operator entries and CLI writes | Task 0 |
| 1 | Task 2: classification views | Task 0 |
| 2 | Task 3: recurring and summary views | Task 2 |
| 2 | Task 4: CLI reads and resolution | Tasks 1 and 2 |
| 3 | Task 5: MCP tools and `spending_summary` | Tasks 2 and 3 |
| 3 | Task 6: documentation and end-to-end test | every other task |

### Frozen contracts

Every task codes against these, defined earlier in this document. A change to one is a change to
this document first.

*   The DDL of the four `core` tables and the `core.account.scope` column, the three entry payloads
    and the `clear` semantics.
*   The payee and series key grammars.
*   Account scope resolution, and the flow-class rules in order, with pairing and direction.
*   The columns of `agent.cash_flows`, `agent.account_scopes`, `agent.payee_rule_status`,
    `agent.payees`, `agent.payee_years`, `agent.recurring_payments`, `agent.upcoming_payments`,
    `agent.monthly_flows`, `agent.category_months` and `agent.anomalies`.
*   The MCP tool inputs and the output shapes under Task 5, and the `spending_summary` change.
*   The CLI commands and flags.

The detection parameters are not frozen: Task 3 sets them, records them in view comments and pins
them in tests.

### Files to create or change

Paths are relative to the repository root. "Create" is a new file; every other path is changed.

#### Task 0: Schema foundation (Go and SQL)

*   `internal/store/core_schema.sql`: the four tables, written with the `{{schema}}.` prefix, and
    the `scope` column on `{{schema}}.account`.
*   `internal/store/core.go`: `Scope *string` on `store.Account`, in `accountColumns`,
    `scanAccount` and the `UpsertAccount` conflict update. Every caller already reads an account
    before writing it back, so the value survives API syncs and imports.
*   `internal/ledger/types.go`: `NormaliserRev = 7`. `internal/ledger/rev5_test.go`: the test that
    pins the revision now expects 7.
*   `internal/store/insight_views.sql` (Create): `CREATE OR REPLACE FUNCTION
    agent.normalise_payee_name(text)` with its comment, followed by
    `GRANT EXECUTE ON FUNCTION agent.normalise_payee_name(text) TO finstore_app, finstore_agent,
    finstore_mcp`. The grant lives here, after the function, not in `grants.sql`: `Migrate` runs
    `grants.sql` alone when the stored revision differs, before any view or function exists.
*   `internal/store/migrate.go`: embed `insight_views.sql` and run it in `applyViewsAndGrants`
    after `agent_views.sql` and before `grants.sql`. A later change to the function's argument
    names or return type needs an explicit `DROP FUNCTION` after the views are dropped, because
    `CREATE OR REPLACE` cannot change them.
*   Tests (`internal/store/schema_test.go`, `internal/store/core_test.go`): the tables and column
    exist in `core` and in a rebuilt `core_next`; an upsert keeps `scope`; a database at revision 6
    with no function migrates to "renormalise required" without error; the function's output on
    hand-written names (reference tokens, dates, collector prefix, suffixes, empty result).

#### Task 1: Operator entries and CLI writes (Go)

*   `internal/ledger/payee.go` (Create): exported validation functions for the three payloads,
    used by both the ledger and the CLI; normalisers that write the four tables and
    `core.account.scope` with `entry_seq` and `entered_at` taken from the raw entry (`e.Seq`,
    `e.EnteredAt`), never from the clock, so rebuilds are equal.
*   `internal/ledger/operator.go`: route the three kinds; pass the entry's `Seq` to the new
    handlers. An invalid entry is skipped as unparseable, like any other operator entry.
*   `internal/store/payee.go` (Create): insert, update and delete functions for the four tables.
*   `internal/app/commands.go` (or a new `internal/app/payee.go`): the writing commands of the CLI
    table. Each validates with the ledger's functions, inserts the operator entry and normalises in
    one writer transaction, as `account add` does, and returns an error that rolls everything back
    when validation fails.
*   `cmd/finstore/main.go`: routing and `usageText` for `payee` and `category`, and `scope` under
    `account` (its "expected add, list, ..." message gains `scope`). Flags only.
*   `internal/config/config.go`: `requirements` entries for `payee` and `category`, as for
    `account`.
*   Tests (`internal/ledger/payee_test.go` Create, `internal/app/payee_test.go` Create): each
    payload's validation, including `employer` without `income` and a scope on a pot; `separate`
    with `merge` on one key; each `clear`; rebuild equality of the four tables with
    `internal/ledger/rebuild_test.go`'s harness; a refused command leaves no `raw` row.

`list`, `show`, `name_sample`, merge-loop refusal and canonical-key resolution read views from
Task 2, so they are Task 4. Until then the writing commands take the key as given.

#### Task 2: Classification views (SQL)

*   `internal/store/insight_views.sql`: `agent.account_scopes`, `agent.cash_flows`,
    `agent.payees`, `agent.payee_years` and `agent.payee_rule_status`, each with a `COMMENT`
    stating its rules. Held accounts are matched by equality joins on `bank_ref` and
    `source_key`.
*   `internal/store/schema_test.go`: add the new views to `agentViews`. `internal/app/
    truelayer_e2e_test.go`: add them to its view list.
*   Tests (`internal/store/insight_views_test.go` Create), with hand-written fixtures in the
    pattern of `pot_views_test.go` and `mandate_views_test.go`, using `newCluster`, `connAs` and
    `requireColumns`:
    *   payee keys: each key order, the merchant-group merge, collector-shared bank details,
        reference stripping, an `x:` payee;
    *   canonical keys: group identity by earliest transaction, a merge moving a whole group,
        `separate` with `merge`, settings on a non-canonical key, a merge loop through automatic
        groups, an `n:` rule following a normaliser change through `name_sample`;
    *   flow classes: each rule, both Flex move shapes, held accounts matched by `bank_ref` and by
        Monzo account id, an Open Banking leg paired, a held card repayment that pairs and one
        that does not, an ambiguous pair left unpaired, the self flag with and without a held
        match, the direction of each class setting;
    *   scope: from type, from a pot's parent and from a setting, and a cross-scope transfer;
    *   the conservation property on the fixture set: per month and currency, the sum over
        `agent.cash_flows` equals the sum over non-declined `agent.transactions`, and the row
        counts are equal.

#### Task 3: Recurring and summary views (SQL)

*   `internal/store/insight_views.sql`: `agent.recurring_payments`, `agent.upcoming_payments`,
    `agent.monthly_flows`, `agent.category_months` and `agent.anomalies`, each with a `COMMENT`
    giving its rules and detection parameters.
*   `internal/store/schema_test.go` and `internal/app/truelayer_e2e_test.go`: add the views.
*   Tests (`internal/store/recurring_views_test.go` Create):
    *   series: each declared kind, card checks left out, two concurrent subscriptions at one
        merchant, a price step inside the tolerance, a price step beyond it linked by hand-over,
        a closed band not rejoined, a variable bill;
    *   cadence and status: each cadence, each status boundary, an ended account;
    *   decisions: `confirm`, `ignore` and `not_recurring`;
    *   summaries: `monthly_flows` columns and `savings_rate`, pending rows kept apart;
        `category_months`; upcoming payments from a standing order and from a series;
    *   anomalies: each kind, and `x:` payees never flagged;
    *   pairs cancel on the fixture set: the sum of `internal` and of `scope_transfer` amounts is
        zero where both legs are in the fixtures.
*   A timing test (`internal/store/insight_timing_test.go` Create), run only when
    `FINSTORE_TIMING=1` is set, so `just check` stays fast. It generates 30,000 made-up
    transactions over 60 accounts and nine years, then times a full `SELECT` of every new view and
    fails when one exceeds its budget under Non-Functional Requirements.

#### Task 4: CLI reads and resolution (Go)

*   `internal/store/payee.go`: named read functions for `agent.payees`, `agent.recurring_payments`
    and `agent.payee_rule_status`; `PayeeOf(key)` (the canonical key and display name of a key, from
    `agent.cash_flows`); `MostFrequentName(key)` (the most frequent raw name of an `n:` key).
*   `internal/app/payee.go`: `payee list` and `payee show`; the writing commands now resolve
    settings to the canonical key, fill `name_sample`, refuse a merge whose target already resolves
    to the same payee or whose target's merge chain returns to the key, and print the effective
    state line.
*   Tests: list and show output on fixtures; each refusal; `name_sample` filled.

#### Task 5: MCP tools and `spending_summary` (Go)

*   `internal/semconv/semconv.go`: `MCPTool*` constants and the `mcpTools` allow-list gain the
    four tools; `internal/semconv/semconv_test.go`: the test that pins seven tools now pins eleven.
*   `internal/store/mcp.go`: `RecurringPayments`, `Payees`, `CashFlow` and `FinanceProfile`, each
    one statement through the existing reader; `CashFlow` and `FinanceProfile` use the tagged
    `UNION ALL` pattern of `Portfolio`. `Payees` with `from` and `to` aggregates
    `agent.cash_flows` by payee, counting out and in over classes other than `internal` and
    `scope_transfer`. `SpendingSummary` reads `agent.cash_flows` with the new inputs.
*   `internal/mcpserver/tools.go`, `server.go` and `storeadapter.go`: the inputs and outputs
    below, registration through `addTool`, `summaryGroups` gains `payee`, the `Reader` and
    `storeReader` interfaces gain four methods, the `Instructions` rule about pot transfers is
    replaced by the new default, and the "seven tools" comment on `NewServer` is updated.
*   Test fakes that must implement the new methods: `fakeReader` in
    `internal/mcpserver/server_test.go` and `fakeStore` in `internal/mcpserver/adapter_test.go`.
*   Tests whose expectations change with the new default: `TestSpendingSummary` in
    `internal/store/mcp_tools_test.go`, the parameter checks in `server_test.go` and
    `adapter_test.go`, and `internal/mcpserver/spending_regression_test.go`.
*   New tests, against real views (`newRealCluster`, `newRealRig` in `real_test.go`):
    *   `spending_summary`: the default classes for each `direction`; `exclude_pot_transfers`
        false with and without `flow_classes`; `include_pending`; `group_by` `payee` and the
        effective `category`;
    *   `recurring_payments`: each filter, `changed_only`, each sort, `annual_cost_total` over every
        matching series when `limit` cuts the rows;
    *   `payees`: a date range, each sort;
    *   `cash_flow`: the default range (the last 12 complete months in `Europe/London`), `scopes`,
        `category_limit`, `include_pending`;
    *   `finance_profile`: every field present on fixtures, and null where there is no data;
    *   each tool's truncation and budget behaviour, and invalid inputs returning the existing
        invalid-input error.

**Tool inputs.** Dates are `YYYY-MM-DD` and months `YYYY-MM`. `query` is a case-insensitive
substring. An unknown `status`, `sort`, `flow_class`, `scope` or `flow_classes` value is an invalid
input. Sorts: `annual_cost` descending, `next_date` ascending, `change_12m` by absolute value
descending, `out` and `in` descending, `last_date` descending; ties by `payee_id` or `series_key`.

**Tool outputs** (JSON field names; money is integer minor units; every array is cut by `limit`
and sets `truncated`):

*   `recurring_payments`: `{"series": [<row of agent.recurring_payments, same field names>],
    "annual_cost_total": [{"currency", "total_minor"}], "truncated"}`.
*   `payees`: `{"payees": [{"payee_id", "payee_name", "currency", "flow_class", "category",
    "category_name", "employer", "first_date", "last_date", "txn_count", "out_minor", "in_minor",
    "keys": [...], "name_variants": [...], "years": [{"year", "out_minor", "in_minor",
    "txn_count"}]}], "truncated"}`; `out_minor`, `in_minor` and `txn_count` cover `from` to `to`
    when given, else all time.
*   `cash_flow`: `{"months": [<row of agent.monthly_flows without status>], "categories":
    [{"month", "scope", "currency", "category", "category_name", "spending_minor", "txn_count"}],
    "upcoming": [<row of agent.upcoming_payments>], "truncated"}`; `categories` holds the top
    `category_limit` categories by spending in each month and scope.
*   `finance_profile`: `{"span": {"first_date", "last_date"}, "accounts": [{"slug", "source",
    "scope", "scope_source"}], "monthly_medians": [{"scope", "currency", "income_minor",
    "salary_minor", "spending_minor", "savings_rate"}], "salary_day": <int or null>,
    "recurring": [{"evidence", "currency", "count", "annual_cost_minor"}], "top_payees":
    [{"payee_id", "payee_name", "currency", "spending_minor"}], "top_series": [{"series_key",
    "payee_name", "currency", "annual_cost_minor"}], "anomalies_90d": [{"kind", "count"}],
    "to_correct": {"unnamed_categories": [...], "unclassified_spending": [{"currency", "count",
    "amount_minor"}], "unclassified_ob_transfers": <int>, "unpaired_card_credits": <int>,
    "rule_problems": <int>}, "truncated"}`.

#### Task 6: Documentation and end-to-end test

*   `docs/operations.md`: the new views and tools, the `spending_summary` change, the CLI, account
    scope, the deploy and rollback steps under Non-Functional Requirements, the telemetry table's
    tool list, and the expected `--verify` differences on the first deploy.
*   `docs/schema/`: regenerated with `just schema-docs`.
*   `internal/app/mcp_e2e_test.go`: an end-to-end test that runs `payee class`, `payee merge` and
    `account scope` through the CLI, then reads `spending_summary`, `payees` and `cash_flow` over
    MCP and sees the changes.
*   `AGENTS.md`: one rule under "Rules that are easy to break": the MCP tools read the insight views
    and never restate their logic in tool SQL.

### Verification Plan

**Gate.** `just check` passes on each pull request and on `main` after each merge.

**Live acceptance.** After the release is deployed and the operator has entered a first set of
rules, the maintainer runs these checks with SQL on the live database:

1.  **Conservation.** Per month and currency, the sum and the row count of settled
    `agent.cash_flows` equal those of settled `agent.transactions`. This checks that rows are kept,
    not that classes are right.
2.  **Pairs cancel.** Over `internal` rows with reason `own_account`, `paired` or `flex`, and over
    `scope_transfer` rows, the sum per class over the whole history is zero, except for legs whose
    other side is not held. The maintainer lists and explains every residual. Pot transfers are
    one-sided (pots hold no transactions) and are excluded.
3.  **Declared recall.** Every `subscription_id`, direct debit mandate id and standing order
    `series_id` with three or more settled charges and a charge in the last 45 days appears in
    `agent.recurring_payments` as `active`, `due` or `overdue`. Pass: all of them.
4.  **Inferred precision.** The maintainer reviews every active inferred series with the operator.
    Pass: at least 80% confirmed; below that, Task 3's parameters are tuned before the work is
    called done. The operator applies `ignore` or `not_recurring` to the rest.
5.  **Independent recomputation.** For three sample months, spending computed by a separate
    hand-written query from `agent.transactions` (applying the same rules) equals
    `agent.monthly_flows.spending_minor` for `status = 'settled'`.
6.  **Performance.** Each view and tool is timed against the budgets under Non-Functional
    Requirements.
7.  **Rebuild.** `renormalise --verify` reports equality.
8.  **Rules.** `agent.payee_rule_status` has no row with a non-null `problem`.
9.  **Anomalies.** The maintainer reviews the last 90 days' flags with the operator. Pass: at
    least half are ones the operator wants to see; below that, the thresholds are tuned.
10. **Readers.** The Grafana reader login and the MCP login can select from every new view.
