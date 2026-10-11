package targetaccept_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/faronov/hwlab/internal/targetaccept"
)

// The fixtures are the reports of one real CC2340 (Cortex-M0+) run, taken
// through the native XDS110 backend and through the CMSIS-DAP HID backend on
// the same probe and target. These tests only read them; they need no hardware.
func loadFixture(t *testing.T, backend string) *targetaccept.Report {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "cc2340", backend+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var r targetaccept.Report
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return &r
}

func TestCC2340FixturesAgree(t *testing.T) {
	x, c := loadFixture(t, "xds110"), loadFixture(t, "cmsisdap")
	for _, r := range []*targetaccept.Report{x, c} {
		if err := r.Err(); err != nil {
			t.Errorf("%s: %v", r.Backend, err)
		}
		if n := r.Count(targetaccept.Fail); n != 0 {
			t.Errorf("%s: %d failed checks", r.Backend, n)
		}
		f := r.Facts
		for k, want := range map[string]string{
			"dpidr": "0x6ba02477", "ap_idr": "0x84770001", "cpuid": "0x410cc601",
			"core": "Cortex-M0+", "architecture": "ARMv6-M", "revision": "r0p1",
			"bp.present": "true", "bp.slots": "4", "bp.rounds_hit": "5/5",
			"wp.present": "false", "wp.comparators": "0",
			"reset.core reset (VECTRESET)": "UNSUPPORTED_BY_ATTACHED_HW",
		} {
			if f[k] != want {
				t.Errorf("%s: fact %s = %q, want %q", r.Backend, k, f[k], want)
			}
		}
		if r.Find("watchpoint resources").Status != targetaccept.Info {
			t.Errorf("%s: watchpoint discovery missing", r.Backend)
		}
	}
	for _, d := range targetaccept.Compare(x, c) {
		t.Errorf("backends differ: %s", d)
	}
	if x.PerfMS["2 KiB SRAM read"] >= c.PerfMS["2 KiB SRAM read"] {
		t.Logf("xds110 was not faster than cmsisdap at 2 KiB reads: %v vs %v", x.PerfMS, c.PerfMS)
	}
}

func loadNamed(t *testing.T, name string, v any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "cc2340", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

// Every reset of the real CC2340 goes through SACI and keeps debug access.
func TestCC2340FixturesResetsReturnDebug(t *testing.T) {
	for _, name := range []string{"xds110", "cmsisdap", "flash-xds110", "flash-cmsisdap"} {
		r := loadFixture(t, name)
		n := 0
		for k, v := range r.Facts {
			if strings.HasSuffix(k, ".debug_after") {
				n++
				if v != "returned" {
					t.Errorf("%s: %s = %q", name, k, v)
				}
			}
		}
		if n < 7 {
			t.Errorf("%s: only %d reset checks", name, n)
		}
		for _, k := range []string{"reset.sysreset while running", "reset.sysreset and halt", "reset.hardware nRESET and halt"} {
			if r.Facts[k] != "ok" {
				t.Errorf("%s: %s = %q", name, k, r.Facts[k])
			}
		}
	}
}

// The destructive run (erase, program the acceptance firmware, boot, debug,
// restore) on both probes.
func TestCC2340FlashFixtures(t *testing.T) {
	x, c := loadFixture(t, "flash-xds110"), loadFixture(t, "flash-cmsisdap")
	for _, r := range []*targetaccept.Report{x, c} {
		if err := r.Err(); err != nil {
			t.Errorf("%s: %v", r.Backend, err)
		}
		if r.Facts["firmware.bp_hits"] != "5/5" {
			t.Errorf("%s: bp hits %q", r.Backend, r.Facts["firmware.bp_hits"])
		}
		if r.Facts["flash_0_32"] != x.Facts["flash_0_32"] {
			t.Errorf("%s: flash prefix differs between probes", r.Backend)
		}
		if got := r.Facts["flash_0_32"][:16]; got != "0090002041000000" {
			t.Errorf("%s: vector table %s is not the acceptance image", r.Backend, got)
		}
	}
	for _, d := range targetaccept.Compare(x, c) {
		t.Errorf("backends differ: %s", d)
	}
	var tm map[string]float64
	loadNamed(t, "flash-timing.json", &tm)
	if tm["cmsisdap_restore_ms"] <= 0 || tm["xds110_erase_ms"] <= 0 {
		t.Errorf("timing fixture incomplete: %v", tm)
	}
}

type speedRow struct {
	RequestedHz int                `json:"requested_hz"`
	Stable      bool               `json:"stable"`
	Fail        int                `json:"fail"`
	FlashMatch  bool               `json:"flash_read_matches_reference"`
	PerfMS      map[string]float64 `json:"perf_ms"`
}

func TestCC2340SpeedFixture(t *testing.T) {
	var s map[string][]speedRow
	loadNamed(t, "speed.json", &s)
	for _, be := range []string{"xds110", "cmsisdap"} {
		rows := s[be]
		if len(rows) != 7 {
			t.Fatalf("%s: %d clocks", be, len(rows))
		}
		for _, r := range rows {
			if !r.Stable || r.Fail != 0 || !r.FlashMatch {
				t.Errorf("%s @ %d Hz: stable=%v fail=%d flash=%v", be, r.RequestedHz, r.Stable, r.Fail, r.FlashMatch)
			}
		}
		if rows[len(rows)-1].RequestedHz != 10_000_000 {
			t.Errorf("%s: top clock %d", be, rows[len(rows)-1].RequestedHz)
		}
	}
}
