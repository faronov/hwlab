package espusbjtag

import (
	"encoding/binary"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/faronov/hwlab/internal/riscv/rvsim"
	"github.com/faronov/hwlab/plugins/espusbjtag/esp"
)

// loaderPC is where the SYNTHETIC hart sits in "download mode": inside the
// ROM, past spi_flash_attach, so the straight-line model of rvsim runs from
// the attach entry into the return breakpoint.
const loaderPC = 0x40000400

// romCalls records the hart state at every resume at the attach entry.
type romCalls struct {
	n           int
	a0, a1, ra  uint32
	otherResume int
}

// downloadModeH2 is the SYNTHETIC H2 with a ROM of c.nop instructions and its
// flash detached; resuming at the ROM attach entry attaches it.
func downloadModeH2(t *testing.T, attachWorks bool) (*opsRig, *romCalls) {
	t.Helper()
	sc, fl, _ := simConfigFlashH2()
	rom := make([]byte, 0x1000)
	for i := 0; i < len(rom); i += 2 {
		binary.LittleEndian.PutUint16(rom[i:], 0x0001) // c.nop
	}
	sc.Target.Memory = append(sc.Target.Memory, rvsim.Region{Base: 0x40000000, Data: rom})
	calls := &romCalls{}
	sc.Target.OnResume = func(_ int, pc uint32, gpr *[32]uint32) {
		if pc != esp.ChipH2.ROMSPIAttach {
			calls.otherResume++
			return
		}
		calls.n++
		calls.a0, calls.a1, calls.ra = gpr[10], gpr[11], gpr[1]
		if attachWorks {
			fl.SetDetached(false)
		}
	}
	fl.SetDetached(true)
	r := newOpsSim(t, allow, sc, fl)
	r.target.SetCSR(0, 0x7B1, loaderPC)
	return r, calls
}

func TestDownloadModeFlashIsAttachedThroughROM(t *testing.T) {
	r, calls := downloadModeH2(t, true)
	a0, ra, sp := r.target.GPR(10), r.target.GPR(1), r.target.GPR(2)
	m := r.must(t, "FlashProgramming", "info", nil)
	at, ok := m["rom_flash_attach"].(map[string]any)
	if !ok || at["address"] != "0x400001d4" || at["halted_at_pc"] != "0x40000400" {
		t.Fatalf("attach not reported: %v", m)
	}
	if calls.n != 1 || calls.a0 != 0 || calls.a1 != 0 || calls.ra != loaderPC {
		t.Fatalf("ROM call: %+v", calls)
	}
	// Every register the call used is back, the trigger slot is free again.
	if r.target.GPR(10) != a0 || r.target.GPR(1) != ra || r.target.GPR(2) != sp || r.target.CSR(0, 0x7B1) != loaderPC {
		t.Fatalf("registers not restored: a0 %#x ra %#x sp %#x dpc %#x", r.target.GPR(10), r.target.GPR(1), r.target.GPR(2), r.target.CSR(0, 0x7B1))
	}
	if r.target.CSR(0, 0x300)&(1<<3) != 0x1800&(1<<3) {
		t.Fatalf("mstatus not restored: %#x", r.target.CSR(0, 0x300))
	}
	for i := 0; i < 4; i++ {
		if t1, _ := r.target.Trigger(0, i); t1&(1<<2|1<<6) != 0 {
			t.Fatalf("trigger %d left armed: %#x", i, t1)
		}
	}
	// Attached now: the next calls do not call the ROM again, and the
	// result carries no attach report.
	m = r.must(t, "FlashProgramming", "backup", map[string]any{"dir": t.TempDir()})
	if m["reads_identical"] != true || m["rom_flash_attach"] != nil || calls.n != 1 {
		t.Fatalf("%v (rom calls %d)", m, calls.n)
	}
	img := appImageFor(esp.ChipIDESP32H2, 0x33)
	f := &flashFix{opsRig: r, dir: t.TempDir(), backup: m["backup_path"].(string)}
	p := f.image(t, "h2.bin", img)
	m = f.must(t, "FlashProgramming", "program", map[string]any{"path": p, "address": float64(0x20000), "confirm": "program-flash", "backup": f.backup, "reset": "none"})
	if m["verified"] != true || string(r.fl.Mem[0x20000:0x20000+len(img)]) != string(img) {
		t.Fatalf("%v", m)
	}
}

// A flash that does not answer while the hart is outside the ROM (an
// application halted in flash or SRAM) is not attached: the ROM is not called.
func TestNoROMAttachOutsideROM(t *testing.T) {
	r, calls := downloadModeH2(t, true)
	r.target.SetCSR(0, 0x7B1, 0x40800000)
	r.fail(t, codes.FailedPrecondition, "outside the ROM download loader", "FlashProgramming", "info", nil)
	if calls.n != 0 {
		t.Fatalf("ROM called %d times", calls.n)
	}
}

// If the ROM attach returns but the flash still does not answer, the call
// fails and says so; the ROM is called once.
func TestROMAttachThatDoesNotHelpFails(t *testing.T) {
	r, calls := downloadModeH2(t, false)
	if _, err := r.call("FlashProgramming", "info", nil); err == nil || !strings.Contains(err.Error(), "still does not answer") || calls.n != 1 {
		t.Fatalf("err %v, rom calls %d", err, calls.n)
	}
}

// The C6 profile has no ROM attach address: a flash that does not answer is
// the same error as before, and nothing is resumed into the ROM.
func TestC6NoFlashUnchanged(t *testing.T) {
	sc, fl := SimConfigFlash()
	resumes := 0
	sc.Target.OnResume = func(int, uint32, *[32]uint32) { resumes++ }
	fl.SetDetached(true)
	r := newOpsSim(t, allow, sc, fl)
	r.target.SetCSR(0, 0x7B1, 0x40000400)
	if _, err := r.call("FlashProgramming", "info", nil); err == nil {
		t.Fatal("C6 without flash answered")
	}
	if resumes != 1 { // only the read-only call's own resume of the hart it halted
		t.Fatalf("resumes %d", resumes)
	}
}
