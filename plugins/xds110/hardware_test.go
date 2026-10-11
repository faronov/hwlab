//go:build hardware && linux

package xds110plugin

import (
	"context"
	"os"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/faronov/hwlab/internal/usb"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	cmsisdapplugin "github.com/faronov/hwlab/plugins/cmsisdap"
	"github.com/faronov/hwlab/plugins/xds110/xds"
)

// Plugin-level real-hardware tests, opt-in with HWLAB_XDS110_HW=1. The probe is
// selected by USB serial (HWLAB_XDS110_SERIAL) or is the only one found. They
// need NO target on the probe's debug connector: target-side operations are
// expected to fail cleanly, and the tests assert that they do. The
// unplug/replug test additionally needs HWLAB_XDS110_UNPLUG=1 and a person to
// pull and re-insert the USB cable while it runs.

func hwID(t *testing.T, p *Plugin) string {
	t.Helper()
	if os.Getenv("HWLAB_XDS110_HW") != "1" {
		t.Skip("set HWLAB_XDS110_HW=1 to run against a real XDS110")
	}
	want := os.Getenv("HWLAB_XDS110_SERIAL")
	var ids []string
	for _, e := range p.list() {
		if e.USB != nil && (want == "" || e.USB.Serial == want) {
			ids = append(ids, e.ID)
		}
	}
	if len(ids) != 1 {
		t.Skipf("need exactly one XDS110 probe (found %v); select one with HWLAB_XDS110_SERIAL", ids)
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

func hwDevice(t *testing.T, id string) usb.Device {
	t.Helper()
	for _, d := range usb.Scan("/sys", xds.Match) {
		if DeviceID(d) == id {
			return d
		}
	}
	t.Fatalf("%s not found in sysfs", id)
	return usb.Device{}
}

func TestHWInfoAndStableID(t *testing.T) {
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
	if r["firmware"] == "" || r["hardware_id"] == "" {
		t.Fatalf("info %v", r)
	}
}

func TestHWSecondOpenIsBusy(t *testing.T) {
	p, q := New(), New()
	id := hwID(t, p)
	if err := p.OpenDevice(context.Background(), id, nil); err != nil {
		t.Fatal(err)
	}
	defer p.CloseDevice(context.Background(), id)
	if err := q.OpenDevice(context.Background(), id, nil); !IsBusy(err) {
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

func TestHWRepeatedOpenClose(t *testing.T) {
	p := New()
	id := hwID(t, p)
	for i := 0; i < 25; i++ {
		if err := p.OpenDevice(context.Background(), id, nil); err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		if _, err := hwInvoke(p, id, "DebugProbe", "info", nil); err != nil {
			t.Fatalf("info %d: %v", i, err)
		}
		if err := p.CloseDevice(context.Background(), id); err != nil {
			t.Fatalf("close %d: %v", i, err)
		}
	}
}

// TestHWOnlyTheDebugInterfaceIsClaimed proves the composite device is shared:
// while hwlab holds the XDS110 interface, every other interface keeps its
// kernel driver and the CDC serial ports can still be opened.
func TestHWOnlyTheDebugInterfaceIsClaimed(t *testing.T) {
	p := New()
	id := hwID(t, p)
	before := hwDevice(t, id)
	if err := p.OpenDevice(context.Background(), id, nil); err != nil {
		t.Fatal(err)
	}
	defer p.CloseDevice(context.Background(), id)
	if _, err := hwInvoke(p, id, "DebugProbe", "info", nil); err != nil {
		t.Fatal(err)
	}
	during := hwDevice(t, id)
	l, _ := xds.LayoutFor(during.VID, during.PID)
	var ttys []string
	for _, i := range during.Interfaces {
		b, _ := before.Iface(i.Number)
		if i.Number == l.Interface {
			t.Logf("interface %d: %q -> %q (held by hwlab)", i.Number, b.Driver, i.Driver)
			continue
		}
		if i.Driver != b.Driver {
			t.Errorf("interface %d driver changed %q -> %q while hwlab holds the debug interface", i.Number, b.Driver, i.Driver)
		}
		if i.TTY != "" {
			ttys = append(ttys, i.TTY)
		}
	}
	if len(ttys) == 0 {
		t.Fatal("no CDC tty present")
	}
	sort.Strings(ttys)
	for _, tty := range ttys {
		f, err := os.OpenFile("/dev/"+tty, os.O_RDWR|syscall.O_NOCTTY|syscall.O_NONBLOCK, 0)
		if err != nil {
			t.Errorf("/dev/%s cannot be opened while hwlab owns the debug interface: %v", tty, err)
			continue
		}
		t.Logf("/dev/%s opened while the debug interface is held", tty)
		_ = f.Close()
	}
	for _, n := range []int{5, 6} {
		if a, b := before.Interfaces[n], during.Interfaces[n]; a.Driver != b.Driver {
			t.Errorf("interface %d changed %q -> %q", n, a.Driver, b.Driver)
		}
	}
}

func TestHWNoTargetIsNeverSuccess(t *testing.T) {
	p := New()
	id := hwID(t, p)
	_, err := hwInvoke(p, id, "DebugProbe", "connect", nil)
	if err == nil {
		t.Log("a target answered: this hardware has a target attached")
		return
	}
	t.Logf("connect without a target: %v", err)
	if _, err := hwInvoke(p, id, "MemoryAccess", "read", map[string]any{"address": 0x20000000, "length": 4}); err == nil {
		t.Fatal("memory read succeeded with no target")
	}
	if _, err := hwInvoke(p, id, "DebugProbe", "info", nil); err != nil {
		t.Fatalf("probe unusable after a failed attach: %v", err)
	}
}

func TestHWClockProbe(t *testing.T) {
	p := New()
	id := hwID(t, p)
	r, err := hwInvoke(p, id, "DebugProbe", "clock_probe", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range r["clocks"].([]any) {
		m := row.(map[string]any)
		t.Logf("%v", m)
		if m["accepted"] != true {
			t.Errorf("clock rejected: %v", m)
		}
	}
	t.Logf("usb round trip %vus", r["usb_round_trip_us"])
}

func TestHWJTAGScanWithoutTarget(t *testing.T) {
	p := New()
	id := hwID(t, p)
	r, err := hwInvoke(p, id, "DebugProbe", "jtag_scan", nil)
	t.Logf("jtag_scan: %v err=%v", r, err)
	if err == nil && r["status"] == "ok" {
		t.Log("a JTAG chain answered: this hardware has a JTAG target attached")
	}
	if _, err := hwInvoke(p, id, "DebugProbe", "info", nil); err != nil {
		t.Fatalf("probe unusable after a JTAG scan: %v", err)
	}
}

func TestHWCommandLatency(t *testing.T) {
	devs := usb.Scan("/sys", xds.Match)
	if os.Getenv("HWLAB_XDS110_HW") != "1" || len(devs) == 0 {
		t.Skip("needs HWLAB_XDS110_HW=1 and a probe")
	}
	tr, err := xds.OpenUSB(devs[0])
	if err != nil {
		t.Fatal(err)
	}
	pr := xds.NewProbe(tr)
	defer pr.Close()
	if _, err := pr.Connect(); err != nil {
		t.Fatal(err)
	}
	const n = 500
	ts := make([]time.Duration, n)
	for i := range ts {
		t0 := time.Now()
		if _, err := pr.Version(); err != nil {
			t.Fatal(err)
		}
		ts[i] = time.Since(t0)
	}
	sort.Slice(ts, func(i, j int) bool { return ts[i] < ts[j] })
	t.Logf("XDS_VERSION round trip over %d calls: min %v median %v p95 %v max %v", n, ts[0], ts[n/2], ts[n*95/100], ts[n-1])
}

// TestHWUnplugReplug watches one probe across a physical unplug and replug. It
// fails if a call hangs, succeeds while the device is gone, or the same serial
// does not come back.
func TestHWUnplugReplug(t *testing.T) {
	if os.Getenv("HWLAB_XDS110_UNPLUG") != "1" {
		t.Skip("set HWLAB_XDS110_UNPLUG=1 and unplug/replug the probe while this runs")
	}
	p := New()
	id := hwID(t, p)
	if err := p.OpenDevice(context.Background(), id, nil); err != nil {
		t.Fatal(err)
	}
	t.Logf("READY: probe %s is open; unplug it now", id)
	deadline := time.Now().Add(150 * time.Second)
	var gone bool
	start := time.Now()
	for time.Now().Before(deadline) && !gone {
		done := make(chan error, 1)
		go func() { _, err := hwInvoke(p, id, "DebugProbe", "info", nil); done <- err }()
		select {
		case err := <-done:
			if err != nil {
				t.Logf("%s: %v (device_gone=%v)", time.Since(start).Round(time.Millisecond), err, IsDeviceGone(err))
				gone = IsDeviceGone(err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a call hung for 10 s")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !gone {
		t.Fatal("the probe was never reported DEVICE_GONE (not unplugged?)")
	}
	for i := 0; i < 5; i++ {
		if _, err := hwInvoke(p, id, "DebugProbe", "status", nil); err == nil {
			t.Fatal("stale success while the probe is unplugged")
		}
	}
	t.Log("WAITING for replug")
	back := time.Now()
	for time.Now().Before(deadline.Add(120 * time.Second)) {
		if r, err := hwInvoke(p, id, "DebugProbe", "info", nil); err == nil {
			t.Logf("reconnected after %s: firmware=%v hw=%v", time.Since(back).Round(time.Millisecond), r["firmware"], r["hardware_id"])
			_ = p.CloseDevice(context.Background(), id)
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatal("the same serial did not come back after replug")
}

// TestHWCoexistsWithCMSISDAPFace: the same probe also exposes a CMSIS-DAP v1
// HID interface that the cmsisdap plugin drives through hidraw. The two
// plugins use different USB interfaces, so both can be open at once.
func TestHWCoexistsWithCMSISDAPFace(t *testing.T) {
	p := New()
	id := hwID(t, p)
	if err := p.OpenDevice(context.Background(), id, nil); err != nil {
		t.Fatal(err)
	}
	defer p.CloseDevice(context.Background(), id)
	c := cmsisdapplugin.New()
	if err := c.OpenDevice(context.Background(), id, nil); err != nil {
		t.Fatalf("the CMSIS-DAP face cannot be opened while the XDS110 interface is held: %v", err)
	}
	defer c.CloseDevice(context.Background(), id)
	s, _ := structpb.NewStruct(nil)
	r, err := c.Invoke(context.Background(), &pluginv1.InvokeRequest{DeviceId: id, Capability: "DebugProbe", Method: "info", Args: s})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("cmsisdap info while xds110 is open: product=%v dap=%v", r.Result.AsMap()["product"], r.Result.AsMap()["dap_protocol_version"])
	if _, err := hwInvoke(p, id, "DebugProbe", "info", nil); err != nil {
		t.Fatalf("xds110 broke after the CMSIS-DAP face was used: %v", err)
	}
}
