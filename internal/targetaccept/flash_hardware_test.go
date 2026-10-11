//go:build hardware && linux

package targetaccept_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/faronov/hwlab/internal/targetaccept"
	cmsisdapplugin "github.com/faronov/hwlab/plugins/cmsisdap"
	xds110plugin "github.com/faronov/hwlab/plugins/xds110"
)

// DESTRUCTIVE real-hardware flash acceptance of the CC2340. It erases the whole
// chip, programs the hwlab acceptance firmware, debugs it through both probes
// and restores the original contents from a verified backup.
//
//	HWLAB_CC2340_FLASH=1
//	HWLAB_CC2340_DESTRUCTIVE=erase-and-restore-this-chip
//	HWLAB_CC2340_BACKUP_DIR=<dir>   backup directory (default: $HWLAB_HOME/artifacts/cc23xx-flash)
//	HWLAB_CC2340_SERIAL, HWLAB_CC2340_REPORT_DIR as in hardware_test.go
//
// Sequence: XDS110 backup -> chip erase -> program (blank base) -> debug
// acceptance; CMSIS-DAP verify -> debug acceptance -> restore -> byte, hash,
// SACI CRC and boot verification. A failed run still tries to restore.
func TestCC2340FlashDestructive(t *testing.T) {
	if os.Getenv("HWLAB_CC2340_FLASH") != "1" || os.Getenv("HWLAB_CC2340_DESTRUCTIVE") != "erase-and-restore-this-chip" {
		t.Skip("set HWLAB_CC2340_FLASH=1 and HWLAB_CC2340_DESTRUCTIVE=erase-and-restore-this-chip to ERASE the attached CC2340")
	}
	ctx := context.Background()
	x, c := xds110plugin.New(), cmsisdapplugin.New()
	id := probeID(t, x)
	if cid := probeID(t, c); cid != id {
		t.Fatalf("the two plugins see different probes: %s vs %s", id, cid)
	}
	perms := map[string]any{"allow_flash": true, "allow_erase": true, "allow_chip_erase": true}
	xcfg := map[string]any{}
	for k, v := range perms {
		xcfg[k] = v
	}
	if err := x.OpenDevice(ctx, id, xcfg); err != nil {
		t.Fatal(err)
	}
	defer x.CloseDevice(ctx, id)
	if err := c.OpenDevice(ctx, id, perms); err != nil {
		t.Fatal(err)
	}
	defer c.CloseDevice(ctx, id)
	xinv, cinv := invoker(t, x, id), invoker(t, c, id)
	must := func(inv targetaccept.Invoker, cp, m string, a map[string]any) map[string]any {
		t.Helper()
		r, err := inv(cp, m, a)
		if err != nil {
			t.Fatalf("%s.%s: %v", cp, m, err)
		}
		return r
	}
	timing := map[string]any{}
	fw := targetaccept.AcceptanceFirmware()
	work := t.TempDir()
	imgPath := filepath.Join(work, "hwlab-cc2340-acceptance.bin")
	if err := os.WriteFile(imgPath, fw.Image, 0o600); err != nil {
		t.Fatal(err)
	}

	// 1-3: original state, CCFG, backup.
	info0 := must(xinv, "FlashProgramming", "info", nil)
	ccfg0 := must(xinv, "FlashProgramming", "ccfg_info", nil)
	t.Logf("original: geometry %v, ccfg %v", info0["geometry"], ccfg0["ccfg"])
	bargs := map[string]any{}
	if d := os.Getenv("HWLAB_CC2340_BACKUP_DIR"); d != "" {
		bargs["dir"] = d
	}
	bk := must(xinv, "FlashProgramming", "backup", bargs)
	backup, _ := bk["path"].(string)
	t.Logf("verified backup %s (%v bytes, identical reads %v, %v ms)", backup, bk["size_bytes"], bk["identical_reads"], bk["elapsed_ms"])
	if bk["identical_reads"] != true || backup == "" {
		t.Fatal("the backup is not verified; refusing to erase")
	}
	orig, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	origSum := sha256.Sum256(orig)
	t.Logf("backup sha256 %s", hex.EncodeToString(origSum[:]))
	timing["backup_ms"] = bk["elapsed_ms"]

	restored := false
	restoreArgs := map[string]any{"path": backup, "confirm": "restore-flash"}
	defer func() {
		if restored {
			return
		}
		t.Logf("restoring the original contents after a failure")
		for name, inv := range map[string]targetaccept.Invoker{"xds110": xinv, "cmsisdap": cinv} {
			_, _ = xinv("DebugProbe", "disconnect", nil)
			_, _ = cinv("DebugProbe", "disconnect", nil)
			if _, err := inv("FlashProgramming", "restore", restoreArgs); err == nil {
				t.Logf("restored through %s", name)
				return
			} else {
				t.Logf("restore through %s: %v", name, err)
			}
		}
		t.Errorf("THE ORIGINAL FLASH WAS NOT RESTORED; run FlashProgramming.restore with %s", backup)
	}()

	// 4-5: chip erase and blank verification (program checks the same SACI CRCs).
	er := must(xinv, "FlashProgramming", "erase", map[string]any{"confirm": "erase-entire-chip", "backup": backup})
	t.Logf("erase: %v", er)
	timing["xds110_erase_ms"] = er["elapsed_ms"]

	// 6-11: program the acceptance image on a blank device and boot it.
	pr := must(xinv, "FlashProgramming", "program", map[string]any{"path": imgPath, "address": "0x0", "confirm": "program-flash", "backup": backup, "reset": "run"})
	t.Logf("program: %v", pr)
	if pr["merged_over"] != "blank" || pr["main_verified"] != true || pr["ccfg_verified"] != true {
		t.Errorf("program did not start from a verified blank device or did not verify: %v", pr)
	}
	for _, k := range []string{"erase_ms", "program_ms", "verify_ms"} {
		timing["xds110_program_"+k] = pr[k]
	}
	ccfg1 := must(xinv, "FlashProgramming", "ccfg_info", nil)
	if !jsonEq(ccfg0["ccfg"], ccfg1["ccfg"]) || ccfg0["sha256"] != ccfg1["sha256"] {
		t.Errorf("the CCFG changed: before %v after %v", ccfg0, ccfg1)
	}
	if v := must(xinv, "FlashProgramming", "verify", map[string]any{"path": imgPath, "address": "0x0"}); v["verified"] != true {
		t.Errorf("verify: %v", v)
	}

	// 12-18 through the XDS110.
	opts := func(backend string) targetaccept.Options {
		return targetaccept.Options{
			Backend: backend, Log: t.Logf, ClockHz: 400000, Steps: 100,
			Reset: true, ResetVectorBreakpoint: true, SkipROMTable: true,
			Phases: []string{"sram", "registers", "stepping", "breakpoints", "reset"},
		}
	}
	reports := map[string]*targetaccept.Report{}
	runBackend := func(backend string, inv targetaccept.Invoker) {
		fr := targetaccept.RunFirmware(inv, opts(backend), fw)
		gr := targetaccept.Run(inv, opts(backend))
		fr.Checks = append(fr.Checks, gr.Checks...)
		for k, v := range gr.Facts {
			fr.Facts[k] = v
		}
		fr.PerfMS = gr.PerfMS
		fr.Backend = backend
		reports[backend] = fr
		if err := fr.Err(); err != nil {
			t.Errorf("%s: %v", backend, err)
		}
		t.Logf("%s: %d pass, %d fail, %d unsupported, %d info", backend, fr.Count(targetaccept.Pass), fr.Count(targetaccept.Fail), fr.Count(targetaccept.UnsupportedHW), fr.Count(targetaccept.Info))
	}
	runBackend("xds110", xinv)
	must(xinv, "DebugProbe", "disconnect", nil)

	// Second probe: no second erase, it verifies and debugs what the first one wrote.
	if v := must(cinv, "FlashProgramming", "verify", map[string]any{"path": imgPath, "address": "0x0"}); v["verified"] != true {
		t.Errorf("cmsisdap verify: %v", v)
	}
	ccfgC := must(cinv, "FlashProgramming", "ccfg_info", nil)
	if ccfgC["sha256"] != ccfg0["sha256"] {
		t.Errorf("the second probe sees a different CCFG: %v vs %v", ccfgC["sha256"], ccfg0["sha256"])
	}
	runBackend("cmsisdap", cinv)

	// Restore through the second probe: erase, program the backup, verify.
	rs := must(cinv, "FlashProgramming", "restore", restoreArgs)
	t.Logf("restore: %v", rs)
	if rs["restored"] != true || rs["whole_flash_matches_backup"] != true {
		t.Errorf("restore: %v", rs)
	}
	timing["cmsisdap_restore_ms"] = rs["elapsed_ms"]
	restored = true
	rb := filepath.Join(work, "restored.bin")
	rd := must(cinv, "FlashProgramming", "read", map[string]any{"address": "0x0", "length": float64(len(orig)), "path": rb})
	got, err := os.ReadFile(rb)
	if err != nil {
		t.Fatal(err)
	}
	gotSum := sha256.Sum256(got)
	if !bytes.Equal(got, orig) || gotSum != origSum {
		t.Errorf("restored flash differs from the backup (sha256 %s)", hex.EncodeToString(gotSum[:]))
	}
	t.Logf("restored flash read back: %v bytes, byte compare %v, sha256 equal %v", rd["length"], bytes.Equal(got, orig), gotSum == origSum)
	ccfg2 := must(cinv, "FlashProgramming", "ccfg_info", nil)
	if ccfg2["sha256"] != ccfg0["sha256"] || !jsonEq(ccfg2["ccfg"], ccfg0["ccfg"]) {
		t.Errorf("the restored CCFG differs: %v vs %v", ccfg2, ccfg0)
	}
	st := must(cinv, "DebugProbe", "saci_status", nil)
	t.Logf("boot state after restore: %v", st)
	if s, _ := st["state"].(string); !strings.Contains(s, "app") {
		t.Errorf("the restored device did not boot the application: %v", st)
	}
	must(cinv, "DebugProbe", "connect", map[string]any{"clock_hz": 400000.0})
	if _, err := cinv("DebugProbe", "halt", nil); err != nil {
		t.Errorf("halt after restore: %v", err)
	}
	w, err := cinv("MemoryAccess", "read", map[string]any{"address": "0x0", "length": 8.0})
	if err != nil || w["hex"] != hex.EncodeToString(orig[:8]) {
		t.Errorf("vector table after restore: %v %v", w, err)
	}
	must(cinv, "DebugProbe", "resume", nil)
	must(cinv, "DebugProbe", "disconnect", nil)

	if dir := os.Getenv("HWLAB_CC2340_REPORT_DIR"); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for b, r := range reports {
			j, _ := json.MarshalIndent(r, "", "  ")
			_ = os.WriteFile(filepath.Join(dir, "flash-"+b+".json"), append(j, '\n'), 0o644)
		}
		j, _ := json.MarshalIndent(timing, "", "  ")
		_ = os.WriteFile(filepath.Join(dir, "flash-timing.json"), append(j, '\n'), 0o644)
	}
	time.Sleep(10 * time.Millisecond)
}

func jsonEq(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}
