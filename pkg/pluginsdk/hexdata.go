package pluginsdk

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// DecodeHexData decodes a byte-string argument ("data"): hex digits, two per
// byte in memory order, always hex even when every digit is decimal
// ("12345678" is the four bytes 12 34 56 78). One leading 0x/0X is allowed,
// and whitespace, ':', '-', '_' and ',' between bytes are ignored. An odd
// number of digits is an error, never padded.
func DecodeHexData(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if len(s) > 1 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X') {
		s = s[2:]
	}
	s = strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\r', ':', '-', '_', ',':
			return -1
		}
		return r
	}, s)
	if s == "" {
		return nil, fmt.Errorf("no hex digits")
	}
	if len(s)%2 != 0 {
		return nil, fmt.Errorf("%d hex digits: want two per byte", len(s))
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("not hex: %w", err)
	}
	return b, nil
}
