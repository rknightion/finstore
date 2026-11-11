package truelayertest

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"time"
)

// granted is the scopes the provider's consent grants.
func (p *provider) granted() []string {
	if p.scopes != nil {
		return p.scopes
	}
	return AllScopes
}

// SetScopes sets the scopes a provider's consent grants, as /data/v1/me and the token reply report
// them. A data call to a path that needs a scope outside them answers 403 access_denied. With no
// scopes given, none are granted.
func (f *Fake) SetScopes(providerID string, scopes ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prov(providerID).scopes = append([]string{}, scopes...)
}

// AddCard adds a credit card to provider and returns it.
func (f *Fake) AddCard(providerID string, c Card) *FakeCard {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c.Currency == "" {
		c.Currency = "GBP"
	}
	if c.DisplayName == "" {
		c.DisplayName = "Made Up Card"
	}
	p := f.prov(providerID)
	p.cardN++
	card := &FakeCard{ID: fmt.Sprintf("card-%02d", p.cardN), Card: c}
	p.cards = append(p.cards, card)
	return card
}

// RemoveCard removes a card from provider's listing, as when a card is replaced or closed.
func (f *Fake) RemoveCard(providerID string, c *FakeCard) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.prov(providerID)
	p.cards = slices.DeleteFunc(p.cards, func(x *FakeCard) bool { return x == c })
}

// SetCardBalance sets a card's current balance (the amount owed) and available credit, in minor
// units.
func (f *Fake) SetCardBalance(c *FakeCard, current, available int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c.current, c.available = current, available
}

// AddCardTransaction adds a transaction to a card. Empty ids get synthetic stable ones.
func (f *Fake) AddCardTransaction(c *FakeCard, t CardTxn) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.order++
	if t.NormalisedID == "" {
		t.NormalisedID = fmt.Sprintf("cnorm-%06d", f.order)
	}
	if t.ProviderID == "" {
		t.ProviderID = fmt.Sprintf("cprov-%06d", f.order)
	}
	t.Date = civil(t.Date)
	c.txs = append(c.txs, &cardTxn{CardTxn: t, order: f.order})
}

// AddPending adds a pending transaction to the account or card with the given id (the id an
// account was added with, or FakeCard.ID); both pending paths serve it.
func (f *Fake) AddPending(providerID, id string, t PendingTxn) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.prov(providerID)
	t.Date = civil(t.Date)
	if a := p.account(id); a != nil {
		a.pending = append(a.pending, t)
		return
	}
	for _, c := range p.cards {
		if c.ID == id {
			c.pending = append(c.pending, t)
			return
		}
	}
	panic("truelayertest: unknown account or card " + id)
}

func (f *Fake) listCards(p *provider, now time.Time) reply {
	f.listings++
	var out []map[string]any
	for _, c := range p.cards {
		served := fmt.Sprintf("%s-%04d", c.ID, f.listings)
		c.served = append(c.served, served)
		out = append(out, map[string]any{
			"account_id": served, "card_network": "VISA", "card_type": "CREDIT", "currency": c.Currency,
			"display_name": c.DisplayName, "partial_card_number": c.PartialNumber, "name_on_card": "A MADE UP",
			"valid_from": "2023-01-01", "valid_to": "2028-12-31", "update_timestamp": ts(now),
			"provider": map[string]any{"provider_id": p.id, "display_name": "Made Up Bank"},
		})
	}
	return results(out...)
}

func cardBalance(c *FakeCard, now time.Time) reply {
	return results(map[string]any{
		"currency": c.Currency, "available": Money(c.available), "current": Money(c.current),
		"credit_limit": Money(150000), "last_statement_balance": Money(c.current),
		"last_statement_date": "2026-10-31", "payment_due": Money(2500), "payment_due_date": "2026-11-25",
		"update_timestamp": ts(now),
	})
}

func (f *Fake) cardTransactions(p *provider, c *FakeCard, q url.Values) reply {
	from, okFrom := parseDay(q.Get("from"))
	to, okTo := parseDay(q.Get("to"))
	if !okFrom || !okTo || to.Before(from) || from.Before(f.earliest) {
		return oauthErr(http.StatusBadRequest, "invalid_date_range")
	}
	txs := slices.Clone(c.txs)
	slices.SortStableFunc(txs, func(x, y *cardTxn) int { return cmp.Or(x.Date.Compare(y.Date), cmp.Compare(x.order, y.order)) })
	var out []map[string]any
	for i, t := range txs {
		if t.Date.Before(from) || t.Date.After(to) || (!p.historyStart.IsZero() && t.Date.Before(p.historyStart)) {
			continue
		}
		item := map[string]any{
			// transaction_id changes on every call, as TrueLayer documents it may.
			"transaction_id": fmt.Sprintf("ctxn-%06d-%04d", f.calls, i), "timestamp": dateTS(t.Date),
			"description": t.Description, "amount": json.Number(t.Amount), "currency": c.Currency,
			"transaction_category": "PURCHASE", "transaction_classification": []string{},
			"normalised_provider_transaction_id": t.NormalisedID, "provider_transaction_id": t.ProviderID,
			"meta": map[string]any{"cardNumber": "XXXXXXXXXXXX" + c.PartialNumber},
		}
		if t.Type != "" {
			item["transaction_type"] = t.Type
		}
		out = append(out, item)
	}
	return results(out...)
}

// pendingRows serves pending transactions. transaction_id changes on every call.
func (f *Fake) pendingRows(rows []PendingTxn, currency, prefix string) reply {
	var out []map[string]any
	for i, r := range rows {
		item := map[string]any{
			"transaction_id": fmt.Sprintf("%s-%06d-%04d", prefix, f.calls, i), "timestamp": dateTS(r.Date),
			"description": r.Description, "amount": json.Number(r.Amount), "currency": currency,
			"transaction_category": "PURCHASE", "transaction_classification": []string{},
		}
		if r.Type != "" {
			item["transaction_type"] = r.Type
		}
		if r.MerchantName != "" {
			item["merchant_name"] = r.MerchantName
		}
		if r.NormalisedID != "" {
			item["normalised_provider_transaction_id"] = r.NormalisedID
		}
		if r.ProviderID != "" {
			item["provider_transaction_id"] = r.ProviderID
		}
		out = append(out, item)
	}
	return results(out...)
}
