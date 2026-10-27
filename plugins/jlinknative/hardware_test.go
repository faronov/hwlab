//go:build hardware && (linux || darwin)

package jlinknativeplugin

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/faronov/hwlab/internal/targetaccept"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
	jusb "github.com/faronov/hwlab/plugins/jlinknative/usb"
)

// Real-hardware tests, opt-in with HWLAB_JLINKNATIVE_HW=1. The probe is chosen
// by USB serial (HWLAB_JLINKNATIVE_SERIAL), or is the only one found. Only a
// probe wired to a user-owned target may be used. The reset phase needs
// HWLAB_JLINKNATIVE_RESET=1; HWLAB_JLINKNATIVE_SCRATCH picks a 256-byte SRAM
// window when the vector table cannot (a target whose initial SP is low); the unplug test needs HWLAB_JLINKNATIVE_UNPLUG=1
// and a person to pull and re-insert the USB cable while it runs.

func hwID(t *testing.T, p *Plugin) string {
	t.Helper()
	if os.Getenv("HWLAB_JLINKNATIVE_HW") != "1" {
		t.Skip("set HWLAB_JLINKNATIVE_HW=1 to run against a real J-Link probe")
	}
	want := os.Getenv("HWLAB_JLINKNATIVE_SERIAL")
	var ids []string
	for _, e := range p.list() {
		if e.USB != nil && (want == "" || e.USB.Serial == want) {
			ids = append(ids, e.ID)
		}
	}
	if len(ids) != 1 {
		t.Skipf("need exactly one J-Link probe (found %v); select one with HWLAB_JLINKNATIVE_SERIAL", ids)
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

func TestHWInfoAndStableID(t *testing.T) {
	p := New()
	id := hwID(t, p)
	if strings.Contains(id, "@") || !strings.HasPrefix(id, "jlink-native:") {
		t.Fatalf("identity %q must be the USB serial", id)
	}
	r, err := hwInvoke(p, id, "DebugProbe", "info", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("info %v", r)
	if r["usb_serial"] == "" || r["synthetic"] != false {
		t.Fatalf("info %v", r)
	}
}

func TestHWConnectIdentify(t *testing.T) {
	p := New()
	id := hwID(t, p)
	r, err := hwInvoke(p, id, "DebugProbe", "connect", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("connect %v", r)
	if r["dpidr"] == nil || r["core"] == nil {
		t.Fatalf("connect %v", r)
	}
	m, err := hwInvoke(p, id, "MemoryAccess", "read", map[string]any{"address": 0xE000ED00, "length": 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("cpuid %v", m)
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
	if err := p.CloseDevice(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if err := q.OpenDevice(context.Background(), id, nil); err != nil {
		t.Fatalf("after release: %v", err)
	}
	_ = q.CloseDevice(context.Background(), id)
}

func TestHWRepeatedOpenConnectClose(t *testing.T) {
	p := New()
	id := hwID(t, p)
	for i := 0; i < 10; i++ {
		if err := p.OpenDevice(context.Background(), id, nil); err != nil {
			t.Fatalf("round %d open: %v", i, err)
		}
		if _, err := hwInvoke(p, id, "DebugProbe", "connect", nil); err != nil {
			t.Fatalf("round %d connect: %v", i, err)
		}
		if err := p.CloseDevice(context.Background(), id); err != nil {
			t.Fatalf("round %d close: %v", i, err)
		}
	}
}

func TestHWClockProbe(t *testing.T) {
	p := New()
	id := hwID(t, p)
	r, err := hwInvoke(p, id, "DebugProbe", "clock_probe", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("clock_probe %v", r)
}

func TestHWAcceptance(t *testing.T) {
	p := New()
	id := hwID(t, p)
	if err := p.OpenDevice(context.Background(), id, nil); err != nil {
		t.Fatal(err)
	}
	defer p.CloseDevice(context.Background(), id)
	var scratch uint32
	if v := os.Getenv("HWLAB_JLINKNATIVE_SCRATCH"); v != "" {
		n, err := strconv.ParseUint(v, 0, 32)
		if err != nil {
			t.Fatalf("HWLAB_JLINKNATIVE_SCRATCH: %v", err)
		}
		scratch = uint32(n)
	}
	rep := targetaccept.Run(func(c, m string, a map[string]any) (map[string]any, error) { return hwInvoke(p, id, c, m, a) },
		targetaccept.Options{
			Backend: "jlink-native", Log: t.Logf, Reset: os.Getenv("HWLAB_JLINKNATIVE_RESET") == "1",
			ResetVectorBreakpoint: true, PerfRepeats: 5, ScratchAddr: scratch,
		})
	for k, v := range rep.Facts {
		t.Logf("fact %s = %s", k, v)
	}
	if err := rep.Err(); err != nil {
		t.Fatal(err)
	}
}

// TestHWWatchpointActualHit makes the core itself access a watched SRAM word.
// The firmware under test is idle, so a four-byte Thumb routine is placed in
// the scratch window ("str r1,[r0]; b ." or "ldr r2,[r0]; b ."), the core is
// pointed at it and a DWT write/read watchpoint must halt it. The window, the
// core registers and the watchpoint hardware are restored afterwards.
func TestHWWatchpointActualHit(t *testing.T) {
	p := New()
	id := hwID(t, p)
	ctx := context.Background()
	const code, data = 0x20007000, 0x20007100
	if err := p.OpenDevice(ctx, id, nil); err != nil {
		t.Fatal(err)
	}
	defer p.CloseDevice(ctx, id)
	call := func(c, m string, a map[string]any) map[string]any {
		t.Helper()
		r, err := hwInvoke(p, id, c, m, a)
		if err != nil {
			t.Fatalf("%s.%s %v: %v", c, m, a, err)
		}
		return r
	}
	rd := func(addr, n int) string {
		return call("MemoryAccess", "read", map[string]any{"address": float64(addr), "length": float64(n)})["hex"].(string)
	}
	call("DebugProbe", "halt", nil)
	defer func() {
		if _, err := hwInvoke(p, id, "DebugProbe", "resume", nil); err != nil {
			t.Errorf("leave target running: %v", err)
		}
	}()
	regs := call("DebugProbe", "read_registers", nil)["registers"].(map[string]any)
	saveCode, saveData := rd(code, 4), rd(data, 4)
	defer func() {
		// best effort: a failure here must not skip the remaining restores
		try := func(c, m string, a map[string]any) {
			if _, err := hwInvoke(p, id, c, m, a); err != nil {
				t.Errorf("restore %s.%s: %v", c, m, err)
			}
		}
		try("DebugProbe", "halt", nil)
		if _, err := hwInvoke(p, id, "DebugProbe", "clear_watchpoint", map[string]any{"all": true}); err != nil && status.Code(err) != codes.NotFound {
			t.Errorf("restore clear_watchpoint: %v", err)
		}
		try("MemoryAccess", "write", map[string]any{"address": float64(code), "data": saveCode})
		try("MemoryAccess", "write", map[string]any{"address": float64(data), "data": saveData})
		for _, n := range []string{"r0", "r1", "r2", "pc"} {
			try("DebugProbe", "write_register", map[string]any{"name": n, "value": regs[n]})
		}
		try("DebugProbe", "resume", nil)
	}()
	for _, tc := range []struct{ name, kind, prog string }{
		{"write", "write", "0160" + "fee7"},
		{"access (ldr)", "access", "0268" + "fee7"},
		{"read", "read", "0268" + "fee7"},
	} {
		call("DebugProbe", "halt", nil)
		call("MemoryAccess", "write", map[string]any{"address": float64(code), "data": tc.prog})
		call("DebugProbe", "write_register", map[string]any{"name": "r0", "value": float64(data)})
		call("DebugProbe", "write_register", map[string]any{"name": "r1", "value": float64(0xA5A5A5A5)})
		call("DebugProbe", "write_register", map[string]any{"name": "pc", "value": float64(code)})
		if _, err := hwInvoke(p, id, "DebugProbe", "set_watchpoint", map[string]any{"address": float64(data), "kind": tc.kind, "mask_bits": 2.0}); err != nil {
			if tc.kind == "read" {
				t.Logf("read-only watchpoint refused honestly by the DWT: %v", err)
				continue
			}
			t.Fatalf("set_watchpoint %s: %v", tc.kind, err)
		}
		call("DebugProbe", "resume", nil)
		w := call("DebugProbe", "wait_halt", map[string]any{"wait_ms": 1500.0})
		t.Logf("%s watchpoint: %v", tc.name, w)
		if w["watchpoint_hit"] != true {
			t.Fatalf("%s watchpoint did not halt the core: %v", tc.name, w)
		}
		if tc.kind == "write" && rd(data, 4) != "a5a5a5a5" {
			t.Fatalf("the routine's write is missing: %s", rd(data, 4))
		}
		if c := call("DebugProbe", "clear_watchpoint", map[string]any{"all": true}); c["installed"] != 0.0 {
			t.Fatalf("watchpoint left behind: %v", c)
		}
	}
}

func TestHWUnplugReplug(t *testing.T) {
	if os.Getenv("HWLAB_JLINKNATIVE_UNPLUG") != "1" {
		t.Skip("set HWLAB_JLINKNATIVE_UNPLUG=1 and pull the probe's USB cable when asked")
	}
	p := New()
	id := hwID(t, p)
	if err := p.OpenDevice(context.Background(), id, nil); err != nil {
		t.Fatal(err)
	}
	before, err := hwInvoke(p, id, "DebugProbe", "connect", nil)
	if err != nil {
		t.Fatal(err)
	}
	// The firmware only has an RTT block for part of its life cycle: reset it
	// so that the block exists, then capture across the unplug. A running
	// capture must end with DEVICE_GONE, not hang or fake data.
	if _, err := hwInvoke(p, id, "DebugProbe", "reset", map[string]any{"kind": "sysreset"}); err != nil {
		t.Fatal(err)
	}
	capCtx, capCancel := context.WithCancel(context.Background())
	defer capCancel()
	capture, _, capErr := p.StartCapture(capCtx, pluginsdk.CaptureRequest{DeviceID: id, Clock: pluginsdk.NewClock(0)}, pluginsdk.NewTestSink(64))
	if capErr != nil {
		t.Logf("no RTT capture during the unplug: %v", capErr)
	}
	t.Log("UNPLUG the probe now")
	if capture != nil {
		time.Sleep(1500 * time.Millisecond)
	}
	deadline := time.Now().Add(300 * time.Second)
	for {
		_, err := hwInvoke(p, id, "MemoryAccess", "read", map[string]any{"address": 0x20000000, "length": 4})
		if err != nil {
			if !IsDeviceGone(err) {
				t.Fatalf("after unplug: want DEVICE_GONE, got %v", err)
			}
			t.Logf("gone: %v", err)
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("probe was not unplugged within 300 s")
		}
		time.Sleep(500 * time.Millisecond)
	}
	if capture != nil {
		done := make(chan error, 1)
		go func() { done <- capture.Wait() }()
		select {
		case err := <-done:
			if !IsDeviceGone(err) {
				t.Errorf("RTT capture ended with %v, want DEVICE_GONE", err)
			} else {
				t.Logf("RTT capture ended: %v", err)
			}
		case <-time.After(15 * time.Second):
			capCancel()
			t.Error("RTT capture did not notice the unplug within 15 s")
		}
	}
	t.Log("REPLUG the probe now")
	deadline = time.Now().Add(120 * time.Second)
	for {
		if _, err := hwInvoke(p, id, "DebugProbe", "connect", nil); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("probe did not return: %v", err)
		}
		time.Sleep(time.Second)
	}
	r, err := hwInvoke(p, id, "DebugProbe", "info", nil)
	if err != nil || r["usb_serial"] == "" {
		t.Fatalf("after replug: %v %v", r, err)
	}
	after, err := hwInvoke(p, id, "DebugProbe", "connect", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"dpidr", "cpuid", "ap_idr"} {
		if before[k] != after[k] {
			t.Errorf("%s changed across unplug: %v -> %v", k, before[k], after[k])
		}
	}
	t.Logf("back with the same id %s, dpidr %v cpuid %v", id, after["dpidr"], after["cpuid"])
}

// TestHWSpeedLadder opens the target at each rate and reads 16 KiB twice: the
// bytes must agree with a read at the reference rate, the time shows the rate
// actually taking effect.
func TestHWSpeedLadder(t *testing.T) {
	p := New()
	id := hwID(t, p)
	r, err := hwInvoke(p, id, "DebugProbe", "clock_probe", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("clock_probe %v", r)
	var ref string
	for _, hz := range []float64{100e3, 400e3, 1e6, 2e6, 4e6, 8e6, 12e6} {
		if err := p.OpenDevice(context.Background(), id, nil); err != nil {
			t.Fatal(err)
		}
		args := map[string]any{"clock_hz": hz}
		if _, err := hwInvoke(p, id, "DebugProbe", "connect", args); err != nil {
			t.Logf("RATE %9.0f Hz: connect failed: %v", hz, err)
			p.CloseDevice(context.Background(), id)
			continue
		}
		best := time.Hour
		var hexd string
		var rerr error
		for i := 0; i < 2; i++ {
			t0 := time.Now()
			res, err := hwInvoke(p, id, "MemoryAccess", "read", map[string]any{"address": float64(0), "length": float64(16384), "clock_hz": hz})
			if err != nil {
				rerr = err
				break
			}
			if d := time.Since(t0); d < best {
				best = d
			}
			hexd, _ = res["hex"].(string)
		}
		p.CloseDevice(context.Background(), id)
		if rerr != nil {
			t.Logf("RATE %9.0f Hz: read failed: %v", hz, rerr)
			continue
		}
		if ref == "" {
			ref = hexd
		}
		t.Logf("RATE %9.0f Hz: 16 KiB read %9s  %6.1f KB/s  identical=%v", hz, best.Round(time.Millisecond), 16384/best.Seconds()/1000, hexd == ref)
		if hexd != ref {
			t.Errorf("%.0f Hz: data differs from the first rate", hz)
		}
	}
}

func TestHWRead64KAt4MHz(t *testing.T) {
	p := New()
	id := hwID(t, p)
	if err := p.OpenDevice(context.Background(), id, nil); err != nil {
		t.Fatal(err)
	}
	defer p.CloseDevice(context.Background(), id)
	if _, err := hwInvoke(p, id, "DebugProbe", "connect", map[string]any{"clock_hz": 4e6}); err != nil {
		if strings.Contains(err.Error(), "above the probe's maximum") {
			t.Skip(err)
		}
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		t0 := time.Now()
		if _, err := hwInvoke(p, id, "MemoryAccess", "read", map[string]any{"address": float64(0), "length": float64(65536), "clock_hz": 4e6}); err != nil {
			t.Fatal(err)
		}
		t.Logf("64 KiB @4 MHz: %s", time.Since(t0).Round(time.Millisecond))
	}
}

// TestHWRTT locates the control block on the real target and captures the up
// channel for HWLAB_JLINKNATIVE_RTT_SECONDS (default 6). It prints metadata and
// counts, not the payload.
func TestHWRTT(t *testing.T) {
	p := New()
	id := hwID(t, p)
	if err := p.OpenDevice(context.Background(), id, nil); err != nil {
		t.Fatal(err)
	}
	defer p.CloseDevice(context.Background(), id)
	args := map[string]any{}
	if v := os.Getenv("HWLAB_JLINKNATIVE_RTT_SIZE"); v != "" {
		args["rtt_search_size"] = v
	}
	info, err := hwInvoke(p, id, "DebugProbe", "rtt_info", args)
	if err != nil {
		t.Skipf("no RTT control block on this target: %v", err)
	}
	t.Logf("rtt_info %v", info)
	secs := 6
	if v := os.Getenv("HWLAB_JLINKNATIVE_RTT_SECONDS"); v != "" {
		secs, _ = strconv.Atoi(v)
	}
	sink := pluginsdk.NewTestSink(4096)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(secs)*time.Second)
	defer cancel()
	cfg := map[string]any{}
	for k, v := range args {
		cfg[k] = v
	}
	c, _, err := p.StartCapture(ctx, pluginsdk.CaptureRequest{DeviceID: id, Config: cfg, Clock: pluginsdk.NewClock(0)}, sink)
	if err != nil {
		t.Fatal(err)
	}
	<-ctx.Done()
	if err := c.Wait(); err != nil {
		t.Fatal(err)
	}
	evs := sink.Drain()
	n := 0
	for _, e := range evs {
		n += len(e.GetLog().GetMessage()) + 1
	}
	t.Logf("%d lines, %d bytes; clock %v", len(evs), n, c.Result().GetClock().GetNote())
}

// TestHWDebugPerf times 100 single steps and 20 halt/resume cycles. The core
// is left running as it was found.
func TestHWDebugPerf(t *testing.T) {
	p := New()
	id := hwID(t, p)
	if err := p.OpenDevice(context.Background(), id, nil); err != nil {
		t.Fatal(err)
	}
	defer p.CloseDevice(context.Background(), id)
	defer func() {
		if _, err := hwInvoke(p, id, "DebugProbe", "resume", nil); err != nil {
			t.Errorf("leave target running: %v", err)
		}
	}()
	call := func(m string, a map[string]any) {
		if _, err := hwInvoke(p, id, "DebugProbe", m, a); err != nil {
			t.Fatalf("%s: %v", m, err)
		}
	}
	call("connect", nil)
	for r := 0; r < 3; r++ {
		call("halt", nil)
		t0 := time.Now()
		call("step", map[string]any{"count": float64(100)})
		t.Logf("100 steps: %s", time.Since(t0).Round(time.Millisecond))
		t0 = time.Now()
		for i := 0; i < 20; i++ {
			call("resume", nil)
			call("halt", nil)
		}
		t.Logf("20x resume+halt: %s", time.Since(t0).Round(time.Millisecond))
		call("resume", nil)
	}
}

type recTransport struct {
	jusb.Transport
	steps []wireStep
}

func (r *recTransport) Exchange(ctx context.Context, cmd []byte, n int) ([]byte, error) {
	rep, err := r.Transport.Exchange(ctx, cmd, n)
	if err == nil {
		r.steps = append(r.steps, wireStep{Cmd: hex.EncodeToString(cmd), Reply: hex.EncodeToString(rep)})
	}
	return rep, err
}

// TestHWRecordWireFixture records wireScript on the real probe into the file
// named by HWLAB_JLINKNATIVE_RECORD (testdata/real-jlink-native/wire.json).
func TestHWRecordWireFixture(t *testing.T) {
	out := os.Getenv("HWLAB_JLINKNATIVE_RECORD")
	if out == "" {
		t.Skip("set HWLAB_JLINKNATIVE_RECORD=<file> to record the wire fixture")
	}
	base := New()
	id := hwID(t, base)
	var rec *recTransport
	p := NewWith(base.list, func(e Entry) (jusb.Transport, error) {
		tr, err := defaultOpen(e)
		if err != nil {
			return nil, err
		}
		rec = &recTransport{Transport: tr}
		return rec, nil
	})
	if err := p.OpenDevice(context.Background(), id, nil); err != nil {
		t.Fatal(err)
	}
	defer func() {
		p.mu.Lock()
		open := p.devs[id] != nil
		p.mu.Unlock()
		if !open {
			return
		}
		if _, err := hwInvoke(p, id, "DebugProbe", "resume", nil); err != nil {
			t.Errorf("leave target running: %v", err)
		}
		if err := p.CloseDevice(context.Background(), id); err != nil {
			t.Errorf("close recorder: %v", err)
		}
	}()
	for _, c := range wireScript {
		if _, err := hwInvoke(p, id, c.cap, c.method, c.args); err != nil {
			t.Fatalf("%s.%s: %v", c.cap, c.method, err)
		}
	}
	if err := p.CloseDevice(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	w := wireFile{
		Note:   "REAL wire recording of the native J-Link stack; no serial, RTT payload or firmware content. Replayed by TestRealWireFixtureReplaysWithoutHardware.",
		Probe:  "SAM-ICE class J-Link (reported J-Link V9, HW 9.70, USB 1366:0105)",
		Target: "EFR32MG1PxxxF256, SWD",
		Steps:  rec.steps,
	}
	b, err := json.MarshalIndent(w, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("recorded %d exchanges", len(rec.steps))
}
