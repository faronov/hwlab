package cmsisdapplugin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/internal/usb"
	"github.com/faronov/hwlab/plugins/cmsisdap/dap"
)

func openSim(t *testing.T, p *Plugin) {
	t.Helper()
	if err := p.OpenDevice(context.Background(), SimID, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.CloseDevice(context.Background(), SimID) })
}

func TestStepCountReportsEveryStep(t *testing.T) {
	p, _ := simPlugin(t)
	mustInvoke(t, p, "DebugProbe", "halt", nil)
	r := mustInvoke(t, p, "DebugProbe", "step", map[string]any{"count": 100.0})
	if r["steps"] != 100.0 || r["all_halted"] != true || r["pc_changed"] != 100.0 || r["halted"] != true {
		t.Fatalf("step %v", r)
	}
	if _, err := invoke(t, p, SimID, "DebugProbe", "step", map[string]any{"count": 0.0}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("count 0: %v", err)
	}
	mustInvoke(t, p, "DebugProbe", "resume", nil)
	if _, err := invoke(t, p, SimID, "DebugProbe", "step", nil); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("step of a running core: %v", err)
	}
}

func TestBreakpointHitAndRestore(t *testing.T) {
	p, _ := simPlugin(t)
	if _, err := invoke(t, p, SimID, "DebugProbe", "set_breakpoint", map[string]any{"address": 0x100}); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "open device") {
		t.Fatalf("a breakpoint without an open device must be refused: %v", err)
	}
	openSim(t, p)
	start := mustInvoke(t, p, "DebugProbe", "halt", nil)["pc"]
	target := mustInvoke(t, p, "DebugProbe", "step", map[string]any{"count": 4.0})["pc"]
	mustInvoke(t, p, "DebugProbe", "write_register", map[string]any{"name": "pc", "value": start})
	lb := mustInvoke(t, p, "DebugProbe", "list_breakpoints", nil)
	total := lb["slots_total"].(float64)
	if total < 1 || lb["slots_in_use"] != 0.0 {
		t.Fatalf("list %v", lb)
	}
	b := mustInvoke(t, p, "DebugProbe", "set_breakpoint", map[string]any{"address": target, "run_wait_ms": 1000.0})
	if b["hit"] != true || b["halted_pc"] != target || b["slots_total"] != total {
		t.Fatalf("breakpoint %v", b)
	}
	if _, err := invoke(t, p, SimID, "DebugProbe", "set_breakpoint", map[string]any{"address": target}); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("duplicate: %v", err)
	}
	// resuming from the breakpoint steps over it instead of re-halting at once
	rs := mustInvoke(t, p, "DebugProbe", "resume", nil)
	if rs["stepped_over_breakpoint"] != true {
		t.Fatalf("resume %v", rs)
	}
	mustInvoke(t, p, "DebugProbe", "halt", nil)
	c := mustInvoke(t, p, "DebugProbe", "clear_breakpoint", map[string]any{"address": target})
	if c["installed"] != 0.0 || c["original_fpb_state_restored"] != true {
		t.Fatalf("clear %v", c)
	}
	if _, err := invoke(t, p, SimID, "DebugProbe", "clear_breakpoint", map[string]any{"address": target}); status.Code(err) != codes.NotFound {
		t.Fatalf("clear twice: %v", err)
	}
}

func TestWatchpointSetListClear(t *testing.T) {
	p, _ := simPlugin(t)
	openSim(t, p)
	w := mustInvoke(t, p, "DebugProbe", "set_watchpoint", map[string]any{"address": 0x20000100, "kind": "write", "mask_bits": 2.0})
	if w["size_bytes"] != 4.0 || w["kind"] != "write" || w["comparators_total"].(float64) < 1 {
		t.Fatalf("set %v", w)
	}
	l := mustInvoke(t, p, "DebugProbe", "list_watchpoints", nil)
	if l["in_use"] != 1.0 {
		t.Fatalf("list %v", l)
	}
	if _, err := invoke(t, p, SimID, "DebugProbe", "set_watchpoint", map[string]any{"address": 0x20000101, "kind": "write", "mask_bits": 2.0}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unaligned: %v", err)
	}
	c := mustInvoke(t, p, "DebugProbe", "clear_watchpoint", map[string]any{"all": true})
	if c["installed"] != 0.0 {
		t.Fatalf("clear %v", c)
	}
	if l := mustInvoke(t, p, "DebugProbe", "list_watchpoints", nil); l["in_use"] != 0.0 {
		t.Fatalf("not restored: %v", l)
	}
}

func TestResetKinds(t *testing.T) {
	p, sim := simPlugin(t)
	for _, k := range []string{"sysreset", "core"} {
		r := mustInvoke(t, p, "DebugProbe", "reset", map[string]any{"kind": k, "halt": true})
		if r["halted"] != true || r["at_reset_vector"] != true || r["kind"] != k {
			t.Fatalf("%s: %v", k, r)
		}
	}
	before := sim.Resets()
	r := mustInvoke(t, p, "DebugProbe", "reset", map[string]any{"kind": "hardware", "halt": true})
	if r["halted"] != true || r["reset_seen"] != true || sim.Resets() <= before {
		t.Fatalf("hardware: %v (resets %d -> %d)", r, before, sim.Resets())
	}
}

func TestPinResetWithoutEvidenceIsNotClaimed(t *testing.T) {
	p, sim := simPlugin(t)
	sim.IgnorePinReset(true)
	_, err := invoke(t, p, SimID, "DebugProbe", "reset", map[string]any{"kind": "hardware"})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "NOT claimed") {
		t.Fatalf("an unobserved pin reset must be an error: %v", err)
	}
}

func TestDAPResetNotImplementedIsNotClaimed(t *testing.T) {
	p, _ := simPlugin(t)
	_, err := invoke(t, p, SimID, "DebugProbe", "reset", map[string]any{"kind": "dap_reset"})
	if err == nil || !strings.Contains(err.Error(), "nothing was reset") {
		t.Fatalf("got %v", err)
	}
}

func TestApScan(t *testing.T) {
	p, _ := simPlugin(t)
	r := mustInvoke(t, p, "DebugProbe", "ap_scan", nil)
	aps := r["aps"].([]any)
	if len(aps) < 1 || aps[0].(map[string]any)["index"] != 0.0 {
		t.Fatalf("ap_scan %v", r)
	}
}

func TestPinsAndClockProbeNeedNoTarget(t *testing.T) {
	p, sim := simPlugin(t)
	sim.SetNoTarget(true)
	r := mustInvoke(t, p, "DebugProbe", "pins", nil)
	if r["levels"] == nil || !strings.Contains(r["note"].(string), "does not prove") {
		t.Fatalf("pins %v", r)
	}
	c := mustInvoke(t, p, "DebugProbe", "clock_probe", map[string]any{"rates_hz": []any{100000.0, 1000000.0}})
	rows := c["clocks"].([]any)
	if len(rows) != 2 || rows[0].(map[string]any)["accepted"] != true {
		t.Fatalf("clock_probe %v", c)
	}
	if _, err := invoke(t, p, SimID, "DebugProbe", "clock_probe", map[string]any{"rates_hz": []any{5.0}}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad rate: %v", err)
	}
}

func TestJTAGScanNeedsReportedJTAG(t *testing.T) {
	p, _ := simPlugin(t)
	_, err := invoke(t, p, SimID, "DebugProbe", "jtag_scan", nil)
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "JTAG") {
		t.Fatalf("got %v", err)
	}
}

// goneTransport wraps the simulator and fails like usbfs does after an unplug.
type goneTransport struct {
	*dap.Sim
	gone *atomic.Bool
}

func (g goneTransport) Exchange(req []byte, d time.Duration) ([]byte, error) {
	if g.gone.Load() {
		return nil, fmt.Errorf("DAP command 0x%02x: %w", req[0], usb.ErrDisconnected)
	}
	return g.Sim.Exchange(req, d)
}

func TestDeviceGoneThenReconnectBySerial(t *testing.T) {
	sim := dap.NewSim()
	var gone atomic.Bool
	opens := 0
	list := func() []Entry { return []Entry{{ID: SimID, Sim: sim}} }
	open := func(e Entry) (dap.Transport, error) {
		if gone.Load() {
			return nil, fmt.Errorf("open: %w", usb.ErrDisconnected)
		}
		opens++
		return goneTransport{Sim: sim, gone: &gone}, nil
	}
	p := NewWith(list, open)
	if err := p.OpenDevice(context.Background(), SimID, nil); err != nil {
		t.Fatal(err)
	}
	mustInvoke(t, p, "DebugProbe", "halt", nil)
	gone.Store(true)
	for i := 0; i < 2; i++ { // no stale success, and it never hangs
		done := make(chan error, 1)
		go func() { _, err := invoke(t, p, SimID, "DebugProbe", "status", nil); done <- err }()
		select {
		case err := <-done:
			if !IsDeviceGone(err) {
				t.Fatalf("round %d: want DEVICE_GONE, got %v", i, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("hang after unplug")
		}
	}
	if _, err := invoke(t, p, SimID, "DebugProbe", "info", nil); !IsDeviceGone(err) {
		t.Fatalf("info while gone: %v", err)
	}
	gone.Store(false)
	r := mustInvoke(t, p, "DebugProbe", "info", nil)
	if r["serial"] != "SIM0001" || opens < 2 {
		t.Fatalf("reopen: %v (opens %d)", r, opens)
	}
	mustInvoke(t, p, "DebugProbe", "status", nil)
}

func TestBusyIsExplicit(t *testing.T) {
	sim := dap.NewSim()
	list := func() []Entry { return []Entry{{ID: SimID, Sim: sim}} }
	open := func(Entry) (dap.Transport, error) {
		return nil, fmt.Errorf("usbfs: %w: interface 0 is claimed by another process", usb.ErrBusy)
	}
	p := NewWith(list, open)
	err := p.OpenDevice(context.Background(), SimID, nil)
	if !IsBusy(err) || IsDeviceGone(err) {
		t.Fatalf("want BUSY, got %v", err)
	}
	if _, err := invoke(t, p, SimID, "DebugProbe", "info", nil); !IsBusy(err) {
		t.Fatalf("transient call: want BUSY, got %v", err)
	}
}

func TestReopenReleasesOwnHandleFirst(t *testing.T) {
	sim := dap.NewSim()
	list := func() []Entry { return []Entry{{ID: SimID, Sim: sim}} }
	held := false
	open := func(Entry) (dap.Transport, error) {
		if held {
			return nil, fmt.Errorf("%w: held by this process", usb.ErrBusy)
		}
		held = true
		return releasing{Sim: sim, held: &held}, nil
	}
	p := NewWith(list, open)
	for i := 0; i < 3; i++ {
		if err := p.OpenDevice(context.Background(), SimID, nil); err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
	}
	if err := p.CloseDevice(context.Background(), SimID); err != nil {
		t.Fatal(err)
	}
	if held {
		t.Fatal("the interface is still claimed after CloseDevice")
	}
}

type releasing struct {
	*dap.Sim
	held *bool
}

func (r releasing) Close() error { *r.held = false; return nil }

func TestUSBErrorsClassify(t *testing.T) {
	cases := []struct {
		err  error
		code codes.Code
		gone bool
		busy bool
	}{
		{usb.ErrDisconnected, codes.Unavailable, true, false},
		{fmt.Errorf("x: %w", usb.ErrBusy), codes.FailedPrecondition, false, true},
		{usb.ErrPermission, codes.PermissionDenied, false, false},
		{usb.ErrTimeout, codes.DeadlineExceeded, false, false},
		{errors.New("plain"), codes.FailedPrecondition, false, false},
	}
	for _, c := range cases {
		got := classify(c.err)
		if status.Code(got) != c.code || IsDeviceGone(got) != c.gone || IsBusy(got) != c.busy {
			t.Errorf("%v -> %v (gone %v busy %v)", c.err, got, IsDeviceGone(got), IsBusy(got))
		}
	}
}
