package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// PayeeAlias is a core.payee_alias row: the merge and separate settings of one payee key.
type PayeeAlias struct {
	PayeeKey   string
	Separate   bool
	IntoKey    *string
	NameSample *string
	EntrySeq   int64
	EnteredAt  time.Time
}

// PayeeSetting is a core.payee_setting row. A nil field has no setting.
type PayeeSetting struct {
	PayeeKey   string
	Name       *string
	FlowClass  *string
	Employer   *bool
	Category   *string
	NameSample *string
	EntrySeq   int64
	EnteredAt  time.Time
}

// SeriesSetting is a core.series_setting row.
type SeriesSetting struct {
	SeriesKey string
	Recurring string
	EntrySeq  int64
	EnteredAt time.Time
}

// CategoryName is a core.category_name row.
type CategoryName struct {
	Category  string
	Name      string
	EntrySeq  int64
	EnteredAt time.Time
}

// TxnClass is a core.txn_class row: the operator's flow class for one transaction.
type TxnClass struct {
	TxnID     uuid.UUID
	FlowClass string
	Employer  bool
	EntrySeq  int64
	EnteredAt time.Time
}

const aliasColumns = `payee_key, separate, into_key, name_sample, entry_seq, entered_at`

// GetPayeeAlias returns the alias row of key, or ErrNotFound.
func (c Core) GetPayeeAlias(ctx context.Context, key string) (PayeeAlias, error) {
	var a PayeeAlias
	err := c.q.QueryRow(ctx, c.sql(`SELECT `+aliasColumns+` FROM core.payee_alias WHERE payee_key = $1`), key).
		Scan(&a.PayeeKey, &a.Separate, &a.IntoKey, &a.NameSample, &a.EntrySeq, &a.EnteredAt)
	return a, oneRow(err, "payee alias")
}

// PutPayeeAlias inserts the row, or replaces the row of the same key.
func (c Core) PutPayeeAlias(ctx context.Context, a PayeeAlias) error {
	_, err := c.q.Exec(ctx, c.sql(`INSERT INTO core.payee_alias (`+aliasColumns+`) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (payee_key) DO UPDATE SET separate = EXCLUDED.separate, into_key = EXCLUDED.into_key,
		  name_sample = EXCLUDED.name_sample, entry_seq = EXCLUDED.entry_seq, entered_at = EXCLUDED.entered_at`),
		a.PayeeKey, a.Separate, a.IntoKey, a.NameSample, a.EntrySeq, a.EnteredAt)
	if err != nil {
		return fmt.Errorf("store: put payee alias: %w", err)
	}
	return nil
}

// DeletePayeeAlias deletes the alias row of key, if any.
func (c Core) DeletePayeeAlias(ctx context.Context, key string) error {
	_, err := c.q.Exec(ctx, c.sql(`DELETE FROM core.payee_alias WHERE payee_key = $1`), key)
	if err != nil {
		return fmt.Errorf("store: delete payee alias: %w", err)
	}
	return nil
}

const settingColumns = `payee_key, name, flow_class, employer, category, name_sample, entry_seq, entered_at`

// GetPayeeSetting returns the setting row of key, or ErrNotFound.
func (c Core) GetPayeeSetting(ctx context.Context, key string) (PayeeSetting, error) {
	var s PayeeSetting
	err := c.q.QueryRow(ctx, c.sql(`SELECT `+settingColumns+` FROM core.payee_setting WHERE payee_key = $1`), key).
		Scan(&s.PayeeKey, &s.Name, &s.FlowClass, &s.Employer, &s.Category, &s.NameSample, &s.EntrySeq, &s.EnteredAt)
	return s, oneRow(err, "payee setting")
}

// PutPayeeSetting inserts the row, or replaces the row of the same key.
func (c Core) PutPayeeSetting(ctx context.Context, s PayeeSetting) error {
	_, err := c.q.Exec(ctx, c.sql(`INSERT INTO core.payee_setting (`+settingColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (payee_key) DO UPDATE SET name = EXCLUDED.name, flow_class = EXCLUDED.flow_class,
		  employer = EXCLUDED.employer, category = EXCLUDED.category, name_sample = EXCLUDED.name_sample,
		  entry_seq = EXCLUDED.entry_seq, entered_at = EXCLUDED.entered_at`),
		s.PayeeKey, s.Name, s.FlowClass, s.Employer, s.Category, s.NameSample, s.EntrySeq, s.EnteredAt)
	if err != nil {
		return fmt.Errorf("store: put payee setting: %w", err)
	}
	return nil
}

// DeletePayeeSetting deletes the setting row of key, if any.
func (c Core) DeletePayeeSetting(ctx context.Context, key string) error {
	_, err := c.q.Exec(ctx, c.sql(`DELETE FROM core.payee_setting WHERE payee_key = $1`), key)
	if err != nil {
		return fmt.Errorf("store: delete payee setting: %w", err)
	}
	return nil
}

// PutSeriesSetting inserts the row, or replaces the row of the same series key.
func (c Core) PutSeriesSetting(ctx context.Context, s SeriesSetting) error {
	_, err := c.q.Exec(ctx, c.sql(`INSERT INTO core.series_setting (series_key, recurring, entry_seq, entered_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (series_key) DO UPDATE SET recurring = EXCLUDED.recurring,
		  entry_seq = EXCLUDED.entry_seq, entered_at = EXCLUDED.entered_at`),
		s.SeriesKey, s.Recurring, s.EntrySeq, s.EnteredAt)
	if err != nil {
		return fmt.Errorf("store: put series setting: %w", err)
	}
	return nil
}

// DeleteSeriesSetting deletes the row of the series key, if any.
func (c Core) DeleteSeriesSetting(ctx context.Context, key string) error {
	_, err := c.q.Exec(ctx, c.sql(`DELETE FROM core.series_setting WHERE series_key = $1`), key)
	if err != nil {
		return fmt.Errorf("store: delete series setting: %w", err)
	}
	return nil
}

// PutTxnClass inserts the row, or replaces the row of the same transaction.
func (c Core) PutTxnClass(ctx context.Context, t TxnClass) error {
	_, err := c.q.Exec(ctx, c.sql(`INSERT INTO core.txn_class (txn_id, flow_class, employer, entry_seq, entered_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (txn_id) DO UPDATE SET flow_class = EXCLUDED.flow_class, employer = EXCLUDED.employer,
		  entry_seq = EXCLUDED.entry_seq, entered_at = EXCLUDED.entered_at`),
		t.TxnID, t.FlowClass, t.Employer, t.EntrySeq, t.EnteredAt)
	if err != nil {
		return fmt.Errorf("store: put txn class: %w", err)
	}
	return nil
}

// GetTxnClass returns the row of the transaction, or ErrNotFound.
func (c Core) GetTxnClass(ctx context.Context, id uuid.UUID) (TxnClass, error) {
	var t TxnClass
	err := c.q.QueryRow(ctx, c.sql(`SELECT txn_id, flow_class, employer, entry_seq, entered_at FROM core.txn_class WHERE txn_id = $1`), id).
		Scan(&t.TxnID, &t.FlowClass, &t.Employer, &t.EntrySeq, &t.EnteredAt)
	return t, oneRow(err, "txn class")
}

// DeleteTxnClass deletes the row of the transaction, if any.
func (c Core) DeleteTxnClass(ctx context.Context, id uuid.UUID) error {
	_, err := c.q.Exec(ctx, c.sql(`DELETE FROM core.txn_class WHERE txn_id = $1`), id)
	if err != nil {
		return fmt.Errorf("store: delete txn class: %w", err)
	}
	return nil
}

// PutCategoryName inserts the row, or replaces the row of the same category.
func (c Core) PutCategoryName(ctx context.Context, n CategoryName) error {
	_, err := c.q.Exec(ctx, c.sql(`INSERT INTO core.category_name (category, name, entry_seq, entered_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (category) DO UPDATE SET name = EXCLUDED.name,
		  entry_seq = EXCLUDED.entry_seq, entered_at = EXCLUDED.entered_at`),
		n.Category, n.Name, n.EntrySeq, n.EnteredAt)
	if err != nil {
		return fmt.Errorf("store: put category name: %w", err)
	}
	return nil
}

// DeleteCategoryName deletes the row of the category, if any.
func (c Core) DeleteCategoryName(ctx context.Context, category string) error {
	_, err := c.q.Exec(ctx, c.sql(`DELETE FROM core.category_name WHERE category = $1`), category)
	if err != nil {
		return fmt.Errorf("store: delete category name: %w", err)
	}
	return nil
}

func oneRow(err error, what string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: get %s: %w", what, err)
	}
	return nil
}
