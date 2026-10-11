package target

import (
	"errors"
	"strings"
	"testing"

	"github.com/faronov/hwlab/plugins/telink/sws"
)

func flashWith(blankEUI bool) ReadFlash {
	return func(addr uint32, n int) ([]byte, error) {
		b := make([]byte, n)
		if blankEUI {
			for i := range b {
				b[i] = 0xff
			}
		}
		return b, nil
	}
}

func TestFlashSize(t *testing.T) {
	for jedec, want := range map[uint32]uint32{0xc86013: 0x80000, 0xef4013: 0x80000, 0xc84015: 0x200000, 0x81003c: 0, 0xc86003: 0} {
		if got := FlashSize(jedec); got != want {
			t.Errorf("0x%06x: %#x want %#x", jedec, got, want)
		}
	}
}

func TestIdentify(t *testing.T) {
	id := Identify(0x5562, 2, 0xc86013)
	if id.Family != "TLSR825x" || id.Variant != "undetermined" || id.FlashBytes != 0x80000 {
		t.Fatalf("%+v", id)
	}
	if id := Identify(0x1234, 0, 0); id.Family != "unknown" || id.FlashBytes != 0 {
		t.Fatalf("%+v", id)
	}
}

func TestProtectionFailsClosed(t *testing.T) {
	f := Lookup(0x5562)
	cases := []struct {
		name   string
		layout string
		size   uint32
		read   ReadFlash
		want   string
	}{
		{"auto", AutoLayout, 0x80000, flashWith(false), "not verified"},
		{"empty means auto", "", 0x80000, flashWith(false), "not verified"},
		{"unknown layout", "nope", 0x80000, flashWith(false), "unknown layout"},
		{"unknown size", "tb04-512k", 0, flashWith(false), "size unknown"},
		{"wrong size", "tb04-512k", 0x100000, flashWith(false), "1024 KiB"},
		{"blank factory", "tb04-512k", 0x80000, flashWith(true), "reads blank"},
		{"no reader", "tb04-512k", 0x80000, nil, "cannot be read"},
		{"read error", "tb04-512k", 0x80000, func(uint32, int) ([]byte, error) { return nil, errors.New("boom") }, "boom"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := f.Protection(c.layout, c.size, c.read)
			if p.Verified || !strings.Contains(p.Reason, c.want) {
				t.Fatalf("%+v", p)
			}
			if err := p.Layout.CheckModify(0x1000, 16, sws.Policy{Journals: true}); err == nil {
				t.Fatal("unverified layout must not allow any modification")
			}
		})
	}
}

func TestProtectionVerified(t *testing.T) {
	p := Lookup(0x5562).Protection("tb04-512k", 0x80000, flashWith(false))
	if !p.Verified || p.Layout.Name != "tb04-512k" {
		t.Fatalf("%+v", p)
	}
}

func TestNilFamilyIsUnverified(t *testing.T) {
	var f *Family
	if p := f.Protection("tb04-512k", 0x80000, flashWith(false)); p.Verified {
		t.Fatal("unknown chip must not have a verified map")
	}
}

func TestLayoutNames(t *testing.T) {
	if !ValidLayout("auto") || !ValidLayout("tb04-512k") || ValidLayout("x") {
		t.Fatal("ValidLayout")
	}
}
