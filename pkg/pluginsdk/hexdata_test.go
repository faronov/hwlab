package pluginsdk

import (
	"encoding/hex"
	"testing"
)

func TestDecodeHexData(t *testing.T) {
	for in, want := range map[string]string{
		"12345678":       "12345678",
		"0x12345678":     "12345678",
		"0x1234":         "1234",
		"0XdeadBEEF":     "deadbeef",
		" 12 34\t56 78 ": "12345678",
		"12:34-56_78,9a": "123456789a",
		"1234 5678":      "12345678",
		"0x12:34":        "1234",
		"00":             "00",
		"0x00ff":         "00ff",
	} {
		got, err := DecodeHexData(in)
		if err != nil || hex.EncodeToString(got) != want {
			t.Errorf("%q -> %x, %v; want %s", in, got, err, want)
		}
	}
	for _, in := range []string{
		"1:2:3:4", "1,2,3,4", "-12", "12::34", "12:", ":12", "12  34", "12 :34",
		"", "0x", "123", "12 3", "zz", "0x0x12", "1234567g", "0x12 0x34",
	} {
		if b, err := DecodeHexData(in); err == nil {
			t.Errorf("%q accepted as %x", in, b)
		}
	}
}
