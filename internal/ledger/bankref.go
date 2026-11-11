package ledger

import "strings"

// BankRef is an account's bank reference as core.account.bank_ref holds it: the sort code and the
// account number with every non-digit removed, joined by one space ("112233 12345678"), or empty
// when both are. Every source that knows an account's sort code and number (a statement, Monzo's
// /accounts, TrueLayer's /accounts) builds the reference with this function, so the same bank
// account gives the same value whichever source describes it.
func BankRef(sortCode, accountNumber string) string {
	digits := func(s string) string {
		return strings.Map(func(r rune) rune {
			if r >= '0' && r <= '9' {
				return r
			}
			return -1
		}, s)
	}
	sc, an := digits(sortCode), digits(accountNumber)
	if sc == "" && an == "" {
		return ""
	}
	return sc + " " + an
}
