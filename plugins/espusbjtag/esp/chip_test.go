package esp_test

import (
	"errors"
	"testing"
	"time"

	"github.com/faronov/hwlab/internal/jtag"
	"github.com/faronov/hwlab/internal/riscv"
	"github.com/faronov/hwlab/internal/riscv/rvsim"
	"github.com/faronov/hwlab/plugins/espusbjtag/esp"
)

func TestDetect(t *testing.T) {
	if c, err := esp.Detect(0x0000dc25, 0x2CE0806F); err != nil || c != &esp.ChipC6 {
		t.Fatalf("C6: %v %v", c, err)
	}
	if c, err := esp.Detect(0x00010c25, 0x97E30068); err != nil || c != &esp.ChipH2 {
		t.Fatalf("H2: %v %v", c, err)
	}
	for _, bad := range [][2]uint32{
		{0x0000dc25, 0x97E30068}, // C6 IDCODE, H2 ROM
		{0x00010c25, 0x2CE0806F}, // H2 IDCODE, C6 ROM
		{0x00010c25, 0},          // H2 IDCODE, other ROM revision
		{0x0000dc25, 0xFFFFFFFF},
		{0x12345025, 0x2CE0806F},
		{0x12345025, 0x12345678},
		{0, 0},
	} {
		if c, err := esp.Detect(bad[0], bad[1]); c != nil || !errors.Is(err, esp.ErrUnknownChip) {
			t.Errorf("%#x/%#x: %v %v", bad[0], bad[1], c, err)
		}
	}
}

// The C6 profile is the pre-profile constants, unchanged.
func TestChipC6MatchesLegacy(t *testing.T) {
	c := esp.ChipC6
	if esp.ChipIDESP32C6 != 0x000D || c.ImageChipID != 0x000D || c.IDCode != 0x0000dc25 || c.ROMWord != esp.ChipMagicC6 {
		t.Fatalf("%+v", c)
	}
	w := c.WDT
	if w.TIMG0 != 0x60008000 || w.TIMG1 != 0x60009000 || w.LP != 0x600B1C00 || w.LPConfig0 != 0 || w.LPConfig1 != 4 ||
		w.LPFeed != 0x14 || w.LPProtect != 0x18 || w.SWDConf != 0 || w.SWDProtect != 0 {
		t.Fatalf("%+v", w)
	}
	if esp.SPI1Base != 0x60003000 || c.SPI1 != 0x60003000 || c.EfuseRepeat0 != 0x600B0830 {
		t.Fatalf("%+v", c)
	}
	for _, a := range []uint32{0x10, 0x40000000, 0x4004FFFF, 0x40800000, 0x4087FFFF, 0x40880000, 0x50000000, 0x50003FFF, 0x50004000, 0x42000000, 0x60008000} {
		if c.Classify(a) != esp.Classify(a) {
			t.Errorf("%#x: %s vs %s", a, c.Classify(a), esp.Classify(a))
		}
	}
	for _, r := range [][2]uint32{{0x40800000, 16}, {0x4087FFF0, 32}, {0x50003FF0, 0x10}, {0x50003FF0, 0x20}, {0x40000000, 4}} {
		if c.WritableSRAM(r[0], int(r[1])) != esp.WritableSRAM(r[0], int(r[1])) {
			t.Errorf("%#x+%d", r[0], r[1])
		}
	}
}

func TestChipH2Map(t *testing.T) {
	c := esp.ChipH2
	if esp.ChipIDESP32H2 != 0x0010 || c.ImageChipID != 0x0010 || c.IDCode != 0x00010c25 {
		t.Fatalf("%+v", c)
	}
	w := c.WDT
	// esp32h2 PAC: TIMG0 0x6000_9000, TIMG1 0x6000_A000 (LEDC is at 0x6000_8000),
	// LP_WDT 0x600B_1C00 with CONFIG5 at +0x14, FEED +0x18, WPROTECT +0x1C,
	// SWD_CONF +0x20, SWD_WPROTECT +0x24.
	if w.TIMG0 != 0x60009000 || w.TIMG1 != 0x6000A000 || w.LP != 0x600B1C00 || w.LPConfig0 != 0 || w.LPConfig1 != 4 ||
		w.LPFeed != 0x18 || w.LPProtect != 0x1C || w.SWDConf != 0x20 || w.SWDProtect != 0x24 {
		t.Fatalf("%+v", w)
	}
	if c.SPI1 != 0x60003000 || c.EfuseRepeat0 != 0x600B0830 {
		t.Fatalf("%+v", c)
	}
	cases := map[uint32]esp.Class{
		0x40000000: esp.ClassROM, 0x4001FFFF: esp.ClassROM, 0x40020000: esp.ClassUnmapped,
		0x40800000: esp.ClassSRAM, 0x4084FFFF: esp.ClassSRAM, 0x40850000: esp.ClassUnmapped,
		0x50000000: esp.ClassSRAM, 0x50000FFF: esp.ClassSRAM, 0x50001000: esp.ClassUnmapped,
		0x42000000: esp.ClassFlashMap, 0x60009000: esp.ClassPeripheral,
	}
	for a, want := range cases {
		if got := c.Classify(a); got != want {
			t.Errorf("%#x: %s want %s", a, got, want)
		}
	}
	if !c.WritableSRAM(0x40800000, 16) || !c.WritableSRAM(0x4084FFF0, 16) || c.WritableSRAM(0x4084FFF0, 32) ||
		c.WritableSRAM(0x40860000, 4) || c.WritableSRAM(0x50000FF0, 0x20) || c.WritableSRAM(0x40000000, 4) {
		t.Fatal("H2 WritableSRAM boundaries")
	}
}

// h2Rig is an ESP32-H2-shaped bus: the watchdogs where the H2 has them and
// NOTHING at 0x60008000 (the C6 TIMG0, LEDC on the H2), so a C6 address
// write errors on the bus instead of passing silently.
func h2Rig(t *testing.T) (*riscv.DM, map[uint32]uint32, map[uint32]uint32, map[uint32]uint32) {
	t.Helper()
	tg0d, tg0 := regfile(0x60009000, 0x100)
	tg1d, tg1 := regfile(0x6000A000, 0x100)
	lpd, lp := regfile(0x600B1C00, 0x100)
	efd, _ := regfile(0x600B0800, 0x100)
	tgt := rvsim.New(rvsim.Config{
		Harts: 1, IDCode: 0x00010c25,
		Memory:  []rvsim.Region{{Base: esp.HPSRAMBase, Data: make([]byte, 4096)}},
		Devices: []rvsim.Device{tg0d, tg1d, lpd, efd},
	})
	dtm, err := riscv.NewDTM(ctx, jtag.NewDriver(rvsim.Port{T: tgt}), 5)
	if err != nil {
		t.Fatal(err)
	}
	dm := riscv.NewDM(dtm)
	dm.Timeout = 500 * time.Millisecond
	if _, err := dm.Halt(ctx); err != nil {
		t.Fatal(err)
	}
	return dm, tg0, tg1, lp
}

func TestQuiesceRestoreH2(t *testing.T) {
	dm, tg0, tg1, lp := h2Rig(t)
	tg0[0x48] = 1<<31 | 5
	tg1[0x48] = 1<<31 | 6
	lp[0x00] = 1<<31 | 7
	lp[0x20] = 0x123 << 20 // SWD signal width, auto-feed off
	q, err := esp.Quiesce(ctx, dm, &esp.ChipH2)
	if err != nil {
		t.Fatal(err)
	}
	if q.TIMG0 != 1<<31|5 || q.TIMG1 != 1<<31|6 || q.LP != 1<<31|7 || q.SWD != 0x123<<20 {
		t.Fatalf("%+v", q)
	}
	if tg0[0x48]&(1<<31) != 0 || tg1[0x48]&(1<<31) != 0 || lp[0x00]&(1<<31) != 0 {
		t.Fatal("a watchdog is still enabled")
	}
	if lp[0x20] != 0x123<<20|1<<18 {
		t.Fatalf("SWD_CONF %#x: auto-feed not set", lp[0x20])
	}
	if tg0[0x64] != 0 || tg1[0x64] != 0 || lp[0x1C] != 0 || lp[0x24] != 0 {
		t.Fatal("write protection left open")
	}
	if lp[0x18] != 0 {
		t.Fatal("C6 WPROTECT offset (+0x18) written on the H2: that is FEED there")
	}
	if err := q.Restore(ctx, dm); err != nil {
		t.Fatal(err)
	}
	if tg0[0x48] != 1<<31|5 || tg1[0x48] != 1<<31|6 || lp[0x00] != 1<<31|7 || lp[0x20] != 0x123<<20 {
		t.Fatalf("restore: %#x %#x %#x %#x", tg0[0x48], tg1[0x48], lp[0x00], lp[0x20])
	}
	if tg0[0x60] != 1 || tg1[0x60] != 1 || lp[0x18] != 1<<31 {
		t.Fatal("restore did not feed the H2 watchdogs")
	}
	// The C6 profile on this H2-shaped bus touches 0x60008000, which is not
	// there: the bus error proves the per-chip map matters.
	if _, err := esp.Quiesce(ctx, dm, &esp.ChipC6); err == nil {
		t.Fatal("C6 map accepted on an H2 bus")
	}
}

func TestSoCResetH2(t *testing.T) {
	dm, _, _, lp := h2Rig(t)
	if err := esp.ArmSoCReset(ctx, dm, &esp.ChipH2); err != nil {
		t.Fatal(err)
	}
	if lp[0x04] != 40000 || lp[0x00] != 1<<31|3<<28|1<<13|1<<12 || lp[0x18] != 1<<31 || lp[0x1C] != 0 {
		t.Fatalf("%#v", lp)
	}
	if ok, err := esp.SoCResetArmed(ctx, dm, &esp.ChipH2); err != nil || !ok {
		t.Fatal(ok, err)
	}
}

func TestNilChipRefused(t *testing.T) {
	dm, tg0, _, lp := h2Rig(t)
	if _, err := esp.Quiesce(ctx, dm, nil); err == nil {
		t.Fatal("Quiesce without a chip")
	}
	if err := esp.ArmSoCReset(ctx, dm, nil); err == nil {
		t.Fatal("ArmSoCReset without a chip")
	}
	if _, err := esp.SoCResetArmed(ctx, dm, nil); err == nil {
		t.Fatal("SoCResetArmed without a chip")
	}
	if _, err := esp.ReadSecurity(ctx, dm, nil); err == nil {
		t.Fatal("ReadSecurity without a chip")
	}
	if len(tg0)+len(lp) != 0 {
		t.Fatal("registers written without a chip profile")
	}
	if err := (esp.Quiesced{}).Restore(ctx, dm); err == nil {
		t.Fatal("Restore without a chip")
	}
}
