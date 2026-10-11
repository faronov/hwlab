//go:build hardware

package jlink

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
)

// Run with: go test -tags=hardware ./plugins/jlink/ -run Hardware -v
//
// Needs a J-Link (or a J-Link OB board) on USB and SEGGER's J-Link Commander
// installed. Without a probe the tests skip. Environment:
//
//	HWLAB_JLINK_SERIAL   probe serial number to use. With several probes
//	                     attached and no serial the single-probe tests skip:
//	                     the plugin never guesses between probes.
//	HWLAB_JLINK_TARGET   J-Link device name, e.g. NRF52840_XXAA; enables the
//	                     target tests (connect, memory read, halt, registers...).
//	HWLAB_JLINK_PROBES   "serial=device,serial=device" for the two-probe test,
//	                     e.g. 90000001=EFR32MG1PxxxF256,900000002=NRF52840_XXAA
//	HWLAB_JLINK_RTT=1    also run the RTT test (the target must run RTT firmware).
//
// The tests only read: no flash, erase or memory write.

func realProbe(t *testing.T) (*Plugin, string) {
	t.Helper()
	p := New()
	devs, err := p.ListDevices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) == 0 {
		t.Skip("no J-Link probe (USB 1366) connected")
	}
	want := os.Getenv("HWLAB_JLINK_SERIAL")
	if want == "" {
		if len(devs) > 1 {
			t.Skipf("%d J-Link probes attached: set HWLAB_JLINK_SERIAL to pick one", len(devs))
		}
		want = devs[0].Id
	}
	for _, d := range devs {
		if d.Id == NormalizeSerial(want) {
			t.Logf("probe: %s serial %s caps %d", d.Name, d.Id, len(d.Capabilities))
			return p, d.Id
		}
	}
	t.Fatalf("probe %s is not attached (have %d probes)", want, len(devs))
	return nil, ""
}

func invokeHW(p *Plugin, id, capName, method string, args map[string]any) (map[string]any, error) {
	st, err := structpb.NewStruct(args)
	if err != nil {
		return nil, err
	}
	resp, err := p.Invoke(context.Background(), &pluginv1.InvokeRequest{DeviceId: id, Capability: capName, Method: method, Args: st})
	if err != nil {
		return nil, err
	}
	return resp.GetResult().AsMap(), nil
}

func runRTTHW(t *testing.T, ctx context.Context, p *Plugin, id string, cfg map[string]any) ([]*pluginv1.Event, *pluginv1.StopCaptureResponse) {
	t.Helper()
	sink := pluginsdk.NewTestSink(256)
	c, _, err := p.StartCapture(ctx, pluginsdk.CaptureRequest{DeviceID: id, Config: cfg, Clock: pluginsdk.NewClock(0)}, sink)
	if err != nil {
		t.Fatal(err)
	}
	<-ctx.Done()
	if err := c.Wait(); err != nil {
		t.Fatal(err)
	}
	return sink.Drain(), c.Result()
}

func TestHardwareProbeInfo(t *testing.T) {
	p, id := realProbe(t)
	if _, err := p.cmd.Available(); err != nil {
		t.Skipf("commander not installed: %v", err)
	}
	res, err := invokeHW(p, id, "DebugProbe", "info", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("info: %v", res)
	if res["firmware"] == nil {
		t.Errorf("no firmware line parsed: the output format may differ from the parser: %v", res["output"])
	}
}

func TestHardwareConnectAndReadMemory(t *testing.T) {
	p, id := realProbe(t)
	target := os.Getenv("HWLAB_JLINK_TARGET")
	if target == "" {
		t.Skip("HWLAB_JLINK_TARGET not set")
	}
	res, err := invokeHW(p, id, "DebugProbe", "connect", map[string]any{"target": target})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("connect: %v", res)
	if res["ok"] != true {
		t.Errorf("connection not confirmed: %v", res)
	}
	mem, err := invokeHW(p, id, "MemoryAccess", "read", map[string]any{"target": target, "address": "0x0", "length": float64(16)})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("flash[0:16] = %v", mem["hex"])
}

func TestHardwareRTT(t *testing.T) {
	if os.Getenv("HWLAB_JLINK_RTT") != "1" {
		t.Skip("HWLAB_JLINK_RTT not set")
	}
	p, id := realProbe(t)
	target := os.Getenv("HWLAB_JLINK_TARGET")
	if target == "" {
		t.Skip("HWLAB_JLINK_TARGET not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	evs, res := runRTTHW(t, ctx, p, id, map[string]any{"target": target})
	var app int
	for _, e := range evs {
		if e.GetLog().GetSource() == LogSource {
			app++
		}
	}
	t.Logf("%d events (%d from the target), clock %+v", len(evs), app, res.GetClock())
	if app == 0 {
		t.Skip("no target RTT output (banner only): the target is not running RTT firmware")
	}
}

func TestHardwareEnumeratesEveryProbeAndInfoMatchesSerial(t *testing.T) {
	p := New()
	devs, err := p.ListDevices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) == 0 {
		t.Skip("no J-Link probe connected")
	}
	if _, err := p.cmd.Available(); err != nil {
		t.Skipf("commander not installed: %v", err)
	}
	seen := map[string]bool{}
	for _, d := range devs {
		if seen[d.Id] {
			t.Errorf("serial %s listed twice", d.Id)
		}
		seen[d.Id] = true
		res, err := invokeHW(p, d.Id, "DebugProbe", "info", nil)
		if err != nil {
			t.Errorf("%s: %v", d.Id, err)
			continue
		}
		if res["serial"] != d.Id {
			t.Errorf("asked for probe %s, answered from %v", d.Id, res["serial"])
		}
		t.Logf("%s: model=%v nickname=%v firmware=%v hardware=%v", d.Id, res["model"], res["nickname"], res["firmware"], res["hardware"])
	}
}

func TestHardwareTargetLifecycle(t *testing.T) {
	p, id := realProbe(t)
	target := os.Getenv("HWLAB_JLINK_TARGET")
	if target == "" {
		t.Skip("HWLAB_JLINK_TARGET not set")
	}
	lifecycle(t, p, id, target)
}

// lifecycle runs the non-destructive sequence: connect, identify, flash read,
// halt (confirmed with IsHalted), registers, resume, reset, reconnect.
func lifecycle(t *testing.T, p *Plugin, id, target string) {
	t.Helper()
	a := map[string]any{"target": target}
	step := func(capName, method string, args map[string]any) map[string]any {
		t.Helper()
		res, err := invokeHW(p, id, capName, method, args)
		if err != nil {
			t.Fatalf("%s %s on probe %s: %v", capName, method, id, err)
		}
		if res["probe_serial"] != nil && (res["probe_serial"] != id || res["target"] != target) {
			t.Fatalf("%s answered as %v/%v, want %s/%s", method, res["probe_serial"], res["target"], id, target)
		}
		return res
	}
	if r := step("DebugProbe", "connect", a); r["ok"] != true || r["core"] == nil {
		t.Fatalf("connect: %v", r)
	}
	if r := step("MemoryAccess", "read", map[string]any{"target": target, "address": "0x0", "length": float64(16)}); len(r["hex"].(string)) != 32 {
		t.Fatalf("flash read: %v", r)
	}
	if r := step("DebugProbe", "halt", a); r["halted"] != true {
		t.Fatalf("halt: %v", r)
	}
	if r := step("DebugProbe", "read_registers", a); r["registers"].(map[string]any)["PC"] == nil {
		t.Fatalf("registers: %v", r)
	}
	if r := step("DebugProbe", "resume", a); r["halted"] != false {
		t.Fatalf("resume: %v", r)
	}
	if r := step("DebugProbe", "reset", a); r["halted"] != false {
		t.Fatalf("reset: %v", r)
	}
	if r := step("DebugProbe", "connect", a); r["ok"] != true {
		t.Fatalf("reconnect: %v", r)
	}
}

// TestHardwareTwoProbesSwitch drives two probes alternately and requires that
// every answer comes from the probe that was asked.
func TestHardwareTwoProbesSwitch(t *testing.T) {
	spec := os.Getenv("HWLAB_JLINK_PROBES")
	if spec == "" {
		t.Skip("HWLAB_JLINK_PROBES not set")
	}
	var ids, targets []string
	for _, kv := range strings.Split(spec, ",") {
		s, d, ok := strings.Cut(kv, "=")
		if !ok || s == "" || d == "" {
			t.Fatalf("HWLAB_JLINK_PROBES %q: want serial=device,serial=device", spec)
		}
		ids, targets = append(ids, NormalizeSerial(s)), append(targets, d)
	}
	if len(ids) < 2 {
		t.Skip("needs two probes")
	}
	p := New()
	devs, err := p.ListDevices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, d := range devs {
		have[d.Id] = true
	}
	for _, id := range ids {
		if !have[id] {
			t.Skipf("probe %s is not attached", id)
		}
	}
	for round := 0; round < 3; round++ {
		for i, id := range ids {
			res, err := invokeHW(p, id, "DebugProbe", "connect", map[string]any{"target": targets[i]})
			if err != nil || res["probe_serial"] != id || res["target"] != targets[i] {
				t.Fatalf("round %d probe %s: %v err=%v", round, id, res, err)
			}
		}
	}
	// Concurrent calls to different probes must not mix up either.
	var wg sync.WaitGroup
	errs := make(chan error, len(ids))
	for i, id := range ids {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			res, err := invokeHW(p, id, "MemoryAccess", "read", map[string]any{"target": targets[i], "address": "0x0", "length": float64(8)})
			if err == nil && res["probe_serial"] != id {
				err = fmt.Errorf("asked %s, answered %v", id, res["probe_serial"])
			}
			errs <- err
		}(i, id)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	for i, id := range ids {
		lifecycle(t, p, id, targets[i])
	}
}
