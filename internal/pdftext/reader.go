// Package pdftext reads the visible text, with positions, from the simple PDF
// documents that bank statements are: unencrypted, no object or cross-reference
// streams, Flate-compressed page content in which every visible string is drawn
// by one Tj with a literal or hex string, placed by its own absolute Tm (a Td
// after that Tm is honoured), sized by Tf, in text rendering mode 0, with no
// coordinate transformation (cm) in effect. Anything else is rejected, so a
// changed layout stops an import instead of producing wrong rows. In particular:
//
//   - a visible TJ, ' or " is refused;
//   - a visible Tj with no Tm since the previous visible Tj (or since BT) is
//     refused, as is one after T*, or after a Td that follows a Tm that scales
//     or rotates. Invisible text (under 1 point) is exempt: it is not returned,
//     and statements draw hidden labels straight after a visible string;
//   - any Tr operand other than 0 is refused;
//   - a visible Tj while a non-identity cm is in effect is refused. The graphics state
//     stack is tracked (q pushes, Q pops), so a cm wrapped in q/Q around an
//     image or form Do, with no text drawn inside, is accepted.
//
// Accepted limits, deliberately not checked: fonts and their encodings are
// never inspected (every string is decoded as WinAnsi), the scale and rotation
// parts of Tm are ignored (only its translation is used, and Size is the Tf
// operand), and form XObjects drawn by Do are skipped, not entered. A file
// that relies on any of those to place or spell its text is misread, but a
// misread statement fails the importer's required header fields and its
// numeric checks (totals, running balance, closing balance) rather than
// producing rows.
//
// Size is bounded: the inflated content of all pages, the number of pages and
// the number of visible texts are capped, and passing any cap returns
// ErrTooLarge as soon as it happens.
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
	// maxTexts bounds the number of visible texts a file may yield; a small
	// stream of short strings would otherwise expand to millions of them.
	maxTexts = 200_000
	// maxPages bounds the number of page objects a file may have.
	maxPages = 2000
	// maxGraphicsDepth bounds the q/Q nesting of a content stream.
	maxGraphicsDepth = 64
)

// Errors returned by Read, to be tested with errors.Is.
var (
	ErrMalformed   = errors.New("pdftext: malformed PDF")
	ErrEncrypted   = errors.New("pdftext: encrypted PDF")
	ErrUnsupported = errors.New("pdftext: unsupported PDF structure")
	ErrNoText      = errors.New("pdftext: no text found")
	ErrTooLarge    = errors.New("pdftext: PDF too large")
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

	pages := pageObjects(objs, order)
	if len(pages) > maxPages {
		return nil, fmt.Errorf("%w: too many pages", ErrTooLarge)
	}
	budget := maxInflated
	var out []Text
	pageNo := 0
	for _, pg := range pages {
		content, err := pageContent(objs, pg, &budget)
		if err != nil {
			return nil, err
		}
		texts, err := extractText(content, maxTexts-len(out))
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
			return nil, fmt.Errorf("%w: content streams", ErrTooLarge)
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
		return nil, fmt.Errorf("%w: content streams", ErrTooLarge)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: bad compressed stream", ErrMalformed)
	}
	*budget -= len(body)
	return body, nil
}

// extractText runs the content-stream operators that position and draw text.
// It returns ErrTooLarge as soon as the page yields more than limit texts.
func extractText(content []byte, limit int) ([]Text, error) {
	lx := &lexer{data: content}
	var (
		ops          []token
		out          []Text
		size         float64
		x, y         float64 // text matrix translation
		lineX, lineY float64
		// placed is true from a Tm until the next Tj: each Tj needs its own Tm.
		placed bool
		// scaled is true when the last Tm scales or rotates, so a Td in it
		// would move by more or less than its operands say.
		scaled bool
		// transformed is the graphics state stack: whether a non-identity cm
		// is in effect at each q level, the current level last.
		transformed = []bool{false}
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
			case "q":
				if len(transformed) >= maxGraphicsDepth {
					return nil, fmt.Errorf("%w: graphics state nesting", ErrUnsupported)
				}
				transformed = append(transformed, transformed[len(transformed)-1])
			case "Q":
				// An unmatched Q keeps the current state, the cautious choice.
				if len(transformed) > 1 {
					transformed = transformed[:len(transformed)-1]
				}
			case "cm":
				n, ok := numbers(ops, 6)
				if !ok {
					return nil, fmt.Errorf("%w: bad cm", ErrMalformed)
				}
				if [6]float64(n) != [6]float64{1, 0, 0, 1, 0, 0} {
					transformed[len(transformed)-1] = true
				}
			case "Tr":
				n, ok := numbers(ops, 1)
				if !ok {
					return nil, fmt.Errorf("%w: bad Tr", ErrMalformed)
				}
				if n[0] != 0 {
					return nil, fmt.Errorf("%w: text rendering mode", ErrUnsupported)
				}
			case "BT":
				x, y, lineX, lineY = 0, 0, 0, 0
				placed, scaled = false, false
			case "Tm":
				n, ok := numbers(ops, 6)
				if !ok {
					return nil, fmt.Errorf("%w: bad Tm", ErrMalformed)
				}
				x, y = n[4], n[5]
				lineX, lineY = x, y
				placed, scaled = true, n[0] != 1 || n[1] != 0 || n[2] != 0 || n[3] != 1
			case "Td", "TD":
				n, ok := numbers(ops, 2)
				if !ok {
					return nil, fmt.Errorf("%w: bad %s", ErrMalformed, t.str)
				}
				lineX, lineY = lineX+n[0], lineY+n[1]
				x, y = lineX, lineY
				if scaled {
					placed = false
				}
			case "T*":
				placed = false
			case "Tf":
				if len(ops) < 2 || ops[len(ops)-1].kind != kNumber || ops[len(ops)-2].kind != kName {
					return nil, fmt.Errorf("%w: bad Tf", ErrMalformed)
				}
				size = ops[len(ops)-1].num
			case "Tj":
				if len(ops) < 1 || ops[len(ops)-1].kind != kString {
					return nil, fmt.Errorf("%w: bad Tj", ErrMalformed)
				}
				// Invisible text is not returned, so where it lands does not matter: it neither
				// needs a Tm nor uses one up. Statements draw a hidden label straight after a
				// visible string, with no Tm between them. A hidden string drawn before a
				// visible one moves it by less than a point, which no column edge is near.
				if size < minVisibleSize {
					break
				}
				if !placed {
					return nil, fmt.Errorf("%w: Tj without its own Tm", ErrUnsupported)
				}
				if transformed[len(transformed)-1] {
					return nil, fmt.Errorf("%w: text under a cm", ErrUnsupported)
				}
				placed = false
				if len(out) >= limit {
					return nil, fmt.Errorf("%w: too many texts", ErrTooLarge)
				}
				out = append(out, Text{X: x, Y: y, Size: size, S: decodeWinAnsi(ops[len(ops)-1].raw)})
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
