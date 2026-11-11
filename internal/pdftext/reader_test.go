package pdftext_test

import (
	"bytes"
	"compress/zlib"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rknightion/finstore/internal/pdftext"
	"github.com/rknightion/finstore/internal/pdftext/pdftexttest"
)

func page(parts ...string) []byte { return []byte(strings.Join(parts, "")) }

func TestReadPositionsAndDecoys(t *testing.T) {
	content := page(
		pdftexttest.Text(57, 700.5, 9, "Alpha (beta) \\ gamma"),
		pdftexttest.Text(10, 10, 0.05, "invisible label"),
		"BT /F1 9 Tf 1 0 0 1 100 650 Tm <4142> Tj 20 -5 Td (via Td)Tj ET\n",
		"/Xf1 Do\n",
		pdftexttest.Text(300.25, 600, 12, "Cost £12.50"),
	)
	got, err := pdftext.Read(pdftexttest.Build([][]byte{content}, pdftexttest.Options{}))
	if err != nil {
		t.Fatal(err)
	}
	want := []pdftext.Text{
		{Page: 1, X: 57, Y: 700.5, Size: 9, S: "Alpha (beta) \\ gamma"},
		{Page: 1, X: 100, Y: 650, Size: 9, S: "AB"},
		{Page: 1, X: 120, Y: 645, Size: 9, S: "via Td"},
		{Page: 1, X: 300.25, Y: 600, Size: 12, S: "Cost £12.50"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d texts, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("text %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestReadWinAnsiMapping(t *testing.T) {
	// Octal escapes and raw high bytes decode the same way.
	content := page("BT /F1 9 Tf 1 0 0 1 0 0 Tm (\\243\\200\\226\xe9\\377)Tj ET\n")
	got, err := pdftext.Read(pdftexttest.Build([][]byte{content}, pdftexttest.Options{}))
	if err != nil {
		t.Fatal(err)
	}
	if want := "£€–éÿ"; got[0].S != want {
		t.Fatalf("got %q, want %q", got[0].S, want)
	}
}

func TestReadPagesNumberedAndUncompressed(t *testing.T) {
	blank := page("BT ET\n")
	pages := [][]byte{
		page(pdftexttest.Text(1, 2, 9, "one")),
		blank,
		page(pdftexttest.Text(3, 4, 9, "two")),
	}
	for _, unc := range []bool{false, true} {
		got, err := pdftext.Read(pdftexttest.Build(pages, pdftexttest.Options{Uncompressed: unc}))
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0].Page != 1 || got[1].Page != 2 || got[1].S != "two" {
			t.Fatalf("uncompressed=%v: unexpected result %+v", unc, got)
		}
	}
}

func TestReadRejects(t *testing.T) {
	good := page(pdftexttest.Text(1, 2, 9, "x"))
	tests := []struct {
		name string
		data []byte
		want error
	}{
		{"object stream", pdftexttest.Build([][]byte{good}, pdftexttest.Options{ExtraObjects: []string{"<< /Type /ObjStm /N 1 /First 5 >>"}}), pdftext.ErrUnsupported},
		{"xref stream", pdftexttest.Build([][]byte{good}, pdftexttest.Options{ExtraObjects: []string{"<< /Type /XRef >>"}}), pdftext.ErrUnsupported},
		{"encrypted", pdftexttest.Build([][]byte{good}, pdftexttest.Options{TrailerExtra: "/Encrypt 9 0 R"}), pdftext.ErrEncrypted},
		{"TJ", pdftexttest.Build([][]byte{page("BT /F1 9 Tf 1 0 0 1 5 5 Tm [(a) -20 (b)] TJ ET\n")}, pdftexttest.Options{}), pdftext.ErrUnsupported},
		{"quote", pdftexttest.Build([][]byte{page("BT /F1 9 Tf 1 0 0 1 5 5 Tm (a) ' ET\n")}, pdftexttest.Options{}), pdftext.ErrUnsupported},
		{"double quote", pdftexttest.Build([][]byte{page("BT /F1 9 Tf 1 0 0 1 5 5 Tm 1 2 (a) \" ET\n")}, pdftexttest.Options{}), pdftext.ErrUnsupported},
		{"no text", pdftexttest.Build([][]byte{page("BT ET\n")}, pdftexttest.Options{}), pdftext.ErrNoText},
		{"only invisible text", pdftexttest.Build([][]byte{page(pdftexttest.Text(1, 1, 0.05, "x"))}, pdftexttest.Options{}), pdftext.ErrNoText},
		{"not a pdf", []byte("hello"), pdftext.ErrMalformed},
		{"empty", nil, pdftext.ErrMalformed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := pdftext.Read(tc.data)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestReadAllowsInvisibleTJ(t *testing.T) {
	content := page("BT /F1 0.05 Tf 1 0 0 1 5 5 Tm [(a)] TJ ET\n", pdftexttest.Text(1, 2, 9, "x"))
	if _, err := pdftext.Read(pdftexttest.Build([][]byte{content}, pdftexttest.Options{})); err != nil {
		t.Fatal(err)
	}
}

func TestReadErrorsCarryNoContent(t *testing.T) {
	secret := "SECRET-LABEL"
	content := page(pdftexttest.Text(1, 2, 9, secret), "BT [(a)] TJ ET\n")
	_, err := pdftext.Read(pdftexttest.Build([][]byte{content}, pdftexttest.Options{}))
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("unexpected error %v", err)
	}
}

func TestReadDecompressionBomb(t *testing.T) {
	var z bytes.Buffer
	w := zlib.NewWriter(&z)
	chunk := bytes.Repeat([]byte(" "), 1<<20)
	for range 20 {
		_, _ = w.Write(chunk)
	}
	_ = w.Close()
	pdf := fmt.Sprintf("%%PDF-1.4\n1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n"+
		"2 0 obj\n<< /Type /Pages /Kids [3 0 R] >>\nendobj\n"+
		"3 0 obj\n<< /Type /Page /Contents 4 0 R >>\nendobj\n"+
		"4 0 obj\n<< /Filter /FlateDecode /Length %d >>\nstream\n%s\nendstream\nendobj\n", z.Len(), z.Bytes())
	if _, err := pdftext.Read([]byte(pdf)); !errors.Is(err, pdftext.ErrTooLarge) {
		t.Fatalf("got %v, want ErrTooLarge", err)
	}
}

// Truncated and garbage input must return errors, never panic.
func TestReadMalformedNeverPanics(t *testing.T) {
	good := pdftexttest.Build([][]byte{page(pdftexttest.Text(1, 2, 9, "x"))}, pdftexttest.Options{})
	var inputs [][]byte
	// Cut anywhere before the content stream ends; later cuts only lose the xref table.
	for n := 0; n < bytes.Index(good, []byte("endstream")); n += 7 {
		inputs = append(inputs, good[:n])
	}
	for _, s := range []string{
		"%PDF-1.4\n1 0 obj\n<< /Kids [ << << [[[",
		"%PDF-1.4\n1 0 obj\n(unterminated",
		"%PDF-1.4\n1 0 obj\n<< /Type /Page /Contents 1 0 R >>\nendobj\n",
		"%PDF-1.4\n1 0 obj\n<< /Length 99999999999 >>\nstream\nabc",
		"%PDF-1.4\n1 0 obj\n<< /Type /Catalog /Pages 1 0 R >>\nendobj\n",
		"%PDF-1.4\n99999999999999999999 0 obj << >> endobj",
		"%PDF-1.4 obj obj obj stream endstream trailer",
		"%PDF-1.4\n\x00\xff\xfe<<>>>>)))(((",
	} {
		inputs = append(inputs, []byte(s))
	}
	for _, body := range []string{
		"(unterminated", "BT /F1 9 Tf 1 0 0 1 Tm (x)Tj", "<4", "<zz> Tj", "[ [ [", "]", ">>", ")", "BI abc",
		"BT /F1 Tf (x) Tj ET", "1 2 3 Tm (x) Tj", "/F1 9 Tf (x) Tj Tj Tj", "<< /A [ (x) ] >> BDC (y) Tj",
		"1e5 -.5 +.5 4. . - 0.0.0 Tj", "\\", "/#4", "/#zz #",
	} {
		inputs = append(inputs, pdftexttest.Build([][]byte{[]byte(body)}, pdftexttest.Options{}))
	}
	for i, in := range inputs {
		if _, err := pdftext.Read(in); err == nil {
			t.Errorf("input %d: expected an error (%q)", i, in[:min(len(in), 60)])
		}
	}
}

func FuzzRead(f *testing.F) {
	f.Add(pdftexttest.Build([][]byte{page(pdftexttest.Text(1, 2, 9, "x"))}, pdftexttest.Options{}))
	f.Add([]byte("%PDF-1.4\n1 0 obj\n<< /Type /Catalog >>\nendobj\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = pdftext.Read(data) // must not panic
	})
}
