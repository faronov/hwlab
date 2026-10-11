package xds110plugin

import (
	"os"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/internal/ti/cc23xx/service"
	"github.com/faronov/hwlab/plugins/xds110/xds"
)

func ccPlugin(t *testing.T) (*Plugin, *xds.Sim) {
	t.Helper()
	sim := xds.NewSimCC23xx()
	list := func() []Entry { return []Entry{{ID: SimID, Sim: sim}} }
	open := func(e Entry) (xds.Transport, error) { return e.Sim, nil }
	return NewWith(list, open), sim
}

func wantErr(t *testing.T, err error, c codes.Code, text string) {
	t.Helper()
	if err == nil || status.Code(err) != c || !strings.Contains(err.Error(), text) {
		t.Fatalf("want %v %q, got %v", c, text, err)
	}
}

func TestCC23xxCoreMethodLeavesSACI(t *testing.T) {
	p, _ := ccPlugin(t)
	openSim(t, p, nil)
	if st := mustInvoke(t, p, "DebugProbe", "cc23xx_info", nil); st["in_saci"] != true {
		t.Fatalf("%v", st)
	}
	// The device waits in SACI after attach; a core method lets it in.
	r := mustInvoke(t, p, "DebugProbe", "halt", nil)
	if r["halted"] != true {
		t.Fatalf("%v", r)
	}
	b := mustInvoke(t, p, "MemoryAccess", "read", map[string]any{"address": 0.0, "length": 8.0})
	if b["hex"] == nil {
		t.Fatalf("%v", b)
	}
}

func TestCC23xxKeepSACI(t *testing.T) {
	p, _ := ccPlugin(t)
	openSim(t, p, map[string]any{"saci_exit": "none"})
	_, err := invoke(t, p, SimID, "DebugProbe", "halt", nil)
	wantErr(t, err, codes.FailedPrecondition, "SACI_ACTIVE")
	if r := mustInvoke(t, p, "DebugProbe", "saci_status", nil); r["saci_active"] != true {
		t.Fatalf("%v", r)
	}
	mustInvoke(t, p, "DebugProbe", "saci_exit", nil)
	if r := mustInvoke(t, p, "DebugProbe", "halt", nil); r["halted"] != true {
		t.Fatalf("%v", r)
	}
}

func TestCC23xxReset(t *testing.T) {
	p, _ := ccPlugin(t)
	openSim(t, p, nil)
	for _, kind := range []string{"sysreset", "hardware"} {
		r := mustInvoke(t, p, "DebugProbe", "reset", map[string]any{"kind": kind, "halt": true})
		if r["halted"] != true || r["at_reset_vector"] != true || r["reset_seen"] != true {
			t.Fatalf("%s: %v", kind, r)
		}
		r = mustInvoke(t, p, "DebugProbe", "reset", map[string]any{"kind": kind})
		if r["halted"] != false {
			t.Fatalf("%s: %v", kind, r)
		}
		if s := mustInvoke(t, p, "DebugProbe", "status", nil); s["halted"] == true {
			t.Fatalf("%s: running after reset: %v", kind, s)
		}
	}
	_, err := invoke(t, p, SimID, "DebugProbe", "reset", map[string]any{"kind": "core"})
	wantErr(t, err, codes.FailedPrecondition, "UNSUPPORTED_BY_ATTACHED_HW")
}

func TestCC23xxFlashPermissionsAreDeviceLevel(t *testing.T) {
	p, _ := ccPlugin(t)
	dir := t.TempDir()
	t.Setenv("HWLAB_HOME", dir)
	openSim(t, p, nil)
	bk := mustInvoke(t, p, "FlashProgramming", "backup", map[string]any{"dir": dir})["path"].(string)
	args := map[string]any{"confirm": service.TokEraseChip, "backup": bk, "allow_flash": true, "allow_erase": true, "allow_chip_erase": true}
	_, err := invoke(t, p, SimID, "FlashProgramming", "erase", args)
	wantErr(t, err, codes.PermissionDenied, "FLASH_PERMISSION_DENIED")
	// A one-shot lease cannot grant them either.
	p2, _ := ccPlugin(t)
	_, err = invoke(t, p2, SimID, "FlashProgramming", "erase", args)
	wantErr(t, err, codes.PermissionDenied, "FLASH_PERMISSION_DENIED")
	// Unknown keys at open are still rejected.
	if err := p.OpenDevice(t.Context(), SimID, map[string]any{"allow_flash": "yes"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("%v", err)
	}
}

func TestCC23xxEraseRestoreThroughPlugin(t *testing.T) {
	p, _ := ccPlugin(t)
	dir := t.TempDir()
	openSim(t, p, map[string]any{"allow_flash": true, "allow_erase": true, "allow_chip_erase": true})
	bk := mustInvoke(t, p, "FlashProgramming", "backup", map[string]any{"dir": dir})["path"].(string)
	if _, err := os.Stat(bk + ".json"); err != nil {
		t.Fatal(err)
	}
	r := mustInvoke(t, p, "FlashProgramming", "erase", map[string]any{"confirm": service.TokEraseChip, "backup": bk})
	if r["erased"] != true {
		t.Fatalf("%v", r)
	}
	// An erased device cannot run: SACI refuses to exit without a CCFG.
	_, err := invoke(t, p, SimID, "DebugProbe", "halt", nil)
	wantErr(t, err, codes.FailedPrecondition, "CCFG_INVALID")
	r = mustInvoke(t, p, "FlashProgramming", "restore", map[string]any{"path": bk, "confirm": service.TokRestore, "reset": "run"})
	if r["restored"] != true {
		t.Fatalf("%v", r)
	}
	if r := mustInvoke(t, p, "DebugProbe", "halt", nil); r["halted"] != true {
		t.Fatalf("%v", r)
	}
}

func TestFlashOnNonCC23xxIsUnsupportedByHardware(t *testing.T) {
	p, _ := simPlugin(t)
	_, err := invoke(t, p, SimID, "DebugProbe", "saci_status", nil)
	wantErr(t, err, codes.FailedPrecondition, "UNSUPPORTED_BY_ATTACHED_HW")
}
