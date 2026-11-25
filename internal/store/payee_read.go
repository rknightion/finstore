package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// The reads behind `finstore payee list`, `finstore payee show` and the resolution the writing
// commands do before they write. They read the agent views, never core directly, except the merge
// chain, which reads the stored merge targets.

// PayeeSummary is one row of agent.payees: a canonical payee in one currency.
type PayeeSummary struct {
	PayeeID      string
	Name         string
	Currency     string
	FlowClass    *string
	Category     *string
	Keys         []string
	NameVariants []string
	TxnCount     int64
	OutMinor     int64
	InMinor      int64
	Out12mMinor  int64
	In12mMinor   int64
}

const payeeSummaryColumns = `payee_id, payee_name, currency, flow_class, category, keys, coalesce(name_variants, '{}'),
	txn_count, out_minor, in_minor, out_12m_minor, in_12m_minor`

func scanPayeeSummary(row pgx.CollectableRow) (PayeeSummary, error) {
	var p PayeeSummary
	err := row.Scan(&p.PayeeID, &p.Name, &p.Currency, &p.FlowClass, &p.Category, &p.Keys, &p.NameVariants,
		&p.TxnCount, &p.OutMinor, &p.InMinor, &p.Out12mMinor, &p.In12mMinor)
	return p, err
}

// ListPayees returns the rows of agent.payees whose name contains query (case-insensitive; an empty
// query matches every payee), busiest first, at most limit rows.
func ListPayees(ctx context.Context, q DBTX, query string, limit int) ([]PayeeSummary, error) {
	rows, err := q.Query(ctx, `SELECT `+payeeSummaryColumns+` FROM agent.payees
		WHERE $1 = '' OR strpos(lower(payee_name), lower($1)) > 0
		ORDER BY txn_count DESC, payee_id, currency LIMIT $2`, query, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list payees: %w", err)
	}
	out, err := pgx.CollectRows(rows, scanPayeeSummary)
	if err != nil {
		return nil, fmt.Errorf("store: list payees: %w", err)
	}
	return out, nil
}

// PayeeRuleStatus is one row of agent.payee_rule_status.
type PayeeRuleStatus struct {
	RuleTable string
	Key       string
	Detail    string
	EnteredAt time.Time
	Matches   bool
	Problem   *string
}

// PayeeSeries is the part of a row of agent.recurring_payments that payee show prints.
type PayeeSeries struct {
	SeriesKey    string
	Cadence      string
	Status       string
	TypicalMinor *int64
	Currency     string
	OperatorMode *string
}

// PayeeDetail is everything payee show prints about one payee.
type PayeeDetail struct {
	PayeeID string
	Name    string
	Keys    []string
	Rows    []PayeeSummary
	Rules   []PayeeRuleStatus
	Series  []PayeeSeries
	// SeriesKnown is true when agent.recurring_payments exists, so an empty Series means none.
	SeriesKnown bool
}

// ShowPayee returns the payee that key belongs to. It returns ErrNotFound when no transaction
// gives the key a payee.
func ShowPayee(ctx context.Context, q DBTX, key string) (PayeeDetail, error) {
	id, name, ok, err := PayeeOf(ctx, q, key)
	if err != nil {
		return PayeeDetail{}, err
	}
	if !ok {
		return PayeeDetail{}, ErrNotFound
	}
	d := PayeeDetail{PayeeID: id, Name: name}
	rows, err := q.Query(ctx, `SELECT payee_key FROM agent.payee_keys WHERE payee_id = $1 ORDER BY payee_key`, id)
	if err != nil {
		return d, fmt.Errorf("store: show payee: %w", err)
	}
	if d.Keys, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		return d, fmt.Errorf("store: show payee: %w", err)
	}
	rows, err = q.Query(ctx, `SELECT `+payeeSummaryColumns+` FROM agent.payees WHERE payee_id = $1 ORDER BY currency`, id)
	if err != nil {
		return d, fmt.Errorf("store: show payee: %w", err)
	}
	if d.Rows, err = pgx.CollectRows(rows, scanPayeeSummary); err != nil {
		return d, fmt.Errorf("store: show payee: %w", err)
	}
	rows, err = q.Query(ctx, `SELECT rule_table, key, detail::text, entered_at, matches, problem
		FROM agent.payee_rule_status
		WHERE (rule_table IN ('payee_alias', 'payee_setting') AND key = ANY($1))
		   OR (rule_table = 'category_name' AND key IN
		       (SELECT category FROM agent.payees WHERE payee_id = $2 AND category IS NOT NULL))
		ORDER BY rule_table, key`, d.Keys, id)
	if err != nil {
		return d, fmt.Errorf("store: show payee: %w", err)
	}
	if d.Rules, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (PayeeRuleStatus, error) {
		var s PayeeRuleStatus
		err := r.Scan(&s.RuleTable, &s.Key, &s.Detail, &s.EnteredAt, &s.Matches, &s.Problem)
		return s, err
	}); err != nil {
		return d, fmt.Errorf("store: show payee: %w", err)
	}
	// Series come from agent.recurring_payments, which a database may not have yet.
	var exists bool
	if err := q.QueryRow(ctx, `SELECT to_regclass('agent.recurring_payments') IS NOT NULL`).Scan(&exists); err != nil {
		return d, fmt.Errorf("store: show payee: %w", err)
	}
	if !exists {
		return d, nil
	}
	d.SeriesKnown = true
	rows, err = q.Query(ctx, `SELECT series_key, cadence, status, typical_amount_minor, currency, operator_state
		FROM agent.recurring_payments WHERE payee_id = $1 ORDER BY series_key`, id)
	if err != nil {
		return d, fmt.Errorf("store: show payee: %w", err)
	}
	if d.Series, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (PayeeSeries, error) {
		var s PayeeSeries
		err := r.Scan(&s.SeriesKey, &s.Cadence, &s.Status, &s.TypicalMinor, &s.Currency, &s.OperatorMode)
		return s, err
	}); err != nil {
		return d, fmt.Errorf("store: show payee: %w", err)
	}
	return d, nil
}

// PayeeOf returns the canonical key and the display name of the payee key belongs to, read from
// agent.payee_keys and agent.cash_flows. ok is false when no transaction gives the key a payee; a
// caller then uses the key as it was given.
func PayeeOf(ctx context.Context, q DBTX, key string) (id, name string, ok bool, err error) {
	err = q.QueryRow(ctx, `SELECT k.payee_id,
			coalesce(k.setting_name, (SELECT c.payee_name FROM agent.cash_flows c WHERE c.payee_id = k.payee_id LIMIT 1), k.payee_id)
		FROM agent.payee_keys k WHERE k.payee_key = $1 AND k.txn_count > 0`, key).Scan(&id, &name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("store: payee of key: %w", err)
	}
	return id, name, true, nil
}

// PayeeState is the effective state of a payee, as the writing commands print it.
type PayeeState struct {
	PayeeID   string
	Name      string
	FlowClass string
	Category  string
	Keys      int
}

// EffectivePayeeState returns the state of the payee key belongs to now. A key no transaction or
// rule knows is its own payee.
func EffectivePayeeState(ctx context.Context, q DBTX, key string) (PayeeState, error) {
	st := PayeeState{PayeeID: key, Name: key, Keys: 1}
	var id string
	err := q.QueryRow(ctx, `SELECT payee_id FROM agent.payee_keys WHERE payee_key = $1`, key).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return st, nil
	}
	if err != nil {
		return st, fmt.Errorf("store: payee state: %w", err)
	}
	err = q.QueryRow(ctx, `SELECT $1::text,
		coalesce(max(k.setting_name), (SELECT c.payee_name FROM agent.cash_flows c WHERE c.payee_id = $1 LIMIT 1), $1),
		coalesce(max(k.setting_flow_class), (SELECT p.flow_class FROM agent.payees p WHERE p.payee_id = $1 ORDER BY p.currency LIMIT 1), ''),
		coalesce(max(k.setting_category), (SELECT p.category FROM agent.payees p WHERE p.payee_id = $1 ORDER BY p.currency LIMIT 1), ''),
		count(*)
		FROM agent.payee_keys k WHERE k.payee_id = $1`, id).Scan(&st.PayeeID, &st.Name, &st.FlowClass, &st.Category, &st.Keys)
	if err != nil {
		return st, fmt.Errorf("store: payee state: %w", err)
	}
	return st, nil
}

// MostFrequentName returns the raw name (counterparty, else description) most often seen on the
// transactions of the payee key, ties by the lowest name; empty when the key has no transaction.
func MostFrequentName(ctx context.Context, q DBTX, key string) (string, error) {
	var name string
	err := q.QueryRow(ctx, `SELECT n FROM (
			SELECT coalesce(nullif(counterparty, ''), nullif(description, '')) AS n, count(*) AS c
			FROM agent.cash_flows WHERE payee_key = $1 GROUP BY 1) x
		WHERE n IS NOT NULL ORDER BY c DESC, n LIMIT 1`, key).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: most frequent name: %w", err)
	}
	return name, nil
}

// maxMergeSteps matches the ten steps agent.payee_keys follows a merge chain.
const maxMergeSteps = 10

// MergeChainReturns reports whether merging key into target would close a loop: the target is in
// the key's own group, or following the stored merge of the target's group, and then of the group
// that points at, for at most ten steps, reaches the key's group. A target no transaction or rule
// knows ends the chain.
func MergeChainReturns(ctx context.Context, q DBTX, key, target string) (bool, error) {
	group := func(k string) (string, bool, error) {
		var g string
		err := q.QueryRow(ctx, `SELECT group_id FROM agent.payee_keys WHERE payee_key = $1`, k).Scan(&g)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, nil
		}
		return g, err == nil, err
	}
	own, ok, err := group(key)
	if err != nil {
		return false, fmt.Errorf("store: merge chain: %w", err)
	}
	if !ok {
		own = key
	}
	cur := target
	for range maxMergeSteps {
		g, ok, err := group(cur)
		if err != nil {
			return false, fmt.Errorf("store: merge chain: %w", err)
		}
		if !ok {
			return cur == key, nil
		}
		if g == own {
			return true, nil
		}
		// The merge of any member of the group moves the whole group; the latest entry wins.
		var next string
		err = q.QueryRow(ctx, `SELECT a.into_key FROM core.payee_alias a
			JOIN agent.payee_keys k ON k.payee_key = a.payee_key
			WHERE k.group_id = $1 AND a.into_key IS NOT NULL
			ORDER BY a.entry_seq DESC, a.payee_key LIMIT 1`, g).Scan(&next)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("store: merge chain: %w", err)
		}
		cur = next
	}
	return false, nil
}
