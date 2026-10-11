package stlinkplugin

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/faronov/hwlab/internal/cortexm"
	"github.com/faronov/hwlab/plugins/stlink/stm32"
)

// Addresses in the hwlab-owned acceptance firmware (see acceptanceF1).
const (
	accReset   = 0x08000040
	accWork    = 0x08000060
	accCounter = 0x20000100
)

// acceptanceF1 returns a 128-byte Cortex-M3 image: a vector table (SP at the
// end of a 20 KiB SRAM) and a reset handler that zeroes the word at
// accCounter and then calls work() forever; work() increments the counter.
// The call is a 32-bit BL so single-stepping sees 16- and 32-bit
// instructions. It touches no peripheral, so SWD stays usable. The Thumb
// words were checked with an independent disassembler:
//
//	0x40 ldr r0, [pc, #0x34]  ; =accCounter (literal at 0x78)
//	0x42 movs r1, #0
//	0x44 str r1, [r0]
//	0x46 bl work              ; 0x60
//	0x4a b 0x46
//	0x60 ldr r2, [r0]
//	0x62 adds r2, #1
//	0x64 str r2, [r0]
//	0x66 bx lr
//	0x70 b .                  ; default handler
func acceptanceF1() []byte {
	img := make([]byte, 0x80)
	binary.LittleEndian.PutUint32(img[0:], 0x20005000)
	binary.LittleEndian.PutUint32(img[4:], accReset|1)
	for i := 2; i < 16; i++ {
		binary.LittleEndian.PutUint32(img[4*i:], 0x08000071)
	}
	for off, v := range map[int]uint16{
		0x40: 0x480D, 0x42: 0x2100, 0x44: 0x6001, 0x46: 0xF000, 0x48: 0xF80B, 0x4A: 0xE7FC,
		0x60: 0x6802, 0x62: 0x3201, 0x64: 0x6002, 0x66: 0x4770, 0x70: 0xE7FE,
	} {
		binary.LittleEndian.PutUint16(img[off:], v)
	}
	binary.LittleEndian.PutUint32(img[0x78:], accCounter)
	return img
}

func f1Rig(t *testing.T, cfg map[string]any) (*rig, string, *stm32.SimF1) {
	t.Helper()
	f := stm32.NewSimF1(128)
	f.SizeReg = 0xFFFFFFFF // like the attached part: blank, so the size is probed
	r := newRig(t, map[string]*cortexm.Sim{"SIMF1": f.Sim})
	id := idOf("SIMF1")
	if err := r.OpenDevice(context.Background(), id, cfg); err != nil {
		t.Fatal(err)
	}
	return r, id, f
}

func TestF1FlashInfo(t *testing.T) {
	r, id, _ := f1Rig(t, nil)
	m := r.must(t, id, flash, "info", nil)
	if m["supported"] != true || m["family"] != "STM32F1" || m["flash_kb"] != float64(128) || m["flash_size_source"] != "probed" ||
		m["sector_count"] != float64(128) || m["page_size"] != float64(1024) || m["program_granularity_bytes"] != float64(2) || m["sram_kb_readable"] != float64(20) {
		t.Fatalf("%v", m)
	}
	p := m["protection"].(map[string]any)
	if p["read_only"] != true || p["rdp_level"] != float64(0) || p["flash_obr"] == nil || p["readout_protection_active"] != false {
		t.Fatalf("%v", p)
	}
}

func TestF1AcceptanceCycleOnSimulator(t *testing.T) {
	r, id, f := f1Rig(t, allowAll)
	for i := range f.Flash {
		f.Flash[i] = byte(i*7 + 3)
	}
	orig := append([]byte(nil), f.Flash...)
	bk := r.must(t, id, flash, "backup", map[string]any{"dir": t.TempDir()})
	img := writeFile(t, "acc.bin", acceptanceF1())

	// Erase page 0 on its own, then program: the erased page passes the backup gate.
	em := r.must(t, id, flash, "erase", map[string]any{"mode": "region", "address": "0x08000000", "length": 1024.0, "confirm": tokEraseReg, "backup": bk["path"]})
	if em["verified_erased"] != true || f.Erases != 1 {
		t.Fatalf("%v", em)
	}
	pm := r.must(t, id, flash, "program", map[string]any{"path": img, "address": "0x08000000", "confirm": tokProgram, "backup": bk["path"]})
	if pm["verified"] != true || pm["sectors_erased"] != float64(0) || f.Erases != 1 {
		t.Fatalf("%v", pm)
	}
	if string(f.Flash[:0x80]) != string(acceptanceF1()) || string(f.Flash[0x400:]) != string(orig[0x400:]) {
		t.Fatal("page 0 must hold the image and everything else the original")
	}
	if f.WordWriteFaults != 0 {
		t.Fatal("the F1 path must never store words into flash")
	}
	// Restore from the backup brings the original back.
	rm := r.must(t, id, flash, "restore", map[string]any{"path": bk["path"], "confirm": tokRestore})
	if rm["whole_flash_matches_backup"] != true || string(f.Flash) != string(orig) {
		t.Fatalf("%v", rm)
	}
}

func TestF1BackupGateRejectsChangedFlash(t *testing.T) {
	r, id, f := f1Rig(t, allowAll)
	for i := range f.Flash {
		f.Flash[i] = byte(i)
	}
	bk := r.must(t, id, flash, "backup", map[string]any{"dir": t.TempDir()})
	f.Flash[5] ^= 0xFF // changed but not erased: the backup no longer describes page 0
	r.fail(t, codes.FailedPrecondition, "no longer matches", id, flash, "erase",
		map[string]any{"mode": "region", "address": "0x08000000", "length": 1024.0, "confirm": tokEraseReg, "backup": bk["path"]})
	if f.Erases != 0 {
		t.Fatal("erased despite a stale backup")
	}
}

func TestF1ProtectedFlashIsRefused(t *testing.T) {
	r, id, f := f1Rig(t, allowAll)
	bk := r.must(t, id, flash, "backup", map[string]any{"dir": t.TempDir()})
	f.RDP = 0x00 // pending/active readout protection
	r.fail(t, codes.PermissionDenied, "readout protection", id, flash, "erase",
		map[string]any{"mode": "region", "address": "0x08000000", "length": 1024.0, "confirm": tokEraseReg, "backup": bk["path"]})
	f.RDP, f.WRPR = 0xA5, 0xFFFFFFFE
	r.fail(t, codes.PermissionDenied, "write-protected", id, flash, "erase",
		map[string]any{"mode": "region", "address": "0x08000000", "length": 1024.0, "confirm": tokEraseReg, "backup": bk["path"]})
	if f.Erases != 0 || f.Programs != 0 {
		t.Fatal("a protected part was written")
	}
}

// examples/stlink-stm32f1-accept.bin is the acceptanceF1 image so the recorded
// example session can program it; HWLAB_UPDATE_DOCS=1 regenerates it.
func TestAcceptanceImageFileIsCurrent(t *testing.T) {
	p := filepath.Join("..", "..", "examples", "stlink-stm32f1-accept.bin")
	if os.Getenv("HWLAB_UPDATE_DOCS") == "1" {
		if err := os.WriteFile(p, acceptanceF1(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, acceptanceF1()) {
		t.Fatal("examples/stlink-stm32f1-accept.bin is stale: run with HWLAB_UPDATE_DOCS=1")
	}
}
