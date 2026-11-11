// Package ledger owns the core schema: it turns raw rows into the normalised record.
//
// This file is the seam between the ledger and the packages that understand one source. A source
// package (for example the Monzo worker) implements SourceNormaliser; a statement parser is
// adapted to FileNormaliser. Both are pure: they read bytes and return records, and never touch
// the database. The ledger applies the records, which is how every source gets the same ids,
// versioning, running sums and checks.
package ledger

import (
	"encoding/json"
	"time"
)

// Status is a transaction's state as the source reports it.
type Status string

// The three states a transaction can be in.
const (
	StatusPending  Status = "pending"
	StatusSettled  Status = "settled"
	StatusDeclined Status = "declined"
)

// AccountRec is one account as a source describes it.
type AccountRec struct {
	// SourceKey is the source's own stable id for the account.
	SourceKey string
	// Type is one of: current, joint, pot, savings, mortgage, flex, loan, rewards, business, prepaid.
	Type     string
	Currency string
	Name     string
	// ParentSourceKey is set for a pot: the source key of the account it belongs to.
	ParentSourceKey string
	// OpenedOn is the opening date at midnight UTC, or the zero time when unknown.
	OpenedOn time.Time
	Closed   bool
}

// TxnRec is one transaction as a source describes it at one moment.
type TxnRec struct {
	// AccountSourceKey names the account by its source key.
	AccountSourceKey string
	// SourceKey is the source's own id for the transaction, or the import row key.
	SourceKey string

	AmountMinor      int64
	Currency         string
	LocalAmountMinor *int64
	LocalCurrency    string

	// CreatedAt and SettledAt are nil when the source gives none.
	CreatedAt *time.Time
	SettledAt *time.Time
	// TxnDate is the transaction's calendar day at midnight UTC.
	TxnDate time.Time

	Status       Status
	Description  string
	Counterparty string
	MerchantID   string
	MerchantName string
	Category     string
	Scheme       string
	Notes        string

	// Content is the whole source object, stored with each version.
	Content json.RawMessage
	// HashInput is the canonical encoding the version hash is computed over. Two records for the
	// same transaction with equal HashInput are the same version.
	HashInput []byte
}

// BalanceRec is one balance reading.
type BalanceRec struct {
	AccountSourceKey string
	// At is the instant of the reading; OnDate is used instead when the source gives a day only.
	At                time.Time
	OnDate            time.Time
	BalanceMinor      int64
	TotalBalanceMinor *int64
	Currency          string
}

// Result is everything one raw row yields.
type Result struct {
	Accounts []AccountRec
	Txns     []TxnRec
	Balances []BalanceRec
}

// Response is one stored bank response, as the normaliser sees it.
type Response struct {
	Seq       int64
	Endpoint  string
	Params    map[string]any
	Status    int
	Body      []byte
	FetchedAt time.Time
}

// SourceNormaliser turns one stored response from its source into records. It returns an empty
// Result, not an error, for a response that carries nothing to normalise (an error status, an
// endpoint it does not read). It returns an error only when a 2xx body cannot be understood; the
// ledger counts that row as unparseable and moves on.
type SourceNormaliser interface {
	NormaliseResponse(r Response, loc *time.Location) (Result, error)
}

// File is one stored import file, as the normaliser sees it.
type File struct {
	Seq         int64
	AccountSlug string
	Format      string
	Body        []byte
}

// FileResult is what a statement yields: its rows as transactions and balances, plus the two
// balances the ledger needs to check that this statement follows from what is already stored.
type FileResult struct {
	Txns     []TxnRec
	Balances []BalanceRec
	// OpeningMinor is the balance before the statement's first row; ClosingMinor the balance
	// after its last. FirstDate and LastDate bound the rows.
	OpeningMinor int64
	ClosingMinor int64
	FirstDate    time.Time
	LastDate     time.Time
}

// FileNormaliser turns one stored import file into records. It returns an error when the file
// fails its own checks; the ledger then stores no rows for it.
type FileNormaliser interface {
	NormaliseFile(f File) (FileResult, error)
}

// NormaliserRev is the revision of the normalising code. Bump it whenever a change to a
// normaliser or to the core schema would give different core rows for the same raw rows; a
// deployment must then run `finstore renormalise`.
const NormaliserRev = 1

// Stats counts what one CatchUp did.
type Stats struct {
	RowsProcessed       int
	Unparseable         int
	AccountsCreated     int
	TransactionsCreated int
	TransactionsChanged int
	Disappeared         int
	Returned            int
	FilesRejected       int
}
