//go:build hardware && linux

package native

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/faronov/hwlab/plugins/ccdebugger/native/flashplan"
)

func trim(p string) string {
	b, _ := os.ReadFile(p)
	return strings.TrimSpace(string(b))
}

func findDebugger(t *testing.T) Address {
	t.Helper()
	dirs, _ := filepath.Glob("/sys/bus/usb/devices/*")
	for _, d := range dirs {
		if trim(d+"/idVendor") == "0451" && trim(d+"/idProduct") == "16a2" {
			bus, _ := strconv.Atoi(trim(d + "/busnum"))
			dev, _ := strconv.Atoi(trim(d + "/devnum"))
			return Address{bus, dev}
		}
	}
	t.Skip("no CC-Debugger 0451:16a2 attached")
	return Address{}
}

func openHW(t *testing.T, c Config) *Debugger {
	t.Helper()
	tr, err := Open(findDebugger(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	d, err := New(tr, c)
	if err != nil {
		tr.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// Adapter-only: no target traffic at all.
func TestHardwareAdapterState(t *testing.T) {
	d := openHW(t, Config{Access: AdapterOnly, Timeout: 2 * time.Second})
	st, err := d.ReadAdapterState()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("adapter state: target=0x%04x fw=%d rev=%d", st.TargetID, st.FirmwareVersion, st.FirmwareRev)
	if _, err := d.ReadPC(); err == nil {
		t.Fatal("adapter-only session read the PC")
	}
}

// HWLAB_CC_NATIVE_RESET=1 resets the attached target into debug halt (the
// firmware restarts from reset when the test resumes it), then performs only
// reads and finally resumes the target. It never writes memory or flash.
func TestHardwareResetAttachReadOnly(t *testing.T) {
	if os.Getenv("HWLAB_CC_NATIVE_RESET") != "1" {
		t.Skip("set HWLAB_CC_NATIVE_RESET=1 to reset the attached target into debug halt")
	}
	d := openHW(t, Config{Access: ResetDebugSession, Timeout: 20 * time.Second, Permissions: Permissions{
		CPUControl: true, TargetReset: true, MemoryAccess: true, DMAEnable: true}})
	st, err := d.ReadAdapterState()
	if err != nil || st.TargetID != targetCC2530 {
		t.Skipf("no CC2530 behind the debugger: %+v, %v", st, err)
	}
	att, err := d.AttachReset()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("attach: status=0x%02x", uint8(att.StatusAfter))
	// Leave the target running even if a check below fails.
	defer func() {
		if _, err := d.EnableDMAAfterReset(); err != nil {
			t.Logf("enable DMA: %v", err)
		}
		if r, err := d.Resume(); err != nil {
			t.Errorf("resume: %v", err)
		} else {
			t.Logf("resumed: status=0x%02x", uint8(r.StatusAfter))
		}
	}()
	pc, err := d.ReadPC()
	if err != nil || pc != 0 {
		t.Fatalf("PC after reset = %#x, %v", pc, err)
	}
	regs, err := d.ReadRegisters()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("registers: %+v", regs)
	code, err := d.ReadCode(0, 16)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("CODE[0:16] = %x", code)
	flash, err := d.ReadFlashCode(0x8000, 16)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("flash[0x8000:+16] = %x", flash)
	if bank, err := d.ReadBank(); err != nil {
		t.Fatal(err)
	} else {
		t.Logf("FMAP bank = %d", bank)
	}
	if v, err := d.ReadXDATA(0x0000, 8); err != nil {
		t.Fatal(err)
	} else {
		t.Logf("XDATA[0:8] = %x", v)
	}
}

// HWLAB_CC_NATIVE_DEBUG=1 additionally exercises the conditional debug
// functions on a freshly reset target that has not executed any application
// code yet: an XDATA scratch write with restore, a hardware breakpoint on the
// reset vector's jump target, a single step, and read-only identification and
// CRC verification. It never writes flash, MAC or lock bytes. The target is
// reset again and left running.
func TestHardwareDebugControlAndScratch(t *testing.T) {
	if os.Getenv("HWLAB_CC_NATIVE_DEBUG") != "1" {
		t.Skip("set HWLAB_CC_NATIVE_DEBUG=1 to run halt/step/breakpoint and the XDATA scratch test (resets the target)")
	}
	d := openHW(t, Config{Access: ResetDebugSession, Timeout: 30 * time.Second, Permissions: Permissions{
		CPUControl: true, TargetReset: true, MemoryAccess: true, MemoryWrite: true, Breakpoints: true, DMAEnable: true}})
	if st, err := d.ReadAdapterState(); err != nil || st.TargetID != targetCC2530 {
		t.Skipf("no CC2530 behind the debugger: %+v, %v", st, err)
	}
	if _, err := d.AttachReset(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := d.ResetHalt(); err != nil {
			t.Errorf("final reset: %v", err)
			return
		}
		if _, err := d.EnableDMAAfterReset(); err != nil {
			t.Errorf("enable DMA: %v", err)
		}
		if _, err := d.Resume(); err != nil {
			t.Errorf("resume: %v", err)
		}
	}()
	if _, err := d.EnableDMAAfterReset(); err != nil {
		t.Fatal(err)
	}

	info, err := d.ReadChipInfo()
	if err != nil {
		t.Fatal(err)
	}
	if info.Chip != "CC2530" || info.Geometry.FlashSize != 256*1024 || info.Geometry.RAMSize != 8192 {
		t.Fatalf("identity: %+v", info)
	}
	if id, rev, err := d.ReadChipIDCommand(); err != nil {
		t.Fatalf("GET_CHIP_ID: %v", err)
	} else if id != 0xA5 || rev != info.Geometry.Revision {
		t.Fatalf("GET_CHIP_ID = %#x rev %#x, registers say %+v", id, rev, info.Geometry)
	}
	if _, err := d.ReadLock(); err != nil {
		t.Fatal(err)
	}
	ieee, err := d.ReadIEEE()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("primary IEEE present: %v", ieee.PrimaryPresent)

	before, err := d.ReadRegisters()
	if err != nil {
		t.Fatal(err)
	}
	if sp, err := d.ReadSFR(0x81); err != nil || sp != before.SP {
		t.Fatalf("SFR SP = %#x, %v; register snapshot says %#x", sp, err, before.SP)
	}
	if psw, err := d.ReadSFR(0xD0); err != nil || psw != before.PSW {
		t.Fatalf("SFR PSW = %#x, %v; register snapshot says %#x", psw, err, before.PSW)
	}
	if _, err := d.ReadSFR(0xBC); err == nil {
		t.Fatal("peripheral SFR RNDL was readable")
	}
	const scratch = 0x1C00
	orig, err := d.ReadXDATA(scratch, 16)
	if err != nil {
		t.Fatal(err)
	}
	pattern := []byte{0xA5, 0x5A, 0x01, 0x80, 0xFF, 0x00, 0x3C, 0xC3, 1, 2, 3, 4, 5, 6, 7, 8}
	if err := d.WriteXDATA(scratch, pattern); err != nil {
		t.Fatal(err)
	}
	got, err := d.ReadXDATA(scratch, 16)
	if err != nil || string(got) != string(pattern) {
		t.Fatalf("pattern readback % x, %v", got, err)
	}
	if err := d.WriteXDATA(scratch, orig); err != nil {
		t.Fatal(err)
	}
	got, err = d.ReadXDATA(scratch, 16)
	if err != nil || string(got) != string(orig) {
		t.Fatalf("restore readback % x != % x, %v", got, orig, err)
	}
	after, err := d.ReadRegisters()
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("CPU state changed by the scratch write:\nbefore %+v\nafter  %+v", before, after)
	}

	code, err := d.ReadCode(0, 3)
	if err != nil || code[0] != 0x02 {
		t.Skipf("reset vector is not an LJMP (% x, %v): no safe breakpoint target", code, err)
	}
	vec := uint16(code[1])<<8 | uint16(code[2])
	if _, err := d.SetBreakpoint(0, vec, true); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Resume(); err != nil {
		t.Fatal(err)
	}
	st, pc, err := d.WaitHalted(3 * time.Second)
	if err != nil {
		t.Fatalf("breakpoint not hit: %v", err)
	}
	t.Logf("halted: status=0x%02x pc=%#x (breakpoint at %#x)", uint8(st), pc, vec)
	if pc != vec {
		t.Fatalf("halted at %#x, want %#x", pc, vec)
	}
	if _, err := d.SetBreakpoint(0, 0, false); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Step(); err != nil {
		t.Fatal(err)
	}
	pc2, err := d.ReadPC()
	if err != nil || pc2 == vec {
		t.Fatalf("PC after step %#x (was %#x), %v", pc2, vec, err)
	}
	t.Logf("step: pc %#x -> %#x", vec, pc2)
}

// HWLAB_CC_NATIVE_DEBUG=1: halts the target while its application runs for a
// moment, reads the PC and registers, and resumes it.
func TestHardwareHaltRunningTarget(t *testing.T) {
	if os.Getenv("HWLAB_CC_NATIVE_DEBUG") != "1" {
		t.Skip("set HWLAB_CC_NATIVE_DEBUG=1 to halt and resume the running target")
	}
	d := openHW(t, Config{Access: ResetDebugSession, Timeout: 30 * time.Second, Permissions: Permissions{
		CPUControl: true, TargetReset: true, MemoryAccess: true, DMAEnable: true}})
	if st, err := d.ReadAdapterState(); err != nil || st.TargetID != targetCC2530 {
		t.Skipf("no CC2530 behind the debugger: %+v, %v", st, err)
	}
	if _, err := d.AttachReset(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.EnableDMAAfterReset(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Resume(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	// Whatever happens next, leave the target running.
	defer func() {
		if st, err := d.ReadDebugStatus(); err == nil && st&CPUHalted != 0 {
			if _, err := d.Resume(); err != nil {
				t.Errorf("resume: %v", err)
			}
		}
	}()
	res, err := d.Halt()
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusAfter&CPUHalted == 0 {
		t.Fatalf("not halted: %+v", res)
	}
	pc, err := d.ReadPC()
	if err != nil {
		t.Fatal(err)
	}
	regs, err := d.ReadRegisters()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("running target halted at PC=%#x SP=%d bank=%d", pc, regs.SP, regs.Bank)
	if regs.PC != pc {
		t.Fatalf("PC %#x != register snapshot PC %#x", pc, regs.PC)
	}
	r, err := d.Resume()
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusAfter&CPUHalted != 0 {
		t.Fatalf("still halted after resume: %+v", r)
	}
}

// HWLAB_CC_NATIVE_DEBUG=1: the hardware CRC over several flash ranges equals
// the CRC of the same bytes read back, and a flipped byte is detected. RAM
// scratch and DMA/RNG registers only; flash is never written.
func TestHardwareCRCMatchesReadback(t *testing.T) {
	if os.Getenv("HWLAB_CC_NATIVE_DEBUG") != "1" {
		t.Skip("set HWLAB_CC_NATIVE_DEBUG=1 to run the CRC check (resets the target)")
	}
	d := openHW(t, Config{Access: ResetDebugSession, Timeout: 30 * time.Second, Permissions: Permissions{
		CPUControl: true, TargetReset: true, MemoryAccess: true, MemoryWrite: true, DMAEnable: true}})
	if st, err := d.ReadAdapterState(); err != nil || st.TargetID != targetCC2530 {
		t.Skipf("no CC2530 behind the debugger: %+v, %v", st, err)
	}
	if _, err := d.AttachReset(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := d.ResetHalt(); err != nil {
			t.Errorf("final reset: %v", err)
			return
		}
		if _, err := d.EnableDMAAfterReset(); err != nil {
			t.Errorf("enable DMA: %v", err)
		}
		if _, err := d.Resume(); err != nil {
			t.Errorf("resume: %v", err)
		}
	}()
	if _, err := d.EnableDMAAfterReset(); err != nil {
		t.Fatal(err)
	}
	for _, addr := range []int{0, 0x1000, 0x8000, 0x1C000} {
		data, err := d.ReadFlashBytes(addr, 1024)
		if err != nil {
			t.Fatal(err)
		}
		blocks := []flashplan.Block{{Addr: addr, Data: data}}
		if n, err := d.VerifyFlash(blocks, VerifyBoth, nil); err != nil || n != 1 {
			t.Fatalf("0x%X: %d CRC checks, %v", addr, n, err)
		}
		bad := append([]byte(nil), data...)
		bad[100] ^= 0x10
		if _, err := d.VerifyFlash([]flashplan.Block{{Addr: addr, Data: bad}}, VerifyCRC, nil); !errors.Is(err, ErrVerify) {
			t.Fatalf("0x%X: corrupted image accepted by CRC: %v", addr, err)
		}
	}
}

// HWLAB_CC_NATIVE_DEBUG=1: the programming data path up to, but not
// including, the flash controller. Bytes are burst-written through the debug
// interface and moved by DMA channel 1 into scratch RAM, then read back. FCTL
// is never written, so no flash is erased or programmed.
func TestHardwareBurstStagingRAMOnly(t *testing.T) {
	if os.Getenv("HWLAB_CC_NATIVE_DEBUG") != "1" {
		t.Skip("set HWLAB_CC_NATIVE_DEBUG=1 to run the RAM-only burst staging check (resets the target)")
	}
	d := openHW(t, Config{Access: ResetDebugSession, Timeout: 30 * time.Second, Permissions: Permissions{
		CPUControl: true, TargetReset: true, MemoryAccess: true, MemoryWrite: true, DMAEnable: true}})
	if st, err := d.ReadAdapterState(); err != nil || st.TargetID != targetCC2530 {
		t.Skipf("no CC2530 behind the debugger: %+v, %v", st, err)
	}
	if _, err := d.AttachReset(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := d.ResetHalt(); err != nil {
			t.Errorf("final reset: %v", err)
			return
		}
		if _, err := d.EnableDMAAfterReset(); err != nil {
			t.Errorf("enable DMA: %v", err)
		}
		if _, err := d.Resume(); err != nil {
			t.Errorf("resume: %v", err)
		}
	}()
	if _, err := d.EnableDMAAfterReset(); err != nil {
		t.Fatal(err)
	}
	err := d.do(nil, func(dl *deadline) error {
		if err := d.flashPrep(dl); err != nil {
			return err
		}
		for _, n := range []int{1, 2, 7, 64, 255, 256, 1000, burstMax} {
			if err := d.xput(scratchDesc+4, lenBytes(n), dl); err != nil {
				return err
			}
			data := make([]byte, n)
			for i := range data {
				data[i] = byte(i*7 + n)
			}
			if err := d.sfrPut(sfrDMAARM, 0x02, dl); err != nil {
				return err
			}
			if err := d.burstWrite(data, dl); err != nil {
				return err
			}
			if err := d.dmaWait(0x02, dl); err != nil {
				return err
			}
			got, err := d.xget(scratchBuf, n, false, dl)
			if err != nil {
				return err
			}
			if string(got) != string(data) {
				t.Errorf("burst of %d bytes: scratch RAM differs from the data sent", n)
			} else {
				t.Logf("burst of %d bytes staged correctly", n)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
