package status

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegistryIsConsistent(t *testing.T) {
	if err := Validate(Registry()); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryHasNoOverclaim(t *testing.T) {
	for _, plugin := range []string{"can", "scpi", "modbus", "picoscope", "usbcap"} {
		e, ok := Lookup(plugin)
		if !ok {
			t.Fatalf("%s missing", plugin)
		}
		for _, b := range e.Backends {
			if b.Validation == ValHardware || b.Validation == ValHardwarePartial {
				t.Errorf("%s/%s claims hardware validation; no run on a real device is documented", plugin, b.Name)
			}
		}
	}
	e, _ := Lookup("picoscope")
	var missing bool
	for _, b := range e.Backends {
		if b.Implementation == ImplMissing {
			missing = true
		}
	}
	if !missing {
		t.Error("picoscope must list the missing PicoSDK backend")
	}
}

// FTDI is validated on one adapter with no JTAG target attached: the scan and
// every target-level claim must stay unverified.
func TestFTDIDoesNotClaimTargetScan(t *testing.T) {
	e, ok := Lookup("ftdi")
	if !ok {
		t.Fatal("ftdi missing")
	}
	b := e.Backends[0]
	if b.Validation != ValHardwarePartial || b.Fixture != FixtureReal || b.Cgo != "no" {
		t.Fatalf("%+v", b)
	}
	joined := strings.Join(b.Unverified, "\n")
	for _, want := range []string{"JTAG target scan", "IR/DR shifts", "unplug/replug"} {
		if !strings.Contains(joined, want) {
			t.Errorf("%q must stay unverified", want)
		}
	}
}

// Each ESP USB-JTAG capability is graded on its own: what never ran on the
// real board must not be upgraded because its neighbours did.
func TestESPUSBJTAGCapabilityGrades(t *testing.T) {
	e, ok := Lookup("espusbjtag")
	if !ok {
		t.Fatal("espusbjtag missing")
	}
	b := e.Backends[0]
	if b.Validation != ValHardwarePartial || b.Fixture != FixtureReal || b.Cgo != "no" || b.Dependency != DepNative {
		t.Fatalf("%+v", b)
	}
	got := map[string]CapStatus{}
	for _, c := range b.Capabilities {
		got[c.Name] = c.Status
	}
	for name, want := range map[string]CapStatus{
		"memory write":                           CapRealHW,
		"breakpoints":                            CapRealHW,
		"unplug/replug":                          CapRealHW,
		"flash program":                          CapRealHW,
		"multi-hart: debugging hart 1":           CapUntested,
		"flash erase (app partition, full chip)": CapSimulatedOnly,
		"eFuse access":                           CapNotImplemented,
		"OpenOCD cross-check":                    CapNotImplemented,
	} {
		if got[name] != want {
			t.Errorf("%s: %q, want %q", name, got[name], want)
		}
	}
	joined := strings.Join(b.Unverified, "\n") + "\n" + strings.Join(b.Gaps, "\n")
	for _, want := range []string{"hart 1", "power loss", "full-chip erase", "OpenOCD"} {
		if !strings.Contains(joined, want) {
			t.Errorf("%q must stay listed as unverified or a gap", want)
		}
	}
}

// Telink is validated read-only on one board; flash modification must stay
// listed as unverified until a protection map is verified and a sacrificial
// board is used.
// TestTelinkClaimsMatchHardwareEvidence keeps the telink claims at what
// docs/notes/phase15-telink-flash-hardware-results.md and
// docs/notes/telink-debug-hardware-results.md show on the real board.
func TestTelinkClaimsMatchHardwareEvidence(t *testing.T) {
	e, _ := Lookup("telink")
	b := e.Backends[0]
	if b.Validation != ValHardwarePartial || b.Fixture != FixtureReal {
		t.Fatalf("%+v", b)
	}
	join := func(l []string) string { return strings.Join(l, "\n") }
	for _, want := range []string{"full-flash backup", "refused without a board-matched backup", "erase and program of one application sector", "restore of application sectors", "single-step", "three hardware breakpoints"} {
		if !strings.Contains(join(b.Verified), want) {
			t.Errorf("%q should be listed as verified", want)
		}
	}
	for _, want := range []string{"whole application image", "memory write", "PC write"} {
		if !strings.Contains(join(b.Unverified), want) {
			t.Errorf("%q must stay unverified", want)
		}
	}
	for _, note := range []string{"phase15-telink-flash-hardware-results.md", "telink-debug-hardware-results.md"} {
		if _, err := os.Stat(filepath.Join("..", "..", "docs", "notes", note)); err != nil {
			t.Errorf("hardware evidence note missing: %v", err)
		}
	}
}

// TestSupportMatrixDocIsCurrent keeps docs/support-matrix.md identical to the
// registry. HWLAB_UPDATE_DOCS=1 regenerates it.
func TestSupportMatrixDocIsCurrent(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "support-matrix.md")
	want := Markdown(Registry())
	if os.Getenv("HWLAB_UPDATE_DOCS") == "1" {
		if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatal("docs/support-matrix.md is stale: run HWLAB_UPDATE_DOCS=1 go test ./internal/status")
	}
}

// CMSIS-DAP target operations were run on one real CC2340 (Cortex-M0+) through
// the LP-XDS110ET HID interface; the BL702-DAPLink never had a target. Only what
// was run is REAL_HW_VERIFIED, and nothing is upgraded because a neighbour was.
func TestCMSISDAPClaimsOnlyWhatRanOnTheCC2340(t *testing.T) {
	e, ok := Lookup("cmsisdap")
	if !ok {
		t.Fatal("cmsisdap missing")
	}
	b := e.Backends[0]
	if b.Validation != ValHardwarePartial || b.Fixture != FixtureReal || b.Cgo != "no" {
		t.Fatalf("%+v", b)
	}
	got := map[string]CapStatus{}
	for _, c := range b.Capabilities {
		got[c.Name] = c.Status
	}
	for name, want := range map[string]CapStatus{
		"target identify": CapRealHW, "halt/resume": CapRealHW, "registers": CapRealHW, "memory read": CapRealHW,
		"single-step": CapRealHW, "SWD target link (DPIDR, AP, memory)": CapRealHW, "v1 HID transport": CapRealHW,
		"memory write": CapRealHW, "register write": CapRealHW, "reset (SYSRESETREQ)": CapRealHW,
		"reset (VECTRESET core reset)": CapUnsupportedHW,
		"breakpoint (FPB)":             CapRealHW, "watchpoint (DWT)": CapUnsupportedHW,
		"reset (nRESET pin, BL702 GPIO24)": CapUnsupportedHW,
		"flash programming":                CapRealHW, "JTAG debugging of a core": CapNotImplemented,
		"v2 bulk transport": CapRealHW, "CDC coexistence": CapRealHW, "reconnect / rediscovery": CapRealHW,
	} {
		if got[name] != want {
			t.Errorf("%s = %q, want %q", name, got[name], want)
		}
	}
}

// The XDS110 native SWD path and the CMSIS-DAP HID path ran the same CC2340
// acceptance (internal/targetaccept/testdata/cc2340). The attached core has a
// 4-slot BPU and no DWT, which is UNSUPPORTED_BY_ATTACHED_HW, never NOT_IMPLEMENTED.
func TestXDS110DoesNotOverclaimTargetOperationsOrEnergyTrace(t *testing.T) {
	e, ok := Lookup("xds110")
	if !ok {
		t.Fatal("xds110 missing")
	}
	b := e.Backends[0]
	if b.Validation != ValHardwarePartial || b.Fixture != FixtureReal || b.Cgo != "no" {
		t.Fatalf("%+v", b)
	}
	got := map[string]CapStatus{}
	for _, c := range b.Capabilities {
		got[c.Name] = c.Status
	}
	for name, want := range map[string]CapStatus{
		"SWD attach / DPIDR / AP discovery": CapRealHW, "target identify": CapRealHW, "memory read": CapRealHW,
		"halt/resume": CapRealHW, "registers": CapRealHW, "single-step": CapRealHW,
		"memory write": CapRealHW, "register write": CapRealHW, "reset (SYSRESETREQ)": CapRealHW,
		"reset (VECTRESET core reset)": CapUnsupportedHW, "reset (nRESET / SRST pin)": CapRealHW,
		"breakpoint (FPB)": CapRealHW, "watchpoint (DWT)": CapUnsupportedHW,
		"JTAG TAP enumeration with a real chain": CapUnsupportedHW,
	} {
		if got[name] != want {
			t.Errorf("%s = %q, want %q", name, got[name], want)
		}
	}
	for name, want := range map[string]CapStatus{
		"EnergyTrace: stream capture (range 0)": CapProtocolPartial, "EnergyTrace: current accuracy against a calibrated load": CapUntested,
		"EnergyTrace: low current ranges (calibration 4 and 1)": CapHypothesisOnly, "EnergyTrace: voltage and energy": CapNotImplemented,
		"EnergyTrace: replay of the real stream": CapReplayVerified, "flash programming": CapRealHW, "target_power_source (jumper P9)": CapRealHW, "target_power_switch": CapUnsupportedHW, "target_power_switch (stand-alone XDS110, id 0x21)": CapUntested, "target_power_measure (EnergyTrace)": CapRealHW,
		"discovery": CapRealHW, "clock control": CapRealHW,
		"reconnect / rediscovery": CapRealHW, "concurrent open": CapRealHW, "CDC / HID coexistence": CapRealHW,
	} {
		if got[name] != want {
			t.Errorf("%s = %q, want %q", name, got[name], want)
		}
	}
}
