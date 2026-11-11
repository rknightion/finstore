package pdftext

import (
	"fmt"
	"strconv"
)

type kind int

const (
	kEOF kind = iota
	kNumber
	kName
	kString
	kArrayOpen
	kArrayClose
	kDictOpen
	kDictClose
	kKeyword
)

// token is one lexical element of PDF syntax. str holds a name or keyword,
// raw holds the decoded bytes of a string.
type token struct {
	kind kind
	num  float64
	str  string
	raw  []byte
}

// lexer tokenises PDF syntax (object and content-stream alike).
type lexer struct {
	data []byte
	pos  int
}

func isSpace(c byte) bool {
	switch c {
	case 0, '\t', '\n', '\f', '\r', ' ':
		return true
	}
	return false
}

func isDelim(c byte) bool {
	switch c {
	case '(', ')', '<', '>', '[', ']', '{', '}', '/', '%':
		return true
	}
	return false
}

func (l *lexer) skipSpace() {
	for l.pos < len(l.data) {
		c := l.data[l.pos]
		switch {
		case isSpace(c):
			l.pos++
		case c == '%':
			for l.pos < len(l.data) && l.data[l.pos] != '\n' && l.data[l.pos] != '\r' {
				l.pos++
			}
		default:
			return
		}
	}
}

func (l *lexer) next() (token, error) {
	l.skipSpace()
	if l.pos >= len(l.data) {
		return token{kind: kEOF}, nil
	}
	c := l.data[l.pos]
	switch c {
	case '(':
		return l.literalString()
	case '<':
		if l.peekByte(1) == '<' {
			l.pos += 2
			return token{kind: kDictOpen}, nil
		}
		return l.hexString()
	case '>':
		if l.peekByte(1) == '>' {
			l.pos += 2
			return token{kind: kDictClose}, nil
		}
		return token{}, fmt.Errorf("%w: stray '>'", ErrMalformed)
	case '[':
		l.pos++
		return token{kind: kArrayOpen}, nil
	case ']':
		l.pos++
		return token{kind: kArrayClose}, nil
	case ')':
		return token{}, fmt.Errorf("%w: stray ')'", ErrMalformed)
	case '{', '}':
		l.pos++
		return token{kind: kKeyword, str: string(c)}, nil
	case '/':
		return l.name(), nil
	}
	run := l.run()
	if isNumber(run) {
		f, err := strconv.ParseFloat(run, 64)
		if err != nil {
			return token{}, fmt.Errorf("%w: bad number", ErrMalformed)
		}
		return token{kind: kNumber, num: f}, nil
	}
	return token{kind: kKeyword, str: run}, nil
}

func (l *lexer) peekByte(off int) byte {
	if l.pos+off < len(l.data) {
		return l.data[l.pos+off]
	}
	return 0
}

// run consumes a maximal run of regular characters.
func (l *lexer) run() string {
	start := l.pos
	for l.pos < len(l.data) && !isSpace(l.data[l.pos]) && !isDelim(l.data[l.pos]) {
		l.pos++
	}
	return string(l.data[start:l.pos])
}

func isNumber(s string) bool {
	i := 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	digits, dots := 0, 0
	for ; i < len(s); i++ {
		switch {
		case s[i] >= '0' && s[i] <= '9':
			digits++
		case s[i] == '.':
			dots++
		default:
			return false
		}
	}
	return digits > 0 && dots <= 1
}

func (l *lexer) name() token {
	l.pos++ // the slash
	raw := l.run()
	out := make([]byte, 0, len(raw))
	for i := 0; i < len(raw); i++ {
		if raw[i] == '#' && i+2 < len(raw) {
			hi, ok1 := hexVal(raw[i+1])
			lo, ok2 := hexVal(raw[i+2])
			if ok1 && ok2 {
				out = append(out, hi<<4|lo)
				i += 2
				continue
			}
		}
		out = append(out, raw[i])
	}
	return token{kind: kName, str: string(out)}
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

func (l *lexer) hexString() (token, error) {
	l.pos++ // '<'
	var out []byte
	var hi byte
	half := false
	for l.pos < len(l.data) {
		c := l.data[l.pos]
		l.pos++
		if c == '>' {
			if half {
				out = append(out, hi<<4)
			}
			return token{kind: kString, raw: out}, nil
		}
		if isSpace(c) {
			continue
		}
		v, ok := hexVal(c)
		if !ok {
			return token{}, fmt.Errorf("%w: bad hex string", ErrMalformed)
		}
		if half {
			out = append(out, hi<<4|v)
		} else {
			hi = v
		}
		half = !half
	}
	return token{}, fmt.Errorf("%w: unterminated hex string", ErrMalformed)
}

func (l *lexer) literalString() (token, error) {
	l.pos++ // '('
	var out []byte
	depth := 1
	for l.pos < len(l.data) {
		c := l.data[l.pos]
		l.pos++
		switch c {
		case '(':
			depth++
			out = append(out, c)
		case ')':
			depth--
			if depth == 0 {
				return token{kind: kString, raw: out}, nil
			}
			out = append(out, c)
		case '\\':
			out = l.escape(out)
		default:
			out = append(out, c)
		}
	}
	return token{}, fmt.Errorf("%w: unterminated string", ErrMalformed)
}

// escape handles the byte(s) after a backslash inside a literal string.
func (l *lexer) escape(out []byte) []byte {
	if l.pos >= len(l.data) {
		return out
	}
	c := l.data[l.pos]
	l.pos++
	switch c {
	case 'n':
		return append(out, '\n')
	case 'r':
		return append(out, '\r')
	case 't':
		return append(out, '\t')
	case 'b':
		return append(out, '\b')
	case 'f':
		return append(out, '\f')
	case '\r':
		if l.peekByte(0) == '\n' {
			l.pos++
		}
		return out
	case '\n':
		return out
	}
	if c >= '0' && c <= '7' {
		v := int(c - '0')
		for n := 0; n < 2 && l.pos < len(l.data); n++ {
			d := l.data[l.pos]
			if d < '0' || d > '7' {
				break
			}
			v = v*8 + int(d-'0')
			l.pos++
		}
		return append(out, byte(v&0xFF)) //nolint:gosec // masked to a byte
	}
	return append(out, c)
}
