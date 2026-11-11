// Package pdftexttest builds small PDF files for tests, in the structure the
// pdftext reader accepts: uncompressed objects, one Flate-compressed content
// stream per page, text drawn by Tj at an absolute Tm. Everything it writes is
// supplied by the caller, so fixtures hold only made-up values.
package pdftexttest

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"strings"
)

// Options adds to the document structure.
type Options struct {
	// TrailerExtra is written inside the trailer dictionary, for example "/Encrypt 9 0 R".
	TrailerExtra string
	// ExtraObjects are written as extra indirect objects, numbered after the pages.
	ExtraObjects []string
	// Uncompressed writes content streams without a filter.
	Uncompressed bool
}

// Build returns a PDF with one page per content stream.
func Build(contents [][]byte, o Options) []byte {
	var buf bytes.Buffer
	var offsets []int
	obj := func(body string) {
		offsets = append(offsets, buf.Len())
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", len(offsets), body)
	}
	buf.WriteString("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")

	var kids []string
	for i := range contents {
		kids = append(kids, fmt.Sprintf("%d 0 R", 3+2*i))
	}
	obj("<< /Type /Catalog /Pages 2 0 R >>")
	obj(fmt.Sprintf("<< /Type /Pages /Count %d /Kids [%s] >>", len(contents), strings.Join(kids, " ")))
	for i, c := range contents {
		obj(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /Contents %d 0 R /Resources << /Font << /F1 << /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >> >> >> >>", 4+2*i))
		offsets = append(offsets, buf.Len())
		body, filter := c, ""
		if !o.Uncompressed {
			body, filter = deflate(c), "/Filter /FlateDecode "
		}
		fmt.Fprintf(&buf, "%d 0 obj\n<< %s/Length %d >>\nstream\n", len(offsets), filter, len(body))
		buf.Write(body)
		buf.WriteString("\nendstream\nendobj\n")
	}
	for _, x := range o.ExtraObjects {
		obj(x)
	}

	xref := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
	for _, off := range offsets {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R %s >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, o.TrailerExtra, xref)
	return buf.Bytes()
}

func deflate(b []byte) []byte {
	var out bytes.Buffer
	w := zlib.NewWriter(&out)
	_, _ = w.Write(b)
	_ = w.Close()
	return out.Bytes()
}

// Text returns the operators that draw s at (x, y) in the given size.
func Text(x, y, size float64, s string) string {
	return fmt.Sprintf("BT\n/F1 %g Tf\n1 0 0 1 %g %g Tm\n(%s)Tj\nET\n", size, x, y, Escape(s))
}

// LabelledText returns the operators that draw s at (x, y) in the given size and then, with no
// Tm between them, a hidden label: white, in a 0.05-point font. That is how statements attach
// their invisible labels to visible text.
func LabelledText(x, y, size float64, s, label string) string {
	return fmt.Sprintf("BT\n1 0 0 1 %g %g Tm\n/F1 %g Tf\n(%s)Tj\n/F1 0.05 Tf\n1 1 1 rg\n(%s)Tj\n0 g\nET\n",
		x, y, size, Escape(s), Escape(label))
}

// Escape writes s as the body of a PDF literal string in WinAnsiEncoding:
// parentheses and backslashes are escaped and the pound sign is an octal escape.
func Escape(s string) string {
	var sb strings.Builder
	for _, r := range s {
		switch r {
		case '(', ')', '\\':
			sb.WriteByte('\\')
			sb.WriteRune(r)
		case '£':
			sb.WriteString(`\243`)
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}
