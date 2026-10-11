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

func espInvokes(t *testing.T, a *app.App, ref string, want []string) []map[string]any {
	t.Helper()
	evs, err := a.QueryTimeline(app.TimelineQuery{Session: ref, Types: []string{"invoke"}})
	if err != nil || len(evs) != len(want) {
		t.Fatalf("invoke events: %d err=%v", len(evs), err)
	}
	var out []map[string]any
	for i, e := range evs {
		if e.Data["method"] != want[i] || e.Data["ok"] != true {
			t.Fatalf("event %d: %+v", i, e.Data)
		}
		out = append(out, e.Data["result"].(map[string]any))
	}
	return out
}

// REAL: an ESP32-C6 over its native USB Serial/JTAG (serial sanitized).
func TestRealESPUSBJTAGSessionReplaysWithoutHardware(t *testing.T) {
	ftdiReplay(t, "real-espusbjtag", func(t *testing.T, a *app.App, ref string) {
		r := espInvokes(t, a, ref, []string{"info", "scan", "identify", "chip_info", "halt", "read_registers", "read", "read", "read", "resume", "scan"})
		if r[0]["synthetic"] != nil {
			t.Errorf("real session marked synthetic: %v", r[0])
		}
		for _, i := range []int{1, 10} {
			devs := r[i]["devices"].([]any)
			if r[i]["status"] != "ok" || len(devs) != 1 || r[i]["target_verified"] != true {
				t.Fatalf("scan %d: %v", i, r[i])
			}
			d := devs[0].(map[string]any)
			if d["idcode"].(map[string]any)["raw"] != float64(0xdc25) || d["ir_length"] != float64(5) {
				t.Errorf("idcode: %v", d)
			}
		}
		if r[3]["debug_spec"] != "0.13" || r[3]["harts"] != float64(2) || r[3]["progbuf_words"] != float64(16) {
			t.Errorf("chip_info: %v", r[3])
		}
		if r[4]["halted"] != true || r[9]["halted"] != false {
			t.Errorf("halt/resume: %v %v", r[4], r[9])
		}
		regs := r[5]["registers"].(map[string]any)
		if regs["misa"] != "0x40903105" || regs["mvendorid"] != "0x00000612" || regs["marchid"] != "0x80000002" {
			t.Errorf("registers: %v", regs)
		}
		if r[6]["hex"] != "6f100068000000000000000000000000" || r[7]["hex"] != "6f00801a6f00c02d6f0080236f004024" {
			t.Errorf("memory: %v %v", r[6], r[7])
		}
	})
}

// SYNTHETIC: the in-process USB model with a fabricated RISC-V target.
func TestSyntheticESPUSBJTAGSessionReplaysWithoutHardware(t *testing.T) {
	ftdiReplay(t, "synthetic-espusbjtag", func(t *testing.T, a *app.App, ref string) {
		r := espInvokes(t, a, ref, []string{"info", "scan", "identify", "chip_info", "halt", "read_registers", "read", "read", "resume", "scan"})
		for i, x := range r {
			if x["synthetic"] != true || x["target_verified"] == true {
				t.Errorf("result %d not marked synthetic/unverified: %v", i, x)
			}
		}
	})
}

// REAL: ESP32-C6 debug session (step, hardware breakpoint, SRAM write+restore,
// flash geometry, SoC reset with USB re-enumeration); serial sanitized.
func TestRealESPUSBJTAGDebugSessionReplaysWithoutHardware(t *testing.T) {
	ftdiReplay(t, "real-espusbjtag-debug", func(t *testing.T, a *app.App, ref string) {
		r := espInvokes(t, a, ref, []string{"info", "scan", "identify", "chip_info", "harts", "reset", "step", "reset", "set_breakpoint",
			"list_breakpoints", "clear_breakpoint", "read_registers", "read", "write", "read", "write", "read", "info", "reset", "read", "scan"})
		for _, i := range []int{1, 20} {
			if r[i]["status"] != "ok" || r[i]["target_verified"] != true {
				t.Fatalf("scan %d: %v", i, r[i])
			}
		}
		if r[5]["kind"] != "system" || r[5]["reset_observed"] != true || r[5]["usb_dropped"] != false || r[5]["pc"] != "0x40000000" {
			t.Errorf("system reset: %v", r[5])
		}
		if r[6]["cause_name"] != "step" || r[6]["count"] != float64(4) {
			t.Errorf("step: %v", r[6])
		}
		for _, st := range r[6]["steps"].([]any) {
			m := st.(map[string]any)
			if m["cause"] != float64(4) || m["pc_after"] == m["pc_before"] {
				t.Errorf("step not a real step: %v", m)
			}
		}
		if r[8]["hit"] != true || r[8]["cause_name"] != "trigger" || r[8]["halted_pc"] != "0x40001688" {
			t.Errorf("breakpoint: %v", r[8])
		}
		if r[10]["installed"] != float64(0) {
			t.Errorf("breakpoint not cleared: %v", r[10])
		}
		const zeros, pattern = "00000000000000000000000000000000", "a55a1234deadbeef0102030405060708"
		if r[12]["hex"] != zeros || r[14]["hex"] != pattern || r[16]["hex"] != zeros || r[13]["verified"] != true || r[15]["previous_hex"] != pattern {
			t.Errorf("SRAM write+restore evidence: %v %v %v", r[12], r[14], r[16])
		}
		fl := r[17]["flash"].(map[string]any)
		if fl["jedec_id"] != "5e4016" || fl["size_bytes"] != float64(4194304) || fl["sector_size"] != float64(4096) {
			t.Errorf("flash geometry: %v", fl)
		}
		if r[18]["kind"] != "soc" || r[18]["usb_dropped"] != true || r[18]["reenumerated"] != true || r[18]["identity_verified"] != true {
			t.Errorf("soc reset: %v", r[18])
		}
		if r[19]["hex"] != "6f80e02c" {
			t.Errorf("post-reset ROM word: %v", r[19])
		}
	})
}

// The committed evidence is metadata and hashes only: no MAC, no firmware.
func TestESPEvidenceIsSanitized(t *testing.T) {
	root := testutil.ModuleRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "testdata", "real-espusbjtag-evidence", "flash-and-unplug.json"))
	if err != nil {
		t.Skip("evidence missing")
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "60:55") || strings.Contains(string(b), "60_55") {
		t.Error("real USB serial leaked")
	}
	if len(b) > 16<<10 {
		t.Errorf("evidence is %d bytes: it must not carry firmware contents", len(b))
	}
	if v["flash_program_verify_boot_restore"].(map[string]any)["restore_full_readback_sha256_equal"] != true {
		t.Error("restore evidence missing")
	}
}
