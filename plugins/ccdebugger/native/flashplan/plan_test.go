package flashplan

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/faronov/hwlab/plugins/ccdebugger/native/target"
)

func geom(t *testing.T) target.Geometry {
	t.Helper()
	g, err := target.Decode(0xA5, 4, 0x44, 7)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func unlocked() *target.Lock {
	raw := bytes.Repeat([]byte{0xFF}, 16)
	l, _ := target.DecodeLock(target.Geometry{Pages: 128}, raw)
	return &l
}

func img(t *testing.T, addr int, data []byte) Image {
	t.Helper()
	i, err := FromBinary(data, addr)
	if err != nil {
		t.Fatal(err)
	}
	return i
}

func TestCRC16KnownVector(t *testing.T) {
	if got := CRC16([]byte("123456789")); got != 0xAEE7 {
		t.Fatalf("CRC16 = %#04x, want 0xAEE7", got)
	}
}

// Values read back from the CC2530 hardware CRC (seed 0xFFFF in RNDH:RNDL).
func TestCRC16MatchesCC2530Hardware(t *testing.T) {
	for _, c := range []struct {
		data []byte
		want uint16
	}{
		{[]byte{0x02}, 0x7D0D}, {[]byte{0x00}, 0xFD02}, {[]byte{0x01}, 0x7D07},
		{[]byte{0x02, 0x30}, 0x0CAE}, {[]byte{0x02, 0x30, 0xA1}, 0x2DED}, {[]byte{0x02, 0x30, 0xA1, 0x00}, 0xEDEE},
	} {
		if got := CRC16(c.data); got != c.want {
			t.Errorf("CRC16(% x) = %#04x, want %#04x", c.data, got, c.want)
		}
	}
}

func TestModes(t *testing.T) {
	for in, want := range map[string]Mode{"": PreserveDeviceConfig, "preserve-mac": PreserveMAC, "full-chip-image": FullChipImage} {
		if m, err := ParseMode(in); err != nil || m != want {
			t.Errorf("ParseMode(%q) = %q, %v", in, m, err)
		}
	}
	if _, err := ParseMode("whatever"); err == nil {
		t.Error("bad mode accepted")
	}
}

func TestBuildBasicAndFFSkip(t *testing.T) {
	data := make([]byte, 4096)
	for i := range data {
		data[i] = 0xFF
	}
	copy(data[:16], bytes.Repeat([]byte{1}, 16))
	copy(data[3000:], bytes.Repeat([]byte{2}, 5)) // unaligned tail
	p, err := Build(geom(t), unlocked(), img(t, 0x800, data), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Mode != PreserveDeviceConfig || len(p.ErasePages) != 2 || p.ErasePages[0] != 1 || p.ErasePages[1] != 2 {
		t.Fatalf("plan = %+v", p)
	}
	if len(p.Blocks) != 2 || p.SkippedBlocks == 0 {
		t.Fatalf("blocks = %d skipped = %d", len(p.Blocks), p.SkippedBlocks)
	}
	for _, b := range p.Blocks {
		if b.Addr%4 != 0 || len(b.Data)%4 != 0 || len(b.Data) > BlockSize || b.Addr/1024 != (b.Addr+len(b.Data)-1)/1024 {
			t.Errorf("bad block %#x+%d", b.Addr, len(b.Data))
		}
	}
	last := p.Blocks[1]
	if end := last.Addr + len(last.Data); end < 0x13C0 || end > 0x1400 {
		t.Errorf("tail block ends at %#x", end)
	}
	if off := 0x800 + 3000 - last.Addr; !bytes.Equal(last.Data[off:off+5], bytes.Repeat([]byte{2}, 5)) {
		t.Error("tail data misplaced")
	}
}

func TestBuildBounds(t *testing.T) {
	g := geom(t)
	for name, i := range map[string]Image{
		"past flash": img(t, g.FlashSize-2, []byte{1, 2, 3, 4}),
		"empty":      {},
	} {
		if _, err := Build(g, unlocked(), i, Options{}); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := Build(g, nil, img(t, 0, []byte{1}), Options{}); err == nil {
		t.Error("unknown lock state accepted")
	}
	if _, err := Build(g, unlocked(), img(t, 0, []byte{1}), Options{ChipErase: true}); err == nil {
		t.Error("chip erase allowed outside full-chip-image")
	}
	other, _ := target.Decode(0xB5, 0, 0x44, 7)
	if _, err := Build(other, unlocked(), img(t, 0, []byte{1}), Options{}); err == nil {
		t.Error("non-programmable chip accepted")
	}
}

func TestBuildPreservationPolicy(t *testing.T) {
	g := geom(t)
	lp := g.LockPageAddr()
	if _, err := Build(g, unlocked(), img(t, lp, []byte{1, 2, 3, 4}), Options{}); err == nil {
		t.Error("default mode wrote the lock page")
	}
	p, err := Build(g, unlocked(), img(t, lp, []byte{1, 2, 3, 4}), Options{Mode: PreserveMAC})
	if err != nil || !p.TouchesConfig || len(p.Preserve) != 1 || p.Preserve[0] != (Range{g.SecondaryIEEEAddr(), 8}) {
		t.Fatalf("preserve-mac: %+v %v", p, err)
	}
	if _, err := Build(g, unlocked(), img(t, g.SecondaryIEEEAddr(), make([]byte, 8)), Options{Mode: PreserveMAC}); err == nil {
		t.Error("preserve-mac overwrote the IEEE address")
	}
	if _, err := Build(g, unlocked(), img(t, g.SecondaryIEEEAddr(), make([]byte, 8)), Options{Mode: FullChipImage}); err != nil {
		t.Errorf("full-chip-image refused: %v", err)
	}
	p, err = Build(g, nil, img(t, 0, []byte{1, 2, 3, 4}), Options{Mode: FullChipImage, ChipErase: true})
	if err != nil || !p.ChipErase || len(p.ErasePages) != 0 {
		t.Fatalf("chip erase plan: %+v %v", p, err)
	}
}

func TestBuildLockedPage(t *testing.T) {
	raw := bytes.Repeat([]byte{0xFF}, 16)
	raw[0] = 0xFE
	g := geom(t)
	l, _ := target.DecodeLock(g, raw)
	if _, err := Build(g, &l, img(t, 0, []byte{1}), Options{}); err == nil {
		t.Error("write to a locked page accepted")
	}
	if _, err := Build(g, &l, img(t, 2048, []byte{1}), Options{}); err != nil {
		t.Errorf("unlocked page refused: %v", err)
	}
}

func TestNormalize(t *testing.T) {
	if _, err := Normalize([]Section{{0, []byte{1, 2}}, {1, []byte{3}}}); err == nil {
		t.Error("overlap accepted")
	}
	i, err := Normalize([]Section{{4, []byte{3}}, {0, []byte{1, 2}}, {2, []byte{9, 9}}})
	if err != nil || len(i.Sections) != 1 || i.Highest() != 5 || i.Size() != 5 {
		t.Fatalf("normalize: %+v %v", i, err)
	}
	if _, err := FromBinary(make([]byte, MaxImageBytes+1), 0); err == nil {
		t.Error("oversize image accepted")
	}
	if _, err := FromBinary([]byte{1}, -1); err == nil {
		t.Error("negative base accepted")
	}
}

func rec(typ byte, addr int, data ...byte) string {
	b := append([]byte{byte(len(data)), byte(addr >> 8), byte(addr), typ}, data...)
	var sum byte
	for _, v := range b {
		sum += v
	}
	return fmt.Sprintf(":%X%02X\n", b, byte(-sum))
}

func TestIntelHex(t *testing.T) {
	good := rec(4, 0, 0, 1) + rec(0, 0x0010, 1, 2, 3) + rec(1, 0)
	i, err := ParseIntelHex(strings.NewReader(good))
	if err != nil || len(i.Sections) != 1 || i.Sections[0].Addr != 0x10010 || !bytes.Equal(i.Sections[0].Data, []byte{1, 2, 3}) {
		t.Fatalf("hex: %+v %v", i, err)
	}
	bad := map[string]string{
		"no eof":       rec(0, 0, 1),
		"checksum":     strings.Replace(rec(0, 0, 1), "0100", "0101", 1) + rec(1, 0),
		"after eof":    rec(1, 0) + rec(0, 0, 1),
		"no colon":     "00000001FF\n",
		"length":       ":0300000001FC\n" + rec(1, 0),
		"unknown type": rec(9, 0, 1) + rec(1, 0),
		"garbage":      ":zz\n",
	}
	for name, h := range bad {
		if _, err := ParseIntelHex(strings.NewReader(h)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
