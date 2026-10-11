//go:build hardware

package espusbjtag

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/faronov/hwlab/internal/usb"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
)

// Opt-in: HWLAB_ESPJTAG_HW=1 go test -tags hardware ./plugins/espusbjtag/
//
// Needs an Espressif USB Serial/JTAG device (303a:1001) and write access to
// its usbfs node (udev rule, docs/hardware.md). Only read-only operations run:
// JTAG scan, halt/resume, register and memory reads. Nothing is written,
// erased, flashed or reset.
//
// HWLAB_ESPJTAG_SERIAL=<usb serial> selects a device when several are
// attached.

type hw struct {
	*Plugin
	id string
}

func hwRig(t *testing.T) hw {
	t.Helper()
	if os.Getenv("HWLAB_ESPJTAG_HW") != "1" {
		t.Skip("set HWLAB_ESPJTAG_HW=1 to run against a real device")
	}
	p := New()
	devs, err := p.ListDevices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := os.Getenv("HWLAB_ESPJTAG_SERIAL")
	for _, d := range devs {
		if d.Properties["vid"] != "303a" || strings.HasPrefix(d.Serial, "SIM:") {
			continue
		}
		if want == "" || d.Serial == want {
			return hw{p, d.Id}
		}
	}
	t.Skip("no Espressif USB Serial/JTAG device attached")
	return hw{}
}

func (h hw) call(t *testing.T, capName, method string, args map[string]any) map[string]any {
	t.Helper()
	s, _ := structpb.NewStruct(args)
	res, err := h.Invoke(context.Background(), &pluginv1.InvokeRequest{DeviceId: h.id, Capability: capName, Method: method, Args: s})
	if err != nil {
		t.Fatalf("%s.%s: %v", capName, method, err)
	}
	return res.Result.AsMap()
}

func ifaceDriver(t *testing.T, serial string, n int) string {
	t.Helper()
	for _, d := range usb.Scan("/sys", func(vid, pid uint16) bool { return vid == 0x303a && pid == 0x1001 }) {
		if d.Serial != serial {
			continue
		}
		for _, i := range d.Interfaces {
			if i.Number == n {
				return i.Driver
			}
		}
	}
	t.Fatalf("interface %d of %s not found", n, serial)
	return ""
}

func TestHardwareIdentity(t *testing.T) {
	h := hwRig(t)
	devs, _ := h.ListDevices(context.Background())
	for _, d := range devs {
		if d.Id != h.id {
			continue
		}
		p := d.Properties
		if p["vid"] != "303a" || p["pid"] != "1001" || p["jtag_interface"] != "2" || p["companion_cdc_interfaces"] != "0,1" {
			t.Errorf("identity: %v", p)
		}
		return
	}
	t.Fatal("device vanished")
}

func TestHardwareScanIsStable(t *testing.T) {
	h := hwRig(t)
	for i := 0; i < 3; i++ {
		r := h.call(t, pluginv1.CapJTAGScan, "scan", map[string]any{"measure_ir": true})
		devs, _ := r["devices"].([]any)
		if r["status"] != "ok" || len(devs) != 1 {
			t.Fatalf("scan %d: %v", i, r)
		}
		id := devs[0].(map[string]any)["idcode"].(map[string]any)
		if id["raw"] != float64(0xdc25) || devs[0].(map[string]any)["ir_length"] != float64(5) {
			t.Fatalf("scan %d: %v", i, devs[0])
		}
	}
}

func TestHardwareHaltReadResume(t *testing.T) {
	h := hwRig(t)
	serial := ""
	devs, _ := h.ListDevices(context.Background())
	for _, d := range devs {
		if d.Id == h.id {
			serial = d.Serial
		}
	}
	// Whatever happens, leave the CPU running.
	t.Cleanup(func() {
		s, _ := structpb.NewStruct(nil)
		_, _ = h.Invoke(context.Background(), &pluginv1.InvokeRequest{DeviceId: h.id, Capability: pluginv1.CapDebugProbe, Method: "resume", Args: s})
	})
	if r := h.call(t, pluginv1.CapDebugProbe, "halt", nil); r["halted"] != true {
		t.Fatalf("halt: %v", r)
	}
	regs := h.call(t, pluginv1.CapDebugProbe, "read_registers", map[string]any{"keep_halted": true})["registers"].(map[string]any)
	if regs["misa"] != "0x40903105" || regs["mvendorid"] != "0x00000612" || regs["marchid"] != "0x80000002" {
		t.Errorf("core identity: %v", regs)
	}
	for _, a := range []float64{0x40000000, 0x40800000, 0x42000000} {
		r := h.call(t, pluginv1.CapMemoryAccess, "read", map[string]any{"address": a, "length": 16})
		if len(r["hex"].(string)) != 32 {
			t.Errorf("read 0x%x: %v", uint32(a), r)
		}
	}
	if r := h.call(t, pluginv1.CapDebugProbe, "resume", nil); r["halted"] != false {
		t.Fatalf("resume: %v", r)
	}
	if d := ifaceDriver(t, serial, 0); d != "cdc_acm" {
		t.Errorf("interface 0 driver = %q, want cdc_acm", d)
	}
}

func TestHardwareRepeatedOpenClose(t *testing.T) {
	h := hwRig(t)
	for i := 0; i < 10; i++ {
		r := h.call(t, pluginv1.CapTargetIdentify, "identify", nil)
		if r["count"] != float64(1) {
			t.Fatalf("round %d: %v", i, r)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestHardwareSecondOpenIsRefused(t *testing.T) {
	h := hwRig(t)
	if err := h.OpenDevice(context.Background(), h.id, nil); err != nil {
		t.Fatal(err)
	}
	defer h.CloseDevice(context.Background(), h.id)
	// A different process cannot claim interface 2 while this one holds it.
	second := New()
	if err := second.OpenDevice(context.Background(), h.id, nil); err == nil {
		_ = second.CloseDevice(context.Background(), h.id)
		t.Fatal("a second opener was not refused")
	}
}
