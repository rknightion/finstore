package importer_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rknightion/finstore/internal/importer"
	"github.com/rknightion/finstore/internal/ledger"
	"github.com/rknightion/finstore/internal/store"
	"github.com/rknightion/finstore/internal/testpg"
)

// Every value here is made up and written by hand to match the documented shape of an
// aviva-capture-json file. Nothing is derived from a real capture.

// avivaGood is a valid capture: two funds worth 1735.50, three contributions adding up to 2250.00
// and two rows of value history.
const avivaGood = `{
  "schema_version": 1,
  "captured_at": "2030-02-01T12:00:00Z",
  "as_of": "2030-02-01",
  "plan_ref": "PLAN00001234",
  "total_value": "1735.50",
  "funds": [
    {"name": "Example Growth Fund", "units": "1000.0000", "price": "1.2345", "value": "1234.50", "allocation_percent": "71.1"},
    {"name": "  Sample   Bond Fund ", "units": "250.5000", "price": "2.0000", "value": "501.00", "allocation_percent": "28.9"}
  ],
  "contributions_complete": true,
  "contributions_total": "2250.00",
  "contributions": [
    {"date": "2030-01-05", "type": "Regular", "payer": "employer", "gross": "1000.00"},
    {"date": "2030-01-05", "type": "Regular", "payer": "member", "gross": "250.00"},
    {"date": "2029-12-05", "type": "Regular", "payer": "employer", "gross": "1000.00"}
  ],
  "value_history": [
    {"date": "2029-12-05", "contributions_to_date": "1000.00", "value": "1010.00"},
    {"date": "2030-01-05", "contributions_to_date": "2250.00", "value": "2301.00"}
  ]
}`

// avivaTokens are values of avivaGood that must never be echoed by an error.
var avivaTokens = []string{"PLAN00001234", "Example Growth Fund", "Sample", "1234.50", "1735.50", "1000.00", "250.00", "2250.00",
	"employer", "member", "Regular", "2030-01-05", "1010.00", "2301.00"}

// avivaMutate returns avivaGood decoded, changed by f and encoded again.
func avivaMutate(t *testing.T, f func(m map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(avivaGood), &m); err != nil {
		t.Fatal(err)
	}
	f(m)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func avivaRows(m map[string]any, key string) []any { return m[key].([]any) }

func avivaNormalise(t *testing.T, body []byte) (ledger.InvestmentResult, error) {
	t.Helper()
	return importer.AvivaCaptureNormaliser{}.NormaliseInvestmentFile(
		ledger.File{Format: string(importer.FormatAvivaCapture), Body: body}, time.UTC)
}

func avivaDate(s string) time.Time {
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return d
}

// AC1: schema version 1 is parsed as specified.
func TestAvivaCaptureParsesSchemaVersion1(t *testing.T) {
	c, err := importer.ParseAvivaCapture([]byte(avivaGood))
	if err != nil {
		t.Fatal(err)
	}
	if !c.CapturedAt.Equal(time.Date(2030, 2, 1, 12, 0, 0, 0, time.UTC)) || !c.AsOf.Equal(avivaDate("2030-02-01")) ||
		c.PlanRef != "PLAN00001234" || c.TotalMinor != 173550 || !c.ContributionsComplete || c.ContributionsMinor != 225000 {
		t.Fatalf("header %+v", c)
	}
	if len(c.Funds) != 2 || c.Funds[0] != (importer.AvivaFund{Name: "Example Growth Fund", Units: "1000.0000", Price: "1.2345",
		ValueMinor: 123450, AllocationPercent: "71.1"}) || c.Funds[1].Name != "Sample Bond Fund" || c.Funds[1].ValueMinor != 50100 {
		t.Fatalf("funds %+v", c.Funds)
	}
	if len(c.Contributions) != 3 || c.Contributions[1].Payer != "member" || c.Contributions[1].GrossMinor != 25000 ||
		!c.Contributions[2].Date.Equal(avivaDate("2029-12-05")) || c.Contributions[0].Type != "Regular" {
		t.Fatalf("contributions %+v", c.Contributions)
	}
	if len(c.ValueHistory) != 2 || c.ValueHistory[1].ContributionsToDate != 225000 || c.ValueHistory[1].ValueMinor != 230100 {
		t.Fatalf("value history %+v", c.ValueHistory)
	}
}

func TestAvivaCaptureNormalisesToACaptureResult(t *testing.T) {
	res, err := avivaNormalise(t, []byte(avivaGood))
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != ledger.InvestmentKindCapture || res.CapturedAt == nil || !res.CapturedAt.Equal(time.Date(2030, 2, 1, 12, 0, 0, 0, time.UTC)) ||
		res.RowCount != 7 || !reflect.DeepEqual(res.CompleteContributionsFor, []string{"PLAN00001234"}) {
		t.Fatalf("result %+v", res)
	}
	if !reflect.DeepEqual(res.Accounts, []ledger.InvestmentAccountRec{{AccountKey: "PLAN00001234", Product: ledger.ProductPensionPlan}}) {
		t.Fatalf("accounts %+v", res.Accounts)
	}
	if !reflect.DeepEqual(res.Valuations, []ledger.ValuationRec{{AccountKey: "PLAN00001234", AsOf: avivaDate("2030-02-01"), TotalMinor: 173550}}) {
		t.Fatalf("valuations %+v", res.Valuations)
	}
	want := []ledger.HoldingRec{
		{AccountKey: "PLAN00001234", InstrumentKey: "Example Growth Fund", InstrumentName: "Example Growth Fund", AsOf: avivaDate("2030-02-01"),
			Units: "1000.0000", Price: "1.2345", PriceUnit: ledger.PriceUnitGBP, ValueMinor: 123450},
		{AccountKey: "PLAN00001234", InstrumentKey: "Sample Bond Fund", InstrumentName: "Sample Bond Fund", AsOf: avivaDate("2030-02-01"),
			Units: "250.5000", Price: "2.0000", PriceUnit: ledger.PriceUnitGBP, ValueMinor: 50100},
	}
	if !reflect.DeepEqual(res.Holdings, want) {
		t.Fatalf("holdings %+v", res.Holdings)
	}
	if len(res.Events) != 3 {
		t.Fatalf("%d events, want one for each contribution", len(res.Events))
	}
	for i, e := range res.Events {
		if e.EventType != ledger.EventContribution || e.ProviderType != "Regular" || e.CashMinor != 0 || e.FlowMinor <= 0 ||
			e.GrossMinor != e.FlowMinor || !e.SettleDate.IsZero() || e.Units != "" || e.InstrumentKey != "" || e.Currency != "" ||
			e.AccountKey != "PLAN00001234" || string(e.Content) != string(e.HashInput) || !json.Valid(e.Content) {
			t.Errorf("event %d: %+v", i, e)
		}
	}
	if e := res.Events[1]; string(e.Content) != `{"date":"2030-01-05","gross_minor":25000,"payer":"member","type":"Regular"}` || e.FlowMinor != 25000 {
		t.Fatalf("event content %s", e.Content)
	}
}

// A normaliser is a pure function of the file: the ledger calls it again on stored captures, and the
// capture time carries its own offset, so the zone given changes nothing.
func TestAvivaCaptureNormaliserIsPure(t *testing.T) {
	a, err := avivaNormalise(t, []byte(avivaGood))
	if err != nil {
		t.Fatal(err)
	}
	zone := time.FixedZone("made-up", 5*3600)
	b, err := importer.AvivaCaptureNormaliser{}.NormaliseInvestmentFile(
		ledger.File{Seq: 99, AccountSlug: "other", Format: "aviva-capture-json", Body: []byte(avivaGood)}, zone)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatal("two runs of one file differ")
	}
	if n := (importer.AvivaCaptureNormaliser{}); n.Provider() != ledger.ProviderAviva {
		t.Fatal("provider")
	}
	// A capture time in another offset is the same instant.
	c, err := avivaNormalise(t, avivaMutate(t, func(m map[string]any) { m["captured_at"] = "2030-02-01T17:30:00+05:30" }))
	if err != nil {
		t.Fatal(err)
	}
	if !c.CapturedAt.Equal(*a.CapturedAt) || c.CapturedAt.Location() != time.UTC {
		t.Fatalf("captured at %v", c.CapturedAt)
	}
	if _, err := (importer.AvivaCaptureNormaliser{}).NormaliseInvestmentFile(ledger.File{Format: "halifax-pdf", Body: []byte(avivaGood)}, time.UTC); !errors.Is(err, importer.ErrFormat) {
		t.Fatalf("another format: %v", err)
	}
}

// AC1: an unknown field rejects the file, and so does every other departure from the schema. No
// error carries anything from the file.
func TestAvivaCaptureRejectsWhatTheSchemaDoesNotAllow(t *testing.T) {
	cases := map[string][]byte{
		"unknown top-level field": avivaMutate(t, func(m map[string]any) { m["extra"] = "x" }),
		"unknown fund field": avivaMutate(t, func(m map[string]any) {
			avivaRows(m, "funds")[0].(map[string]any)["isin"] = "x"
		}),
		"unknown contribution field": avivaMutate(t, func(m map[string]any) {
			avivaRows(m, "contributions")[0].(map[string]any)["reference"] = "x"
		}),
		"unknown history field": avivaMutate(t, func(m map[string]any) {
			avivaRows(m, "value_history")[0].(map[string]any)["growth"] = "1.00"
		}),
		"object where a scalar belongs": avivaMutate(t, func(m map[string]any) { m["plan_ref"] = map[string]any{"a": "b"} }),
		"key in another case":           []byte(strings.Replace(avivaGood, `"plan_ref"`, `"Plan_Ref"`, 1)),
		"repeated key":                  []byte(strings.Replace(avivaGood, `"schema_version": 1,`, `"schema_version": 1, "schema_version": 1,`, 1)),
		"repeated fund key":             []byte(strings.Replace(avivaGood, `"units": "1000.0000",`, `"units": "1000.0000", "units": "1000.0000",`, 1)),
		"trailing data":                 []byte(avivaGood + " {}"),
		"trailing garbage":              []byte(avivaGood + "x"),
		"byte order mark":               []byte("\xef\xbb\xbf" + avivaGood),
		"not an object":                 []byte(`[]`),
		"empty":                         {},
		"not JSON":                      []byte("plan_ref,total_value\nPLAN00001234,1735.50\n"),
		"truncated":                     []byte(avivaGood[:len(avivaGood)/2]),
		"version 0":                     avivaMutate(t, func(m map[string]any) { m["schema_version"] = 0 }),
		"version 2":                     avivaMutate(t, func(m map[string]any) { m["schema_version"] = 2 }),
		"version as text":               avivaMutate(t, func(m map[string]any) { m["schema_version"] = "1" }),
		"amount as number":              avivaMutate(t, func(m map[string]any) { m["total_value"] = 1735.5 }),
		"units as number": avivaMutate(t, func(m map[string]any) {
			avivaRows(m, "funds")[0].(map[string]any)["units"] = 1000
		}),
		"complete as text":      avivaMutate(t, func(m map[string]any) { m["contributions_complete"] = "true" }),
		"null funds":            avivaMutate(t, func(m map[string]any) { m["funds"] = nil }),
		"null fund":             avivaMutate(t, func(m map[string]any) { m["funds"] = []any{nil} }),
		"funds as object":       avivaMutate(t, func(m map[string]any) { m["funds"] = map[string]any{} }),
		"bad captured_at":       avivaMutate(t, func(m map[string]any) { m["captured_at"] = "2030-02-01 12:00:00" }),
		"captured_at no zone":   avivaMutate(t, func(m map[string]any) { m["captured_at"] = "2030-02-01T12:00:00" }),
		"bad as_of":             avivaMutate(t, func(m map[string]any) { m["as_of"] = "01/02/2030" }),
		"impossible as_of":      avivaMutate(t, func(m map[string]any) { m["as_of"] = "2030-02-30" }),
		"as_of one digit":       avivaMutate(t, func(m map[string]any) { m["as_of"] = "2030-2-1" }),
		"empty plan_ref":        avivaMutate(t, func(m map[string]any) { m["plan_ref"] = "" }),
		"short plan_ref":        avivaMutate(t, func(m map[string]any) { m["plan_ref"] = "AB-1" }),
		"padded plan_ref":       avivaMutate(t, func(m map[string]any) { m["plan_ref"] = " PLAN00001234" }),
		"control in plan_ref":   avivaMutate(t, func(m map[string]any) { m["plan_ref"] = "PLAN0000\n1234" }),
		"bad payer":             avivaMutate(t, func(m map[string]any) { avivaRows(m, "contributions")[0].(map[string]any)["payer"] = "Employer" }),
		"empty type":            avivaMutate(t, func(m map[string]any) { avivaRows(m, "contributions")[0].(map[string]any)["type"] = " " }),
		"bad contribution date": avivaMutate(t, func(m map[string]any) { avivaRows(m, "contributions")[0].(map[string]any)["date"] = "5 Jan 2030" }),
		"zero contribution": avivaMutate(t, func(m map[string]any) {
			avivaRows(m, "contributions")[0].(map[string]any)["gross"] = "0.00"
			m["contributions_total"] = "1250.00"
		}),
		"empty fund name": avivaMutate(t, func(m map[string]any) { avivaRows(m, "funds")[0].(map[string]any)["name"] = "  " }),
		"repeated fund after tidying": avivaMutate(t, func(m map[string]any) {
			avivaRows(m, "funds")[1].(map[string]any)["name"] = "Example  Growth   Fund"
		}),
		"bad history date":  avivaMutate(t, func(m map[string]any) { avivaRows(m, "value_history")[0].(map[string]any)["date"] = "x" }),
		"bad history value": avivaMutate(t, func(m map[string]any) { avivaRows(m, "value_history")[0].(map[string]any)["value"] = "1,010.00" }),
	}
	for _, key := range []string{"schema_version", "captured_at", "as_of", "plan_ref", "total_value", "funds", "contributions_complete",
		"contributions_total", "contributions", "value_history"} {
		cases["missing "+key] = avivaMutate(t, func(m map[string]any) { delete(m, key) })
	}
	for _, key := range []string{"name", "units", "price", "value", "allocation_percent"} {
		cases["fund without "+key] = avivaMutate(t, func(m map[string]any) { delete(avivaRows(m, "funds")[0].(map[string]any), key) })
	}
	for _, key := range []string{"date", "type", "payer", "gross"} {
		cases["contribution without "+key] = avivaMutate(t, func(m map[string]any) { delete(avivaRows(m, "contributions")[0].(map[string]any), key) })
	}
	for _, key := range []string{"date", "contributions_to_date", "value"} {
		cases["history row without "+key] = avivaMutate(t, func(m map[string]any) { delete(avivaRows(m, "value_history")[0].(map[string]any), key) })
	}
	// Amounts have exactly two decimals and plain digits; units and prices are plain decimals.
	for _, bad := range []string{"", "1", "1.5", "1.500", ".50", "-1.00", "+1.00", " 1.00", "1.00 ", "£1.00", "1,000.00", "1e3", "0x10", "NaN", "１.00", "99999999999999.00"} {
		cases["total_value "+bad] = avivaMutate(t, func(m map[string]any) { m["total_value"] = bad })
		cases["gross "+bad] = avivaMutate(t, func(m map[string]any) { avivaRows(m, "contributions")[0].(map[string]any)["gross"] = bad })
	}
	for _, bad := range []string{"", ".5", "1.", "-1", "1e3", "1,0", " 1", "1.12345678901", "99999999999999"} {
		cases["units "+bad] = avivaMutate(t, func(m map[string]any) { avivaRows(m, "funds")[0].(map[string]any)["units"] = bad })
		cases["price "+bad] = avivaMutate(t, func(m map[string]any) { avivaRows(m, "funds")[0].(map[string]any)["price"] = bad })
		cases["allocation "+bad] = avivaMutate(t, func(m map[string]any) { avivaRows(m, "funds")[0].(map[string]any)["allocation_percent"] = bad })
	}
	if len(cases) < 100 {
		t.Fatalf("only %d cases", len(cases))
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := importer.ParseAvivaCapture(body)
			if err == nil {
				t.Fatal("accepted")
			}
			var ce *importer.CheckError
			if !errors.Is(err, importer.ErrFormat) && !errors.As(err, &ce) {
				t.Fatalf("error %v is neither ErrFormat nor a CheckError", err)
			}
			for _, tok := range avivaTokens {
				if strings.Contains(err.Error(), tok) {
					t.Fatalf("error %q carries file content %q", err, tok)
				}
			}
			if _, err := avivaNormalise(t, body); err == nil {
				t.Fatal("the normaliser accepted it")
			}
		})
	}
}

// AC2: a Value, Total or contributions check that fails rejects the file.
func TestAvivaCaptureChecks(t *testing.T) {
	fund := func(i int, key, v string) func(m map[string]any) {
		return func(m map[string]any) { avivaRows(m, "funds")[i].(map[string]any)[key] = v }
	}
	type tc struct {
		name  string
		f     func(m map[string]any)
		check importer.CheckName // empty when the file is accepted
		row   int
	}
	for _, c := range []tc{
		{"good", func(map[string]any) {}, "", 0},
		// 1000.0000 units at 1.2345 is 1234.50 pounds. The price is known to half of 0.0001 pounds,
		// which across 1000 units is 5 pence, and the value is a rounded figure, half a penny, so the
		// value may differ by 5.5 pence at most (and a little more for the units' own rounding).
		{"value within rounding", func(m map[string]any) { fund(0, "value", "1234.55")(m); m["total_value"] = "1735.55" }, "", 0},
		{"value on the edge below", func(m map[string]any) { fund(0, "value", "1234.45")(m); m["total_value"] = "1735.45" }, "", 0},
		{"value past the edge", func(m map[string]any) { fund(0, "value", "1234.56")(m); m["total_value"] = "1735.56" }, importer.CheckAvivaValue, 1},
		{"value past the edge below", func(m map[string]any) { fund(0, "value", "1234.44")(m); m["total_value"] = "1735.44" }, importer.CheckAvivaValue, 1},
		{"value far out", func(m map[string]any) { fund(0, "value", "1334.50")(m); m["total_value"] = "1835.50" }, importer.CheckAvivaValue, 1},
		{"value of the second fund", func(m map[string]any) { fund(1, "value", "502.00")(m); m["total_value"] = "1736.50" }, importer.CheckAvivaValue, 2},
		{"a price printed to fewer places allows a wider gap", fund(0, "price", "1.23"), "", 0},
		{"but not an unbounded one", fund(0, "price", "1.20"), importer.CheckAvivaValue, 1},
		{"total off by a penny a holding", func(m map[string]any) { m["total_value"] = "1735.52" }, "", 0},
		{"total off by one more", func(m map[string]any) { m["total_value"] = "1735.53" }, importer.CheckAvivaTotal, 0},
		{"total off below", func(m map[string]any) { m["total_value"] = "1735.47" }, importer.CheckAvivaTotal, 0},
		{"total with no funds", func(m map[string]any) { m["funds"] = []any{} }, importer.CheckAvivaTotal, 0},
		{"no funds and no value", func(m map[string]any) { m["funds"] = []any{}; m["total_value"] = "0.00" }, "", 0},
		{"contributions short by a penny", func(m map[string]any) { m["contributions_total"] = "2250.01" }, importer.CheckAvivaContributions, 0},
		{"contributions over by a penny", func(m map[string]any) { m["contributions_total"] = "2249.99" }, importer.CheckAvivaContributions, 0},
		{"contributions of an incomplete list are checked too", func(m map[string]any) {
			m["contributions_complete"] = false
			m["contributions_total"] = "2300.00"
		}, importer.CheckAvivaContributions, 0},
		{"no contributions", func(m map[string]any) { m["contributions"] = []any{}; m["contributions_total"] = "0.00" }, "", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			body := avivaMutate(t, c.f)
			_, perr := importer.ParseAvivaCapture(body)
			_, nerr := avivaNormalise(t, body)
			if c.check == "" {
				if perr != nil || nerr != nil {
					t.Fatalf("rejected: %v, %v", perr, nerr)
				}
				return
			}
			var ce *importer.CheckError
			if !errors.As(perr, &ce) || ce.Check != c.check || ce.Row != c.row || nerr == nil {
				t.Fatalf("parse %v, normalise %v, want %s at %d", perr, nerr, c.check, c.row)
			}
			for _, tok := range []string{"1234", "1735", "2250", "PLAN"} {
				if strings.Contains(perr.Error(), tok) {
					t.Fatalf("error %q carries file content", perr)
				}
			}
		})
	}
}

// AC3: a contribution key is the SHA-256 of its date, type, payer and amount, then "#" and its place
// among identical rows, in the pattern of importer.Keys. It holds no plaintext amount or payer.
func TestAvivaCaptureContributionKeys(t *testing.T) {
	want := func(parts ...string) string {
		sum := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
		return hex.EncodeToString(sum[:])
	}
	res, err := avivaNormalise(t, []byte(avivaGood))
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{
		want("2030-01-05", "Regular", "employer", "100000") + "#1",
		want("2030-01-05", "Regular", "member", "25000") + "#1",
		want("2029-12-05", "Regular", "employer", "100000") + "#1",
	}
	shape := regexp.MustCompile(`^[0-9a-f]{64}#[0-9]+$`)
	for i, e := range res.Events {
		if e.SourceKey != keys[i] || !shape.MatchString(e.SourceKey) {
			t.Errorf("key %d is %q", i, e.SourceKey)
		}
		for _, plain := range []string{"employer", "member", "Regular", "2030-", "2029-", "1000.00", "250.00", "100000", "25000"} {
			if strings.Contains(e.SourceKey, plain) {
				t.Errorf("key %d holds %q", i, plain)
			}
		}
	}

	// Identical rows are told apart by position, and each field changes the key.
	twin := avivaMutate(t, func(m map[string]any) {
		row := avivaRows(m, "contributions")[0]
		m["contributions"] = append(avivaRows(m, "contributions"), row)
		m["contributions_total"] = "3250.00"
	})
	tr, err := avivaNormalise(t, twin)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Events[0].SourceKey != keys[0] || tr.Events[3].SourceKey != strings.TrimSuffix(keys[0], "#1")+"#2" {
		t.Fatalf("twin keys %q, %q", tr.Events[0].SourceKey, tr.Events[3].SourceKey)
	}
	seen := map[string]bool{}
	for _, mut := range []func(r map[string]any){
		func(r map[string]any) {},
		func(r map[string]any) { r["date"] = "2030-01-06" },
		func(r map[string]any) { r["type"] = "Single" },
		func(r map[string]any) { r["payer"] = "member" },
		func(r map[string]any) { r["gross"] = "1000.01" },
	} {
		body := avivaMutate(t, func(m map[string]any) {
			r := avivaRows(m, "contributions")[0].(map[string]any)
			mut(r)
			m["contributions_total"] = "2250.00"
			if r["gross"] != "1000.00" {
				m["contributions_total"] = "2250.01"
			}
		})
		r, err := avivaNormalise(t, body)
		if err != nil {
			t.Fatal(err)
		}
		if seen[r.Events[0].SourceKey] {
			t.Fatal("two different rows share a key")
		}
		seen[r.Events[0].SourceKey] = true
	}

	// The same payment keys the same in another capture, wherever it sits in the list.
	other := avivaMutate(t, func(m map[string]any) {
		rows := avivaRows(m, "contributions")
		m["contributions"] = []any{rows[2], rows[0]}
		m["contributions_total"] = "2000.00"
		m["captured_at"] = "2030-03-01T12:00:00Z"
	})
	or, err := avivaNormalise(t, other)
	if err != nil {
		t.Fatal(err)
	}
	if or.Events[0].SourceKey != keys[2] || or.Events[1].SourceKey != keys[0] {
		t.Fatalf("keys across captures %q, %q", or.Events[0].SourceKey, or.Events[1].SourceKey)
	}
}

// ---- the ledger, with this normaliser registered ----

type avivaEnv struct {
	t     *testing.T
	app   *store.Store
	l     *ledger.Ledger
	clock time.Time
	logs  *bytes.Buffer
}

func newAvivaEnv(t *testing.T) *avivaEnv {
	t.Helper()
	ctx := context.Background()
	super := testpg.Start(t)
	if err := store.Bootstrap(ctx, super, "owner-secret", "app-secret"); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	ownerURL := testpg.UserURL(t, super, store.RoleOwner)
	if err := store.Migrate(ctx, ownerURL, ledger.NormaliserRev); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	app, err := store.Open(ctx, testpg.UserURL(t, super, store.RoleApp))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	logs := &bytes.Buffer{}
	e := &avivaEnv{t: t, app: app, clock: time.Date(2030, time.April, 1, 9, 0, 0, 0, time.UTC), logs: logs}
	e.l = ledger.New(nil, map[string]ledger.FileNormaliser{}, time.UTC, slog.New(slog.NewJSONHandler(logs, nil)), nil,
		ledger.WithInvestmentNormalisers(map[string]ledger.InvestmentNormaliser{
			string(importer.FormatAvivaCapture): importer.AvivaCaptureNormaliser{},
		}))
	return e
}

func (e *avivaEnv) catchUp() ledger.Stats {
	e.t.Helper()
	var stats ledger.Stats
	err := e.app.WithWriterLock(context.Background(), store.HolderTest, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		stats, err = e.l.CatchUp(ctx, tx, store.SchemaCore)
		return err
	})
	if err != nil {
		e.t.Fatalf("CatchUp: %v", err)
	}
	return stats
}

func (e *avivaEnv) platform(slug string) {
	e.t.Helper()
	p, err := json.Marshal(ledger.AccountAdd{Slug: slug, Type: "platform", Currency: "GBP", Name: "Made-up", ExpectsImports: true,
		Provider: ledger.ProviderAviva})
	if err != nil {
		e.t.Fatal(err)
	}
	e.clock = e.clock.Add(time.Minute)
	if _, err := store.InsertOperatorEntry(context.Background(), e.app.DB(),
		store.OperatorEntry{Kind: ledger.EntryAccountAdd, Payload: p, EnteredAt: e.clock}); err != nil {
		e.t.Fatal(err)
	}
	e.catchUp()
}

// importCapture stores body as a file for the platform, imported a minute after the last, and
// normalises it.
func (e *avivaEnv) importCapture(platform string, body []byte) ledger.Stats {
	e.t.Helper()
	e.clock = e.clock.Add(time.Minute)
	if _, _, err := store.InsertImportFile(context.Background(), e.app.DB(), store.ImportFile{AccountSlug: platform,
		Format: string(importer.FormatAvivaCapture), FileName: "made-up", ImportedAt: e.clock, Body: body}); err != nil {
		e.t.Fatal(err)
	}
	return e.catchUp()
}

func (e *avivaEnv) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.app.DB().QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// disappeared maps each of the platform's contributions, named "date amount", to whether it has
// disappeared.
func (e *avivaEnv) disappeared(platform string) map[string]bool {
	e.t.Helper()
	rows, err := e.app.DB().Query(context.Background(), `SELECT e.order_date::text || ' ' || e.gross_minor::text, e.disappeared_at IS NOT NULL
		FROM core.investment_event e JOIN core.account a ON a.id = e.account_id JOIN core.account p ON p.id = a.parent_id
		WHERE p.slug = $1`, platform)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var k string
		var gone bool
		if err := rows.Scan(&k, &gone); err != nil {
			e.t.Fatal(err)
		}
		out[k] = gone
	}
	if err := rows.Err(); err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *avivaEnv) wantGone(platform string, want map[string]bool) {
	e.t.Helper()
	got := e.disappeared(platform)
	if !reflect.DeepEqual(got, want) {
		e.t.Fatalf("disappeared: got %v, want %v", got, want)
	}
}

// avivaPay is one contribution of a built capture.
type avivaPay struct {
	date  string
	payer string
	pence int64
}

func (p avivaPay) name() string { return fmt.Sprintf("%s %d", p.date, p.pence) }

// avivaFile builds a valid capture of a plan with one fund worth the whole total.
func avivaFile(t *testing.T, plan, capturedAt, asOf string, complete bool, pays ...avivaPay) []byte {
	t.Helper()
	var total int64
	rows := []any{}
	for _, p := range pays {
		total += p.pence
		rows = append(rows, map[string]any{"date": p.date, "type": "Regular", "payer": p.payer, "gross": avivaPounds(p.pence)})
	}
	b, err := json.Marshal(map[string]any{
		"schema_version": 1, "captured_at": capturedAt, "as_of": asOf, "plan_ref": plan, "total_value": "1000.00",
		"funds": []any{map[string]any{"name": "Example Growth Fund", "units": "1000.0000", "price": "1.0000", "value": "1000.00",
			"allocation_percent": "100"}},
		"contributions_complete": complete, "contributions_total": avivaPounds(total), "contributions": rows,
		"value_history": []any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func avivaPounds(pence int64) string { return fmt.Sprintf("%d.%02d", pence/100, pence%100) }

var (
	payA = avivaPay{"2029-12-05", "employer", 100000}
	payB = avivaPay{"2030-01-05", "member", 25000}
	payC = avivaPay{"2030-01-05", "employer", 100000}
)

// A good capture passes the ledger's own result rules and makes the plan's child account, holdings,
// valuation and contributions. value_history stays in raw and gives no core row (AC5).
func TestAvivaCaptureIsStoredAndValueHistoryStaysInRaw(t *testing.T) {
	e := newAvivaEnv(t)
	e.platform("avi")
	body := []byte(avivaGood)
	if st := e.importCapture("avi", body); st.FilesRejected != 0 || st.Unparseable != 0 {
		t.Fatalf("stats %+v", st)
	}
	if n := e.count(`SELECT count(*) FROM core.account WHERE slug = 'avi-pension-1234' AND type = 'pension' AND source_key = 'PLAN00001234'`); n != 1 {
		t.Fatal("no child account")
	}
	if n := e.count(`SELECT count(*) FROM core.investment_event WHERE event_type = 'contribution' AND cash_minor = 0 AND flow_minor > 0`); n != 3 {
		t.Fatalf("%d contributions", n)
	}
	if n := e.count(`SELECT count(*) FROM core.holding_snapshot`); n != 2 {
		t.Fatalf("%d holdings", n)
	}
	var total, holdings int64
	if err := e.app.DB().QueryRow(context.Background(), `SELECT total_minor, (SELECT sum(value_minor)::bigint FROM core.holding_snapshot) FROM core.valuation`).
		Scan(&total, &holdings); err != nil || total != 173550 || holdings != 173550 {
		t.Fatalf("valuation %d, holdings %d, %v", total, holdings, err)
	}
	if n := e.count(`SELECT count(*) FROM core.valuation`); n != 1 {
		t.Fatalf("%d valuations", n)
	}
	// Nothing of the value history, which has two rows, reaches any core row.
	for _, table := range []string{"investment_event", "holding_snapshot", "valuation", "instrument", "pension_statement", "trade_note"} {
		var n int
		if err := e.app.DB().QueryRow(context.Background(), fmt.Sprintf(
			`SELECT count(*) FROM core.%s t WHERE t::text ~ '(1010\.00|2301\.00|101000|230100)'`, table)).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s holds value history: %d, %v", table, n, err)
		}
	}
	if n := e.count(`SELECT row_count FROM core.investment_import`); n != 7 {
		t.Fatalf("row count %d", n)
	}
	var stored []byte
	if err := e.app.DB().QueryRow(context.Background(), `SELECT body FROM raw.import_file`).Scan(&stored); err != nil || !bytes.Equal(stored, body) {
		t.Fatalf("raw body: %v", err)
	}
	res, err := avivaNormalise(t, stored)
	if err != nil || len(res.Events) != 3 || len(res.Holdings) != 2 || len(res.Valuations) != 1 || len(res.Statements) != 0 || len(res.TradeNotes) != 0 {
		t.Fatalf("value history produced records: %+v, %v", res, err)
	}
}

// AC2: a file that fails a check is rejected as check_failed, which the import command reports as
// exit 2, and stores no rows.
func TestAvivaCaptureThatFailsACheckStoresNothing(t *testing.T) {
	e := newAvivaEnv(t)
	e.platform("avi")
	for name, body := range map[string][]byte{
		"value": avivaMutate(t, func(m map[string]any) {
			avivaRows(m, "funds")[0].(map[string]any)["value"] = "1334.50"
			m["total_value"] = "1835.50"
		}),
		"total":                avivaMutate(t, func(m map[string]any) { m["total_value"] = "1735.60" }),
		"contributions total":  avivaMutate(t, func(m map[string]any) { m["contributions_total"] = "2250.01" }),
		"unknown field":        avivaMutate(t, func(m map[string]any) { m["extra"] = 1 }),
		"not a plan reference": avivaMutate(t, func(m map[string]any) { m["plan_ref"] = "A-1" }),
	} {
		t.Run(name, func(t *testing.T) {
			e.t = t
			st := e.importCapture("avi", body)
			if st.FilesRejected != 1 || !reflect.DeepEqual(st.Rejections, []string{"check_failed"}) {
				t.Fatalf("stats %+v", st)
			}
			for _, table := range []string{"investment_event", "holding_snapshot", "valuation", "instrument", "investment_import"} {
				if n := e.count(`SELECT count(*) FROM core.` + table); n != 0 {
					t.Fatalf("%s has %d rows", table, n)
				}
			}
			if n := e.count(`SELECT count(*) FROM core.account WHERE parent_id IS NOT NULL`); n != 0 {
				t.Fatalf("%d child accounts", n)
			}
			// The file itself is kept in raw.
			if n := e.count(`SELECT count(*) FROM raw.import_file`); n == 0 {
				t.Fatal("the rejected file was not kept")
			}
		})
	}
	e.t = t
	if got := e.logs.String(); strings.Contains(got, "PLAN00001234") || strings.Contains(got, "Example Growth") {
		t.Fatalf("log carries file content: %s", got)
	}
}

// AC4: a complete capture marks an earlier contribution it lacks as disappeared, and clears it again
// when a later complete capture lists it. A contribution dated after the capture's date is not
// disappeared by a capture that lacks it.
func TestAvivaCompleteCaptureMarksAMissingContributionDisappeared(t *testing.T) {
	e := newAvivaEnv(t)
	e.platform("avi")
	later := avivaPay{"2030-03-10", "employer", 12345}
	e.importCapture("avi", avivaFile(t, "PLAN00004001", "2030-02-01T12:00:00Z", "2030-02-01", true, payA, payB, payC, later))
	e.wantGone("avi", map[string]bool{payA.name(): false, payB.name(): false, payC.name(): false, later.name(): false})

	// A newer complete capture lacks payB, which is dated before it, and `later`, which is not.
	e.importCapture("avi", avivaFile(t, "PLAN00004001", "2030-03-01T12:00:00Z", "2030-03-01", true, payA, payC))
	e.wantGone("avi", map[string]bool{payA.name(): false, payB.name(): true, payC.name(): false, later.name(): false})

	// A still newer complete capture lists payB again.
	e.importCapture("avi", avivaFile(t, "PLAN00004001", "2030-03-20T12:00:00Z", "2030-03-20", true, payA, payB, payC, later))
	e.wantGone("avi", map[string]bool{payA.name(): false, payB.name(): false, payC.name(): false, later.name(): false})
}

func TestAvivaIncompleteCaptureDisappearsNothing(t *testing.T) {
	e := newAvivaEnv(t)
	e.platform("avi")
	e.importCapture("avi", avivaFile(t, "PLAN00004002", "2030-02-01T12:00:00Z", "2030-02-01", true, payA, payB, payC))
	e.importCapture("avi", avivaFile(t, "PLAN00004002", "2030-03-01T12:00:00Z", "2030-03-01", false, payA))
	e.wantGone("avi", map[string]bool{payA.name(): false, payB.name(): false, payC.name(): false})
	// Not even when it is the only capture there is.
	e.platform("avi2")
	e.importCapture("avi2", avivaFile(t, "PLAN00004003", "2030-02-01T12:00:00Z", "2030-02-01", true, payA, payB))
	e.importCapture("avi2", avivaFile(t, "PLAN00004003", "2030-03-01T12:00:00Z", "2030-03-01", false))
	e.wantGone("avi2", map[string]bool{payA.name(): false, payB.name(): false})
}

// An older capture imported late never overrides a newer one, in either order of import.
func TestAvivaOlderCaptureImportedLateNeverOverridesANewerOne(t *testing.T) {
	e := newAvivaEnv(t)
	file := func(plan, capturedAt, asOf string, pays ...avivaPay) []byte {
		return avivaFile(t, plan, capturedAt, asOf, true, pays...)
	}
	// The older capture, printed a month before, lacks payB, as it would if it were taken before.
	e.platform("late")
	e.importCapture("late", file("PLAN00004004", "2030-03-01T12:00:00Z", "2030-03-01", payA, payB, payC))
	e.wantGone("late", map[string]bool{payA.name(): false, payB.name(): false, payC.name(): false})
	e.importCapture("late", file("PLAN00004004", "2030-02-01T12:00:00Z", "2030-02-01", payA, payC))
	e.wantGone("late", map[string]bool{payA.name(): false, payB.name(): false, payC.name(): false})
	if n := e.count(`SELECT count(*) FROM core.valuation`); n != 2 {
		t.Fatalf("%d valuations", n)
	}

	// The same two files in the other order give the same state.
	e.platform("ordered")
	e.importCapture("ordered", file("PLAN00004007", "2030-02-01T12:00:00Z", "2030-02-01", payA, payC))
	e.wantGone("ordered", map[string]bool{payA.name(): false, payC.name(): false})
	e.importCapture("ordered", file("PLAN00004007", "2030-03-01T12:00:00Z", "2030-03-01", payA, payB, payC))
	e.wantGone("ordered", map[string]bool{payA.name(): false, payB.name(): false, payC.name(): false})

	// Where the newer capture is the one that lacks the row, it decides, whenever it is imported.
	e.platform("gone")
	e.importCapture("gone", file("PLAN00004005", "2030-03-01T12:00:00Z", "2030-03-01", payA, payC))
	e.importCapture("gone", file("PLAN00004005", "2030-02-01T12:00:00Z", "2030-02-01", payA, payB, payC))
	e.wantGone("gone", map[string]bool{payA.name(): false, payB.name(): true, payC.name(): false})
}

// A repeat of a stored capture adds nothing and changes nothing.
func TestAvivaRepeatCaptureChangesNothing(t *testing.T) {
	e := newAvivaEnv(t)
	e.platform("avi")
	for _, f := range [][]byte{
		avivaFile(t, "PLAN00004006", "2030-02-01T12:00:00Z", "2030-02-01", true, payA, payB, payC),
		avivaFile(t, "PLAN00004006", "2030-03-01T12:00:00Z", "2030-03-01", true, payA, payC),
	} {
		e.importCapture("avi", f)
	}
	want := map[string]bool{payA.name(): false, payB.name(): true, payC.name(): false}
	e.wantGone("avi", want)
	e.importCapture("avi", avivaFile(t, "PLAN00004006", "2030-03-01T12:00:00Z", "2030-03-01", true, payA, payC))
	e.wantGone("avi", want)
	if n := e.count(`SELECT count(*) FROM core.investment_event`); n != 3 {
		t.Fatalf("%d events", n)
	}
}
