package esp_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/faronov/hwlab/internal/riscv"
	"github.com/faronov/hwlab/plugins/espusbjtag/esp"
)

// wordBus is a sparse word-addressed esp.Bus: reads of words never written
// return zero, every write is recorded.
type wordBus struct {
	m      map[uint32]uint32
	writes []uint32
	log    [][2]uint32 // (address, value) of every write, in order
}

func (b *wordBus) ReadMem(_ context.Context, addr uint32, n int) ([]byte, error) {
	out := make([]byte, n)
	for i := range out {
		a := addr + uint32(i)
		out[i] = byte(b.m[a&^3] >> (8 * (a & 3)))
	}
	return out, nil
}

func (b *wordBus) BusBatch(_ context.Context, ops []riscv.BusOp) ([][]uint32, error) {
	res := make([][]uint32, len(ops))
	for i, op := range ops {
		if op.Addr%4 != 0 {
			return nil, fmt.Errorf("unaligned %#x", op.Addr)
		}
		switch op.Kind {
		case riscv.BusRead:
			for w := 0; w < op.Words; w++ {
				res[i] = append(res[i], b.m[op.Addr+uint32(4*w)])
			}
		case riscv.BusWrite:
			for w, v := range op.Vals {
				b.m[op.Addr+uint32(4*w)] = v
				b.writes = append(b.writes, op.Addr+uint32(4*w))
				b.log = append(b.log, [2]uint32{op.Addr + uint32(4*w), v})
			}
		}
	}
	return res, nil
}

func TestDetectS3(t *testing.T) {
	if c, err := esp.Detect(0x120034e5, 9); err != nil || c != &esp.ChipS3 {
		t.Fatalf("S3: %v %v", c, err)
	}
	for _, bad := range [][2]uint32{{0x120034e5, 0x2CE0806F}, {0x120034e5, 0}, {0x0000dc25, 9}, {0x00010c25, 9}} {
		if c, err := esp.Detect(bad[0], bad[1]); c != nil || !errors.Is(err, esp.ErrUnknownChip) {
			t.Errorf("%#x/%#x: %v %v", bad[0], bad[1], c, err)
		}
	}
	if esp.ChipIDESP32S3 != 9 || esp.ChipS3.ImageChipID != 9 {
		t.Fatal("S3 image chip id")
	}
}

func TestChipS3Map(t *testing.T) {
	c := &esp.ChipS3
	cases := map[uint32]esp.Class{
		0x40000000: esp.ClassROM, 0x4005FFFF: esp.ClassROM, 0x40060000: esp.ClassUnmapped,
		0x3FF00000: esp.ClassROM, 0x3FF1FFFF: esp.ClassROM, 0x3FF20000: esp.ClassUnmapped,
		0x40370000: esp.ClassSRAM, 0x403DFFFF: esp.ClassSRAM, 0x403E0000: esp.ClassUnmapped,
		0x3FC87FFC: esp.ClassUnmapped, 0x3FC88000: esp.ClassSRAM, 0x3FCFFFFF: esp.ClassSRAM, 0x3FD00000: esp.ClassUnmapped,
		0x50000000: esp.ClassSRAM, 0x50002000: esp.ClassUnmapped,
		0x600FE000: esp.ClassSRAM, 0x600FFFFF: esp.ClassSRAM, 0x600FDFFC: esp.ClassUnmapped,
		0x60000000: esp.ClassPeripheral, 0x600D0FFF: esp.ClassPeripheral, 0x600D1000: esp.ClassUnmapped,
		0x3C000000: esp.ClassFlashMap, 0x42000000: esp.ClassFlashMap, 0x43FFFFFF: esp.ClassFlashMap,
		0x40800000: esp.ClassUnmapped, 0x10: esp.ClassUnmapped,
	}
	for a, want := range cases {
		if got := c.Classify(a); got != want {
			t.Errorf("%#x: %s want %s", a, got, want)
		}
	}
	ok := [][2]uint32{{0x3FC90000, 8}, {0x3FCEFFF0, 16}, {0x3FCF0000, 0x10000}, {0x40370000, 4}, {0x600FE000, 0x2000}, {0x50001FFC, 4}}
	bad := [][2]uint32{{0x3FCEFFF0, 32}, {0x40377FFC, 8}, {0x4005FFFC, 4}, {0x60000000, 4}, {0x3FCFFFFC, 8}, {0x3C000000, 4}, {0x3FC90000, 0}}
	for _, r := range ok {
		if !c.WritableSRAM(r[0], int(r[1])) {
			t.Errorf("%#x+%d should be writable SRAM", r[0], r[1])
		}
	}
	for _, r := range bad {
		if c.WritableSRAM(r[0], int(r[1])) {
			t.Errorf("%#x+%d should not be writable SRAM", r[0], r[1])
		}
	}
	for a, want := range map[uint32]bool{0x40041a7c: true, 0x40380000: true, 0x3FC90000: false, 0x3FF00000: false, 0x42000000: true, 0x3C000000: false, 0x600FE000: true, 0x60000000: false, 0x10: false} {
		if c.Executable(a) != want {
			t.Errorf("Executable(%#x) != %v", a, want)
		}
	}
	if !esp.ChipC6.Executable(0x40800000) || esp.ChipC6.Executable(0x60000000) {
		t.Error("C6 Executable")
	}
	if c.SPI1 != 0x60002000 || c.EfuseRepeat0 != 0x60007030 || c.MMUTable != 0x600C5000 || c.WDT.SWDKey != 0x8F1D312A {
		t.Fatalf("S3 SPI1/eFuse/MMU/SWD key %#x %#x %#x %#x", c.SPI1, c.EfuseRepeat0, c.MMUTable, c.WDT.SWDKey)
	}
}

// The S3 RTC_CNTL watchdog map, verified against a real ESP32-S3 in download
// mode: SWD_CONF +0xB4 with AUTO_FEED at bit 31, SWD_WPROTECT +0xB8.
func TestQuiesceRestoreS3(t *testing.T) {
	b := &wordBus{m: map[uint32]uint32{
		0x6001F048: 1<<31 | 0x4c000, 0x60020048: 0x4c000, 0x60008098: 1<<31 | 0x13214, 0x600080B4: 0x04b00000,
	}}
	q, err := esp.Quiesce(ctx, b, &esp.ChipS3)
	if err != nil {
		t.Fatal(err)
	}
	if q.TIMG0 != 1<<31|0x4c000 || q.LP != 1<<31|0x13214 || q.SWD != 0x04b00000 {
		t.Fatalf("%+v", q)
	}
	if b.m[0x6001F048]&(1<<31) != 0 || b.m[0x60008098]&(1<<31) != 0 {
		t.Fatal("watchdog still enabled")
	}
	if b.m[0x600080B4] != 0x04b00000|1<<31 {
		t.Fatalf("SWD_CONF %#x: S3 auto-feed is bit 31", b.m[0x600080B4])
	}
	if b.m[0x6001F064] != 0 || b.m[0x60020064] != 0 || b.m[0x600080B0] != 0 || b.m[0x600080B8] != 0 {
		t.Fatal("write protection left open")
	}
	// SWD_CONF is written only right after SWD_WPROTECT got the S3 SWD key.
	for i, w := range b.log {
		if w[0] == 0x600080B4 && (i == 0 || b.log[i-1] != [2]uint32{0x600080B8, 0x8F1D312A}) {
			t.Fatalf("SWD_CONF written without the 0x8F1D312A unlock: %#v", b.log)
		}
		if w[0] == 0x60008098 && (i == 0 || b.log[i-1] != [2]uint32{0x600080B0, 0x50D83AA1}) {
			t.Fatalf("RTC WDT written without the 0x50D83AA1 unlock: %#v", b.log)
		}
	}
	if err := q.Restore(ctx, b); err != nil {
		t.Fatal(err)
	}
	if b.m[0x6001F048] != 1<<31|0x4c000 || b.m[0x60008098] != 1<<31|0x13214 || b.m[0x600080B4] != 0x04b00000 || b.m[0x600080AC] != 1<<31 {
		t.Fatalf("restore %#v", b.m)
	}
}

// The S3 security fields: the C6 positions except DIS_USB_JTAG (BLK0 bit
// 118 = RD_REPEAT_DATA2 bit 22), at EFUSE_RD_REPEAT_DATA0 0x60007030.
func TestReadSecurityS3(t *testing.T) {
	b := &wordBus{m: map[uint32]uint32{}}
	s, err := esp.ReadSecurity(ctx, b, &esp.ChipS3)
	if err != nil || s.CheckFlashWritable() != nil || s.USBJTAGDisabled || s.SecureBootEnabled || s.FlashEncrypted() {
		t.Fatalf("blank S3 eFuses: %+v %v", s, err)
	}
	b.m[0x60007030] = 1 << 9 // C6 DIS_USB_JTAG position: not the S3 one
	b.m[0x60007038] = 1<<22 | 1<<20
	b.m[0x60007034] = 1 << 18
	b.m[0x6000703C] = 1 | 1<<5
	s, err = esp.ReadSecurity(ctx, b, &esp.ChipS3)
	if err != nil || !s.USBJTAGDisabled || !s.SecureBootEnabled || s.FlashEncryptionCount != 1 || !s.FlashEncrypted() ||
		!s.DownloadModeDisabled || !s.SecurityDownload || s.CheckFlashWritable() == nil {
		t.Fatalf("%+v %v", s, err)
	}
	b.m[0x60007038] = 0
	if s, _ = esp.ReadSecurity(ctx, b, &esp.ChipS3); s.USBJTAGDisabled {
		t.Fatal("word 0 bit 9 must not count as DIS_USB_JTAG on the S3")
	}
	if len(b.writes) != 0 {
		t.Fatal("ReadSecurity wrote")
	}
	ev, err := esp.ReadEvidence(ctx, &wordBus{m: map[uint32]uint32{esp.ChipMagicAddr: 9}})
	if err != nil || ev.ROMMagic != 9 || ev.MagicMatchesC6 {
		t.Fatalf("%+v %v", ev, err)
	}
}
