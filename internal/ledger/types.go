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
	// Type is one of: current, joint, pot, savings, mortgage, flex, loan, rewards, business, prepaid,
	// credit_card.
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
	// Content is the source's whole account object, kept as core.account.detail, or nil when the
	// source gives none (the account keeps the detail it already has).
	Content json.RawMessage
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

// PendingRec is one pending transaction as a bank lists it. It never becomes a core.transaction:
// a pending row has another id than the settled row it turns into.
type PendingRec struct {
	// ID is the bank's id for the row, unique within its snapshot.
	ID string
	// Date is the calendar day of the row's timestamp as the bank wrote it, at midnight UTC.
	Date        time.Time
	AmountMinor int64
	Currency    string
	Description string
	// MerchantName is empty when the bank gives none.
	MerchantName string
	// Content is the whole row.
	Content json.RawMessage
}

// PendingSnapshot is the whole list of pending transactions a successful response gave for one
// account: the ledger replaces the account's pending rows with Rows, an empty list included (it says
// nothing was pending at AsOf).
type PendingSnapshot struct {
	// AccountSourceKey names the account by its source key.
	AccountSourceKey string
	// AsOf is when the response was fetched and RawSeq its raw row.
	AsOf   time.Time
	RawSeq int64
	Rows   []PendingRec
	// Skipped counts the rows the normaliser left out (no usable id, or an id already in the
	// snapshot). The ledger reports it in its skip log.
	Skipped int
}

// CardList says that a response was a complete list of a source's cards: Keys are the source keys
// of every card it listed, whether or not the normaliser made an account of it. The ledger warns of
// every card account of Source that has not ended and is not among Keys.
type CardList struct {
	Source string
	Keys   []string
}

// Result is everything one raw row yields.
type Result struct {
	Accounts       []AccountRec
	Txns           []TxnRec
	Balances       []BalanceRec
	StandingOrders []StandingOrderRec
	DirectDebits   []DirectDebitRec
	MandateLists   []MandateList
	Pending        []PendingSnapshot
	CardLists      []CardList
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

// Investment providers: the value of `account add --provider` and of InvestmentNormaliser.Provider.
const (
	ProviderFidelity = "fidelity"
	ProviderAviva    = "aviva"
)

// Products an InvestmentAccountRec names. A parser maps the provider's printed product to one of
// these. Each fixes the child account's type and the middle word of its slug.
const (
	ProductSIPP           = "sipp"    // Fidelity SIPP: type pension, slug <platform>-sipp-<last four>
	ProductISA            = "isa"     // Fidelity Investment ISA: type isa, slug <platform>-isa-<last four>
	ProductCashManagement = "cash"    // Fidelity Cash Management Account: type investment_cash, slug <platform>-cash-<last four>
	ProductPensionPlan    = "pension" // Aviva pension plan: type pension, slug <platform>-pension-<last four>
)

// Kinds of investment file: InvestmentResult.Kind and core.investment_import.kind. The kind fixes
// which parts of a result may be set; see InvestmentResult.
const (
	InvestmentKindLedger    = "ledger"
	InvestmentKindHoldings  = "holdings"
	InvestmentKindCapture   = "capture"
	InvestmentKindStatement = "statement"
	InvestmentKindTradeNote = "trade_note"
)

// Investment event types: core.investment_event.event_type.
const (
	EventContribution    = "contribution"
	EventBuy             = "buy"
	EventSell            = "sell"
	EventTransferIn      = "transfer_in"
	EventTransferOut     = "transfer_out"
	EventFee             = "fee"
	EventTax             = "tax"
	EventInterest        = "interest"
	EventWithdrawal      = "withdrawal"
	EventAdjustment      = "adjustment"
	EventSiblingTransfer = "sibling_transfer"
	EventInternal        = "internal"
)

// Price units: GBP is pounds, GBX is pence.
const (
	PriceUnitGBP = "GBP"
	PriceUnitGBX = "GBX"
)

// InvestmentAccountRec is one provider account a file names. The ledger creates a child account of
// the platform for it the first time it is seen.
type InvestmentAccountRec struct {
	// AccountKey is the provider's account number, exactly as every other record of the result
	// names it. It becomes the child's source_key; its last four alphanumeric characters,
	// lower-cased, end the child's slug, and a number with fewer than four rejects the file.
	AccountKey string
	// Product is one of the Product constants, and must belong to the normaliser's provider.
	Product string
}

// InvestmentEventRec is one provider ledger row. Dates are calendar days at midnight UTC.
type InvestmentEventRec struct {
	// AccountKey names the account by its InvestmentAccountRec.AccountKey.
	AccountKey string
	// SourceKey is the provider's reference, or for a row with none the SHA-256 key described in
	// the design. It is unique within an account.
	SourceKey    string
	EventType    string // an Event constant
	ProviderType string // the provider's own word for the row
	// InstrumentKey is the instrument's provider key, set exactly when Units is. Fidelity's is the
	// ticker; Aviva's is NormaliseFundName of the fund name.
	InstrumentKey  string
	InstrumentName string
	OrderDate      time.Time
	// SettleDate is the zero time when the row has none; the effective date is then OrderDate.
	SettleDate time.Time
	// Units is an unsigned decimal string, and event_type gives direction. Required on buy and sell,
	// allowed on transfer_in and transfer_out, refused on every other type. Empty on a cash row.
	Units string
	// Price and PriceUnit are set together or not at all, and only with Units.
	Price     string
	PriceUnit string
	// GrossMinor is the row's amount as printed; CashMinor its effect on cash; FlowMinor its value
	// as an external flow for return.
	GrossMinor int64
	CashMinor  int64
	FlowMinor  int64
	// Currency is empty for the platform's currency.
	Currency string
	// Content is the whole row as JSON, stored with each version. HashInput is the canonical
	// encoding the version hash is computed over: two records of one event with equal HashInput are
	// the same version, so it must not hold anything that varies between exports of the same row.
	Content   json.RawMessage
	HashInput []byte
}

// HoldingRec is one holding at this file's capture. The file's result must carry a ValuationRec for
// the same account and AsOf.
type HoldingRec struct {
	AccountKey     string
	InstrumentKey  string // as InvestmentEventRec.InstrumentKey
	InstrumentName string
	AsOf           time.Time // a calendar day at midnight UTC
	Units          string    // unsigned decimal string
	Price          string    // unsigned decimal string
	PriceUnit      string    // PriceUnitGBP or PriceUnitGBX
	ValueMinor     int64
	BookCostMinor  *int64
	Currency       string // empty for the platform's currency
}

// ValuationRec is one account's value at this file's capture.
type ValuationRec struct {
	AccountKey        string
	AsOf              time.Time // a calendar day at midnight UTC
	TotalMinor        int64
	CashMinor         int64
	ReportedGainMinor *int64
	Currency          string // empty for the platform's currency
}

// PensionStatementRec is one annual statement. Dates are calendar days at midnight UTC, and
// PeriodEnd is not before PeriodStart. Charges and Projections are JSON, stored as printed.
type PensionStatementRec struct {
	AccountKey   string
	PeriodStart  time.Time
	PeriodEnd    time.Time
	IssuedOn     time.Time
	OpeningMinor int64
	PaidInMinor  int64
	PaidOutMinor int64
	GrowthMinor  int64
	ChargesMinor *int64
	ClosingMinor int64
	Charges      json.RawMessage
	Projections  json.RawMessage
}

// TradeNoteRec is one trade confirmation. It is stored against the platform and its printed account
// number, even when no child or event matches yet, and never creates a child.
type TradeNoteRec struct {
	// AccountKey is the printed account number, with at least four alphanumeric characters. It need
	// not be in InvestmentResult.Accounts.
	AccountKey string
	// Reference is the SourceKey of the event the note confirms.
	Reference  string
	ISIN       string
	ExecutedAt time.Time
	Venue      string // empty when not printed
	Units      string // unsigned decimal string
	ExactPrice string // unsigned decimal string
	GrossMinor int64
	FeeMinor   int64
	TaxMinor   int64
	NetMinor   int64
	SettleDate time.Time // a calendar day at midnight UTC
}

// InvestmentResult is everything one investment file yields. A result breaking any rule below is
// rejected as bad_result before anything is written.
//
//   - InvestmentKindLedger: Events.
//   - InvestmentKindHoldings: Holdings and Valuations; CapturedAt required.
//   - InvestmentKindCapture: Holdings, Valuations, Events of type contribution and
//     CompleteContributionsFor; CapturedAt required.
//   - InvestmentKindStatement: Statements and Events of type transfer_out.
//   - InvestmentKindTradeNote: TradeNotes, and Accounts must be empty: a note is matched by number alone, so
//     it can never create a child.
//
// Every record's AccountKey, a trade note's excepted, must be listed in Accounts. Within one result
// an account has at most one valuation and one statement, an instrument at most one holding per
// account, and a source key or a trade reference at most one row per account. A file with no rows
// (an export of a quiet period) is valid and has an empty Accounts.
type InvestmentResult struct {
	Kind string
	// CapturedAt is the printed capture time, converted with the loc given to the normaliser.
	CapturedAt *time.Time
	Accounts   []InvestmentAccountRec
	Events     []InvestmentEventRec
	Holdings   []HoldingRec
	Valuations []ValuationRec
	Statements []PensionStatementRec
	TradeNotes []TradeNoteRec
	// CompleteContributionsFor names the accounts whose contribution list this file states in full.
	// Each must have a ValuationRec in this result, whose AsOf is the capture's date.
	CompleteContributionsFor []string
	// RowCount is the file's row count, reported to the operator and stored on investment_import.
	RowCount int
}

// InvestmentNormaliser turns one stored investment file into records. It is a pure function of the
// file's bytes and loc, and uses no clock: replay calls it again, and may call it for an earlier
// file while applying a later one. It returns an error, with no file content in it, when the file
// fails its own checks; the ledger then rejects the file as check_failed and stores no rows.
type InvestmentNormaliser interface {
	// Provider is ProviderFidelity or ProviderAviva. A file imports only against a platform of it.
	Provider() string
	NormaliseInvestmentFile(f File, loc *time.Location) (InvestmentResult, error)
}

// NormaliserRev is the revision of the normalising code. Bump it whenever a change to a
// normaliser or to the core schema would give different core rows for the same raw rows; a
// deployment must then run `finstore renormalise`.
const NormaliserRev = 6

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
	// child_key_shared, or bad_transaction (a row the schema would refuse). They are the words the
	// skip log uses.
	Rejections []string
	// PendingSnapshots counts the pending snapshots applied and PendingSkipped the rows their
	// normaliser left out.
	PendingSnapshots int
	PendingSkipped   int
	// CardsUnlisted maps a source to the number of its card accounts, not ended, that the newest card
	// list applied here left out. A source appears only when a card list for it was applied; when
	// several were, the last one counts.
	CardsUnlisted map[string]int
}
