package cc26xx

import "testing"

func TestDecodeVariants(t *testing.T) {
	const pa, cc13 = 1 << 25, 1 << 23
	for _, c := range []struct {
		ice, user uint32
		name      string
	}{
		{0x3BB4102F, 0x3202F000, "CC2652P"}, // as read from a SONOFF ZBDongle-P
		{0x3BB4102F, 0x3002F000, "CC2652R"},
		{0x0BB7702F, 0x0002F000 | pa, "CC2652P7"},
		{0x0BB7702F, 0x0002F000 | pa | cc13, "CC1352P7"},
		{0x0BB4102F, 0x0002F000 | cc13, "CC1352R"},
	} {
		ch := Decode(c.ice, c.user)
		if ch.Name != c.name || !ch.Agama() || !ch.Writable || ch.SectorBytes != 8192 {
			t.Errorf("0x%08X/0x%08X: %+v, want %s", c.ice, c.user, ch, c.name)
		}
	}
	if ch := Decode(0x0B99A02F, 0x0002F000); ch.Agama() || ch.SectorBytes != 4096 {
		t.Errorf("CC26x0: %+v", ch)
	}
}

func TestDebugLocked(t *testing.T) {
	cfg := make([]byte, CCFGLen)
	for i := range cfg {
		cfg[i] = 0xFF
	}
	if !DebugLocked(cfg) {
		t.Fatal("a blank CCFG leaves CPU_DAP_ENABLE at 0xFF")
	}
	copy(cfg[SecurityOffset:], DefaultSecurity)
	if DebugLocked(cfg) || !BackdoorOf(cfg).BootloaderEnabled || BackdoorOf(cfg).BackdoorEnabled {
		t.Fatalf("default security %x", cfg)
	}
}
