//go:build hardware

package stlinkplugin

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestHWFlashAcceptance is the destructive cycle on a real STM32F1: backup
// twice, erase one page, program a hwlab-owned image, boot it, debug it, then
// restore the original and prove it by hash. It only runs with
//
//	HWLAB_STLINK_HW=1 HWLAB_STLINK_FLASH=1
//
// and refuses to start unless the chip identifies itself as an F1 whose
// protection is clear. If any step fails the original is restored from the
// backup. Backups stay in HWLAB_STLINK_BACKUP_DIR (default
// ~/.hwlab/artifacts/stlink-flash), never in the repository.
func TestHWFlashAcceptance(t *testing.T) {
	if os.Getenv("HWLAB_STLINK_FLASH") != "1" {
		t.Skip("set HWLAB_STLINK_FLASH=1 to run the destructive flash cycle")
	}
	h := hwOpen(t, map[string]any{"allow_memory_write": true, "allow_reset": true, "allow_flash": true, "allow_erase": true})
	info := h.call(t, "FlashProgramming", "info", nil)
	if info["supported"] != true || info["family"] != "STM32F1" {
		t.Skipf("not a supported STM32F1: %v", info)
	}
	if p := info["protection"].(map[string]any); p["readout_protection_active"] != false || p["write_protected_sectors"] != nil && fmt.Sprint(p["write_protected_sectors"]) != "[]" {
		t.Fatalf("protected part, refusing: %v", p)
	}
	dir := os.Getenv("HWLAB_STLINK_BACKUP_DIR")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".hwlab", "artifacts", "stlink-flash")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	// 1-3. Behaviour of the original firmware, then two full reads that must match.
	origPC := h.resetHalt(t, "system")
	origVec := parseHex(t, h.call(t, "DebugProbe", "reset", map[string]any{"kind": "system", "halt": true})["reset_vector"])
	origSamples := h.samplePCs(t, 6)
	t.Logf("original: reset PC 0x%08x, reset vector 0x%08x, running PC samples %#x", origPC, origVec, origSamples)
	bk := h.call(t, "FlashProgramming", "backup", map[string]any{"dir": dir})
	if bk["identical_reads"] != true || bk["sha256"] != bk["second_read_sha256"] {
		t.Fatalf("backup reads differ: %v", bk)
	}
	if want := os.Getenv("HWLAB_STLINK_EXPECT_SHA"); want != "" && bk["sha256"] != want {
		t.Fatalf("flash SHA-256 %v is not the expected %s: refusing to continue", bk["sha256"], want)
	}
	bkPath, bkSHA := bk["path"].(string), bk["sha256"].(string)
	t.Logf("backup %s sha256=%s second=%s read %.2fs", bkPath, bkSHA, bk["second_read_sha256"], bk["read_seconds"])
	orig, _ := os.ReadFile(bkPath)
	if sha256Hex(orig) != bkSHA {
		t.Fatal("backup file does not hash to its manifest")
	}

	restored := false
	restore := func() map[string]any {
		r := h.call(t, "FlashProgramming", "restore", map[string]any{"path": bkPath, "confirm": tokRestore, "reset": "none"})
		restored = true
		return r
	}
	t.Cleanup(func() {
		if restored {
			return
		}
		t.Log("cycle did not finish: restoring the original firmware from the backup")
		if _, err := h.try("FlashProgramming", "restore", map[string]any{"path": bkPath, "confirm": tokRestore, "reset": "run"}); err != nil {
			t.Errorf("EMERGENCY RESTORE FAILED: %v (backup %s)", err, bkPath)
		}
	})

	// 4. Safety refusals before anything changes.
	for name, c := range map[string]struct {
		m    string
		a    map[string]any
		code codes.Code
	}{
		"no token":    {"erase", map[string]any{"mode": "region", "address": "0x08000000", "length": 1024.0, "backup": bkPath}, codes.PermissionDenied},
		"no backup":   {"erase", map[string]any{"mode": "region", "address": "0x08000000", "length": 1024.0, "confirm": tokEraseReg}, 0},
		"unaligned":   {"erase", map[string]any{"mode": "region", "address": "0x08000100", "length": 1024.0, "confirm": tokEraseReg, "backup": bkPath}, codes.InvalidArgument},
		"chip no tok": {"erase", map[string]any{"mode": "chip", "confirm": tokEraseReg, "backup": bkPath}, 0},
		"past end":    {"erase", map[string]any{"mode": "region", "address": "0x08020000", "length": 1024.0, "confirm": tokEraseReg, "backup": bkPath}, codes.OutOfRange},
	} {
		_, err := h.try("FlashProgramming", c.m, c.a)
		if err == nil || (c.code != 0 && status.Code(err) != c.code) {
			t.Fatalf("%s: want a refusal (%v), got %v", name, c.code, err)
		}
	}
	if got := h.read(t, 0x08000000, 1024); !bytes.Equal(got, orig[:1024]) {
		t.Fatal("a refused operation changed page 0")
	}
	if _, err := h.try("FlashProgramming", "erase", map[string]any{"mode": "chip", "confirm": tokEraseChip, "backup": bkPath}); err == nil {
		t.Fatal("full-chip erase must be refused without allow_chip_erase")
	}

	// 5. Erase exactly page 0 and prove it is blank.
	er := h.call(t, "FlashProgramming", "erase", map[string]any{"mode": "region", "address": "0x08000000", "length": 1024.0, "confirm": tokEraseReg, "backup": bkPath})
	t.Logf("erase page 0: %v", er["erased_sectors"])
	if er["verified_erased"] != true || !isBlank(h.read(t, 0x08000000, 1024)) {
		t.Fatalf("page 0 not erased: %v", er)
	}
	if !bytes.Equal(h.read(t, 0x08000400, 4096), orig[0x400:0x1400]) {
		t.Fatal("the neighbouring pages changed")
	}

	// 6-7. Program the acceptance image, read back byte for byte.
	img := acceptanceF1()
	imgPath := filepath.Join(t.TempDir(), "acc.bin")
	if err := os.WriteFile(imgPath, img, 0o600); err != nil {
		t.Fatal(err)
	}
	pm := h.call(t, "FlashProgramming", "program", map[string]any{"path": imgPath, "address": "0x08000000", "confirm": tokProgram, "backup": bkPath, "reset": "none"})
	t.Logf("program: %v bytes in %v ms, sha256 %v", pm["length"], pm["elapsed_ms"], pm["sha256"])
	if pm["verified"] != true || !bytes.Equal(h.read(t, 0x08000000, len(img)), img) {
		t.Fatalf("image not in flash: %v", pm)
	}
	if !bytes.Equal(h.read(t, 0x08000400, 4096), orig[0x400:0x1400]) {
		t.Fatal("programming changed another page")
	}
	if v := h.call(t, "FlashProgramming", "verify", map[string]any{"path": imgPath, "address": "0x08000000"}); v["verified"] != true {
		t.Fatalf("%v", v)
	}

	// 8-9. Reset and prove the image runs.
	rr := h.call(t, "DebugProbe", "reset", map[string]any{"kind": "system", "halt": true})
	if pc := parseHex(t, rr["pc"]); pc != accReset&^1 {
		t.Fatalf("reset PC 0x%08x, want 0x%08x", pc, uint32(accReset))
	}
	h.resume(t)
	time.Sleep(150 * time.Millisecond)
	h.halt(t)
	c1 := h.counter(t)
	pc1 := h.pc(t)
	h.resume(t)
	time.Sleep(150 * time.Millisecond)
	h.halt(t)
	c2 := h.counter(t)
	if c1 == 0 || c1 == c2 {
		t.Fatalf("counter did not advance: %d -> %d", c1, c2)
	}
	if pc1 < accReset || pc1 >= 0x08000070 {
		t.Fatalf("PC 0x%08x is outside the acceptance firmware", pc1)
	}
	t.Logf("acceptance firmware runs: counter %d -> %d in 150 ms, PC 0x%08x", c1, c2, pc1)

	// 10-11. Registers and SRAM scratch write/restore while halted.
	regs := h.call(t, "DebugProbe", "read_registers", map[string]any{"keep_halted": true})["registers"].(map[string]any)
	t.Logf("registers: pc=%v sp=%v xpsr=%v msp=%v control=%v", regs["pc"], regs["sp"], regs["xpsr"], regs["msp"], regs["control"])
	sc := h.scratch(t, 64)
	save := h.read(t, sc, 64)
	pat := make([]byte, 64)
	for i := range pat {
		pat[i] = byte(0xA0 + i)
	}
	h.write(t, sc, pat)
	if !bytes.Equal(h.read(t, sc, 64), pat) {
		t.Fatal("SRAM pattern mismatch")
	}
	h.write(t, sc, save)
	if !bytes.Equal(h.read(t, sc, 64), save) {
		t.Fatal("SRAM not restored")
	}

	// 12. 100 single steps through the known loop; every PC must follow the
	// program order and the counter must grow once per work() call.
	h.resetHalt(t, "system")
	h.call(t, "DebugProbe", "step", map[string]any{"count": 3.0}) // ldr, movs, str
	if pc := h.pc(t); pc != 0x08000046 {
		t.Fatalf("PC 0x%08x after the 3 set-up instructions, want 0x08000046", pc)
	}
	loop := []uint32{0x08000060, 0x08000062, 0x08000064, 0x08000066, 0x0800004A, 0x08000046}
	steps, strs := 0, 0
	for round := 0; round < 4; round++ {
		r := h.call(t, "DebugProbe", "step", map[string]any{"count": 25.0})
		if r["halted"] != true || r["step_bit_restored"] != true {
			t.Fatalf("%v", r)
		}
		for _, s := range r["steps"].([]any) {
			m := s.(map[string]any)
			want := loop[steps%len(loop)]
			if after := parseHex(t, m["pc_after"]) &^ 1; after != want {
				t.Fatalf("step %d: PC 0x%08x, want 0x%08x (%v)", steps, after, want, m)
			}
			if parseHex(t, m["pc_before"])&^1 == 0x08000064 {
				strs++
			}
			steps++
		}
	}
	if got := h.counter(t); got != uint32(strs) {
		t.Fatalf("after %d steps the counter is %d, want %d (str executions)", steps, got, strs)
	}
	t.Logf("%d single steps (16- and 32-bit) followed the program order; counter == %d", steps, strs)

	// 13. Hardware breakpoint at work().
	var hits []uint32
	for round := 0; round < 5; round++ {
		sb := h.call(t, "DebugProbe", "set_breakpoint", map[string]any{"address": hexAddr(accWork), "run_wait_ms": 2000.0})
		if sb["hit"] != true || parseHex(t, sb["halted_pc"]) != accWork {
			t.Fatalf("round %d: %v", round, sb)
		}
		hits = append(hits, h.counter(t))
		h.call(t, "DebugProbe", "clear_breakpoint", map[string]any{"address": hexAddr(accWork)})
		h.call(t, "DebugProbe", "step", map[string]any{"count": 1.0}) // off the breakpoint address
	}
	for i := 1; i < len(hits); i++ {
		if hits[i] <= hits[i-1] {
			t.Fatalf("counter did not advance between hits: %v", hits)
		}
	}
	t.Logf("breakpoint at work() hit 5 times, counter at hits %v", hits)

	// 14. DWT write watchpoint on the counter word.
	wl := h.call(t, "DebugProbe", "list_watchpoints", nil)
	t.Logf("DWT: %v", wl)
	h.call(t, "DebugProbe", "set_watchpoint", map[string]any{"address": hexAddr(accCounter), "kind": "write", "mask_bits": 2.0})
	h.resume(t)
	w, err := h.try("DebugProbe", "wait_halt", map[string]any{"wait_ms": 2000.0})
	h.call(t, "DebugProbe", "clear_watchpoint", map[string]any{"all": true})
	if err != nil || w["watchpoint_hit"] != true {
		t.Fatalf("write watchpoint on the counter did not fire: %v %v", w, err)
	}
	t.Logf("write watchpoint fired: %v", w)

	// 15. Larger program for throughput, then 16-18. restore and prove it.
	big := make([]byte, 8192)
	for i := range big {
		big[i] = byte(i*131 + i>>8)
	}
	bigPath := filepath.Join(t.TempDir(), "big.bin")
	_ = os.WriteFile(bigPath, big, 0o600)
	bm := h.call(t, "FlashProgramming", "program", map[string]any{"path": bigPath, "address": "0x0801E000", "confirm": tokProgram, "backup": bkPath, "reset": "none"})
	t.Logf("THROUGHPUT flash program: %d B (8 pages erased+programmed) in %v ms = %.1f KB/s", len(big), bm["elapsed_ms"], float64(len(big))/1000/(bm["elapsed_ms"].(float64)/1000))
	if bm["verified"] != true {
		t.Fatalf("%v", bm)
	}
	t0 := time.Now()
	rm := restore()
	el := time.Since(t0)
	if rm["whole_flash_matches_backup"] != true || rm["sha256"] != bkSHA {
		t.Fatalf("%v", rm)
	}
	t.Logf("restore: %v erases, %v sectors programmed, %v ms (call %v)", rm["sectors_erased"], rm["sectors_programmed"], rm["elapsed_ms"], el)
	after := h.call(t, "FlashProgramming", "backup", map[string]any{"dir": t.TempDir()})
	if after["sha256"] != bkSHA || after["second_read_sha256"] != bkSHA {
		t.Fatalf("restored flash hashes to %v, original %s", after["sha256"], bkSHA)
	}
	t.Logf("restored whole-flash SHA-256 (two reads) == original %s", bkSHA)

	// 19-20. Original behaviour is back.
	pc := h.resetHalt(t, "system")
	vec := parseHex(t, h.call(t, "DebugProbe", "reset", map[string]any{"kind": "system", "halt": true})["reset_vector"])
	if pc != origPC || vec != origVec {
		t.Fatalf("after restore reset PC/vector 0x%08x/0x%08x, original 0x%08x/0x%08x", pc, vec, origPC, origVec)
	}
	samples := h.samplePCs(t, 6)
	t.Logf("restored running PC samples %#x (original %#x)", samples, origSamples)
	for _, s := range samples {
		if s < 0x08000100 || s >= 0x08020000 {
			t.Fatalf("PC 0x%08x is not in the original firmware", s)
		}
	}
	if h.call(t, "DebugProbe", "chip_info", nil)["state"].(map[string]any)["halted"] == true {
		h.resume(t)
	}
}

func sha256Hex(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func (h hw) pc(t *testing.T) uint32 {
	t.Helper()
	r := h.call(t, "DebugProbe", "read_registers", map[string]any{"keep_halted": true})
	return parseHex(t, r["registers"].(map[string]any)["pc"])
}

func (h hw) counter(t *testing.T) uint32 {
	t.Helper()
	return binary.LittleEndian.Uint32(h.read(t, accCounter, 4))
}

// samplePCs resets, lets the core run briefly and records the PC at n halts.
func (h hw) samplePCs(t *testing.T, n int) []uint32 {
	t.Helper()
	h.call(t, "DebugProbe", "reset", map[string]any{"kind": "system", "halt": false})
	var out []uint32
	for i := 0; i < n; i++ {
		time.Sleep(40 * time.Millisecond)
		h.halt(t)
		out = append(out, h.pc(t))
		h.resume(t)
	}
	return out
}
