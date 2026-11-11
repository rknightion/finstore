package importer

import (
	"fmt"
	"strings"
)

const maxIntegerDigits = 13

// parsePounds parses an amount in pounds into pence without floating point:
// an optional pound sign, an integer part with optional thousands commas, a
// point and exactly two decimals. With signed, a leading minus sign is
// allowed, before or after the pound sign.
func parsePounds(s string, signed bool) (int64, error) {
	bad := fmt.Errorf("%w: bad amount", ErrFormat)
	s = strings.TrimSpace(s)
	neg := false
	if signed && strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}
	s = strings.TrimPrefix(s, "£")
	if signed && !neg && strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}
	whole, frac, ok := strings.Cut(s, ".")
	if !ok || len(frac) != 2 || !allDigits(frac) || !validWhole(whole) {
		return 0, bad
	}
	var n int64
	for _, c := range whole + frac {
		if c != ',' {
			n = n*10 + int64(c-'0')
		}
	}
	if neg {
		n = -n
	}
	return n, nil
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

// validWhole accepts plain digits, or digits grouped in threes by commas.
func validWhole(s string) bool {
	if len(s) == 0 || len(s) > maxIntegerDigits+4 {
		return false
	}
	if !strings.Contains(s, ",") {
		return allDigits(s) && len(s) <= maxIntegerDigits
	}
	groups := strings.Split(s, ",")
	if len(groups[0]) < 1 || len(groups[0]) > 3 || !allDigits(groups[0]) {
		return false
	}
	for _, g := range groups[1:] {
		if len(g) != 3 || !allDigits(g) {
			return false
		}
	}
	return true
}
