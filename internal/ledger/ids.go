package ledger

import "github.com/google/uuid"

// namespace is the UUID every core id is derived under. It is fixed for the life of the
// database: changing it changes every account and transaction id, which sync refers to.
var namespace = uuid.MustParse("4c8a89ea-d117-4623-8d6b-b458877667ba")

// sep is U+001F, the separator inside an id's name.
const sep = "\x1f"

// SourceManual is the source of an account created by an account_add operator entry; its source
// key is the slug the operator gives.
const SourceManual = "manual"

// importSourcePrefix is prefixed to the import format to give an imported transaction's source.
const importSourcePrefix = "import:"

// AccountID is the core id of the account a source names by sourceKey: UUID version 5 of
// source + U+001F + sourceKey.
func AccountID(source, sourceKey string) uuid.UUID {
	return uuid.NewSHA1(namespace, []byte(source+sep+sourceKey))
}

// TransactionID is the core id of a transaction: UUID version 5 of the account id's text +
// U+001F + source + U+001F + sourceKey.
func TransactionID(accountID uuid.UUID, source, sourceKey string) uuid.UUID {
	return uuid.NewSHA1(namespace, []byte(accountID.String()+sep+source+sep+sourceKey))
}

// MandateID is the core id of a standing order or direct debit: UUID version 5 of source +
// U+001F + bank reference + U+001F + kind (MandateStandingOrder or MandateDirectDebit) + U+001F +
// mandate key.
func MandateID(source, bankRef, kind, mandateKey string) uuid.UUID {
	return uuid.NewSHA1(namespace, []byte(source+sep+bankRef+sep+kind+sep+mandateKey))
}
