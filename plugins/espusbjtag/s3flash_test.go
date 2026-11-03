package espusbjtag

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/faronov/hwlab/internal/xtensa/xtsim"
	"github.com/faronov/hwlab/plugins/espusbjtag/esp"
)

// s3IOMux is the IO_MUX block of the simulated S3, with the flash pins
// GPIO26..32 in GPIO mode (0x1b00, as the real ROM download mode leaves them)
// and a log of every write.
type s3IOMux struct {
	mu     sync.Mutex
	regs   map[uint32]uint32
	writes [][2]uint32
}

func (m *s3IOMux) device() xtsim.Device {
	return xtsim.Device{Base: 0x60009000, Size: 0x100,
		Read: func(o uint32) uint32 { m.mu.Lock(); defer m.mu.Unlock(); return m.regs[o] },
		Write: func(o, v uint32) {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.regs[o] = v
			m.writes = append(m.writes, [2]uint32{o, v})
		}}
}

type s3FlashRig struct {
	*flashFix
	core0, core1 *xtsim.Target
	iomux        *s3IOMux
}

// newS3Flash is newS3 plus the peripherals a flash call touches: SPI1 (the
// synthetic flash) at 0x60002000, eFuse, RTC_CNTL, both timer groups, IO_MUX,
// EXTMEM and the MMU table, a 256-byte data-bus flash window at 0x3C000000,
// and the bootloader/partition table/app layout of the C6 flash tests with an
// ESP32-S3 bootloader header. core1Running releases core 1 from reset.
func newS3Flash(t *testing.T, cfg map[string]any, core1Running bool) *s3FlashRig {
	t.Helper()
	rom := make([]byte, 0x2000)
	binary.LittleEndian.PutUint32(rom[0x1000:], 9)
	fl := esp.NewSimFlash(256 << 10)
	mmu := make([]byte, 0x800)
	for i := 0; i < len(mmu); i += 4 {
		binary.LittleEndian.PutUint32(mmu[i:], 0x4000)
	}
	io := &s3IOMux{regs: map[uint32]uint32{}}
	for o := uint32(0x6C); o <= 0x84; o += 4 {
		io.regs[o] = 0x1b00
	}
	mk := func(prid uint32, held bool) *xtsim.Target {
		return xtsim.New(xtsim.Config{IDCode: 0x120034e5, OCDID: 0x0b339fd2, PRID: prid, Held: held, Loop: s3Loop,
			Regions: []xtsim.Region{
				{Base: 0x40000000, Data: rom, ReadOnly: true},
				{Base: 0x3FC88000, Data: make([]byte, 0x10000)},
				{Base: 0x3C000000, Data: make([]byte, 0x100), ReadOnly: true},
				{Base: 0x60007000, Data: make([]byte, 0x100)},
				{Base: 0x60008000, Data: make([]byte, 0x100)},
				{Base: 0x6001F000, Data: make([]byte, 0x100)},
				{Base: 0x60020000, Data: make([]byte, 0x100)},
				{Base: 0x600C4000, Data: make([]byte, 0x100)},
				{Base: 0x600C5000, Data: mmu},
			},
			Devices: []xtsim.Device{fl.XtDevice(0x60002000), io.device()},
		})
	}
	c0, c1 := mk(0xCDCD, false), mk(0xABAB, !core1Running)
	sc := SimConfig()
	sc.Xtensa = &xtsim.Chain{TAPs: []*xtsim.Target{c0, c1}}
	r := newRigCfg(t, sc)
	if err := r.OpenDevice(context.Background(), wantID, cfg); err != nil {
		t.Fatal(err)
	}
	f := newFlashOn(t, &opsRig{rig: r, fl: fl}, esp.ChipIDESP32S3)
	return &s3FlashRig{flashFix: f, core0: c0, core1: c1, iomux: io}
}

// pinsRestored checks that the flash pins were switched to the SPI function
// (MCU_SEL 0) during the call and are back in GPIO mode afterwards.
func (r *s3FlashRig) pinsRestored(t *testing.T) {
	t.Helper()
	r.iomux.mu.Lock()
	defer r.iomux.mu.Unlock()
	attached := false
	for _, w := range r.iomux.writes {
		if w[0] < 0x70 || w[0] > 0x84 {
			t.Fatalf("IO_MUX write outside the flash pins GPIO27..32: %#x", w)
		}
		attached = attached || w[1] == 0x0b00
	}
	if !attached {
		t.Fatal("the flash pins were never attached")
	}
	for o := uint32(0x6C); o <= 0x84; o += 4 {
		if r.iomux.regs[o] != 0x1b00 {
			t.Fatalf("IO_MUX %#x left at %#x", o, r.iomux.regs[o])
		}
	}
}

func TestS3FlashInfoReadBackup(t *testing.T) {
	r := newS3Flash(t, allow, false)
	m := r.must(t, "FlashProgramming", "info", nil)
	ev := m["chip_evidence"].(map[string]any)
	if ev["chip"] != "ESP32-S3" || ev["image_chip_id"] != "0x0009" || m["flash_writable"] != true || m["hart"] != 0.0 {
		t.Fatalf("info %v", m)
	}
	if ps := m["partitions"].([]any); len(ps) != 4 {
		t.Fatalf("partitions %v", ps)
	}
	mw := m["mapped_window_check"].([]any)[0].(map[string]any)
	if mw["mapped"] != false || !strings.Contains(mw["reason"].(string), "data cache is disabled") {
		t.Fatalf("mapped window %v", mw)
	}
	for _, c := range []struct{ a, n int }{{0, 4096}, {0x9001, 777}, {0x20000, 2048}} {
		g := r.must(t, "FlashProgramming", "read", map[string]any{"address": float64(c.a), "length": float64(c.n)})
		if g["hex"] != hex.EncodeToString(r.fl.Mem[c.a:c.a+c.n]) {
			t.Fatalf("read %v", c)
		}
	}
	r.fail(t, codes.OutOfRange, "", "FlashProgramming", "read", map[string]any{"address": float64(256<<10 - 1), "length": 2.0})
	if r.core0.Stopped() {
		t.Fatal("a read-only flash call left core 0 halted")
	}
	r.pinsRestored(t)
	// The backup taken by the fixture covers the whole flash.
	if b, _, err := esp.LoadBackup(r.backup); err != nil || sum(b) != sum(r.fl.Mem) {
		t.Fatalf("backup %v", err)
	}
}

func TestS3FlashMappedWindow(t *testing.T) {
	r := newS3Flash(t, allow, false)
	// data cache on, core 0 bus open, MMU entry 0 -> flash page 2 (0x20000)
	r.core0.SetMem(0x600C4000, []byte{1, 0, 0, 0, 0, 0, 0, 0})
	r.core0.SetMem(0x600C5000, []byte{2, 0, 0, 0})
	r.core0.SetMem(0x3C000000, r.fl.Mem[0x20000:0x20100])
	mw := r.must(t, "FlashProgramming", "info", nil)["mapped_window_check"].([]any)[0].(map[string]any)
	if mw["mapped"] != true || mw["flash_offset"] != "0x20000" || mw["window_equals_flash"] != true {
		t.Fatalf("mapped window %v", mw)
	}
	r.core0.SetMem(0x3C000010, []byte{0x5a ^ r.fl.Mem[0x20010]})
	mw = r.must(t, "FlashProgramming", "info", nil)["mapped_window_check"].([]any)[0].(map[string]any)
	if mw["window_equals_flash"] != false {
		t.Fatalf("a differing window compared equal: %v", mw)
	}
	r.core0.SetMem(0x600C4004, []byte{1, 0, 0, 0})
	mw = r.must(t, "FlashProgramming", "info", nil)["mapped_window_check"].([]any)[0].(map[string]any)
	if mw["mapped"] != false || !strings.Contains(mw["reason"].(string), "shut") {
		t.Fatalf("shut bus %v", mw)
	}
}

func TestS3FlashProgramVerifyRestore(t *testing.T) {
	r := newS3Flash(t, allow, false)
	orig := sum(r.fl.Mem)
	img := appImageFor(esp.ChipIDESP32S3, 0x66)
	p := r.image(t, "s3app.bin", img)
	args := func(extra map[string]any) map[string]any {
		a := map[string]any{"path": p, "address": float64(0x20000), "confirm": "program-flash", "backup": r.backup}
		for k, v := range extra {
			a[k] = v
		}
		return a
	}
	r.fail(t, codes.FailedPrecondition, "not available on the ESP32-S3", "FlashProgramming", "program", args(map[string]any{"reset": "run"}))
	r.fail(t, codes.FailedPrecondition, "not available on the ESP32-S3", "FlashProgramming", "program", args(map[string]any{"reset": "halt"}))
	r.fail(t, codes.PermissionDenied, "confirm", "FlashProgramming", "program", args(map[string]any{"confirm": "restore-flash"}))
	r.fail(t, codes.PermissionDenied, "backup", "FlashProgramming", "program", args(map[string]any{"backup": ""}))
	r.fail(t, codes.PermissionDenied, "protected", "FlashProgramming", "program", args(map[string]any{"address": 0.0}))
	c6 := r.image(t, "c6app.bin", appImageFor(esp.ChipIDESP32C6, 0x66))
	r.fail(t, codes.FailedPrecondition, "chip id 0x000d", "FlashProgramming", "program", args(map[string]any{"path": c6}))
	if r.fl.Erases+r.fl.Programs != 0 {
		t.Fatal("a refused program touched flash")
	}
	m := r.must(t, "FlashProgramming", "program", args(nil))
	if m["verified"] != true || m["reset"] != "none" || string(r.fl.Mem[0x20000:0x20000+len(img)]) != string(img) {
		t.Fatalf("program %v", m)
	}
	if !r.core0.Stopped() {
		t.Fatal("a modifying call must leave the core halted")
	}
	r.must(t, "FlashProgramming", "verify", map[string]any{"path": p, "address": float64(0x20000)})
	r.must(t, "DebugProbe", "resume", nil)
	m = r.must(t, "FlashProgramming", "erase", map[string]any{"mode": "region", "address": float64(0x3F000), "length": 4096.0, "confirm": "erase-flash-region", "backup": r.backup})
	if m["verified_erased"] != true {
		t.Fatalf("erase %v", m)
	}
	m = r.must(t, "FlashProgramming", "restore", map[string]any{"path": r.backup, "confirm": "restore-flash", "confirm_protected": "modify-protected-region"})
	if m["verified"] != true || m["reset"] != "none" || sum(r.fl.Mem) != orig {
		t.Fatalf("restore %v", m)
	}
	r.fail(t, codes.FailedPrecondition, "not available on the ESP32-S3", "FlashProgramming", "restore",
		map[string]any{"path": r.backup, "confirm": "restore-flash", "confirm_protected": "modify-protected-region", "reset": "run"})
	r.pinsRestored(t)
}

func TestS3FlashRefusedWhenEncrypted(t *testing.T) {
	r := newS3Flash(t, allow, false)
	r.core0.SetMem(0x60007034, []byte{0, 0, 1 << 2, 0}) // SPI_BOOT_CRYPT_CNT = 1 (word 1 bit 18)
	m := r.must(t, "FlashProgramming", "info", nil)
	sec := m["security"].(map[string]any)
	if m["flash_writable"] != false || sec["flash_encryption_crypt_cnt"] != 1.0 {
		t.Fatalf("info %v", m)
	}
	p := r.image(t, "s3app.bin", appImageFor(esp.ChipIDESP32S3, 0x77))
	r.fail(t, codes.FailedPrecondition, "encrypt", "FlashProgramming", "program",
		map[string]any{"path": p, "address": float64(0x20000), "confirm": "program-flash", "backup": r.backup})
	if r.fl.Erases+r.fl.Programs != 0 {
		t.Fatal("programmed encrypted flash")
	}
}

// A running core 1 is halted for the call (it could fetch from flash): a
// read-only call resumes both cores, a modifying one leaves both halted.
func TestS3FlashHaltsOtherCore(t *testing.T) {
	r := newS3Flash(t, allow, true)
	r.must(t, "FlashProgramming", "read", map[string]any{"address": 0.0, "length": 16.0})
	if r.core0.Stopped() || r.core1.Stopped() {
		t.Fatal("cores left halted after a read")
	}
	r.must(t, "FlashProgramming", "erase", map[string]any{"mode": "region", "address": float64(0x3F000), "length": 4096.0, "confirm": "erase-flash-region", "backup": r.backup})
	if !r.core0.Stopped() || !r.core1.Stopped() {
		t.Fatalf("after an erase both cores must be halted: core0 %v core1 %v", r.core0.Stopped(), r.core1.Stopped())
	}
}
