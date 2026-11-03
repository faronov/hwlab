//go:build hardware

package espusbjtag

import (
	"bytes"
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/internal/riscv"
	"github.com/faronov/hwlab/plugins/espusbjtag/esp"
)

type riscvBusRead struct {
	addr  uint32
	words int
}

type busReads []riscvBusRead

func (r busReads) ops() []riscv.BusOp {
	var o []riscv.BusOp
	for _, x := range r {
		o = append(o, riscv.BusOp{Kind: riscv.BusRead, Addr: x.addr, Words: x.words})
	}
	return o
}

// ESP32-S3 flash over JTAG/SPI1, opt-in like the other S3 hardware tests:
//
//	HWLAB_ESPJTAG_HW=1 HWLAB_ESPJTAG_CHIP=s3 go test -tags hardware ./plugins/espusbjtag/ -run TestHWS3Flash -v
//
// TestHWS3FlashProbe is read-only (SPI1 register writes for the read
// commands, restored on close; no flash program/erase, no eFuse write).

// TestHWS3FlashProbe halts core 0, reads the eFuse security words, the
// watchdog state, the SPI1 registers and the flash JEDEC id / SFDP / status
// through SPI1, and resumes.
func TestHWS3FlashProbe(t *testing.T) {
	_, s := s3Rig(t)
	ctx := context.Background()
	c := s3Cores(t, s)[0]
	if _, err := c.Examine(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Halt(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := c.Resume(context.Background()); err != nil {
			t.Errorf("resume: %v", err)
		}
	}()
	bus := esp.XtensaBus{C: c}
	chip := &esp.ChipS3
	regs, err := bus.BusBatch(ctx, busReads{
		{chip.SPI1, 11}, {chip.SPI1 + 0x58, 4}, {chip.EfuseRepeat0, 5}, {0x600C4000, 2}, {0x600C4060, 1}, {chip.MMUTable, 4},
		{chip.SPI1 + 0x34, 1}, {0x60003000, 16}, {0x60003034, 1}, {0x60009000, 34}, {0x600080A0, 8},
	}.ops())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("SPI1 0x%08x..: %08x", chip.SPI1, regs[0])
	t.Logf("SPI1 W0..W3: %08x", regs[1])
	t.Logf("eFuse RD_REPEAT_DATA0..4: %08x", regs[2])
	t.Logf("EXTMEM DCACHE_CTRL/CTRL1 %08x, ICACHE_CTRL %08x, MMU[0..3] %08x", regs[3], regs[4], regs[5])
	t.Logf("SPI1 MISC %08x; SPI0 0x60003000..: %08x MISC %08x", regs[6], regs[7], regs[8])
	t.Logf("IO_MUX 0x60009000..: %08x", regs[9])
	t.Logf("RTC_CNTL 0x600080A0..: %08x", regs[10])
	sec, err := esp.ReadSecurity(ctx, bus, chip)
	t.Logf("security %+v err=%v writable=%v", sec, err, sec.CheckFlashWritable())
	t0 := time.Now()
	f, err := esp.OpenFlashChip(ctx, bus, chip)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = f.Close(context.Background())
		}
	}()
	id, err := f.JEDEC(ctx)
	t.Logf("JEDEC %x err=%v (%v)", id, err, time.Since(t0))
	if err != nil {
		t.Fatal(err)
	}
	info, err := f.ReadInfo(ctx)
	t.Logf("info %+v err=%v", info, err)
	if err != nil {
		t.Fatal(err)
	}
	t0 = time.Now()
	b, err := f.Read(ctx, 0, 4096)
	el := time.Since(t0)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("read 4 KiB at 0 in %v (%.1f KB/s), transactions %d: %x", el, 4.0/el.Seconds(), f.Transactions, b[:32])
	tb, err := f.Read(ctx, esp.PartitionTableOffset, 0xC00)
	if err != nil {
		t.Fatal(err)
	}
	parts, perr := esp.ParsePartitionTable(tb)
	t.Logf("partition table err=%v", perr)
	for _, p := range parts {
		t.Logf("  %-16s type %#02x sub %#02x offset 0x%06x size 0x%06x", p.Label, p.Type, p.Subtype, p.Offset, p.Size)
	}
	closed = true
	if err := f.Close(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := bus.BusBatch(ctx, busReads{{0x60009000, 34}, {chip.SPI1 + 0x04, 2}, {chip.SPI1 + 0x18, 5}}.ops())
	if err != nil {
		t.Fatal(err)
	}
	for i := range after[0] {
		if after[0][i] != regs[9][i] {
			t.Errorf("IO_MUX word %d not restored: %#x, was %#x", i, after[0][i], regs[9][i])
		}
	}
	if after[1][1] != regs[0][2] || after[2][0] != regs[0][6] || after[2][1] != regs[0][7] || after[2][2] != regs[0][8] {
		t.Errorf("SPI1 registers not restored: %08x %08x", after[1], after[2])
	}
}

// ---- plugin level ----

var s3FlashCfg = map[string]any{"allow_flash": true, "allow_erase": true}

// s3Resume resumes core 0 if a call left it halted and checks it runs the
// download-mode ROM again.
func s3Resume(t *testing.T, h hw) {
	t.Helper()
	if ci := h.call(t, "DebugProbe", "chip_info", nil); ci["hart_state"] == "halted" {
		h.call(t, "DebugProbe", "resume", nil)
	}
	s3Running(t, h)
}

// TestHWS3PluginFlashReadOnly: info, inline and file reads (throughput),
// verify (match and mismatch) and the error paths that must refuse before
// anything is erased: outside the flash, missing token, missing backup, an
// ESP32-C6 image, reset=run, unaligned erase. Nothing is written to flash.
func TestHWS3PluginFlashReadOnly(t *testing.T) {
	h := s3Plugin(t, s3FlashCfg)
	defer s3Resume(t, h)
	in := h.call(t, "FlashProgramming", "info", nil)
	t.Logf("info: flash %v\nsecurity %v\nchip %v\npartitions %v\nprotected %v\nmapped %v",
		in["flash"], in["security"], in["chip_evidence"], in["partitions"], in["protected_ranges"], in["mapped_window_check"])
	fl := in["flash"].(map[string]any)
	ev := in["chip_evidence"].(map[string]any)
	if ev["chip"] != "ESP32-S3" || ev["image_chip_id"] != "0x0009" || fl["jedec_id"] == "" || in["flash_writable"] != true {
		t.Fatalf("info %v", in)
	}
	size := int(fl["size_bytes"].(float64))
	s3Running(t, h)

	r := h.call(t, "FlashProgramming", "read", map[string]any{"address": 0.0, "length": 4096.0})
	raw, _ := hex.DecodeString(r["hex"].(string))
	if len(raw) != 4096 || raw[0] != esp.ImageMagic || raw[12] != 0x09 || raw[13] != 0 {
		t.Fatalf("bootloader header %x", raw[:16])
	}
	t.Logf("read 4 KiB inline: %v ms, %.1f KB/s", r["elapsed_ms"], r["throughput_kb_s"])
	dir := t.TempDir()
	p := filepath.Join(dir, "app64k.bin")
	r = h.call(t, "FlashProgramming", "read", map[string]any{"address": float64(0x10000), "length": 65536.0, "path": p})
	t.Logf("read 64 KiB to file: %v ms, %.1f KB/s sha256 %v", r["elapsed_ms"], r["throughput_kb_s"], r["sha256"])
	v := h.call(t, "FlashProgramming", "verify", map[string]any{"path": p, "address": float64(0x10000)})
	if v["match"] != true {
		t.Fatalf("verify %v", v)
	}
	b, _ := os.ReadFile(p)
	b[100] ^= 0x01
	bad := filepath.Join(dir, "bad.bin")
	if err := os.WriteFile(bad, b, 0o600); err != nil {
		t.Fatal(err)
	}
	want := func(c codes.Code, sub, method string, args map[string]any) {
		t.Helper()
		_, err := h.callErr("FlashProgramming", method, args)
		if status.Code(err) != c || !strings.Contains(err.Error(), sub) {
			t.Errorf("%s %v: want %v %q, got %v", method, args, c, sub, err)
		} else {
			t.Logf("refused as expected: %s: %v", method, err)
		}
	}
	want(codes.DataLoss, "differ", "verify", map[string]any{"path": bad, "address": float64(0x10000)})
	want(codes.OutOfRange, "", "read", map[string]any{"address": float64(size), "length": 4.0})
	want(codes.OutOfRange, "", "read", map[string]any{"address": float64(size - 1), "length": 2.0})
	s3Running(t, h)

	c6 := filepath.Join(dir, "c6.bin")
	if err := os.WriteFile(c6, appImageFor(esp.ChipIDESP32C6, 0x11), 0o600); err != nil {
		t.Fatal(err)
	}
	s3img := filepath.Join(dir, "s3.bin")
	if err := os.WriteFile(s3img, appImageFor(esp.ChipIDESP32S3, 0x11), 0o600); err != nil {
		t.Fatal(err)
	}
	last := float64(size - 4096)
	want(codes.PermissionDenied, "confirm", "program", map[string]any{"path": s3img, "address": last, "confirm": "nope", "backup": "x"})
	want(codes.PermissionDenied, "backup", "program", map[string]any{"path": s3img, "address": last, "confirm": "program-flash"})
	want(codes.FailedPrecondition, "not available on the ESP32-S3", "program", map[string]any{"path": s3img, "address": last, "confirm": "program-flash", "backup": "x", "reset": "run"})
	want(codes.FailedPrecondition, "chip id 0x000d", "program", map[string]any{"path": c6, "address": last, "confirm": "program-flash", "backup": "x"})
	want(codes.InvalidArgument, "multiples", "erase", map[string]any{"mode": "region", "address": last + 1, "length": 4096.0, "confirm": "erase-flash-region", "backup": "x"})
	want(codes.PermissionDenied, "backup", "erase", map[string]any{"mode": "region", "address": last, "length": 4096.0, "confirm": "erase-flash-region"})
	want(codes.FailedPrecondition, "manifest", "erase", map[string]any{"mode": "region", "address": last, "length": 4096.0, "confirm": "erase-flash-region", "backup": p})
	want(codes.PermissionDenied, "allow_chip_erase", "erase", map[string]any{"mode": "chip", "confirm": "erase-entire-chip", "backup": "x"})
}

// TestHWS3PluginFlashBackup takes a verified full backup (two identical
// reads). Opt-in: HWLAB_ESPJTAG_S3_FLASH_BACKUP=<dir>.
func TestHWS3PluginFlashBackup(t *testing.T) {
	dir := os.Getenv("HWLAB_ESPJTAG_S3_FLASH_BACKUP")
	if dir == "" {
		t.Skip("set HWLAB_ESPJTAG_S3_FLASH_BACKUP=<dir> to take a full backup (reads the whole flash twice)")
	}
	h := s3Plugin(t, nil)
	defer s3Resume(t, h)
	b := h.call(t, "FlashProgramming", "backup", map[string]any{"dir": dir})
	t.Logf("BACKUP %v sha256 %v second %v identical %v read_seconds %v size %v", b["backup_path"], b["sha256"], b["second_read_sha256"], b["reads_identical"], b["read_seconds"], b["size_bytes"])
	if b["reads_identical"] != true {
		t.Fatal("the two reads differ")
	}
}

// TestHWS3PluginFlashSectorCycle erases, programs and verifies ONE 4 KiB
// sector that is all 0xFF in the backup and outside every partition and
// protected range, puts the original bytes back, and (with
// HWLAB_ESPJTAG_S3_FLASH_RESTORE=1) restores the whole backup and compares
// the whole-flash SHA-256. Opt-in: HWLAB_ESPJTAG_S3_FLASH_WRITE=1 and
// HWLAB_ESPJTAG_S3_BACKUP=<verified backup .bin of this board>.
func TestHWS3PluginFlashSectorCycle(t *testing.T) {
	bk := os.Getenv("HWLAB_ESPJTAG_S3_BACKUP")
	if os.Getenv("HWLAB_ESPJTAG_S3_FLASH_WRITE") != "1" || bk == "" {
		t.Skip("set HWLAB_ESPJTAG_S3_FLASH_WRITE=1 and HWLAB_ESPJTAG_S3_BACKUP=<backup .bin> (writes one unused flash sector)")
	}
	img, m, err := esp.LoadBackup(bk)
	if err != nil || m == nil || m.SHA256 != m.SecondRead {
		t.Fatalf("backup %s: %v", bk, err)
	}
	h := s3Plugin(t, s3FlashCfg)
	defer s3Resume(t, h)
	in := h.call(t, "FlashProgramming", "info", nil)
	var parts []esp.Partition
	for _, p := range in["partitions"].([]any) {
		pm := p.(map[string]any)
		parts = append(parts, esp.Partition{Label: pm["label"].(string), Offset: uint32(pm["offset"].(float64)), Size: uint32(pm["size"].(float64))})
	}
	sector := -1
	for a := len(img) - esp.SectorSize; a >= 0x10000; a -= esp.SectorSize {
		if len(esp.Overlap(parts, uint32(a), esp.SectorSize)) == 0 && len(esp.OverlapRanges(esp.ProtectedRanges(parts), uint32(a), esp.SectorSize)) == 0 &&
			bytes.Count(img[a:a+esp.SectorSize], []byte{0xFF}) == esp.SectorSize {
			sector = a
			break
		}
	}
	if sector < 0 {
		t.Fatal("no unused all-0xFF sector outside the partitions")
	}
	t.Logf("test sector 0x%06x (all 0xFF in the backup, outside every partition)", sector)
	dir := t.TempDir()
	a := float64(sector)
	e := h.call(t, "FlashProgramming", "erase", map[string]any{"mode": "region", "address": a, "length": 4096.0, "confirm": "erase-flash-region", "backup": bk})
	t.Logf("erase: %v ms verified_erased %v", e["elapsed_ms"], e["verified_erased"])
	pat := make([]byte, esp.SectorSize)
	for i := range pat {
		pat[i] = byte(i*13 + i>>8)
	}
	pp := filepath.Join(dir, "pattern.bin")
	if err := os.WriteFile(pp, pat, 0o600); err != nil {
		t.Fatal(err)
	}
	pr := h.call(t, "FlashProgramming", "program", map[string]any{"path": pp, "address": a, "raw": true, "confirm": "program-flash", "backup": bk})
	t.Logf("program 4 KiB pattern: %v ms (%.1f KB/s) report %v", pr["program_ms"], pr["throughput_kb_s"], pr["write_report"])
	h.call(t, "FlashProgramming", "verify", map[string]any{"path": pp, "address": a})
	rb := h.call(t, "FlashProgramming", "read", map[string]any{"address": a, "length": 4096.0})
	if rb["hex"] != hex.EncodeToString(pat) {
		t.Fatal("read-back differs from the pattern")
	}
	// Put the original (all 0xFF) bytes back. The backup gate would refuse
	// now (the sector differs from the backup), so erase through restore of
	// just this sector: program the original bytes from the backup.
	op := filepath.Join(dir, "orig.bin")
	if err := os.WriteFile(op, img[sector:sector+esp.SectorSize], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.callErr("FlashProgramming", "program", map[string]any{"path": op, "address": a, "raw": true, "confirm": "program-flash", "backup": bk}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("program against a stale backup: %v", err)
	} else {
		t.Logf("stale backup refused as expected: %v", err)
	}
	full := os.Getenv("HWLAB_ESPJTAG_S3_FLASH_RESTORE") == "1"
	if !full {
		t.Fatal("the sector now holds the test pattern: rerun with HWLAB_ESPJTAG_S3_FLASH_RESTORE=1 or call FlashProgramming.restore to put the backup back")
	}
	rs := h.call(t, "FlashProgramming", "restore", map[string]any{"path": bk, "confirm": "restore-flash", "confirm_protected": "modify-protected-region"})
	t.Logf("restore: verified %v sha256 after %v (backup %v) %v ms report %v", rs["verified"], rs["flash_sha256_after"], m.SHA256, rs["elapsed_ms"], rs["write_report"])
	if rs["flash_sha256_after"] != m.SHA256 {
		t.Fatal("whole-flash SHA-256 differs from the backup after the restore")
	}
	h.call(t, "FlashProgramming", "verify", map[string]any{"path": op, "address": a})
}
