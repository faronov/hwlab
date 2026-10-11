//go:build hardware

package espusbjtag

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestHWFlashAcceptance runs the controlled program / verify / boot / debug /
// restore cycle on a REAL ESP32-C6. It rewrites the first flash sector (the
// second-stage bootloader slot) with a tiny RAM-only image and then restores
// the verified full backup. It is skipped unless
//
//	HWLAB_ESPJTAG_HW=1 HWLAB_ESPJTAG_FLASH=1
//	HWLAB_ESPJTAG_BACKUP_DIR=<local directory for the fresh full backup>
//	HWLAB_ESPJTAG_FW=<accept-fw/build/accept.bin>   (accept.sym next to it)
//
// are set. HWLAB_ESPJTAG_EVIDENCE=<file> receives sanitized JSON evidence.
func TestHWFlashAcceptance(t *testing.T) {
	if os.Getenv("HWLAB_ESPJTAG_FLASH") != "1" {
		t.Skip("set HWLAB_ESPJTAG_FLASH=1 (this test programs flash)")
	}
	bdir, fw := os.Getenv("HWLAB_ESPJTAG_BACKUP_DIR"), os.Getenv("HWLAB_ESPJTAG_FW")
	if bdir == "" || fw == "" {
		t.Fatal("HWLAB_ESPJTAG_BACKUP_DIR and HWLAB_ESPJTAG_FW are required")
	}
	syms := loadSyms(t, strings.TrimSuffix(fw, ".bin")+".sym")
	counter, tick := syms["hwlab_counter"], syms["hwlab_tick"]
	scratch := syms["hwlab_scratch"]
	if counter == 0 || tick == 0 || scratch == 0 {
		t.Fatalf("symbols missing: %v", syms)
	}
	ev := map[string]any{"image": filepath.Base(fw)}
	var backup string
	var backupBytes []byte
	var backupSum string
	defer func() {
		if p := os.Getenv("HWLAB_ESPJTAG_EVIDENCE"); p != "" {
			b, _ := json.MarshalIndent(ev, "", "  ")
			_ = os.WriteFile(p, b, 0o644)
		}
	}()

	h := hwOpen(t, map[string]any{"allow_flash": true, "allow_erase": true, "allow_reset": true, "allow_memory_write": true})
	prot := map[string]any{"confirm_protected": "modify-protected-region"}
	with := func(m map[string]any) map[string]any {
		o := map[string]any{"backup": backup}
		for k, v := range prot {
			o[k] = v
		}
		for k, v := range m {
			o[k] = v
		}
		return o
	}

	// ---- 0. fresh verified full backup (two identical reads) ----
	br := h.call(t, "FlashProgramming", "backup", map[string]any{"dir": bdir})
	if br["reads_identical"] != true {
		t.Fatalf("backup reads differ: %v", br)
	}
	backup = br["backup_path"].(string)
	t.Logf("BACKUP: %s sha256=%v", backup, br["sha256"])
	var err error
	if backupBytes, err = os.ReadFile(backup); err != nil {
		t.Fatal(err)
	}
	backupSum = sum(backupBytes)
	ev["backup_sha256"], ev["backup_bytes"], ev["backup_read_seconds"], ev["backup_reads_identical"] = backupSum, len(backupBytes), br["read_seconds"], true

	// ---- 1. identity and geometry ----
	id := h.call(t, "TargetIdentify", "identify", nil)
	t.Logf("identify: targets=%v", id["targets"])
	info := h.call(t, "FlashProgramming", "info", nil)
	fl := info["flash"].(map[string]any)
	ev["flash"] = map[string]any{"jedec": fl["jedec_id"], "manufacturer": fl["manufacturer"], "size_bytes": fl["size_bytes"], "sector": fl["sector_size"], "block": fl["block_size"], "page": fl["page_size"], "sfdp": fl["sfdp_revision"]}
	if int(fl["size_bytes"].(float64)) != len(backupBytes) {
		t.Fatalf("backup size %d != flash size %v", len(backupBytes), fl["size_bytes"])
	}
	readFlash := func(addr, n int) []byte {
		r := h.call(t, "FlashProgramming", "read", map[string]any{"address": addr, "length": n})
		b, err := hex.DecodeString(r["hex"].(string))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if got := readFlash(0, 4096); !bytes.Equal(got, backupBytes[:4096]) {
		t.Fatal("flash sector 0 does not match the backup before the test: take a new backup")
	}
	origSector0 := backupBytes[:4096]

	restore := func(reset string) map[string]any {
		return h.call(t, "FlashProgramming", "restore", map[string]any{"path": backup, "confirm": "restore-flash", "confirm_protected": "modify-protected-region", "reset": reset})
	}
	programmed, restored := false, false
	t.Cleanup(func() {
		if programmed && !restored {
			t.Log("EMERGENCY RESTORE of the original flash after a failed run")
			if _, err := h.try("FlashProgramming", "restore", map[string]any{"path": backup, "confirm": "restore-flash", "confirm_protected": "modify-protected-region", "reset": "run"}); err != nil {
				t.Errorf("emergency restore FAILED, restore by hand from %s: %v", backup, err)
			}
		}
	})

	// ---- 2. error paths: nothing may be written ----
	dir := t.TempDir()
	img, err := os.ReadFile(fw)
	if err != nil {
		t.Fatal(err)
	}
	file := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	bad := map[string]map[string]any{
		"no backup":        {"path": fw, "address": 0, "confirm": "program-flash", "confirm_protected": "modify-protected-region"},
		"no token":         with(map[string]any{"path": fw, "address": 0}),
		"wrong token":      with(map[string]any{"path": fw, "address": 0, "confirm": "yes"}),
		"no protect token": {"path": fw, "address": 0, "confirm": "program-flash", "backup": backup},
		"outside flash":    with(map[string]any{"path": fw, "address": len(backupBytes) - 16, "confirm": "program-flash"}),
		"beyond flash":     with(map[string]any{"path": fw, "address": 0x10000000, "confirm": "program-flash"}),
		"odd address":      with(map[string]any{"path": fw, "address": 3, "confirm": "program-flash"}),
		"truncated image":  with(map[string]any{"path": file("trunc.bin", img[:len(img)-40]), "address": 0, "confirm": "program-flash"}),
		"corrupt checksum": with(map[string]any{"path": file("corrupt.bin", func() []byte { c := append([]byte(nil), img...); c[len(c)-1] ^= 0xFF; return c }()), "address": 0, "confirm": "program-flash"}),
		"wrong chip":       with(map[string]any{"path": file("chip.bin", func() []byte { c := append([]byte(nil), img...); c[12] = 0x09; return c }()), "address": 0, "confirm": "program-flash"}),
		// Deliberately lacks the backup and protected-region tokens, so a regression of the span
		// check still cannot write anything: later gates refuse it too.
		"spans partitions": {"path": file("span.bin", append([]byte{0xAA}, make([]byte, 0x1FF)...)), "raw": true, "address": 0x3EFF00, "confirm": "program-flash"},
		"raw without flag": with(map[string]any{"path": file("raw.bin", make([]byte, 64)), "address": 0x300000, "confirm": "program-flash"}),
	}
	errs := map[string]string{}
	for name, a := range bad {
		if _, err := h.try("FlashProgramming", "program", a); err == nil {
			t.Fatalf("error path %q was accepted", name)
		} else {
			errs[name] = err.Error()
		}
	}
	if _, err := h.try("FlashProgramming", "program", with(map[string]any{"path": file("empty.bin", nil), "address": 0, "confirm": "program-flash"})); err == nil {
		t.Fatal("zero-length program accepted")
	}
	for name, a := range map[string]map[string]any{
		"erase no mode":      {"address": 0x300000, "length": 4096, "confirm": "erase-flash-region", "backup": backup},
		"erase zero length":  {"mode": "region", "address": 0x300000, "length": 0, "confirm": "erase-flash-region", "backup": backup},
		"erase outside":      {"mode": "region", "address": len(backupBytes), "length": 4096, "confirm": "erase-flash-region", "backup": backup},
		"erase unaligned":    {"mode": "region", "address": 0x300010, "length": 4096, "confirm": "erase-flash-region", "backup": backup},
		"erase chip no gate": {"mode": "chip", "confirm": "erase-entire-chip", "backup": backup},
		"erase bootloader":   {"mode": "region", "address": 0, "length": 4096, "confirm": "erase-flash-region", "backup": backup},
	} {
		if _, err := h.try("FlashProgramming", "erase", a); err == nil {
			t.Fatalf("erase error path %q was accepted", name)
		} else {
			errs[name] = err.Error()
		}
	}
	if got := readFlash(0, 4096); !bytes.Equal(got, origSector0) {
		t.Fatal("an error path modified flash")
	}
	if got := readFlash(0x300000, 4096); !bytes.Equal(got, backupBytes[0x300000:0x301000]) {
		t.Fatal("an error path modified flash at 0x300000")
	}
	ev["error_paths_rejected"] = len(errs) + 1
	ev["error_paths_flash_unchanged"] = true
	t.Logf("%d error paths rejected, flash unchanged", len(errs)+1)

	// ---- 3. verify against a corrupted image must report a mismatch ----
	if _, err := h.try("FlashProgramming", "verify", map[string]any{"path": file("c.bin", append([]byte{^origSector0[0]}, origSector0[1:64]...)), "address": 0}); err == nil || !strings.Contains(err.Error(), "differ") {
		t.Fatalf("verify did not report the corrupted image: %v", err)
	}
	ev["corrupted_verify_detected"] = true

	// ---- 4. program the acceptance image ----
	programmed = true
	pr, perr := h.try("FlashProgramming", "program", with(map[string]any{"path": fw, "address": 0, "confirm": "program-flash", "reset": "none"}))
	if perr != nil {
		t.Fatalf("program: %v", perr)
	}
	t.Logf("program: %v", pr)
	ev["program"] = map[string]any{"bytes": pr["length"], "verified": pr["verified"], "program_ms": pr["program_ms"], "throughput_kb_s": pr["throughput_kb_s"], "image_sha256": pr["image_sha256"], "write_report": pr["write_report"]}
	if pr["verified"] != true {
		t.Fatalf("not verified: %v", pr)
	}
	v := h.call(t, "FlashProgramming", "verify", map[string]any{"path": fw, "address": 0})
	if v["match"] != true {
		t.Fatalf("verify after program: %v", v)
	}
	if got := readFlash(0, 4096); !bytes.Equal(got[len(img):], origSector0[len(img):]) {
		t.Fatal("bytes of sector 0 behind the image were not preserved")
	}
	ev["verify_after_program"] = true
	ev["sector0_tail_preserved"] = true

	// ---- 5. boot the new image: hart reset via debug module, then a SoC reset ----
	boot := func(how string) {
		t.Helper()
		if how == "soc" {
			h.call(t, "DebugProbe", "reset", map[string]any{"kind": "soc", "confirm": "reset-soc", "halt": false, "hart": 0})
		} else {
			h.call(t, "DebugProbe", "reset", map[string]any{"kind": "system", "halt": false, "hart": 0})
		}
		time.Sleep(700 * time.Millisecond)
	}
	counterNow := func() uint32 {
		b := h.read(t, counter, 4)
		return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
	}
	booted := false
	var how string
	for _, how = range []string{"system", "soc"} {
		boot(how)
		c1 := counterNow()
		time.Sleep(200 * time.Millisecond)
		c2 := counterNow()
		t.Logf("after %s reset: counter %d -> %d", how, c1, c2)
		if c2 > c1 && c1 != 0 {
			booted = true
			ev["boot"] = map[string]any{"reset_used": how, "counter_before": c1, "counter_after_200ms": c2, "proof": "SRAM counter at the image's hwlab_counter symbol increments: the ROM loaded and started the programmed image"}
			break
		}
	}
	if !booted {
		t.Fatal("the programmed image did not boot (counter does not advance)")
	}
	ev["serial_banner"] = readBanner(t)

	// ---- 6. debug it ----
	h.halt(t)
	regs := h.call(t, "DebugProbe", "read_registers", map[string]any{"keep_halted": true})["registers"].(map[string]any)
	pc := parseHex(t, regs["pc"])
	if pc < 0x40850000 || pc >= 0x40854000 {
		t.Fatalf("pc %#x is outside the programmed image", pc)
	}
	// SRAM write/restore on the image's own scratch array.
	orig := h.read(t, scratch, 64)
	pat := make([]byte, 64)
	for i := range pat {
		pat[i] = byte(0xA0 + i)
	}
	h.write(t, scratch, pat)
	if !bytes.Equal(h.read(t, scratch, 64), pat) {
		t.Fatal("SRAM write did not read back")
	}
	h.write(t, scratch, orig)
	if !bytes.Equal(h.read(t, scratch, 64), orig) {
		t.Fatal("SRAM restore mismatch")
	}
	// single step: pc stays in the loop, counter-visible progress
	steps := h.call(t, "DebugProbe", "step", map[string]any{"count": 100})
	t.Logf("100 steps: %v", steps)
	// breakpoint on the known hot function
	hits := 0
	for round := 0; round < 5; round++ {
		h.halt(t)
		h.call(t, "DebugProbe", "set_breakpoint", map[string]any{"address": hexAddr(tick)})
		h.call(t, "DebugProbe", "resume", map[string]any{"hart": 0})
		w, err := h.try("DebugProbe", "wait_halt", map[string]any{"wait_ms": 2000})
		if err != nil {
			t.Fatalf("round %d: breakpoint at hwlab_tick never hit: %v", round, err)
		}
		if w["cause_name"] != "trigger" || parseHex(t, w["pc"]) != tick {
			t.Fatalf("round %d: %v", round, w)
		}
		hits++
		h.call(t, "DebugProbe", "clear_breakpoint", map[string]any{"address": hexAddr(tick)})
	}
	ev["debug_of_programmed_image"] = map[string]any{"pc_in_image": true, "sram_write_restore_bytes": 64, "steps": 100, "breakpoint_hits_at_hwlab_tick": hits}
	h.call(t, "DebugProbe", "resume", map[string]any{"hart": 0})
	c1 := counterNow()
	time.Sleep(100 * time.Millisecond)
	if counterNow() <= c1 {
		t.Fatal("image did not keep running after the debug session")
	}

	// ---- 7. restore the original firmware ----
	rr := restore("none")
	t.Logf("restore: %v", rr)
	restored = true
	ev["restore"] = map[string]any{"verified": rr["verified"], "flash_sha256_after": rr["flash_sha256_after"], "elapsed_ms": rr["elapsed_ms"], "write_report": rr["write_report"]}
	if rr["flash_sha256_after"] != backupSum {
		t.Fatalf("restore hash %v != backup %s", rr["flash_sha256_after"], backupSum)
	}
	// independent full read-back into a local artifact
	out := filepath.Join(t.TempDir(), "restore-check.bin")
	fr := h.call(t, "FlashProgramming", "read", map[string]any{"address": 0, "length": len(backupBytes), "path": out})
	if fr["sha256"] != backupSum {
		t.Fatalf("flash after restore sha256 %v != backup %s", fr["sha256"], backupSum)
	}
	ev["restore_full_readback_sha256_equal"] = true

	// ---- 8. original firmware behaviour returns ----
	h.call(t, "DebugProbe", "reset", map[string]any{"kind": "soc", "confirm": "reset-soc", "halt": false, "hart": 0})
	time.Sleep(3 * time.Second)
	inFlash := 0
	var samples []string
	for i := 0; i < 12; i++ {
		h.halt(t)
		p := parseHex(t, h.call(t, "DebugProbe", "read_registers", map[string]any{"keep_halted": true})["registers"].(map[string]any)["pc"])
		samples = append(samples, hexAddr(p))
		if p >= 0x42000000 && p < 0x44000000 {
			inFlash++
		}
		h.call(t, "DebugProbe", "resume", map[string]any{"hart": 0})
		time.Sleep(150 * time.Millisecond)
	}
	t.Logf("pc samples after restore: %v (%d in flash-mapped code)", samples, inFlash)
	if inFlash == 0 {
		t.Fatalf("the original firmware does not seem to run: %v", samples)
	}
	ev["original_firmware_back"] = map[string]any{"pc_samples_in_flash_mapped_code": fmt.Sprintf("%d of %d", inFlash, len(samples)), "acceptance_image_gone": true}
	ev["result"] = "program+verify+boot+debug+restore all succeeded"
}

func loadSyms(t *testing.T, path string) map[string]uint32 {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]uint32{}
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) == 3 {
			var a uint32
			if _, err := fmt.Sscanf(f[0], "%x", &a); err == nil {
				m[f[2]] = a
			}
		}
	}
	return m
}

// readBanner reads the image's banner from the CDC port, if it can be opened.
func readBanner(t *testing.T) string {
	t.Helper()
	m, _ := filepath.Glob("/dev/serial/by-id/*Espressif*")
	if len(m) == 0 {
		return "CDC port not found"
	}
	old, err := exec.Command("stty", "-F", m[0], "-g").Output()
	if err != nil {
		return "cannot save CDC terminal state: " + err.Error()
	}
	if err := exec.Command("stty", "-F", m[0], "raw", "-echo", "-hupcl").Run(); err != nil {
		return "cannot configure CDC terminal: " + err.Error()
	}
	defer func() {
		if err := exec.Command("stty", "-F", m[0], strings.TrimSpace(string(old))).Run(); err != nil {
			t.Errorf("restore CDC terminal state: %v", err)
		}
	}()
	f, err := os.OpenFile(m[0], os.O_RDONLY, 0)
	if err != nil {
		return "CDC port not readable: " + err.Error()
	}
	defer f.Close()
	_ = f.SetReadDeadline(time.Now().Add(1500 * time.Millisecond))
	var got bytes.Buffer
	buf := make([]byte, 256)
	for {
		n, err := f.Read(buf)
		got.Write(buf[:n])
		if err != nil || bytes.Contains(got.Bytes(), []byte("HWLAB-ESP32C6-ACCEPT-1")) {
			break
		}
	}
	s := got.String()
	if i := strings.Index(s, "HWLAB-ESP32C6-ACCEPT-1"); i >= 0 {
		s = s[i:]
		if len(s) > 40 {
			s = s[:40]
		}
		return strings.TrimSpace(s)
	}
	return fmt.Sprintf("no banner seen (%d bytes read)", got.Len())
}
