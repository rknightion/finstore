// Package pdfdoc reads the visible text, with positions, from the PDF documents that pension
// providers issue. Those use object streams, cross-reference streams and embedded fonts, which the
// deliberately strict internal/pdftext refuses, so this package reads them through
// github.com/ledongthuc/pdf and returns the same pdftext.Text type. The bank statement path keeps
// using pdftext; nothing here loosens it.
//
// Read returns strings in reading order: page by page, rows from the top of the page down, and
// within a row from left to right. The library reports one glyph at a time, so Read joins the
// glyphs of one line into strings: glyphs join while they share a baseline and sit close together,
// and a wider gap, such as the space between table columns, starts a new string. As in pdftext,
// text under 1 point is invisible and is not returned, and Page counts only pages that carry
// visible text.
//
// Read refuses an encrypted file, even one the library could open with an empty password, and a
// file that passes a size cap. A panic inside the library is recovered. Every error is one of the
// fixed errors below, tested with errors.Is, and never carries text from the file or from the
// library, whose own messages can quote file content.
package pdfdoc

import (
	"bytes"
	"errors"
	"io"
	"math"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"

	"github.com/rknightion/finstore/internal/pdftext"
)

// Limits. The first three mirror the unexported limits of pdftext.
const (
	// maxInflated bounds the total inflated size of the content streams of all pages.
	maxInflated = 16 << 20
	// maxPages bounds the page count of a file.
	maxPages = 2000
	// maxTexts bounds the number of strings a file may yield.
	maxTexts = 200_000
	// maxNodes bounds the nodes visited in the page tree, and maxDepth its depth.
	maxNodes = 4 * maxPages
	maxDepth = 32
	// minVisibleSize is the smallest size, in points, that counts as visible text.
	minVisibleSize = 1.0
)

// Joining and ordering tolerances, as fractions of the font size.
const (
	// sameBaseline is the largest baseline difference between two glyphs of one string.
	sameBaseline = 0.5
	// wordGap is the gap, between the end of one glyph and the start of the next, from which a
	// space is inserted if neither glyph is one.
	wordGap = 0.15
	// columnGap is the gap, in either direction, from which a glyph starts a new string.
	columnGap = 0.5
	// sameRow is the largest baseline difference, from the top string of a row, for another
	// string to sit in that row.
	sameRow = 0.25
)

// Errors returned by Read, to be tested with errors.Is.
var (
	ErrMalformed  = errors.New("pdfdoc: malformed PDF")
	ErrEncrypted  = errors.New("pdfdoc: encrypted PDF")
	ErrTooLarge   = errors.New("pdfdoc: PDF too large")
	ErrNoText     = errors.New("pdfdoc: no text found")
	ErrUnreadable = errors.New("pdfdoc: PDF could not be read")
)

// Read returns every visible string of the document in reading order.
func Read(data []byte) (texts []pdftext.Text, err error) {
	defer func() {
		// The recovered value is dropped on purpose: a library panic message can quote the file.
		if recover() != nil {
			texts, err = nil, ErrUnreadable
		}
	}()
	return read(data)
}

func read(data []byte) ([]pdftext.Text, error) {
	rd, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		// The library reports a wrong password, and refuses an encryption scheme it does not
		// implement, with errors of its own; both leave the Encrypt entry in the file.
		if errors.Is(err, pdf.ErrInvalidPassword) || bytes.Contains(data, []byte("/Encrypt")) {
			return nil, ErrEncrypted
		}
		return nil, ErrMalformed
	}
	// A file encrypted with an empty user password opens without error, but is still encrypted.
	if !rd.Trailer().Key("Encrypt").IsNull() {
		return nil, ErrEncrypted
	}

	pages, ok := pageList(rd.Trailer().Key("Root").Key("Pages"))
	if !ok {
		return nil, ErrTooLarge
	}
	budget := int64(maxInflated)
	var (
		out    []pdftext.Text
		pageNo int
	)
	for _, page := range pages {
		contents := page.V.Key("Contents")
		if contents.IsNull() {
			continue
		}
		if !chargeInflated(contents, &budget) {
			return nil, ErrTooLarge
		}
		texts := lines(page.Content().Text)
		if len(texts) == 0 {
			continue
		}
		if len(out)+len(texts) > maxTexts {
			return nil, ErrTooLarge
		}
		pageNo++
		for j := range texts {
			texts[j].Page = pageNo
		}
		out = append(out, texts...)
	}
	if len(out) == 0 {
		return nil, ErrNoText
	}
	return out, nil
}

// pageList returns the pages of the page tree in order. It walks the tree once, because the
// library's own lookup by page number walks it again for every page, and a file of many pages
// would then take far longer than its size suggests. It reports false when the tree holds more
// pages than maxPages, or is so large or so tangled (a node that lists itself) that it would
// take more than maxNodes visits.
func pageList(root pdf.Value) ([]pdf.Page, bool) {
	var (
		pages  []pdf.Page
		visits int
		walk   func(node pdf.Value, depth int) bool
	)
	walk = func(node pdf.Value, depth int) bool {
		visits++
		if visits > maxNodes || depth > maxDepth {
			return false
		}
		switch node.Key("Type").Name() {
		case "Pages":
			kids := node.Key("Kids")
			for i := 0; i < kids.Len(); i++ {
				if !walk(kids.Index(i), depth+1) {
					return false
				}
			}
		case "Page":
			if len(pages) >= maxPages {
				return false
			}
			pages = append(pages, pdf.Page{V: node})
		}
		return true
	}
	if !walk(root, 0) {
		return nil, false
	}
	return pages, true
}

// chargeInflated inflates a page's content stream, or array of streams, to count its size
// against the budget. It reports false as soon as the budget runs out, so a decompression bomb
// is never fully inflated and the library never interprets one.
func chargeInflated(contents pdf.Value, budget *int64) bool {
	var streams []pdf.Value
	if contents.Kind() == pdf.Array {
		for i := 0; i < contents.Len(); i++ {
			streams = append(streams, contents.Index(i))
		}
	} else {
		streams = []pdf.Value{contents}
	}
	for _, s := range streams {
		rc := s.Reader()
		n, _ := io.Copy(io.Discard, io.LimitReader(rc, *budget+1))
		_ = rc.Close()
		if n > *budget {
			return false
		}
		*budget -= n
	}
	return true
}

// lines joins the glyphs of one page into strings and puts them in reading order.
func lines(glyphs []pdf.Text) []pdftext.Text {
	var (
		out []pdftext.Text
		cur *pdftext.Text
		b   strings.Builder
		end float64 // where the last glyph of cur ends
	)
	flush := func() {
		if cur == nil {
			return
		}
		if s := strings.TrimSpace(b.String()); s != "" {
			cur.S = s
			out = append(out, *cur)
		}
		cur = nil
		b.Reset()
	}
	for _, g := range glyphs {
		size := math.Abs(g.FontSize)
		// The library marks the end of a TJ with a newline glyph.
		if g.S == "\n" {
			flush()
			continue
		}
		if size < minVisibleSize || !finite(g.X, g.Y, g.W, size) {
			continue
		}
		if cur != nil {
			gap := g.X - end
			if math.Abs(g.Y-cur.Y) > sameBaseline*size || gap > columnGap*size || gap < -columnGap*size {
				flush()
			} else if gap > wordGap*size && !endsInSpace(b.String()) && !startsWithSpace(g.S) {
				b.WriteByte(' ')
			}
		}
		if cur == nil {
			cur = &pdftext.Text{X: g.X, Y: g.Y, Size: size}
		}
		b.WriteString(g.S)
		end = g.X + g.W
	}
	flush()

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Y != out[j].Y {
			return out[i].Y > out[j].Y
		}
		return out[i].X < out[j].X
	})
	// Strings of one visual row can differ slightly in baseline, which would let the baseline
	// decide their order. Group each row by its top string, then order the row left to right.
	for i := 0; i < len(out); {
		j := i + 1
		for j < len(out) && out[i].Y-out[j].Y <= sameRow*out[i].Size {
			j++
		}
		row := out[i:j]
		sort.SliceStable(row, func(a, b int) bool { return row[a].X < row[b].X })
		i = j
	}
	return out
}

func finite(vs ...float64) bool {
	for _, v := range vs {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}

func endsInSpace(s string) bool {
	r, _ := utf8.DecodeLastRuneInString(s)
	return unicode.IsSpace(r)
}

func startsWithSpace(s string) bool {
	r, _ := utf8.DecodeRuneInString(s)
	return unicode.IsSpace(r)
}
