package pdfdoc_test

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rknightion/finstore/internal/pdfdoc"
	"github.com/rknightion/finstore/internal/pdfdoc/pdfdoctest"
	"github.com/rknightion/finstore/internal/pdftext"
)

// marker is made-up file content that must never appear in an error.
const marker = "MADE-UP-MARKER-42"

func page(ops ...string) []byte { return []byte(strings.Join(ops, "")) }

func TestReadObjectAndXrefStreams(t *testing.T) {
	// Drawn out of reading order on purpose. Page 2 is drawn after page 1 but holds no text of
	// its own, so page 3 must be numbered 2.
	p1 := page(
		pdfdoctest.Text(300, 500, 10, "Right"),
		pdfdoctest.Text(72, 700, 12, "Title"),
		pdfdoctest.Text(72, 500, 10, "Left"),
		pdfdoctest.Text(72, 100, 8, "Footer"),
	)
	p2 := page("BT ET\n")
	p3 := page(pdfdoctest.Text(50, 400, 9, "Second page"))
	want := []pdftext.Text{
		{Page: 1, X: 72, Y: 700, Size: 12, S: "Title"},
		{Page: 1, X: 72, Y: 500, Size: 10, S: "Left"},
		{Page: 1, X: 300, Y: 500, Size: 10, S: "Right"},
		{Page: 1, X: 72, Y: 100, Size: 8, S: "Footer"},
		{Page: 2, X: 50, Y: 400, Size: 9, S: "Second page"},
	}
	for _, predictor := range []bool{false, true} {
		for _, uncompressed := range []bool{false, true} {
			t.Run(fmt.Sprintf("predictor=%t uncompressed=%t", predictor, uncompressed), func(t *testing.T) {
				data := pdfdoctest.Build([][]byte{p1, p2, p3}, pdfdoctest.Options{Predictor: predictor, Uncompressed: uncompressed})
				// The structures under test are really there, and no classic xref table is.
				for _, s := range []string{"/Type /ObjStm", "/Type /XRef"} {
					if !bytes.Contains(data, []byte(s)) {
						t.Fatalf("fixture has no %q", s)
					}
				}
				if bytes.Contains(data, []byte("\nxref\n")) {
					t.Fatal("fixture has a classic cross-reference table")
				}
				got, err := pdfdoc.Read(data)
				if err != nil {
					t.Fatalf("Read: %v", err)
				}
				equalTexts(t, got, want)
			})
		}
	}
}

func TestReadStringsAndRows(t *testing.T) {
	// Glyphs are 0.5 em wide, so a 10 point string of n characters drawn at x ends at x+5n.
	content := page(
		// One row whose cells differ slightly in baseline: left to right, not by baseline.
		pdfdoctest.Text(300, 700, 10, "Amount"),
		pdfdoctest.Text(72, 699.8, 10, "Label"),
		// A string with spaces stays whole; its Tj ends at 72+5*13=137.
		pdfdoctest.Text(72, 650, 10, "Money paid in"),
		// 3 points (0.3 em) after the end of the previous string: a word gap, so joined.
		pdfdoctest.Text(140, 650, 10, "this year"),
		// 63 points after the end of the string: a column gap, so a separate string.
		pdfdoctest.Text(300, 650, 10, "1,234.56"),
		// A baseline well below the row is another row.
		pdfdoctest.Text(72, 640, 10, "Next row"),
		// A pound sign is WinAnsi 0xA3.
		pdfdoctest.Text(72, 600, 10, "Value \xa3500.00"),
		// Fragments drawn out of left-to-right order still read as one value.
		pdfdoctest.Text(77, 560, 10, "2"),
		pdfdoctest.Text(72, 560, 10, "1"),
		pdfdoctest.Text(82, 560, 10, "3"),
	)
	got, err := pdfdoc.Read(pdfdoctest.Build([][]byte{content}, pdfdoctest.Options{Predictor: true}))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	var s []string
	for _, tx := range got {
		s = append(s, tx.S)
	}
	want := []string{"Label", "Amount", "Money paid in this year", "1,234.56", "Next row", "Value £500.00", "123"}
	if fmt.Sprint(s) != fmt.Sprint(want) {
		t.Fatalf("strings = %q, want %q", s, want)
	}
}

func TestReadIgnoresInvisibleText(t *testing.T) {
	content := page(
		pdfdoctest.Text(72, 700, 0.05, "hidden label"),
		pdfdoctest.Text(72, 600, 10, "visible"),
	)
	got, err := pdfdoc.Read(pdfdoctest.Build([][]byte{content}, pdfdoctest.Options{}))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	equalTexts(t, got, []pdftext.Text{{Page: 1, X: 72, Y: 600, Size: 10, S: "visible"}})
}

func TestReadTJ(t *testing.T) {
	// pdftext refuses TJ; this reader must accept it. The kerning moves "o" 20 units closer.
	// One value drawn by two successive TJ operators is still one string.
	content := page("BT /F1 10 Tf 1 0 0 1 72 700 Tm [(Hell) 20 (o)] TJ ET\n",
		"BT /F1 10 Tf 1 0 0 1 72 650 Tm [(1)] TJ [(23)] TJ ET\n")
	got, err := pdfdoc.Read(pdfdoctest.Build([][]byte{content}, pdfdoctest.Options{}))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	equalTexts(t, got, []pdftext.Text{
		{Page: 1, X: 72, Y: 700, Size: 10, S: "Hello"},
		{Page: 1, X: 72, Y: 650, Size: 10, S: "123"},
	})
}

func TestRefusals(t *testing.T) {
	good := page(pdfdoctest.Text(72, 700, 10, marker))
	enc := func(o pdfdoctest.Options) []byte { return pdfdoctest.Build([][]byte{good}, o) }

	var many bytes.Buffer
	// One string a line, each on a baseline of its own.
	many.WriteString("BT /F1 9 Tf\n")
	for i := 0; i < 200_001; i++ {
		fmt.Fprintf(&many, "1 0 0 1 5 %d Tm (x)Tj\n", 10*(i+1))
	}
	many.WriteString("ET\n")
	half := bytes.Repeat([]byte(" "), 9<<20)

	cases := []struct {
		name string
		data []byte
		want error
	}{
		{"encrypted, empty user password", enc(pdfdoctest.Options{Encrypt: true}), pdfdoc.ErrEncrypted},
		{"encrypted, user password set", enc(pdfdoctest.Options{Encrypt: true, UserPassword: marker}), pdfdoc.ErrEncrypted},
		{"encrypted, handler the library lacks", enc(pdfdoctest.Options{TrailerExtra: "/Encrypt << /Filter /Other >>"}), pdfdoc.ErrEncrypted},
		// Every kid of the page tree is the one page, so the file stays small and fast to read.
		{"too many pages", pdfdoctest.Build([][]byte{good}, pdfdoctest.Options{Kids: strings.Repeat("4 0 R ", 2001)}), pdfdoc.ErrTooLarge},
		{"page tree with too many nodes", pdfdoctest.Build([][]byte{good}, pdfdoctest.Options{Kids: strings.Repeat("0 ", 8001)}), pdfdoc.ErrTooLarge},
		{"page tree that lists itself", pdfdoctest.Build([][]byte{good}, pdfdoctest.Options{Kids: "2 0 R 2 0 R"}), pdfdoc.ErrTooLarge},
		{"inflated content over the cap", pdfdoctest.Build([][]byte{bytes.Repeat([]byte(" "), 17<<20)}, pdfdoctest.Options{}), pdfdoc.ErrTooLarge},
		{"inflated content over the cap in total", pdfdoctest.Build([][]byte{half, half}, pdfdoctest.Options{}), pdfdoc.ErrTooLarge},
		{"too many strings", pdfdoctest.Build([][]byte{many.Bytes()}, pdfdoctest.Options{}), pdfdoc.ErrTooLarge},
		{"only invisible text", pdfdoctest.Build([][]byte{page(pdfdoctest.Text(1, 1, 0.05, marker))}, pdfdoctest.Options{}), pdfdoc.ErrNoText},
		{"empty page", pdfdoctest.Build([][]byte{page("BT ET\n")}, pdfdoctest.Options{}), pdfdoc.ErrNoText},
		{"not a pdf", []byte(marker), pdfdoc.ErrMalformed},
		{"truncated", enc(pdfdoctest.Options{})[:100], pdfdoc.ErrMalformed},
		{"nil", nil, pdfdoc.ErrMalformed},
		// Each of these makes the library panic, and Read must turn it into one fixed error.
		{"panic, Tj with too many operands", pdfdoctest.Build([][]byte{page("BT /F1 9 Tf 1 2 (" + marker + ") Tj ET\n")}, pdfdoctest.Options{}), pdfdoc.ErrUnreadable},
		{"panic, Q without q", pdfdoctest.Build([][]byte{page("Q\n" + pdfdoctest.Text(1, 1, 9, marker))}, pdfdoctest.Options{}), pdfdoc.ErrUnreadable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pdfdoc.Read(tc.data)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if got != nil {
				t.Fatalf("got %d texts with an error", len(got))
			}
			// A fixed error: exactly the sentinel, never wrapped around text from the file.
			if err.Error() != tc.want.Error() {
				t.Fatalf("error %q is not the bare sentinel", err)
			}
			if strings.Contains(err.Error(), marker) {
				t.Fatalf("error holds file content: %q", err)
			}
		})
	}
}

func TestReadNeverPanics(t *testing.T) {
	data := pdfdoctest.Build([][]byte{page(pdfdoctest.Text(72, 700, 10, "Title"), pdfdoctest.Text(72, 600, 10, "Body"))}, pdfdoctest.Options{Predictor: true})
	known := []error{pdfdoc.ErrMalformed, pdfdoc.ErrEncrypted, pdfdoc.ErrTooLarge, pdfdoc.ErrNoText, pdfdoc.ErrUnreadable}
	check := func(label string, in []byte) {
		t.Helper()
		_, err := pdfdoc.Read(in)
		if err == nil {
			return
		}
		for _, k := range known {
			if err.Error() == k.Error() {
				return
			}
		}
		t.Fatalf("%s: error %q is not a fixed error", label, err)
	}
	for n := 0; n < len(data); n += 3 {
		check(fmt.Sprintf("prefix %d", n), data[:n])
	}
	for i := 0; i < len(data); i++ {
		mutated := append([]byte(nil), data...)
		mutated[i] ^= 0xff
		check(fmt.Sprintf("flip %d", i), mutated)
	}
}

func equalTexts(t *testing.T, got, want []pdftext.Text) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d texts %+v, want %d %+v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("text %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}
