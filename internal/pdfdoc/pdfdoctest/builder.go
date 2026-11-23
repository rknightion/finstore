// Package pdfdoctest builds small PDF files for tests of internal/pdfdoc, in the structure of a
// modern producer: the catalogue, page tree, pages and font sit in one compressed object stream,
// the file ends in a cross-reference stream, and each page has a Flate-compressed content stream.
// Everything it writes is supplied by the caller or made up here, so fixtures hold no real data.
package pdfdoctest

import (
	"bytes"
	"compress/zlib"
	"crypto/md5" //nolint:gosec // the standard security handler is defined in terms of MD5
	"crypto/rc4" //nolint:gosec // and RC4; this fixture has to follow the format
	"fmt"
	"strings"
)

// Options changes the document structure.
type Options struct {
	// Predictor writes the cross-reference stream with the PNG Up predictor, as most producers do.
	Predictor bool
	// Uncompressed writes content streams without a filter.
	Uncompressed bool
	// Encrypt adds a standard security handler (RC4, 40 bit) to the trailer. Only the
	// dictionary is written: the streams stay readable, which is enough to show that a reader
	// refuses the file. UserPassword is the password the handler asks for; empty means that
	// a reader can open the file without one.
	Encrypt      bool
	UserPassword string
	// TrailerExtra is written inside the cross-reference stream dictionary, for example "/Foo 1".
	TrailerExtra string
	// Kids replaces the Kids array of the page tree root, for example "2 0 R 2 0 R" to make it
	// list itself. The pages are objects 4 to 3+n.
	Kids string
	// PageDictExtra is written inside every page dictionary.
	PageDictExtra string
}

// Build returns a PDF with one page per content stream. Every page can draw text in font /F1,
// whose glyphs are all 500 units wide, so a string drawn at x ends 0.5 times its size per
// character further right.
func Build(contents [][]byte, o Options) []byte {
	n := len(contents)
	const (
		catalog = 1
		pages   = 2
		font    = 3
		firstPg = 4
	)
	firstContent := firstPg + n
	objStm := firstContent + n
	xrefStm := objStm + 1
	size := xrefStm + 1

	// Objects held in the object stream, in object-number order.
	var held []string
	held = append(held, fmt.Sprintf("<< /Type /Catalog /Pages %d 0 R >>", pages))
	var kids []string
	for i := range contents {
		kids = append(kids, fmt.Sprintf("%d 0 R", firstPg+i))
	}
	kidList := strings.Join(kids, " ")
	if o.Kids != "" {
		kidList = o.Kids
	}
	held = append(held, fmt.Sprintf("<< /Type /Pages /Count %d /Kids [%s] >>", n, kidList))
	widths := strings.TrimSpace(strings.Repeat("500 ", 95))
	held = append(held, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding"+
		" /FirstChar 32 /LastChar 126 /Widths ["+widths+"] >>")
	for i := range contents {
		held = append(held, fmt.Sprintf("<< /Type /Page /Parent %d 0 R /MediaBox [0 0 612 792] /Contents %d 0 R"+
			" /Resources << /Font << /F1 %d 0 R >> >> %s >>", pages, firstContent+i, font, o.PageDictExtra))
	}
	var header, body strings.Builder
	for i, h := range held {
		fmt.Fprintf(&header, "%d %d ", catalog+i, body.Len())
		body.WriteString(h)
		body.WriteByte('\n')
	}
	stmData := deflate([]byte(header.String() + body.String()))

	var buf bytes.Buffer
	buf.WriteString("%PDF-1.5\n%\xe2\xe3\xcf\xd3\n")
	offsets := make(map[int]int)
	for i, c := range contents {
		data, filter := c, ""
		if !o.Uncompressed {
			data, filter = deflate(c), "/Filter /FlateDecode "
		}
		offsets[firstContent+i] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n<< %s/Length %d >>\nstream\n", firstContent+i, filter, len(data))
		buf.Write(data)
		buf.WriteString("\nendstream\nendobj\n")
	}
	offsets[objStm] = buf.Len()
	fmt.Fprintf(&buf, "%d 0 obj\n<< /Type /ObjStm /N %d /First %d /Filter /FlateDecode /Length %d >>\nstream\n",
		objStm, len(held), header.Len(), len(stmData))
	buf.Write(stmData)
	buf.WriteString("\nendstream\nendobj\n")

	// Cross-reference stream: type (1 byte), offset or object stream (4 bytes), generation or
	// index (2 bytes).
	const width = 7
	var rows [][]byte
	entry := func(typ byte, a, b int) []byte {
		return []byte{typ, byte(a >> 24 & 0xff), byte(a >> 16 & 0xff), byte(a >> 8 & 0xff), byte(a & 0xff), byte(b >> 8 & 0xff), byte(b & 0xff)}
	}
	rows = append(rows, entry(0, 0, 65535))
	for num := 1; num < size; num++ {
		switch {
		case num < firstContent:
			rows = append(rows, entry(2, objStm, num-catalog))
		case num == xrefStm:
			rows = append(rows, entry(1, buf.Len(), 0))
		default:
			rows = append(rows, entry(1, offsets[num], 0))
		}
	}
	start := buf.Len()
	raw := bytes.Join(rows, nil)
	parms := ""
	if o.Predictor {
		var enc []byte
		prev := make([]byte, width)
		for _, r := range rows {
			enc = append(enc, 2)
			for i := range r {
				enc = append(enc, r[i]-prev[i])
			}
			prev = r
		}
		raw = enc
		parms = fmt.Sprintf("/DecodeParms << /Predictor 12 /Columns %d >> ", width)
	}
	xdata := deflate(raw)
	extra := o.TrailerExtra
	if o.Encrypt {
		extra += " " + encryptEntries(o.UserPassword)
	}
	fmt.Fprintf(&buf, "%d 0 obj\n<< /Type /XRef /Size %d /W [1 4 2] /Root %d 0 R %s /Filter /FlateDecode %s/Length %d >>\nstream\n",
		xrefStm, size, catalog, extra, parms, len(xdata))
	buf.Write(xdata)
	fmt.Fprintf(&buf, "\nendstream\nendobj\nstartxref\n%d\n%%%%EOF\n", start)
	return buf.Bytes()
}

// Text returns the operators that draw s at (x, y) in font /F1 at the given size.
func Text(x, y, size float64, s string) string {
	return fmt.Sprintf("BT\n/F1 %g Tf\n1 0 0 1 %g %g Tm\n(%s)Tj\nET\n", size, x, y, Escape(s))
}

// Escape escapes the characters of s that a literal PDF string cannot hold.
func Escape(s string) string {
	return strings.NewReplacer(`\`, `\\`, "(", `\(`, ")", `\)`).Replace(s)
}

func deflate(b []byte) []byte {
	var out bytes.Buffer
	w := zlib.NewWriter(&out)
	_, _ = w.Write(b)
	_ = w.Close()
	return out.Bytes()
}

// passwordPad is the padding string of the standard security handler (ISO 32000-1, algorithm 2).
var passwordPad = []byte{
	0x28, 0xBF, 0x4E, 0x5E, 0x4E, 0x75, 0x8A, 0x41, 0x64, 0x00, 0x4E, 0x56, 0xFF, 0xFA, 0x01, 0x08,
	0x2E, 0x2E, 0x00, 0xB6, 0xD0, 0x68, 0x3E, 0x80, 0x2F, 0x0C, 0xA9, 0xFE, 0x64, 0x53, 0x69, 0x7A,
}

// encryptEntries returns the trailer entries of a revision 2 standard security handler whose user
// password is pw, following algorithms 2 and 4 of ISO 32000-1, with made-up O and ID values.
func encryptEntries(pw string) string {
	owner := bytes.Repeat([]byte{0x4f}, 32)
	id := []byte("0123456789abcdef")
	const perms = -4

	padded := append([]byte(pw), passwordPad...)[:32]
	h := md5.New() //nolint:gosec // see the imports
	h.Write(padded)
	h.Write(owner)
	h.Write([]byte{byte(perms & 0xff), 0xff, 0xff, 0xff})
	h.Write(id)
	key := h.Sum(nil)[:5]

	c, err := rc4.NewCipher(key) //nolint:gosec // see the imports
	if err != nil {
		panic(err)
	}
	u := make([]byte, 32)
	c.XORKeyStream(u, passwordPad)
	return fmt.Sprintf("/Encrypt << /Filter /Standard /V 1 /R 2 /P %d /O <%x> /U <%x> >> /ID [<%x> <%x>]", perms, owner, u, id, id)
}
