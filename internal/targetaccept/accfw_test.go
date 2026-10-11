package targetaccept_test

import (
	"encoding/binary"
	"testing"

	"github.com/faronov/hwlab/internal/targetaccept"
)

func TestAcceptanceFirmwareLayout(t *testing.T) {
	f := targetaccept.AcceptanceFirmware()
	le32 := func(o int) uint32 { return binary.LittleEndian.Uint32(f.Image[o:]) }
	le16 := func(o int) uint16 { return binary.LittleEndian.Uint16(f.Image[o:]) }
	if len(f.Image)%16 != 0 {
		t.Errorf("image is %d bytes, want a multiple of the 16-byte program granule", len(f.Image))
	}
	if le32(0) != 0x20009000 || le32(4) != f.ResetHandler|1 {
		t.Errorf("vector table: sp %#x reset %#x", le32(0), le32(4))
	}
	for o := 8; o < 0x40; o += 4 {
		if v := le32(o); v&1 == 0 || v&^1 != 0x80 {
			t.Errorf("vector at %#x = %#x, want the hang handler", o, v)
		}
	}
	// ldr r1,[pc,#imm] at 0x40 and ldr r3,[pc,#imm] at 0x4C resolve to the literal pool.
	lit := func(at int, ins uint16) int { return (at+4)&^3 + int(ins&0xFF)*4 }
	if got := lit(0x40, le16(0x40)); le32(got) != f.CounterAddr {
		t.Errorf("counter literal at %#x = %#x", got, le32(got))
	}
	if got := lit(0x4C, le16(0x4C)); le32(got) != f.BreakpointFunc|1 {
		t.Errorf("bp_func literal at %#x = %#x", got, le32(got))
	}
	// b loop at 0x50 must land on 0x46.
	imm := int(le16(0x50) & 0x7FF)
	if imm >= 0x400 {
		imm -= 0x800
	}
	if target := 0x50 + 4 + 2*imm; target != 0x46 {
		t.Errorf("loop branch lands at %#x, want 0x46", target)
	}
	if got := string(f.Image[f.MarkerOffset : f.MarkerOffset+len(f.Marker)]); got != f.Marker {
		t.Errorf("marker %q", got)
	}
	if le16(int(f.BreakpointFunc)+2) != 0x4770 {
		t.Error("bp_func does not return")
	}
}
