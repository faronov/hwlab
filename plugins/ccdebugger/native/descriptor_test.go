package native

import (
	"encoding/hex"
	"errors"
	"testing"
)

// Device and configuration descriptors read from a real SmartRF04DD (11a0:eb20).
const smartRF04Desc = "1201100100000040a01120eb5100010200010902200001010040320904000002ffffff000705820240000507050202400005"

func TestCheckBulkEndpoints(t *testing.T) {
	real, _ := hex.DecodeString(smartRF04Desc)
	if err := checkBulkEndpoints(real, 0x82, 0x02); err != nil {
		t.Fatalf("SmartRF04 endpoints: %v", err)
	}
	if err := checkBulkEndpoints(real, EndpointIn, EndpointOut); !errors.Is(err, ErrTransport) {
		t.Fatalf("CC Debugger endpoints on a SmartRF04: %v", err)
	}
	cfg := real[18:]
	mutate := func(f func(b []byte)) []byte {
		b := append(append([]byte(nil), real[:18]...), cfg...)
		f(b[18:])
		return b
	}
	for name, b := range map[string][]byte{
		"interrupt endpoint": mutate(func(c []byte) { c[9+9+3] = 0x03 }),
		"interface 1":        mutate(func(c []byte) { c[9+2] = 1 }),
		"configuration 2":    mutate(func(c []byte) { c[5] = 2 }),
		"truncated":          real[:len(real)-3],
		"zero length":        mutate(func(c []byte) { c[9] = 0 }),
	} {
		if err := checkBulkEndpoints(b, 0x82, 0x02); !errors.Is(err, ErrTransport) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestLookupAdapter(t *testing.T) {
	if a, ok := LookupAdapter(0x11A0, 0xEB20); !ok || a.EndpointIn != 0x82 || a.EndpointOut != 0x02 {
		t.Fatalf("SmartRF04DD = %+v, %v", a, ok)
	}
	if a, ok := LookupAdapter(VID, PID); !ok || a.EndpointIn != EndpointIn || a.EndpointOut != EndpointOut {
		t.Fatalf("CC Debugger = %+v, %v", a, ok)
	}
	if _, ok := LookupAdapter(0x11A0, 0xDB20); ok {
		t.Fatal("11a0:db20 is not verified on hardware")
	}
}

// The simulator refuses traffic on any other endpoint, so a full attach and
// memory round trip proves the debugger uses the transport's endpoints.
func TestSmartRF04EndpointsAreUsed(t *testing.T) {
	a, _ := LookupAdapter(0x11A0, 0xEB20)
	s := NewSimulatorFor(a).(*sim)
	d := mustNew(t, s, cfg(ResetDebugSession, allPerms()))
	if _, err := d.AttachReset(); err != nil {
		t.Fatal(err)
	}
	if err := d.WriteXDATA(0x0100, []byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	if b, err := d.ReadXDATA(0x0100, 3); err != nil || string(b) != "\x01\x02\x03" {
		t.Fatalf("readback %x, %v", b, err)
	}
	if s.bulkReads == 0 {
		t.Fatal("no bulk reads")
	}
}
