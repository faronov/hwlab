package esp_test

import (
	"errors"
	"testing"

	"github.com/faronov/hwlab/plugins/espusbjtag/esp"
)

func TestDetectC5(t *testing.T) {
	if c, err := esp.Detect(0x00017c25, 0x5FD1406F); err != nil || c != &esp.ChipC5 {
		t.Fatalf("C5: %v %v", c, err)
	}
	for _, bad := range [][2]uint32{
		{0x00017c25, esp.ChipMagicC6}, // C5 IDCODE, C6 ROM
		{0x0000dc25, 0x5FD1406F},      // C6 IDCODE, C5 ROM
		{0x00017c25, 0},               // another ROM revision
	} {
		if c, err := esp.Detect(bad[0], bad[1]); c != nil || !errors.Is(err, esp.ErrUnknownChip) {
			t.Errorf("%#x/%#x: %v %v", bad[0], bad[1], c, err)
		}
	}
}

// esptool ESP32C5ROM / ESP-IDF soc/esp32c5: chip id 23, the C6 watchdog
// layout plus the super watchdog at +0x1C/+0x20, eFuse at 0x600B4800,
// bootloader at 0x2000, two TAPs with the HP core at chain position 1.
func TestChipC5Map(t *testing.T) {
	c := esp.ChipC5
	if esp.ChipIDESP32C5 != 23 || c.ImageChipID != 23 || c.IDCode != 0x00017c25 || c.BootloaderOffset != 0x2000 ||
		c.JTAGTAPs != 2 || c.JTAGHPTAP != 1 || c.ROMSPIAttach != 0x400001f0 {
		t.Fatalf("%+v", c)
	}
	w := c.WDT
	if w.TIMG0 != 0x60008000 || w.TIMG1 != 0x60009000 || w.LP != 0x600B1C00 || w.LPFeed != 0x14 || w.LPProtect != 0x18 ||
		w.SWDConf != 0x1C || w.SWDProtect != 0x20 {
		t.Fatalf("%+v", w)
	}
	if c.SPI1 != esp.SPI1Base || c.EfuseRepeat0 != 0x600B4830 {
		t.Fatalf("%+v", c)
	}
	cases := map[uint32]esp.Class{
		0x40000000: esp.ClassROM, 0x4003FFFF: esp.ClassROM, 0x40040000: esp.ClassUnmapped,
		0x40800000: esp.ClassSRAM, 0x4085FFFF: esp.ClassSRAM, 0x40860000: esp.ClassUnmapped,
		0x50003FFF: esp.ClassSRAM, 0x50004000: esp.ClassUnmapped, 0x43FFFFFF: esp.ClassFlashMap,
	}
	for a, want := range cases {
		if got := c.Classify(a); got != want {
			t.Errorf("%#x: %s, want %s", a, got, want)
		}
	}
	if c.WritableSRAM(0x4085FFF0, 32) || !c.WritableSRAM(0x4085FFF0, 16) {
		t.Fatal("C5 HP SRAM ends at 0x4085FFFF")
	}
}

// The C5 moved SPI_BOOT_CRYPT_CNT (BLK0 bit 80 = RD_REPEAT_DATA1 bit 16) and
// SECURE_BOOT_EN (bit 121 = RD_REPEAT_DATA2 bit 25); the C6 positions must
// not count there.
func TestReadSecurityC5(t *testing.T) {
	b := &wordBus{m: map[uint32]uint32{0x600B4834: 1 << 20, 0x600B4838: 1 << 20}} // C6 CRYPT_CNT top bit, C6 SECURE_BOOT_EN
	s, err := esp.ReadSecurity(ctx, b, &esp.ChipC5)
	if err != nil || s.FlashEncrypted() || s.SecureBootEnabled || s.CheckFlashWritable() != nil {
		t.Fatalf("C6 positions read as set on the C5: %+v %v", s, err)
	}
	b.m[0x600B4834] = 1 << 16
	if s, _ = esp.ReadSecurity(ctx, b, &esp.ChipC5); s.FlashEncryptionCount != 1 || s.CheckFlashWritable() == nil {
		t.Fatalf("%+v", s)
	}
	b.m[0x600B4834], b.m[0x600B4838] = 0, 1<<25
	if s, _ = esp.ReadSecurity(ctx, b, &esp.ChipC5); !s.SecureBootEnabled || s.CheckFlashWritable() == nil {
		t.Fatalf("%+v", s)
	}
	if len(b.writes) != 0 {
		t.Fatal("ReadSecurity wrote")
	}
}
