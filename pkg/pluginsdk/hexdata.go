package pluginsdk

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// DecodeHexData decodes a byte-string argument ("data"): hex digits in memory
// order, always hex even when every digit is decimal ("12345678" is the four
// bytes 12 34 56 78). One leading 0x/0X is allowed. Groups of digits may be
// separated by a single space, tab, ':', '-', '_' or ',' ("12:34:56:78",
// "1234 5678"); every group needs an even number of digits, and a leading,
// trailing or doubled separator is refused, so "1:2:3:4" or "-12" is an error
// rather than a silently different byte string.
func DecodeHexData(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if len(s) > 1 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X') {
		s = s[2:]
	}
	if s == "" {
		return nil, fmt.Errorf("no hex digits")
	}
	var out []byte
	start := 0
	for i := 0; i <= len(s); i++ {
		if i < len(s) && !isHexSeparator(s[i]) {
			continue
		}
		g := s[start:i]
		switch {
		case g == "":
			return nil, fmt.Errorf("separator at offset %d: separators go between groups of hex digits, one at a time", i)
		case len(g)%2 != 0:
			return nil, fmt.Errorf("group %q has %d hex digits: want two per byte", g, len(g))
		}
		b, err := hex.DecodeString(g)
		if err != nil {
			return nil, fmt.Errorf("group %q is not hex: %w", g, err)
		}
		out = append(out, b...)
		start = i + 1
	}
	return out, nil
}

func isHexSeparator(c byte) bool {
	switch c {
	case ' ', '\t', ':', '-', '_', ',':
		return true
	}
	return false
}
