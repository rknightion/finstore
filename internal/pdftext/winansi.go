package pdftext

import "strings"

// cp1252High maps bytes 0x80 to 0x9F of Windows-1252 to Unicode. The five
// bytes Windows-1252 leaves undefined map to the C1 control of the same value.
var cp1252High = [32]rune{
	0x20AC, 0x0081, 0x201A, 0x0192, 0x201E, 0x2026, 0x2020, 0x2021,
	0x02C6, 0x2030, 0x0160, 0x2039, 0x0152, 0x008D, 0x017D, 0x008F,
	0x0090, 0x2018, 0x2019, 0x201C, 0x201D, 0x2022, 0x2013, 0x2014,
	0x02DC, 0x2122, 0x0161, 0x203A, 0x0153, 0x009D, 0x017E, 0x0178,
}

// decodeWinAnsi converts WinAnsiEncoding (Windows-1252) bytes to UTF-8.
func decodeWinAnsi(b []byte) string {
	var sb strings.Builder
	sb.Grow(len(b))
	for _, c := range b {
		switch {
		case c < 0x80:
			sb.WriteByte(c)
		case c < 0xA0:
			sb.WriteRune(cp1252High[c-0x80])
		default: // 0xA0 to 0xFF match Latin-1, so the byte is the code point
			sb.WriteRune(rune(c))
		}
	}
	return sb.String()
}
