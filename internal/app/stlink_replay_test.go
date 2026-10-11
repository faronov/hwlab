package app_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/faronov/hwlab/internal/app"
	"github.com/faronov/hwlab/internal/testutil"
)

// REAL: an ST-Link V2 (0483:3748) with an nRF52840 behind it, recorded with
// examples/stlink-debug.yaml; the probe serial is sanitized. The recorded
// target is NOT an STM32, so the flash call must be UNSUPPORTED_BY_ATTACHED_HW.
func TestRealSTLinkDebugSessionReplaysWithoutHardware(t *testing.T) {
	aliasReplay(t, "real-stlink-debug", "stlink", func(t *testing.T, a *app.App, ref string) {
		r := espInvokes(t, a, ref, []string{"info", "identify", "chip_info", "harts", "halt", "read_registers", "read", "write", "read", "write", "read",
			"resume", "reset", "step", "reset", "set_breakpoint", "list_breakpoints", "clear_breakpoint", "reset", "set_watchpoint", "resume",
			"wait_halt", "clear_watchpoint", "resume", "reset", "set_speed", "read", "set_speed", "info", "identify"})
		for i, x := range r {
			if x["synthetic"] != nil {
				t.Errorf("result %d marked synthetic: %v", i, x)
			}
		}
		id := r[1]
		probe := id["probe"].(map[string]any)
		if probe["serial"] != "38FFAABBCCDDEEFF00112233" || probe["vid"] != "0483" || probe["pid"] != "3748" || probe["generation"] != "V2" || probe["firmware"] != "V2J39S7" {
			t.Errorf("probe: %v", probe)
		}
		if id["probe_serial"] != probe["serial"] || id["target_verified"] != true {
			t.Errorf("serial/target evidence: %v", id)
		}
		core := id["core"].(map[string]any)
		if core["cpuid"] != "0x410fc241" || core["name"] != "Cortex-M4" || id["debug_port"].(map[string]any)["dp_idcode"] != "0x2ba01477" {
			t.Errorf("core: %v", core)
		}
		if v := id["vendor"].(map[string]any); v["register_word"] != "0x00052840" {
			t.Errorf("vendor evidence: %v", v)
		}
		if r[2]["breakpoint_unit"].(map[string]any)["code_comparators"] != float64(6) || r[2]["watchpoint_unit"].(map[string]any)["comparators"] != float64(4) {
			t.Errorf("comparators: %v %v", r[2]["breakpoint_unit"], r[2]["watchpoint_unit"])
		}
		if r[4]["halted"] != true || r[5]["native_matches_dcrsr"] != true || r[11]["halted"] != false {
			t.Errorf("halt/registers/resume: %v %v %v", r[4], r[5], r[11])
		}
		// SRAM write + restore: the original bytes come back.
		if r[6]["hex"] != r[10]["hex"] || r[8]["hex"] != "a55a1234deadbeef0102030405060708" || r[7]["verified"] != true || r[9]["verified"] != true {
			t.Errorf("SRAM write/restore: %v %v %v", r[6], r[8], r[10])
		}
		if r[12]["reset_observed"] != true || r[12]["halted"] != true || r[12]["pc"] != "0x0002aebc" {
			t.Errorf("reset-halt: %v", r[12])
		}
		if r[13]["moved"] != float64(12) || r[13]["step_bit_restored"] != true || r[13]["pc"] != "0x0002aecc" {
			t.Errorf("step: %v", r[13])
		}
		if r[15]["hit"] != true || r[15]["halted_pc"] != "0x0002aecc" || r[15]["slot"] != float64(0) {
			t.Errorf("breakpoint: %v", r[15])
		}
		if r[17]["installed"] != float64(0) || r[22]["installed"] != float64(0) {
			t.Errorf("triggers not removed: %v %v", r[17], r[22])
		}
		if r[21]["watchpoint_hit"] != true {
			t.Errorf("watchpoint: %v", r[21])
		}
		if r[25]["actual_khz"] != float64(950) || r[27]["actual_khz"] != float64(1800) {
			t.Errorf("speed: %v %v", r[25], r[27])
		}
		if r[28]["supported"] != false || r[28]["status"] != "UNSUPPORTED_BY_ATTACHED_HW" {
			t.Errorf("flash on a non-STM32 target must be unsupported: %v", r[28])
		}
	})
}

// REAL: the same ST-Link V2 with a real STM32F1 medium-density target behind
// it (identified from DBGMCU_IDCODE 0x410 rev 0x2003; serial sanitized),
// recorded with examples/stlink-stm32f1.yaml: the full native-flash cycle
// (two identical backups, erase page 0, program, verify, boot, step,
// breakpoint, watchpoint, restore, whole-flash hash comparison). Only hashes
// and metadata are in the fixture, never firmware bytes.
func TestRealSTLinkSTM32F1FlashCycleReplaysWithoutHardware(t *testing.T) {
	const origSHA = "acba307bbb3fafa322220b35c9add1e5fc31ab4ba54ba9ba58a88255a8fe6a16"
	aliasReplay(t, "real-stlink-stm32-flash", "stlink", func(t *testing.T, a *app.App, ref string) {
		r := espInvokes(t, a, ref, []string{"info", "identify", "chip_info", "halt", "read_registers", "read", "write", "read", "write", "read", "resume", "reset", "step", "resume", "info",
			"backup", "erase", "program", "verify", "reset", "halt", "read", "resume", "halt", "read", "reset", "step", "step", "step", "step", "set_breakpoint", "clear_breakpoint",
			"set_watchpoint", "resume", "wait_halt", "clear_watchpoint", "restore", "backup", "reset", "resume", "halt", "read_registers", "resume", "identify"})
		for i, x := range r {
			if x["synthetic"] != nil {
				t.Errorf("result %d marked synthetic: %v", i, x)
			}
		}
		id := r[1]
		core := id["core"].(map[string]any)
		stm := id["stm32"].(map[string]any)
		if id["probe_serial"] != "38FFAABBCCDDEEFF00112233" || core["cpuid"] != "0x411fc231" || core["name"] != "Cortex-M3" || id["debug_port"].(map[string]any)["dp_idcode"] != "0x1ba01477" {
			t.Errorf("identity: %v", id)
		}
		if stm["dev_id"] != "0x410" || stm["rev_id"] != "0x2003" || stm["family"] != "STM32F1" || stm["flash_size_source"] != "probed" {
			t.Errorf("STM32 evidence: %v", stm)
		}
		if r[2]["breakpoint_unit"].(map[string]any)["code_comparators"] != float64(6) || r[2]["watchpoint_unit"].(map[string]any)["comparators"] != float64(4) {
			t.Errorf("comparators: %v %v", r[2]["breakpoint_unit"], r[2]["watchpoint_unit"])
		}
		if r[6]["verified"] != true || r[7]["hex"] != "a55a1234deadbeef0102030405060708" || r[5]["hex"] != r[9]["hex"] {
			t.Errorf("SRAM write/restore: %v %v %v", r[5], r[7], r[9])
		}
		if r[11]["reset_vector"] != "0x0800149c" || r[11]["pre_reset_vector"] != "0x0800d970" || r[11]["pc"] != "0x0800149c" || r[11]["at_reset_vector"] != true {
			t.Errorf("reset-halt: %v", r[11])
		}
		fi := r[14]
		if fi["supported"] != true || fi["flash_kb"] != float64(128) || fi["sector_count"] != float64(128) || fi["page_size"] != float64(1024) || fi["protection"].(map[string]any)["readout_protection_active"] != false {
			t.Errorf("flash info: %v", fi)
		}
		if r[15]["identical_reads"] != true || r[15]["sha256"] != origSHA || r[15]["second_read_sha256"] != origSHA {
			t.Errorf("backup: %v", r[15])
		}
		if r[16]["verified_erased"] != true || r[17]["verified"] != true || r[18]["verified"] != true || r[17]["sha256"] != "62fbb847e4772881471be1c1c9f3b0e0391343a3cb12388940866b326122a3bc" {
			t.Errorf("erase/program/verify: %v %v %v", r[16], r[17], r[18])
		}
		if r[25]["pc"] != "0x08000040" || r[29]["moved"] != float64(25) || r[29]["pc"] != "0x08000060" {
			t.Errorf("acceptance reset/steps: %v %v", r[25], r[29])
		}
		if r[21]["hex"] == r[24]["hex"] || r[30]["hit"] != true || r[30]["halted_pc"] != "0x08000060" || r[34]["watchpoint_hit"] != true {
			t.Errorf("boot/breakpoint/watchpoint: %v %v %v %v", r[21], r[24], r[30], r[34])
		}
		if r[36]["restored"] != true || r[36]["whole_flash_matches_backup"] != true || r[37]["sha256"] != origSHA || r[37]["second_read_sha256"] != origSHA {
			t.Errorf("restore: %v %v", r[36], r[37])
		}
		if r[38]["pc"] != "0x0800149c" || r[43]["target_verified"] != true {
			t.Errorf("original behaviour: %v %v", r[38], r[43])
		}
	})
}

// The committed evidence is metadata only: no real serial, no firmware.
func TestSTLinkEvidenceIsSanitized(t *testing.T) {
	root := testutil.ModuleRoot(t)
	dirs := []string{"real-stlink-evidence", "real-stlink-debug", "real-stlink-stm32-flash", "real-stlink-stm32-evidence"}
	for _, d := range dirs {
		err := filepath.Walk(filepath.Join(root, "testdata", d), func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return err
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if strings.Contains(string(b), "38FF6F") || strings.Contains(string(b), "38c3bf6f") || strings.Contains(string(b), "/home/") {
				t.Errorf("%s leaks the real serial or a home path", p)
			}
			if len(b) > 256<<10 {
				t.Errorf("%s is %d bytes: it must not carry firmware contents", p, len(b))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(filepath.Join(root, "testdata", "real-stlink-evidence", "evidence.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if u := v["unplug_replug"].(map[string]any); u["physically_performed_by_user"] != true || u["rounds"] != 2.0 {
		t.Errorf("unplug evidence: %v", u)
	}
}

func TestSTLinkSTM32EvidenceMatchesTheRecordedSession(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(testutil.ModuleRoot(t), "testdata", "real-stlink-stm32-evidence", "evidence.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	tg, fl := v["target"].(map[string]any), v["flash"].(map[string]any)
	if tg["dbgmcu_idcode"] != "0x20036410" || tg["dp_idcode"] != "0x1BA01477" || tg["flash_bytes"] != 131072.0 {
		t.Errorf("target evidence: %v", tg)
	}
	if fl["backup_sha256"] != fl["backup_second_read_sha256"] || fl["restored_sha256_equals_original"] != true {
		t.Errorf("flash evidence: %v", fl)
	}
	if u := v["unplug_replug"].(map[string]any); u["physically_performed_by_user"] != true || u["rounds"] != 2.0 {
		t.Errorf("unplug evidence: %v", u)
	}
}
