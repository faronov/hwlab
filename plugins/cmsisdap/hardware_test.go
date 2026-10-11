//go:build hardware && linux

package cmsisdapplugin

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"google.golang.org/protobuf/types/known/structpb"
)

// Plugin-level real-hardware tests, opt-in with HWLAB_CMSISDAP_HW=1. The probe
// is selected by USB serial (HWLAB_CMSISDAP_SERIAL) or is the only one found.
// The unplug/replug test additionally needs HWLAB_CMSISDAP_UNPLUG=1 and a
// person to pull and re-insert the USB cable while it runs.

func hwID(t *testing.T, p *Plugin) string {
	t.Helper()
	if os.Getenv("HWLAB_CMSISDAP_HW") != "1" {
		t.Skip("set HWLAB_CMSISDAP_HW=1 to run against a real CMSIS-DAP probe")
	}
	want := os.Getenv("HWLAB_CMSISDAP_SERIAL")
	var ids []string
	for _, e := range p.list() {
		if e.USB != nil && (want == "" || e.USB.Serial == want) {
			ids = append(ids, e.ID)
		}
	}
	if len(ids) != 1 {
		t.Skipf("need exactly one CMSIS-DAP probe (found %v); select one with HWLAB_CMSISDAP_SERIAL", ids)
	}
	return ids[0]
}

func hwInvoke(p *Plugin, id, capName, method string, args map[string]any) (map[string]any, error) {
	s, _ := structpb.NewStruct(args)
	r, err := p.Invoke(context.Background(), &pluginv1.InvokeRequest{DeviceId: id, Capability: capName, Method: method, Args: s})
	if err != nil {
		return nil, err
	}
	return r.Result.AsMap(), nil
}

func TestHWPluginInfoAndStableID(t *testing.T) {
	p := New()
	id := hwID(t, p)
	if strings.Contains(id, "@") {
		t.Fatalf("identity %q is based on the USB topology, not the serial", id)
	}
	r, err := hwInvoke(p, id, "DebugProbe", "info", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("info %v", r)
	if r["usb_serial"] == "" || r["jtag_driven"] != "chain-scan-only" {
		t.Fatalf("info %v", r)
	}
}

func TestHWPluginSecondOpenIsBusy(t *testing.T) {
	p, q := New(), New()
	id := hwID(t, p)
	if err := p.OpenDevice(context.Background(), id, nil); err != nil {
		t.Fatal(err)
	}
	defer p.CloseDevice(context.Background(), id)
	err := q.OpenDevice(context.Background(), id, nil)
	if !IsBusy(err) {
		t.Fatalf("second open: want BUSY, got %v", err)
	}
	if _, err := hwInvoke(q, id, "DebugProbe", "info", nil); !IsBusy(err) {
		t.Fatalf("one-shot call while held elsewhere: want BUSY, got %v", err)
	}
	if err := p.CloseDevice(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if err := q.OpenDevice(context.Background(), id, nil); err != nil {
		t.Fatalf("after release: %v", err)
	}
	_ = q.CloseDevice(context.Background(), id)
}

func TestHWPluginUnknownSerial(t *testing.T) {
	p := New()
	hwID(t, p)
	if _, err := hwInvoke(p, "d6e7:3507:NO-SUCH", "DebugProbe", "info", nil); err == nil {
		t.Fatal("an unknown serial must not resolve")
	}
}

func TestHWPluginNoTargetIsNeverSuccess(t *testing.T) {
	p := New()
	id := hwID(t, p)
	if _, err := hwInvoke(p, id, "DebugProbe", "connect", nil); err == nil {
		t.Log("a target answered: this hardware has a target attached")
		return
	} else {
		t.Logf("connect without a target: %v", err)
	}
	if _, err := hwInvoke(p, id, "MemoryAccess", "read", map[string]any{"address": 0x20000000, "length": 4}); err == nil {
		t.Fatal("memory read succeeded with no target")
	}
}

func TestHWPluginClockProbeAndPins(t *testing.T) {
	p := New()
	id := hwID(t, p)
	r, err := hwInvoke(p, id, "DebugProbe", "clock_probe", map[string]any{"rates_hz": []any{100000.0, 400000.0, 1e6, 2e6, 4e6}})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("clock_probe %v", r)
	pr, err := hwInvoke(p, id, "DebugProbe", "pins", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("pins %v", pr)
}

// TestHWPluginUnplugReplug watches one probe across a physical unplug and
// replug. It prints progress; the test fails only if a call hangs, succeeds
// while the device is gone, or the same serial does not come back.
func TestHWPluginUnplugReplug(t *testing.T) {
	if os.Getenv("HWLAB_CMSISDAP_UNPLUG") != "1" {
		t.Skip("set HWLAB_CMSISDAP_UNPLUG=1 and unplug/replug the probe while this runs")
	}
	p := New()
	id := hwID(t, p)
	if err := p.OpenDevice(context.Background(), id, nil); err != nil {
		t.Fatal(err)
	}
	t.Logf("READY: probe %s is open; unplug it now", id)
	deadline := time.Now().Add(150 * time.Second)
	var gone bool
	var goneAfter time.Duration
	start := time.Now()
	for time.Now().Before(deadline) && !gone {
		done := make(chan error, 1)
		go func() { _, err := hwInvoke(p, id, "DebugProbe", "info", nil); done <- err }()
		select {
		case err := <-done:
			if err != nil {
				t.Logf("%s: %v (device_gone=%v)", time.Since(start).Round(time.Millisecond), err, IsDeviceGone(err))
				if IsDeviceGone(err) {
					gone, goneAfter = true, time.Since(start)
				}
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a call hung for 10 s")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !gone {
		t.Fatal("the probe was never reported DEVICE_GONE (not unplugged?)")
	}
	t.Logf("DEVICE_GONE reported %s after start", goneAfter.Round(time.Millisecond))
	for i := 0; i < 5; i++ { // no stale success while it is away
		if _, err := hwInvoke(p, id, "DebugProbe", "status", nil); err == nil {
			t.Fatal("stale success while the probe is unplugged")
		}
	}
	t.Log("WAITING for replug")
	back := time.Now()
	for time.Now().Before(deadline.Add(120 * time.Second)) {
		if r, err := hwInvoke(p, id, "DebugProbe", "info", nil); err == nil {
			t.Logf("reconnected after %s: serial=%v caps=%v", time.Since(back).Round(time.Millisecond), r["usb_serial"], r["capabilities"])
			_ = p.CloseDevice(context.Background(), id)
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatal("the same serial did not come back after replug")
}

func TestHWPluginJTAGScanWithoutTarget(t *testing.T) {
	p := New()
	id := hwID(t, p)
	r, err := hwInvoke(p, id, "DebugProbe", "jtag_scan", nil)
	t.Logf("jtag_scan: %v err=%v", r, err)
	if err == nil {
		if n, _ := r["devices"].([]any); len(n) != 0 {
			t.Log("a JTAG chain answered: this hardware has a JTAG target attached")
			return
		}
	}
	// whatever the outcome, the probe must stay usable in SWD afterwards
	if _, err := hwInvoke(p, id, "DebugProbe", "info", nil); err != nil {
		t.Fatalf("probe unusable after a JTAG scan: %v", err)
	}
}
