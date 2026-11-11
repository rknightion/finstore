package truelayer

import (
	"encoding/json"
	"strings"
)

// cardPrefix begins the key of every card: card:<last four digits>. A card has no sort code and
// account number, so this stands in for the bank reference: it is recorded as the bank_ref request
// parameter of every per-card call, which is how the normaliser, the daily limit and the fetch's
// account key all key a card the way they key an account.
const cardPrefix = "card:"

// cardItem is a card the worker and the normaliser can both key: TrueLayer's id for this listing
// (not used as a key, since nothing promises it survives a reconnect), the card key, the name and
// currency the bank gave, and the object as listed.
type cardItem struct {
	ID          string
	Key         string
	DisplayName string
	Currency    string
	Raw         json.RawMessage
}

// cardList is what usableCards makes of a /cards body.
type cardList struct {
	// Cards are the cards that can be keyed, in listing order.
	Cards []cardItem
	// Keys are the keys of every card with usable digits, a skipped card's included, each once, in
	// listing order: they are the cards the bank listed.
	Keys []string
	// NoDigits counts the cards skipped for a partial number that does not end in four digits, and
	// Shared the cards skipped because another card in the list ends in the same four.
	NoDigits int
	Shared   int
}

// cardKey is the key of a card whose partial number ends in four digits, otherwise "".
func cardKey(partial string) string {
	partial = strings.TrimSpace(partial)
	if len(partial) < 4 {
		return ""
	}
	tail := partial[len(partial)-4:]
	for _, c := range []byte(tail) {
		if c < '0' || c > '9' {
			return ""
		}
	}
	return cardPrefix + tail
}

// usableCards turns a /cards body into the cards that can be keyed. It is the one place the two
// skip rules live, used by the worker to decide which cards to call and by the normaliser to decide
// which accounts to make, so the two cannot disagree: a card whose partial_card_number does not end
// in four digits is skipped, and so are all the cards of a list that share their last four (merging
// two cards into one account would be worse than having neither). A card with no id is an
// unexpected body, as an account with none is.
func usableCards(body []byte) (cardList, error) {
	items, err := decodeResults(body, pathCards)
	if err != nil {
		return cardList{}, err
	}
	var all []cardItem
	count := map[string]int{}
	var out cardList
	for _, raw := range items {
		var x struct {
			AccountID         string `json:"account_id"`
			DisplayName       string `json:"display_name"`
			Currency          string `json:"currency"`
			PartialCardNumber string `json:"partial_card_number"`
		}
		if decode(raw, &x) != nil || x.AccountID == "" {
			return cardList{}, errBody(pathCards, "a card has no id")
		}
		key := cardKey(x.PartialCardNumber)
		if key == "" {
			out.NoDigits++
			continue
		}
		count[key]++
		if count[key] == 1 {
			out.Keys = append(out.Keys, key)
		}
		all = append(all, cardItem{ID: x.AccountID, Key: key, DisplayName: x.DisplayName, Currency: x.Currency, Raw: raw})
	}
	for _, c := range all {
		if count[c.Key] > 1 {
			out.Shared++
			continue
		}
		out.Cards = append(out.Cards, c)
	}
	return out, nil
}
