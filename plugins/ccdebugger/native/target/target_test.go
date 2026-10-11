package target

import (
	"errors"
	"testing"
)

func TestDecodeCC2530F256(t *testing.T) {
	g, err := Decode(0xA5, 0x04, 0x44, 0x07)
	if err != nil {
		t.Fatal(err)
	}
	if g.Chip != "CC2530" || g.FlashSize != 256<<10 || g.RAMSize != 8<<10 || g.PageSize != 2048 || g.Pages != 128 ||
		g.Banks != 8 || g.USB || !g.Programmable || g.PrimaryIEEE != 0x780C {
		t.Fatalf("geometry = %+v", g)
	}
	if g.LockAddr() != 0x3FFF0 || g.SecondaryIEEEAddr() != 0x3FFE8 || g.LockPage() != 127 || g.LockPageAddr() != 0x3F800 {
		t.Fatalf("layout: lock %#x ieee %#x page %d", g.LockAddr(), g.SecondaryIEEEAddr(), g.LockPage())
	}
}

func TestDecodeSizes(t *testing.T) {
	for info0, want := range map[uint8]int{0x14: 32 << 10, 0x24: 64 << 10, 0x34: 128 << 10, 0x44: 256 << 10} {
		g, err := Decode(0xA5, 0, info0, 0)
		if err != nil || g.FlashSize != want || g.Pages != want/g.PageSize {
			t.Errorf("info0=%#x: %+v, %v", info0, g, err)
		}
	}
	if g, _ := Decode(0xA5, 0, 0x4C, 0); !g.USB {
		t.Error("USB bit ignored")
	}
}

func TestDecodeRejects(t *testing.T) {
	if _, err := Decode(0x00, 0, 0x44, 0); !errors.Is(err, ErrUnknownChip) {
		t.Errorf("unknown chip: %v", err)
	}
	for _, i0 := range []uint8{0x04, 0x54, 0x40, 0xC4, 0x00} {
		if _, err := Decode(0xA5, 0, i0, 0); !errors.Is(err, ErrMalformed) {
			t.Errorf("info0=%#x: %v", i0, err)
		}
	}
}

func TestOtherFamiliesDecodeButAreNotProgrammable(t *testing.T) {
	for _, c := range Chips() {
		g, err := Decode(c.ChipID, 1, 0x44, 0x07)
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		if g.Programmable != (c.Name == "CC2530") {
			t.Errorf("%s programmable=%v", c.Name, g.Programmable)
		}
	}
	if g, _ := Decode(0x95, 0, 0x24, 0); g.PageSize != 1024 {
		t.Errorf("CC2533 page size %d", g.PageSize)
	}
}

func TestAddressing(t *testing.T) {
	g, _ := Decode(0xA5, 4, 0x44, 7)
	if p, err := g.PageOf(0x3FFFF); err != nil || p != 127 {
		t.Fatalf("PageOf = %d, %v", p, err)
	}
	if _, err := g.PageOf(0x40000); err == nil {
		t.Fatal("past end accepted")
	}
	if f, l, err := g.PageRange(0x7FF, 2); err != nil || f != 0 || l != 1 {
		t.Fatalf("PageRange = %d %d %v", f, l, err)
	}
	if _, _, err := g.PageRange(0, 0); err == nil {
		t.Fatal("empty range accepted")
	}
	if w, err := g.FlashWordAddr(0x3FFFC); err != nil || w != 0xFFFF {
		t.Fatalf("word addr = %#x, %v", w, err)
	}
	for _, a := range []int{1, 0x40000, -4} {
		if _, err := g.FlashWordAddr(a); err == nil {
			t.Errorf("FlashWordAddr(%d) accepted", a)
		}
	}
	for addr, want := range map[int][2]int{0: {0, 0}, 0x7FFF: {0, 0x7FFF}, 0x8000: {1, 0}, 0x3FFFF: {7, 0x7FFF}} {
		if b, o := Bank(addr); b != want[0] || o != want[1] {
			t.Errorf("Bank(%#x) = %d,%#x", addr, b, o)
		}
	}
}

func TestDecodeLock(t *testing.T) {
	g, _ := Decode(0xA5, 4, 0x44, 7)
	raw := make([]byte, 16)
	for i := range raw {
		raw[i] = 0xFF
	}
	l, err := DecodeLock(g, raw)
	if err != nil || l.DebugLocked || len(l.LockedPages) != 0 {
		t.Fatalf("unlocked: %+v %v", l, err)
	}
	raw[0], raw[15] = 0xFC, 0x7F
	l, _ = DecodeLock(g, raw)
	if !l.DebugLocked || !l.PageLocked(0) || !l.PageLocked(1) || l.PageLocked(2) || !l.LockPageLocked {
		t.Fatalf("locked: %+v", l)
	}
	if _, err := DecodeLock(g, raw[:8]); err == nil {
		t.Fatal("short lock accepted")
	}
	if got := IEEE([]byte{1, 2, 3, 4, 5, 6, 7, 8}); got != "0807060504030201" {
		t.Fatalf("IEEE = %s", got)
	}
	if IEEE([]byte{1}) != "" {
		t.Fatal("short IEEE formatted")
	}
}
