package espusbjtag

import (
	"encoding/binary"
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/faronov/hwlab/internal/riscv/rvsim"
	"github.com/faronov/hwlab/plugins/espusbjtag/esp"
	"github.com/faronov/hwlab/plugins/espusbjtag/sim"
)

// simConfigFlashC5 is SimConfigFlash reshaped as a SYNTHETIC ESP32-C5: a
// two-TAP chain whose TAP nearest TDO is an LP-core TAP with the same IDCODE
// (but no ROM word, so addressing it would fail identification), the HP-core
// debug module without hart-select bits, the C5 ROM word, the eFuse block at
// 0x600B4800 and the C6 watchdog addresses.
func simConfigFlashC5() (sim.Config, *esp.SimFlash, *rvsim.Target, map[uint32]map[uint32]uint32) {
	sc, fl := SimConfigFlash()
	regs := map[uint32]map[uint32]uint32{}
	dev := func(base uint32) rvsim.Device {
		m := map[uint32]uint32{}
		regs[base] = m
		return rvsim.Device{Base: base, Size: 0x100, Read: func(o uint32) uint32 { return m[o] }, Write: func(o, v uint32) { m[o] = v }}
	}
	sc.Target.IDCode = esp.ChipC5.IDCode
	sc.Target.Harts, sc.Target.Unavailable, sc.Target.HartSelBits = 1, nil, -1
	for i, r := range sc.Target.Memory {
		if r.Base == esp.ChipMagicAddr {
			sc.Target.Memory[i].Data = binary.LittleEndian.AppendUint32(nil, esp.ChipC5.ROMWord)
		}
	}
	sc.Target.Devices = []rvsim.Device{fl.Device(), dev(0x600B4800), dev(0x60008000), dev(0x60009000), dev(0x600B1C00)}
	lp := rvsim.New(rvsim.Config{IDCode: esp.ChipC5.IDCode, Memory: []rvsim.Region{{Base: 0x50000000, Data: make([]byte, 64)}}})
	sc.NearTDO = []*rvsim.Target{lp}
	return sc, fl, lp, regs
}

func TestC5ChipInfoAddressesHPTAP(t *testing.T) {
	sc, fl, lp, _ := simConfigFlashC5()
	r := newOpsSim(t, allow, sc, fl)
	m := r.must(t, "DebugProbe", "chip_info", nil)
	h, _ := m["chip_hint"].(map[string]any)
	if m["jtag_tap_index"] != 1.0 || m["jtag_taps"] != 2.0 || m["harts"] != 1.0 || m["idcode"] != "0x00017c25" || h["confidence"] != "idcode-only" {
		t.Fatalf("%v", m)
	}
	r.fail(t, codes.FailedPrecondition, "hart", "DebugProbe", "halt", map[string]any{"hart": 1.0})
	if r.target.Halted(0) || lp.Halted(0) || lp.DMIOps != 0 {
		t.Fatal("a refused hart select or the LP TAP saw a debug operation")
	}
}

func TestC5FlashInfoBackupProgram(t *testing.T) {
	sc, fl, lp, regs := simConfigFlashC5()
	f := newFlashOn(t, newOpsSim(t, allow, sc, fl), esp.ChipIDESP32C5)
	m := f.must(t, "FlashProgramming", "info", nil)
	ev, _ := m["chip_evidence"].(map[string]any)
	if ev["chip"] != "ESP32-C5" || ev["jtag_idcode"] != "0x00017c25" || ev["image_chip_id"] != "0x0017" {
		t.Fatalf("%v", m["chip_evidence"])
	}
	c6 := f.image(t, "c6.bin", appImage(0x22))
	f.fail(t, codes.FailedPrecondition, "ESP32-C5", "FlashProgramming", "program",
		map[string]any{"path": c6, "address": float64(0x20000), "confirm": "program-flash", "backup": f.backup, "reset": "none"})
	img := appImageFor(esp.ChipIDESP32C5, 0x11)
	p := f.image(t, "c5.bin", img)
	m = f.must(t, "FlashProgramming", "program", map[string]any{"path": p, "address": float64(0x20000), "confirm": "program-flash", "backup": f.backup, "reset": "none"})
	if m["verified"] != true || m["image_validated"] != true || string(f.fl.Mem[0x20000:0x20000+len(img)]) != string(img) {
		t.Fatalf("%v", m)
	}
	// Watchdogs quiesced and restored at the C5 (C6-layout) addresses, the
	// super watchdog's auto-feed set for the halt and cleared again.
	if regs[0x60008000][0x64] != 0 || regs[0x60009000][0x64] != 0 || regs[0x600B1C00][0x18] != 0 || regs[0x600B1C00][0x20] != 0 {
		t.Fatal("write protection left open")
	}
	if regs[0x60008000][0x60] != 1 || regs[0x60009000][0x60] != 1 || regs[0x600B1C00][0x14] != 1<<31 {
		t.Fatal("C5 watchdogs not fed on restore")
	}
	if regs[0x600B1C00][0x1C]&(1<<18) != 0 {
		t.Fatal("SWD auto-feed left on after restore")
	}
	if lp.DMIOps != 0 || lp.Halted(0) {
		t.Fatal("the LP-core TAP was used")
	}
}
