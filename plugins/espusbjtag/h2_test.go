package espusbjtag

import (
	"encoding/binary"
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/faronov/hwlab/internal/riscv/rvsim"
	"github.com/faronov/hwlab/plugins/espusbjtag/esp"
	"github.com/faronov/hwlab/plugins/espusbjtag/sim"
)

// simConfigFlashH2 is SimConfigFlash reshaped as a SYNTHETIC ESP32-H2: its
// JTAG IDCODE and ROM word, the timer groups at 0x60009000/0x6000A000 and
// nothing at 0x60008000 (LEDC on the H2, TIMG0 on the C6), so any C6-address
// write fails on the bus.
func simConfigFlashH2() (sim.Config, *esp.SimFlash, map[uint32]map[uint32]uint32) {
	sc, fl := SimConfigFlash()
	regs := map[uint32]map[uint32]uint32{}
	dev := func(base uint32) rvsim.Device {
		m := map[uint32]uint32{}
		regs[base] = m
		return rvsim.Device{Base: base, Size: 0x100, Read: func(o uint32) uint32 { return m[o] }, Write: func(o, v uint32) { m[o] = v }}
	}
	sc.Target.IDCode = esp.ChipH2.IDCode
	for i, r := range sc.Target.Memory {
		if r.Base == esp.ChipMagicAddr {
			sc.Target.Memory[i].Data = binary.LittleEndian.AppendUint32(nil, esp.ChipH2.ROMWord)
		}
	}
	sc.Target.Devices = []rvsim.Device{fl.Device(), dev(0x600B0800), dev(0x60009000), dev(0x6000A000), dev(0x600B1C00)}
	return sc, fl, regs
}

func newFlashH2(t *testing.T) (*flashFix, map[uint32]map[uint32]uint32) {
	t.Helper()
	sc, fl, regs := simConfigFlashH2()
	return newFlashOn(t, newOpsSim(t, allow, sc, fl), esp.ChipIDESP32H2), regs
}

func TestH2FlashInfoBackupProgram(t *testing.T) {
	f, regs := newFlashH2(t)
	m := f.must(t, "FlashProgramming", "info", nil)
	ev, _ := m["chip_evidence"].(map[string]any)
	if ev["chip"] != "ESP32-H2" || ev["jtag_idcode"] != "0x00010c25" || ev["image_chip_id"] != "0x0010" {
		t.Fatalf("%v", m["chip_evidence"])
	}
	img := appImageFor(esp.ChipIDESP32H2, 0x11)
	p := f.image(t, "h2.bin", img)
	m = f.must(t, "FlashProgramming", "program", map[string]any{"path": p, "address": float64(0x20000), "confirm": "program-flash", "backup": f.backup, "reset": "none"})
	if m["verified"] != true || m["image_validated"] != true {
		t.Fatalf("%v", m)
	}
	if string(f.fl.Mem[0x20000:0x20000+len(img)]) != string(img) {
		t.Fatal("image not in flash")
	}
	// The H2 watchdogs were quiesced and restored at their H2 addresses,
	// with SWD auto-feed restored too.
	if regs[0x60009000][0x64] != 0 || regs[0x6000A000][0x64] != 0 || regs[0x600B1C00][0x1C] != 0 || regs[0x600B1C00][0x24] != 0 {
		t.Fatal("write protection left open")
	}
	if regs[0x60009000][0x60] != 1 || regs[0x6000A000][0x60] != 1 || regs[0x600B1C00][0x18] != 1<<31 {
		t.Fatal("H2 watchdogs not fed on restore")
	}
	if regs[0x600B1C00][0x20]&(1<<18) != 0 {
		t.Fatal("SWD auto-feed left on after restore")
	}
}

func TestH2RefusesC6ImageAndC6RefusesH2Image(t *testing.T) {
	h2, _ := newFlashH2(t)
	p := h2.image(t, "c6.bin", appImage(0x22))
	h2.fail(t, codes.FailedPrecondition, "ESP32-H2", "FlashProgramming", "program",
		map[string]any{"path": p, "address": float64(0x20000), "confirm": "program-flash", "backup": h2.backup, "reset": "none"})
	if h2.fl.Programs+h2.fl.Erases != 0 {
		t.Fatal("a C6 image touched H2 flash")
	}
	c6 := newFlash(t)
	p = c6.image(t, "h2.bin", appImageFor(esp.ChipIDESP32H2, 0x22))
	c6.fail(t, codes.FailedPrecondition, "ESP32-C6", "FlashProgramming", "program",
		map[string]any{"path": p, "address": float64(0x20000), "confirm": "program-flash", "backup": c6.backup, "reset": "none"})
	if c6.fl.Programs+c6.fl.Erases != 0 {
		t.Fatal("an H2 image touched C6 flash")
	}
}

// Conflicting or unknown identity evidence refuses flash access before the
// hart is halted and before any watchdog/flash register is written.
func TestUnknownChipRefusesFlash(t *testing.T) {
	for name, mod := range map[string]func(*sim.Config){
		"h2-idcode-c6-rom": func(c *sim.Config) { c.Target.IDCode = esp.ChipH2.IDCode },
		"unknown-idcode":   func(c *sim.Config) { c.Target.IDCode = 0x12345025 },
		"unknown-rom": func(c *sim.Config) {
			for i, r := range c.Target.Memory {
				if r.Base == esp.ChipMagicAddr {
					c.Target.Memory[i].Data = []byte{1, 2, 3, 4}
				}
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			sc, fl := SimConfigFlash()
			mod(&sc)
			regs := map[uint32]uint32{}
			for i, d := range sc.Target.Devices {
				if d.Base == 0x60008000 || d.Base == 0x600B1C00 {
					base, inner := d.Base, d.Write
					sc.Target.Devices[i].Write = func(o, v uint32) { regs[base+o] = v; inner(o, v) }
				}
			}
			r := newOpsSim(t, allow, sc, fl)
			r.fail(t, codes.FailedPrecondition, "refusing", "FlashProgramming", "info", nil)
			r.fail(t, codes.FailedPrecondition, "refusing", "FlashProgramming", "backup", map[string]any{"dir": t.TempDir()})
			if len(regs) != 0 || fl.Erases+fl.Programs != 0 {
				t.Fatalf("refused chip had registers written: %v", regs)
			}
		})
	}
}

func TestH2MemoryWriteUsesH2SRAM(t *testing.T) {
	sc, fl, _ := simConfigFlashH2()
	r := newOpsSim(t, allow, sc, fl)
	// inside the C6 SRAM but past the H2's 320 KiB HP SRAM
	r.fail(t, codes.InvalidArgument, "ESP32-H2", "MemoryAccess", "write", map[string]any{"address": "0x40860000", "data": "00112233"})
	r.must(t, "MemoryAccess", "write", map[string]any{"address": "0x40801000", "data": "00112233"})
}
