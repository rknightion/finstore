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
	// BankRef is the account's bank reference (BankRef of its sort code and account number), or
	// empty when the source gives none.
	BankRef string
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

// Mandate kinds: the two kinds of instruction that move money out of an account on a schedule.
const (
	MandateStandingOrder = "standing_order"
	MandateDirectDebit   = "direct_debit"
)

// StandingOrderRec is one standing order as a source describes it at one moment. Dates are
// calendar days at midnight UTC, or the zero time when the bank gives none; amounts are in minor
// units, or nil when the bank gives none.
type StandingOrderRec struct {
	// BankRef is the bank reference of the account the standing order is on. The ledger resolves
	// it to an account: the fetching source's account with that source key, otherwise the one
	// unended account of any source with that bank reference.
	BankRef string
	// MandateKey identifies the standing order within its source, bank reference and kind. Where
	// the bank gives no id it is built from the payee, reference and frequency. Records of one
	// Result that share a key are told apart by the ledger, which escapes every key ("\" to "\\",
	// "#" to "\#") and appends "#2", "#3" ... to all but the first after ordering them by first
	// payment date, then first amount.
	MandateKey string

	Payee            string
	Reference        string
	Frequency        string
	Status           string
	FirstDate        time.Time
	FirstAmountMinor *int64
	NextDate         time.Time
	NextAmountMinor  *int64
	FinalDate        time.Time
	FinalAmountMinor *int64
	Currency         string

	// Content is the whole source object, stored with each version.
	Content json.RawMessage
	// HashInput is the canonical encoding of the business fields the version hash is computed
	// over: never a bank's per-call id or a fetch time.
	HashInput []byte
}

// DirectDebitRec is one direct debit as a source describes it at one moment. Dates and amounts
// are as in StandingOrderRec.
type DirectDebitRec struct {
	// BankRef is resolved to an account as StandingOrderRec.BankRef is.
	BankRef string
	// MandateKey is the bank's mandate id where it gives one, otherwise the name. Records that
	// share a key are told apart as StandingOrderRec.MandateKey says, ordered by previous payment
	// date, then previous amount.
	MandateKey string

	Name                string
	Status              string
	PreviousDate        time.Time
	PreviousAmountMinor *int64
	Currency            string

	Content   json.RawMessage
	HashInput []byte
}

// MandateList says that a response was a complete list of one kind of mandate (MandateStandingOrder
// or MandateDirectDebit) for the account with BankRef. A normaliser emits one for every successful
// list response, empty or not; the ledger then marks that source's mandates of that kind and bank
// reference that the Result does not carry as disappeared. A response that is not a successful
// list carries no MandateList and so proves nothing.
type MandateList struct {
	Kind    string
	BankRef string
}

// Result is everything one raw row yields.
type Result struct {
	Accounts       []AccountRec
	Txns           []TxnRec
	Balances       []BalanceRec
	StandingOrders []StandingOrderRec
	DirectDebits   []DirectDebitRec
	MandateLists   []MandateList
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
	// AccountRef identifies the bank account the statement says it belongs to, or is empty when
	// the format names none. The ledger rejects a statement whose AccountRef differs from the one
	// the account's earlier imports set.
	AccountRef string
	// PeriodExact says FirstDate and LastDate are the statement's printed period, not just its
	// first and last row dates, so two statements whose periods do not abut have a gap between
	// them even when their balances join.
	PeriodExact bool
}

// FileNormaliser turns one stored import file into records. It returns an error when the file
// fails its own checks; the ledger then stores no rows for it.
type FileNormaliser interface {
	NormaliseFile(f File) (FileResult, error)
}

// NormaliserRev is the revision of the normalising code. Bump it whenever a change to a
// normaliser or to the core schema would give different core rows for the same raw rows; a
// deployment must then run `finstore renormalise`.
const NormaliserRev = 2

// Stats counts what one CatchUp did.
type Stats struct {
	RowsProcessed       int
	Unparseable         int
	AccountsCreated     int
	TransactionsCreated int
	TransactionsChanged int
	Disappeared         int
	Returned            int
	MandatesCreated     int
	MandatesChanged     int
	// MandatesUnresolved counts the mandate records applied with no account found for their bank
	// reference. Such a mandate is kept with no account until a later record, or a renormalise,
	// resolves it.
	MandatesUnresolved  int
	MandatesDisappeared int
	MandatesReturned    int
	FilesRejected       int
	// Rejections holds the fixed category of each rejected file, in raw seq order: one of
	// unknown_account, api_account, unknown_format, check_failed, bad_result, account_mismatch,
	// or bad_transaction (a row the schema would refuse). They are the words the skip log uses.
	Rejections []string
}
