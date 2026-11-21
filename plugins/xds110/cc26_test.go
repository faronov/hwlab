package xds110plugin

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/faronov/hwlab/internal/ti/cc26xx"
	"github.com/faronov/hwlab/plugins/xds110/xds"
)

func cc26Plugin(t *testing.T) (*Plugin, *xds.Sim) {
	t.Helper()
	sim := xds.NewSimCC26x2()
	list := func() []Entry { return []Entry{{ID: SimID, Sim: sim}} }
	open := func(e Entry) (xds.Transport, error) { return e.Sim, nil }
	return NewWith(list, open), sim
}

func cjtagCfg(extra map[string]any) map[string]any {
	cfg := map[string]any{"interface": "cjtag"}
	for k, v := range extra {
		cfg[k] = v
	}
	return cfg
}

var allowAll = map[string]any{"allow_flash": true, "allow_erase": true, "allow_ccfg_write": true, "allow_chip_erase": true}

func TestCC26ChipInfoThroughICEPick(t *testing.T) {
	p, sim := cc26Plugin(t)
	openSim(t, p, cjtagCfg(nil))
	r := mustInvoke(t, p, "DebugProbe", "chip_info", nil)
	bd, _ := r["bootloader"].(map[string]any)
	if r["chip"] != "CC2652P" || r["ieee"] != "00:12:4B:00:11:22:33:44" || r["debug_locked"] != false || r["flash_bytes"] != float64(xds.SimCC26Flash) ||
		r["jtag_idcode"] != "0x3bb4102f" || r["cpu_dap_idcode"] != "0x4ba00477" || bd["backdoor_pin"] != float64(15) || r["core_halted"] != false {
		t.Fatalf("%v", r)
	}
	// A new activation from the active two-pin format needs the probe's
	// reset run first; the attach does it, so detaching and attaching again works.
	mustInvoke(t, p, "DebugProbe", "disconnect", nil)
	if sim.CC26Format() != xds.FormatOScan1 {
		t.Fatalf("format %d", sim.CC26Format())
	}
	if r := mustInvoke(t, p, "DebugProbe", "chip_info", nil); r["chip"] != "CC2652P" {
		t.Fatalf("%v", r)
	}
}

func TestCC26CoreAndMemoryOverCJTAG(t *testing.T) {
	p, _ := cc26Plugin(t)
	openSim(t, p, cjtagCfg(nil))
	if r := mustInvoke(t, p, "DebugProbe", "halt", nil); r["halted"] != true {
		t.Fatalf("%v", r)
	}
	mustInvoke(t, p, "MemoryAccess", "write", map[string]any{"address": "0x20004000", "data": "0102030405060708090a"})
	r := mustInvoke(t, p, "MemoryAccess", "read", map[string]any{"address": "0x20004000", "length": 10.0})
	if r["hex"] != "0102030405060708090a" {
		t.Fatalf("%v", r)
	}
	r = mustInvoke(t, p, "MemoryAccess", "read", map[string]any{"address": "0x50001318", "length": 4.0})
	if r["hex"] != "2f10b43b" {
		t.Fatalf("%v", r)
	}
	if r := mustInvoke(t, p, "DebugProbe", "connect", nil); r["core"] != "Cortex-M4" || r["dpidr"] != "0x4ba00477" {
		t.Fatalf("%v", r)
	}
	if r := mustInvoke(t, p, "DebugProbe", "resume", nil); r["halted"] != false {
		t.Fatalf("%v", r)
	}
	// SYSRESETREQ resets the debug logic and drops the two-pin format: the
	// plugin re-attaches and proves the reset by the cleared C_DEBUGEN.
	r = mustInvoke(t, p, "DebugProbe", "reset", nil)
	if r["reattached"] != true || r["reset_seen"] != true || r["debug_lost"] != true || r["boot_rom_wait"] != true || r["left_boot_rom"] != true || r["halted"] != false {
		t.Fatalf("%v", r)
	}
	if r := mustInvoke(t, p, "DebugProbe", "chip_info", nil); r["chip"] != "CC2652P" {
		t.Fatalf("after reset: %v", r)
	}
	// The boot ROM waits for the debugger after the reset; a breakpoint on
	// the reset vector stops the core at the application's first instruction.
	r = mustInvoke(t, p, "DebugProbe", "reset", map[string]any{"halt": true})
	if r["halted"] != true || r["at_reset_vector"] != true || r["pc"] != r["reset_vector"] {
		t.Fatalf("%v", r)
	}
	if bp := mustInvoke(t, p, "DebugProbe", "list_breakpoints", nil); bp["slots_in_use"] != float64(0) {
		t.Fatalf("breakpoint left behind: %v", bp)
	}
}

func TestCC26DebugLockedIsReported(t *testing.T) {
	p, sim := cc26Plugin(t)
	sim.SetDAPLocked(true)
	openSim(t, p, cjtagCfg(nil))
	r := mustInvoke(t, p, "DebugProbe", "chip_info", nil)
	if r["debug_locked"] != true || r["cpu_dap_linked"] != false || r["icepick_idcode"] != "0x3bb4102f" || !strings.Contains(r["recovery"].(string), "cc26xx_chip_erase") {
		t.Fatalf("%v", r)
	}
	_, err := invoke(t, p, SimID, "MemoryAccess", "read", map[string]any{"address": 0.0, "length": 4.0})
	wantErr(t, err, codes.FailedPrecondition, DebugLocked)
}

func TestCC26TwoPinTargetOnFourPinScan(t *testing.T) {
	p, _ := cc26Plugin(t)
	openSim(t, p, nil)
	// The four-pin scan reads TDO, which a two-pin board does not wire.
	if r := mustInvoke(t, p, "DebugProbe", "jtag_scan", nil); r["status"] != "stuck-low" || !strings.Contains(r["hint"].(string), "interface=cjtag") {
		t.Fatalf("four-pin scan of a two-pin target: %v", r)
	}
	r := mustInvoke(t, p, "DebugProbe", "jtag_scan", map[string]any{"interface": "cjtag"})
	if r["icepick_idcode"] != "0x3bb4102f" || r["cpu_dap_linked"] != true {
		t.Fatalf("%v", r)
	}
}

func cc26Backup(t *testing.T, p *Plugin) string {
	t.Helper()
	r := mustInvoke(t, p, "FlashProgramming", "backup", map[string]any{"dir": t.TempDir()})
	if r["identical_reads"] != true {
		t.Fatalf("%v", r)
	}
	return r["path"].(string)
}

func TestCC26BackupProgramRestore(t *testing.T) {
	p, sim := cc26Plugin(t)
	openSim(t, p, cjtagCfg(allowAll))
	orig := sim.CC26Flash()
	bk := cc26Backup(t, p)
	if b, _ := os.ReadFile(bk); !bytes.Equal(b, orig) {
		t.Fatal("backup differs from the flash")
	}
	img := bytes.Repeat([]byte{0x5A, 0xA5, 0x00, 0x11}, 3000) // 12000 bytes over two sectors
	path := filepath.Join(t.TempDir(), "img.bin")
	if err := os.WriteFile(path, img, 0o600); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"path": path, "address": "0x6000", "confirm": tokProgram, "backup": bk}
	// Tokens and backup are checked.
	_, err := invoke(t, p, SimID, "FlashProgramming", "program", map[string]any{"path": path, "address": "0x6000", "backup": bk})
	wantErr(t, err, codes.PermissionDenied, tokProgram)
	_, err = invoke(t, p, SimID, "FlashProgramming", "program", map[string]any{"path": path, "address": "0x6000", "confirm": tokProgram})
	wantErr(t, err, codes.PermissionDenied, "backup")
	r := mustInvoke(t, p, "FlashProgramming", "program", args)
	if r["verified"] != true || r["sectors_changed"] != float64(2) || r["config_sector_changes"] != false || r["reset"] == nil {
		t.Fatalf("%v", r)
	}
	f := sim.CC26Flash()
	if !bytes.Equal(f[0x6000:0x6000+len(img)], img) || !bytes.Equal(f[:0x6000], orig[:0x6000]) || !bytes.Equal(f[0x6000+len(img):], orig[0x6000+len(img):]) {
		t.Fatal("flash content after program")
	}
	if calls := strings.Join(sim.ROMCalls(), ","); !strings.HasPrefix(calls, "erase,program") {
		t.Fatalf("ROM calls %s", calls)
	}
	// Programming the same image again changes nothing.
	if r := mustInvoke(t, p, "FlashProgramming", "program", args); r["sectors_changed"] != float64(0) {
		t.Fatalf("%v", r)
	}
	if r := mustInvoke(t, p, "FlashProgramming", "verify", map[string]any{"path": path, "address": "0x6000"}); r["match"] != true {
		t.Fatalf("%v", r)
	}
	r = mustInvoke(t, p, "FlashProgramming", "restore", map[string]any{"backup": bk, "confirm": tokRestore})
	if r["matches_backup"] != true || r["sectors_changed"] != float64(2) {
		t.Fatalf("%v", r)
	}
	if !bytes.Equal(sim.CC26Flash(), orig) {
		t.Fatal("restore did not bring the flash back")
	}
}

func TestCC26ModificationNeedsDevicePermissions(t *testing.T) {
	p, _ := cc26Plugin(t)
	openSim(t, p, cjtagCfg(nil))
	bk := cc26Backup(t, p)
	_, err := invoke(t, p, SimID, "FlashProgramming", "program", map[string]any{"data": "00", "address": "0x2000", "confirm": tokProgram, "backup": bk})
	wantErr(t, err, codes.PermissionDenied, "allow_flash")
	_, err = invoke(t, p, SimID, "FlashProgramming", "erase", map[string]any{"address": "0x2000", "length": 8192.0, "confirm": tokErase, "backup": bk})
	wantErr(t, err, codes.PermissionDenied, "allow_erase")
	_, err = invoke(t, p, SimID, "DebugProbe", "cc26xx_chip_erase", map[string]any{"confirm": tokICEPickErase})
	wantErr(t, err, codes.PermissionDenied, "allow_chip_erase")
}

func TestCC26CCFGSectorIsProtected(t *testing.T) {
	p, sim := cc26Plugin(t)
	openSim(t, p, cjtagCfg(allowAll))
	bk := cc26Backup(t, p)
	ccfgAddr := uint32(xds.SimCC26Flash - cc26xx.CCFGLen)
	cur := sim.CC26Flash()[ccfgAddr:]
	locked := append([]byte(nil), cur...)
	locked[cc26xx.CCFGTapDap0+2] = 0x00 // CPU_DAP_ENABLE off
	args := map[string]any{"data": hex.EncodeToString(locked), "address": float64(ccfgAddr), "confirm": tokProgram, "backup": bk}
	_, err := invoke(t, p, SimID, "FlashProgramming", "program", args)
	wantErr(t, err, codes.PermissionDenied, "confirm_config")
	args["confirm_config"] = tokCCFG
	_, err = invoke(t, p, SimID, "FlashProgramming", "program", args)
	wantErr(t, err, codes.PermissionDenied, "CPU_DAP_ENABLE")
	if !bytes.Equal(sim.CC26Flash()[ccfgAddr:], cur) {
		t.Fatal("a refused CCFG write changed the flash")
	}
	// A harmless CCFG change (IEEE override) passes with confirm_config.
	ok := append([]byte(nil), cur...)
	copy(ok[cc26xx.CCFGIEEEMAC0:], []byte{1, 2, 3, 4, 5, 6, 7, 8})
	args["data"] = hex.EncodeToString(ok)
	if r := mustInvoke(t, p, "FlashProgramming", "program", args); r["config_sector_changes"] != true || r["verified"] != true {
		t.Fatalf("%v", r)
	}
	if !bytes.Equal(sim.CC26Flash()[ccfgAddr:], ok) {
		t.Fatal("CCFG not written")
	}
}

func TestCC26Erase(t *testing.T) {
	p, sim := cc26Plugin(t)
	openSim(t, p, cjtagCfg(allowAll))
	bk := cc26Backup(t, p)
	r := mustInvoke(t, p, "FlashProgramming", "erase", map[string]any{"address": "0x0", "length": 8192.0, "confirm": tokErase, "backup": bk})
	if r["blank_verified"] != true || !cc26xx.Blank(sim.CC26Flash()[:8192]) {
		t.Fatalf("%v", r)
	}
	// Erasing the CCFG sector by region would leave it blank: refused
	// without the override.
	last := float64(xds.SimCC26Flash - 8192)
	_, err := invoke(t, p, SimID, "FlashProgramming", "erase", map[string]any{"address": last, "length": 8192.0, "confirm": tokErase, "backup": bk, "confirm_config": tokCCFG})
	wantErr(t, err, codes.PermissionDenied, "blank")
	// A bank erase keeps debug and the bootloader backdoor open.
	r = mustInvoke(t, p, "FlashProgramming", "erase", map[string]any{"mode": "bank", "confirm": tokBankErase, "backup": bk, "confirm_config": tokCCFG})
	if r["blank_verified"] != true {
		t.Fatalf("%v", r)
	}
	f := sim.CC26Flash()
	cfg := f[xds.SimCC26Flash-cc26xx.CCFGLen:]
	if !cc26xx.Blank(f[:xds.SimCC26Flash-8192]) || cc26xx.DebugLocked(cfg) || cc26xx.BackdoorOf(cfg).Pin != 15 || cc26xx.Blank(cfg) {
		t.Fatalf("after bank erase: ccfg %x", cfg)
	}
}

func TestCC26ChipEraseThroughICEPick(t *testing.T) {
	p, sim := cc26Plugin(t)
	sim.SetDAPLocked(true)
	openSim(t, p, cjtagCfg(map[string]any{"allow_chip_erase": true}))
	_, err := invoke(t, p, SimID, "DebugProbe", "cc26xx_chip_erase", map[string]any{"confirm": "yes"})
	wantErr(t, err, codes.PermissionDenied, tokICEPickErase)
	r := mustInvoke(t, p, "DebugProbe", "cc26xx_chip_erase", map[string]any{"confirm": tokICEPickErase})
	if r["reattached"] != true || r["first_4k_blank"] != true || sim.ChipErases() != 1 {
		t.Fatalf("%v", r)
	}
	if r := mustInvoke(t, p, "DebugProbe", "chip_info", nil); r["debug_locked"] != false {
		t.Fatalf("%v", r)
	}
}

func TestCC26MethodsAreAdvertised(t *testing.T) {
	p, _ := cc26Plugin(t)
	var names []string
	for _, m := range p.Info().Methods {
		names = append(names, m.Capability+"."+m.Name)
		if m.Capability == "FlashProgramming" && m.Name == "program" && (!strings.Contains(m.Description, "interface=cjtag") || !strings.Contains(m.ArgsSchemaJson, "confirm_config")) {
			t.Fatalf("program spec %q %s", m.Description, m.ArgsSchemaJson)
		}
	}
	for _, want := range []string{"DebugProbe.chip_info", "DebugProbe.icepick_info", "DebugProbe.cc26xx_chip_erase"} {
		if !strings.Contains(strings.Join(names, " "), want) {
			t.Fatalf("%s missing from %v", want, names)
		}
	}
}
