package pdftext

import (
	"bytes"
	"fmt"
	"strconv"
)

const maxDepth = 32

// Parsed object values. Numbers are float64, strings are []byte, arrays are
// []any, and true, false and null are bool, bool and nil.
type (
	name string
	ref  int
	dict map[string]any
)

// object is one indirect object: its value and, if it carries one, the raw
// (still encoded) stream.
type object struct {
	val       any
	stream    []byte
	hasStream bool
}

// scanObjects finds every indirect object in the file by looking for
// "N G obj" and parsing what follows. Stream bodies are skipped, so binary
// data is never tokenised. A later object with the same number replaces an
// earlier one, as in an incremental update.
func scanObjects(data []byte) (map[int]*object, []int) {
	objs := map[int]*object{}
	var order []int
	pos := 0
	for {
		i := bytes.Index(data[pos:], []byte("obj"))
		if i < 0 {
			return objs, order
		}
		i += pos
		pos = i + 3
		num, ok := objectNumber(data, i)
		if !ok || (pos < len(data) && !isSpace(data[pos]) && !isDelim(data[pos])) {
			continue
		}
		lx := &lexer{data: data, pos: pos}
		val, err := parseValue(lx, 0)
		if err != nil {
			continue
		}
		o := &object{val: val}
		lx.skipSpace()
		if bytes.HasPrefix(data[lx.pos:], []byte("stream")) {
			body, end, ok := streamBody(data, lx.pos+len("stream"), val)
			if ok {
				o.stream, o.hasStream, pos = body, true, end
			}
		} else {
			pos = lx.pos
		}
		if _, seen := objs[num]; !seen {
			order = append(order, num)
		}
		objs[num] = o
	}
}

// objectNumber reads the "N G " that precedes the "obj" keyword at i.
func objectNumber(data []byte, i int) (int, bool) {
	j := i
	skipBackSpace := func() bool {
		n := j
		for j > 0 && isSpace(data[j-1]) {
			j--
		}
		return j < n
	}
	digitsBack := func() (int, bool) {
		end := j
		for j > 0 && data[j-1] >= '0' && data[j-1] <= '9' {
			j--
		}
		if j == end || end-j > 9 {
			return 0, false
		}
		n, err := strconv.Atoi(string(data[j:end]))
		return n, err == nil
	}
	if !skipBackSpace() {
		return 0, false
	}
	if _, ok := digitsBack(); !ok {
		return 0, false
	}
	if !skipBackSpace() {
		return 0, false
	}
	return digitsBack()
}

// streamBody returns the stream bytes that start after the "stream" keyword
// at off, and the offset after "endstream".
func streamBody(data []byte, off int, val any) ([]byte, int, bool) {
	switch {
	case bytes.HasPrefix(data[off:], []byte("\r\n")):
		off += 2
	case off < len(data) && (data[off] == '\n' || data[off] == '\r'):
		off++
	}
	if d, ok := val.(dict); ok {
		if n, ok := d["Length"].(float64); ok && n >= 0 && n <= float64(len(data)-off) {
			end := off + int(n)
			rest := bytes.TrimLeft(data[end:], "\r\n \t")
			if bytes.HasPrefix(rest, []byte("endstream")) {
				return data[off:end], len(data) - len(rest) + len("endstream"), true
			}
		}
	}
	i := bytes.Index(data[off:], []byte("endstream"))
	if i < 0 {
		return nil, 0, false
	}
	body := bytes.TrimSuffix(bytes.TrimSuffix(data[off:off+i], []byte("\n")), []byte("\r"))
	return body, off + i + len("endstream"), true
}

// parseValue parses one PDF object value: a number, name, string, array,
// dictionary, indirect reference or keyword constant.
func parseValue(lx *lexer, depth int) (any, error) {
	if depth > maxDepth {
		return nil, fmt.Errorf("%w: nesting too deep", ErrMalformed)
	}
	t, err := lx.next()
	if err != nil {
		return nil, err
	}
	switch t.kind {
	case kNumber:
		return numberOrRef(lx, t), nil
	case kName:
		return name(t.str), nil
	case kString:
		return t.raw, nil
	case kArrayOpen:
		arr := []any{}
		for {
			save := lx.pos
			t, err := lx.next()
			if err != nil {
				return nil, err
			}
			switch t.kind {
			case kArrayClose:
				return arr, nil
			case kEOF:
				return nil, fmt.Errorf("%w: unterminated array", ErrMalformed)
			}
			lx.pos = save
			v, err := parseValue(lx, depth+1)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
	case kDictOpen:
		d := dict{}
		for {
			t, err := lx.next()
			if err != nil {
				return nil, err
			}
			switch t.kind {
			case kDictClose:
				return d, nil
			case kName:
				v, err := parseValue(lx, depth+1)
				if err != nil {
					return nil, err
				}
				d[t.str] = v
			default:
				return nil, fmt.Errorf("%w: bad dictionary", ErrMalformed)
			}
		}
	case kKeyword:
		switch t.str {
		case "true":
			return true, nil
		case "false":
			return false, nil
		case "null":
			return nil, nil
		}
	}
	return nil, fmt.Errorf("%w: unexpected token", ErrMalformed)
}

// numberOrRef turns "N G R" into a reference and any other number into a
// float64.
func numberOrRef(lx *lexer, first token) any {
	save := lx.pos
	if first.num >= 0 && first.num < 1e9 && first.num == float64(int(first.num)) {
		g, err1 := lx.next()
		r, err2 := lx.next()
		if err1 == nil && err2 == nil && g.kind == kNumber && r.kind == kKeyword && r.str == "R" {
			return ref(int(first.num))
		}
	}
	lx.pos = save
	return first.num
}
