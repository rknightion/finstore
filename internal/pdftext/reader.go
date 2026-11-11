// Package pdftext reads the visible text, with positions, from the simple PDF
// documents that bank statements are: unencrypted, no object or cross-reference
// streams, Flate-compressed page content in which every piece of text is drawn
// by Tj at an absolute position. Anything else is rejected, so a changed
// layout stops an import instead of producing wrong rows.
//
// Errors never carry file content.
package pdftext

import (
	"bytes"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"sort"
)

// Limits and thresholds.
const (
	// maxInflated bounds the total size of all inflated content streams, as a
	// guard against decompression bombs.
	maxInflated = 16 << 20
	// minVisibleSize is the smallest font size that counts as visible text.
	// Statements carry invisible accessibility labels at 0.05 points.
	minVisibleSize = 1.0
	// maxOperands bounds the content-stream operand stack.
	maxOperands = 256
)

// Errors returned by Read, to be tested with errors.Is.
var (
	ErrMalformed   = errors.New("pdftext: malformed PDF")
	ErrEncrypted   = errors.New("pdftext: encrypted PDF")
	ErrUnsupported = errors.New("pdftext: unsupported PDF structure")
	ErrNoText      = errors.New("pdftext: no text found")
	ErrTooLarge    = errors.New("pdftext: content stream too large")
)

// Text is one string drawn by a Tj operator.
type Text struct {
	// Page is 1-based and counts only pages that carry visible text.
	Page int
	// X and Y are the translation of the text matrix when the string is drawn.
	X, Y float64
	// Size is the font size set by Tf.
	Size float64
	// S is the string, decoded from WinAnsi to UTF-8.
	S string
}

// Read returns every visible text of the document, page by page, in drawing
// order. Pages follow the page tree; if the tree cannot be followed, pages
// follow object number. Form XObjects are not entered.
func Read(data []byte) ([]Text, error) {
	if !bytes.Contains(data[:min(len(data), 1024)], []byte("%PDF-")) {
		return nil, fmt.Errorf("%w: no PDF header", ErrMalformed)
	}
	if bytes.Contains(data, []byte("/Encrypt")) {
		return nil, ErrEncrypted
	}
	if bytes.Contains(data, []byte("/ObjStm")) || bytes.Contains(data, []byte("/XRef")) {
		return nil, fmt.Errorf("%w: object or cross-reference streams", ErrUnsupported)
	}
	objs, order := scanObjects(data)
	if len(objs) == 0 {
		return nil, fmt.Errorf("%w: no objects", ErrMalformed)
	}

	budget := maxInflated
	var out []Text
	pageNo := 0
	for _, pg := range pageObjects(objs, order) {
		content, err := pageContent(objs, pg, &budget)
		if err != nil {
			return nil, err
		}
		texts, err := extractText(content)
		if err != nil {
			return nil, err
		}
		if len(texts) == 0 {
			continue
		}
		pageNo++
		for i := range texts {
			texts[i].Page = pageNo
		}
		out = append(out, texts...)
	}
	if len(out) == 0 {
		return nil, ErrNoText
	}
	return out, nil
}

// pageObjects lists the page dictionaries in page-tree order. Without a usable
// tree it falls back to every /Type /Page object in object-number order.
func pageObjects(objs map[int]*object, order []int) []dict {
	var catalog dict
	for _, n := range order {
		if d, ok := objs[n].val.(dict); ok && d["Type"] == name("Catalog") {
			catalog = d // the last catalog wins, as after an incremental update
		}
	}
	var pages []dict
	if catalog != nil {
		walkPages(objs, catalog["Pages"], map[int]bool{}, 0, &pages)
	}
	if len(pages) > 0 {
		return pages
	}
	nums := append([]int(nil), order...)
	sort.Ints(nums)
	for _, n := range nums {
		if d, ok := objs[n].val.(dict); ok && d["Type"] == name("Page") {
			pages = append(pages, d)
		}
	}
	return pages
}

func walkPages(objs map[int]*object, node any, seen map[int]bool, depth int, out *[]dict) {
	r, ok := node.(ref)
	if !ok || depth > maxDepth || seen[int(r)] {
		return
	}
	seen[int(r)] = true
	o := objs[int(r)]
	if o == nil {
		return
	}
	d, ok := o.val.(dict)
	if !ok {
		return
	}
	if kids, ok := d["Kids"].([]any); ok {
		for _, k := range kids {
			walkPages(objs, k, seen, depth+1, out)
		}
		return
	}
	if d["Type"] == name("Page") {
		*out = append(*out, d)
	}
}

// pageContent concatenates the inflated content streams of a page.
func pageContent(objs map[int]*object, page dict, budget *int) ([]byte, error) {
	var refs []any
	switch c := page["Contents"].(type) {
	case ref:
		refs = []any{c}
	case []any:
		refs = c
	}
	var buf []byte
	for _, rv := range refs {
		r, ok := rv.(ref)
		if !ok {
			return nil, fmt.Errorf("%w: bad contents entry", ErrMalformed)
		}
		o := objs[int(r)]
		if o == nil || !o.hasStream {
			return nil, fmt.Errorf("%w: missing content stream", ErrMalformed)
		}
		body, err := decodeStream(o, budget)
		if err != nil {
			return nil, err
		}
		buf = append(buf, body...)
		buf = append(buf, '\n')
	}
	return buf, nil
}

// decodeStream inflates a Flate stream, or returns an unfiltered one as is.
func decodeStream(o *object, budget *int) ([]byte, error) {
	d, _ := o.val.(dict)
	filters := d["Filter"]
	if arr, ok := filters.([]any); ok && len(arr) == 1 {
		filters = arr[0]
	}
	switch filters {
	case nil:
		if len(o.stream) > *budget {
			return nil, ErrTooLarge
		}
		*budget -= len(o.stream)
		return o.stream, nil
	case name("FlateDecode"), name("Fl"):
	default:
		return nil, fmt.Errorf("%w: stream filter", ErrUnsupported)
	}
	zr, err := zlib.NewReader(bytes.NewReader(o.stream))
	if err != nil {
		return nil, fmt.Errorf("%w: bad compressed stream", ErrMalformed)
	}
	defer zr.Close()
	body, err := io.ReadAll(io.LimitReader(zr, int64(*budget)+1))
	if len(body) > *budget {
		return nil, ErrTooLarge
	}
	if err != nil {
		return nil, fmt.Errorf("%w: bad compressed stream", ErrMalformed)
	}
	*budget -= len(body)
	return body, nil
}

// extractText runs the content-stream operators that position and draw text.
func extractText(content []byte) ([]Text, error) {
	lx := &lexer{data: content}
	var (
		ops          []token
		out          []Text
		size         float64
		x, y         float64 // text matrix translation
		lineX, lineY float64
	)
	for {
		t, err := lx.next()
		if err != nil {
			return nil, err
		}
		switch t.kind {
		case kEOF:
			return out, nil
		case kArrayOpen, kDictOpen:
			if err := skipComposite(lx); err != nil {
				return nil, err
			}
			ops = push(ops, token{kind: t.kind})
		case kArrayClose, kDictClose:
			return nil, fmt.Errorf("%w: unbalanced brackets", ErrMalformed)
		case kNumber, kName, kString:
			ops = push(ops, t)
		case kKeyword:
			switch t.str {
			case "BT":
				x, y, lineX, lineY = 0, 0, 0, 0
			case "Tm":
				n, ok := numbers(ops, 6)
				if !ok {
					return nil, fmt.Errorf("%w: bad Tm", ErrMalformed)
				}
				x, y = n[4], n[5]
				lineX, lineY = x, y
			case "Td", "TD":
				n, ok := numbers(ops, 2)
				if !ok {
					return nil, fmt.Errorf("%w: bad %s", ErrMalformed, t.str)
				}
				lineX, lineY = lineX+n[0], lineY+n[1]
				x, y = lineX, lineY
			case "Tf":
				if len(ops) < 2 || ops[len(ops)-1].kind != kNumber || ops[len(ops)-2].kind != kName {
					return nil, fmt.Errorf("%w: bad Tf", ErrMalformed)
				}
				size = ops[len(ops)-1].num
			case "Tj":
				if len(ops) < 1 || ops[len(ops)-1].kind != kString {
					return nil, fmt.Errorf("%w: bad Tj", ErrMalformed)
				}
				if size >= minVisibleSize {
					out = append(out, Text{X: x, Y: y, Size: size, S: decodeWinAnsi(ops[len(ops)-1].raw)})
				}
			case "TJ", "'", "\"":
				if size >= minVisibleSize {
					return nil, fmt.Errorf("%w: text operator %s", ErrUnsupported, t.str)
				}
			case "BI":
				if err := skipInlineImage(lx); err != nil {
					return nil, err
				}
			}
			ops = ops[:0]
		}
	}
}

func push(ops []token, t token) []token {
	if len(ops) >= maxOperands {
		// Keep the most recent operands; an operator reads only the last few.
		copy(ops, ops[1:])
		ops = ops[:len(ops)-1]
	}
	return append(ops, t)
}

// numbers returns the last n operands if they are all numbers.
func numbers(ops []token, n int) ([]float64, bool) {
	if len(ops) < n {
		return nil, false
	}
	out := make([]float64, n)
	for i, t := range ops[len(ops)-n:] {
		if t.kind != kNumber {
			return nil, false
		}
		out[i] = t.num
	}
	return out, true
}

// skipComposite consumes the rest of an array or dictionary whose opening
// token has just been read, including nested ones.
func skipComposite(lx *lexer) error {
	depth := 1
	for depth > 0 {
		t, err := lx.next()
		if err != nil {
			return err
		}
		switch t.kind {
		case kArrayOpen, kDictOpen:
			depth++
		case kArrayClose, kDictClose:
			depth--
		case kEOF:
			return fmt.Errorf("%w: unterminated array or dictionary", ErrMalformed)
		}
	}
	return nil
}

// skipInlineImage moves past the EI that ends an inline image.
func skipInlineImage(lx *lexer) error {
	d := lx.data
	for i := lx.pos; i+1 < len(d); i++ {
		if d[i] == 'E' && d[i+1] == 'I' && i > 0 && isSpace(d[i-1]) && (i+2 == len(d) || isSpace(d[i+2])) {
			lx.pos = i + 2
			return nil
		}
	}
	return fmt.Errorf("%w: unterminated inline image", ErrMalformed)
}
