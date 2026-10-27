package usb

import (
	"reflect"
	"strings"
	"testing"
)

// jlinkConfig is shaped like the SEGGER J-Link OB configuration read on macOS:
// an IAD and CDC functional descriptors before the CDC interfaces, an
// alternate setting, and the vendor bulk interface last.
func jlinkConfig() []byte {
	b := []byte{
		9, 0x02, 0, 0, 3, 1, 0, 0x80, 50, // CONFIGURATION, wTotalLength patched below
		8, 0x0B, 0, 2, 0x02, 0x02, 0x01, 0, // IAD
		9, 0x04, 0, 0, 1, 0x02, 0x02, 0x01, 0, // INTERFACE 0 CDC
		5, 0x24, 0x00, 0x10, 0x01, // CDC header (class-specific, skipped)
		7, 0x05, 0x82, 0x03, 64, 0, 1, // EP 0x82 interrupt
		9, 0x04, 1, 0, 2, 0x0A, 0, 0, 0, // INTERFACE 1 CDC DATA
		7, 0x05, 0x81, 0x02, 64, 0, 0, // EP 0x81 bulk
		7, 0x05, 0x01, 0x02, 64, 0, 0, // EP 0x01 bulk
		9, 0x04, 4, 0, 2, 0xFF, 0xFF, 0xFF, 0, // INTERFACE 4 vendor
		7, 0x05, 0x85, 0x02, 0x40, 0x00, 0, // EP 0x85 bulk
		7, 0x05, 0x03, 0x02, 0x40, 0x00, 0, // EP 0x03 bulk
		9, 0x04, 4, 1, 1, 0xFF, 0xFF, 0xFF, 0, // INTERFACE 4 alt 1
		7, 0x05, 0x86, 0x01, 0x00, 0x14, 1, // EP 0x86 isoc, mult bits kept raw
	}
	b[2], b[3] = byte(len(b)), byte(len(b)>>8)
	return b
}

func TestParseConfigDescriptor(t *testing.T) {
	got, err := parseConfigDescriptor(jlinkConfig())
	if err != nil {
		t.Fatal(err)
	}
	want := []descInterface{
		{Number: 0, Class: 0x02, Endpoints: []Endpoint{{Address: 0x82, Type: "Interrupt", MaxPacket: 64}}},
		{Number: 1, Class: 0x0A, Endpoints: []Endpoint{{Address: 0x81, Type: "Bulk", MaxPacket: 64}, {Address: 0x01, Type: "Bulk", MaxPacket: 64}}},
		{Number: 4, Class: 0xFF, Endpoints: []Endpoint{{Address: 0x85, Type: "Bulk", MaxPacket: 64}, {Address: 0x03, Type: "Bulk", MaxPacket: 64}}},
		{Number: 4, Alt: 1, Class: 0xFF, Endpoints: []Endpoint{{Address: 0x86, Type: "Isoc", MaxPacket: 0x1400}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestParseConfigDescriptorBounds(t *testing.T) {
	full := jlinkConfig()
	// Trailing bytes past wTotalLength (another descriptor in the buffer)
	// are ignored.
	extra := append(append([]byte(nil), full...), 9, 0x04, 9, 0, 0, 0xFF, 0, 0, 0)
	if got, err := parseConfigDescriptor(extra); err != nil || len(got) != 4 {
		t.Fatalf("trailing bytes: %v %v", got, err)
	}
	cases := map[string][]byte{
		"too short":           full[:5],
		"not configuration":   append([]byte{9, 0x01}, full[2:]...),
		"total beyond buffer": full[:len(full)-1],
		"zero bLength":        func() []byte { b := append([]byte(nil), full...); b[9] = 0; return b }(),
		"descriptor overruns": func() []byte { b := append([]byte(nil), full...); b[len(b)-7] = 9; return b }(),
	}
	for name, b := range cases {
		if _, err := parseConfigDescriptor(b); err == nil || !strings.HasPrefix(err.Error(), "usb: ") {
			t.Errorf("%s: want an error, got %v", name, err)
		}
	}
}
