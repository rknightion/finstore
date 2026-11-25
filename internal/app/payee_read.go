package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/rknightion/finstore/internal/store"
)

// The reading commands of `finstore payee`. Their output is the operator's terminal and goes to
// standard output only: payee names and totals are bank data and never reach a log line.

// DefaultPayeeListLimit is how many payees `payee list` prints without --limit.
const DefaultPayeeListLimit = 50

// PayeeList prints the payees whose name contains query (every payee for an empty query), the
// busiest first, at most limit rows.
func PayeeList(ctx context.Context, e Env, query string, limit int) int {
	return e.withTelemetry(ctx, "payee list", func(ctx context.Context, t cliTelemetry) int {
		if limit < 1 {
			return e.fail(errors.New("--limit must be at least 1"))
		}
		if err := e.Cfg.Require("payee"); err != nil {
			return e.fail(err)
		}
		st, err := store.Open(ctx, e.Cfg.DatabaseURL, storeTelemetry(t))
		if err != nil {
			return e.fail(err)
		}
		defer st.Close()
		rows, err := store.ListPayees(ctx, st.DB(), query, limit)
		if err != nil {
			return e.fail(err)
		}
		w := tabwriter.NewWriter(e.Stdout, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "PAYEE\tNAME\tCLASS\tCATEGORY\tCUR\tTXNS\tOUT\tIN\tOUT 12M\tIN 12M")
		for _, p := range rows {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n", p.PayeeID, p.Name, orDash(p.FlowClass),
				orDash(p.Category), p.Currency, p.TxnCount, money(p.OutMinor), money(p.InMinor), money(p.Out12mMinor), money(p.In12mMinor))
		}
		_ = w.Flush()
		return ExitOK
	})
}

// PayeeShow prints one payee: its keys, name variants, the rules that name its keys with their
// status, and its recurring series when the recurring payments view exists. The key may be any
// key of the payee.
func PayeeShow(ctx context.Context, e Env, key string) int {
	return e.withTelemetry(ctx, "payee show", func(ctx context.Context, t cliTelemetry) int {
		if err := e.Cfg.Require("payee"); err != nil {
			return e.fail(err)
		}
		st, err := store.Open(ctx, e.Cfg.DatabaseURL, storeTelemetry(t))
		if err != nil {
			return e.fail(err)
		}
		defer st.Close()
		d, err := store.ShowPayee(ctx, st.DB(), key)
		if errors.Is(err, store.ErrNotFound) {
			// The key can carry a name, so the refusal does not repeat it.
			return e.fail(errors.New("no transaction gives the --key a payee"))
		}
		if err != nil {
			return e.fail(err)
		}
		out := e.Stdout
		_, _ = fmt.Fprintf(out, "payee: %s\nname: %q\n", d.PayeeID, d.Name)
		for _, p := range d.Rows {
			_, _ = fmt.Fprintf(out, "%s: class=%s category=%s transactions=%d out=%s in=%s out_12m=%s in_12m=%s\n",
				p.Currency, orDash(p.FlowClass), orDash(p.Category), p.TxnCount, money(p.OutMinor), money(p.InMinor),
				money(p.Out12mMinor), money(p.In12mMinor))
		}
		_, _ = fmt.Fprintf(out, "keys: %s\n", strings.Join(d.Keys, ", "))
		_, _ = fmt.Fprintln(out, "names:")
		seen := map[string]bool{}
		for _, p := range d.Rows {
			for _, n := range p.NameVariants {
				if !seen[n] {
					seen[n] = true
					_, _ = fmt.Fprintf(out, "  %s\n", n)
				}
			}
		}
		_, _ = fmt.Fprintln(out, "rules:")
		for _, r := range d.Rules {
			status := "matches"
			if !r.Matches {
				status = "no match"
			}
			if r.Problem != nil {
				status += ", problem " + *r.Problem
			}
			_, _ = fmt.Fprintf(out, "  %s %s %s (%s, entered %s)\n", r.RuleTable, r.Key, r.Detail, status, r.EnteredAt.UTC().Format("2006-01-02"))
		}
		switch {
		case !d.SeriesKnown:
			_, _ = fmt.Fprintln(out, "series: not available (the recurring payments view does not exist yet)")
		case len(d.Series) == 0:
			_, _ = fmt.Fprintln(out, "series: none")
		default:
			_, _ = fmt.Fprintln(out, "series:")
			for _, s := range d.Series {
				typical := "-"
				if s.TypicalMinor != nil {
					typical = money(*s.TypicalMinor)
				}
				_, _ = fmt.Fprintf(out, "  %s %s %s typical=%s %s decision=%s\n", s.SeriesKey, s.Cadence, s.Status, typical, s.Currency, orDash(s.OperatorMode))
			}
		}
		return ExitOK
	})
}

func orDash(s *string) string {
	if s == nil || *s == "" {
		return "-"
	}
	return *s
}

// money formats pence as pounds and pence without a currency sign, for any currency.
func money(minor int64) string {
	sign := ""
	if minor < 0 {
		sign, minor = "-", -minor
	}
	return fmt.Sprintf("%s%d.%02d", sign, minor/100, minor%100)
}
