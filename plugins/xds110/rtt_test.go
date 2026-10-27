package xds110plugin

import (
	"context"
	"encoding/binary"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/internal/cortexm"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
)

const (
	simCB   = 0x20000400
	simUp   = 0x20000500
	simDown = 0x20000600
)

// placeRTT writes a control block with one 32-byte up and one 16-byte down
// buffer into the simulated SRAM.
func placeRTT(t *testing.T, c *cortexm.Sim) {
	t.Helper()
	b := make([]byte, 24+48)
	copy(b, "SEGGER RTT")
	binary.LittleEndian.PutUint32(b[16:], 1)
	binary.LittleEndian.PutUint32(b[20:], 1)
	binary.LittleEndian.PutUint32(b[24+4:], simUp)
	binary.LittleEndian.PutUint32(b[24+8:], 32)
	binary.LittleEndian.PutUint32(b[48+4:], simDown)
	binary.LittleEndian.PutUint32(b[48+8:], 16)
	if err := c.WriteBlock(context.Background(), simCB, b); err != nil {
		t.Fatal(err)
	}
}

// targetWrite emulates firmware appending to the up ring.
func targetWrite(t *testing.T, p *Plugin, c *cortexm.Sim, wr uint32, data string) uint32 {
	t.Helper()
	// The simulated core is not goroutine safe: serialise with the poller.
	d := p.devs[SimID]
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := 0; i < len(data); i++ {
		if err := c.Store(simUp+(wr+uint32(i))%32, 1, uint32(data[i])); err != nil {
			t.Fatal(err)
		}
	}
	wr = (wr + uint32(len(data))) % 32
	if err := c.Store(simCB+24+12, 4, wr); err != nil {
		t.Fatal(err)
	}
	return wr
}

func startRTTCapture(t *testing.T, p *Plugin, cfg map[string]any, sink *pluginsdk.Sink) (context.CancelFunc, pluginsdk.Capture, *pluginsdk.CaptureStart) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c, start, err := p.StartCapture(ctx, pluginsdk.CaptureRequest{DeviceID: SimID, Clock: pluginsdk.NewClock(0), Config: cfg}, sink)
	if err != nil {
		t.Fatal(err)
	}
	return cancel, c, start
}

func rttLines(t *testing.T, sink *pluginsdk.Sink) (lines, levels []string) {
	t.Helper()
	for _, e := range sink.Drain() {
		l := e.GetLog()
		if l.GetSource() != RTTLogSource || e.GetStreamId() != "rtt" {
			t.Fatalf("event %v", e)
		}
		lines, levels = append(lines, l.Message), append(levels, l.Level)
	}
	return lines, levels
}

func TestInfoAdvertisesRTT(t *testing.T) {
	p, _ := simPlugin(t)
	danger := map[string]pluginv1.Danger{}
	for _, m := range p.Info().Methods {
		danger[m.Capability+"."+m.Name] = m.Danger
	}
	if danger["DebugProbe.rtt_info"] != pluginv1.Danger_DANGER_SAFE || danger["DebugProbe.rtt_write"] != pluginv1.Danger_DANGER_CONFIRM {
		t.Fatalf("rtt_info must be SAFE and rtt_write CONFIRM: %v", danger)
	}
	ds, err := p.ListDevices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range ds[0].Capabilities {
		if c.Attributes["rtt"] != "target-memory" {
			t.Fatalf("rtt attribute: %v", c.Attributes)
		}
	}
}

func TestRTTInfoAndWriteSim(t *testing.T) {
	p, s := simPlugin(t)
	openSim(t, p, nil)
	if _, err := invoke(t, p, SimID, "DebugProbe", "rtt_info", nil); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("no control block must be reported, got %v", err)
	}
	placeRTT(t, s.Core())
	r := mustInvoke(t, p, "DebugProbe", "rtt_info", nil)
	if r["control_block"] != "0x20000400" || len(r["up"].([]any)) != 1 || len(r["down"].([]any)) != 1 {
		t.Fatalf("rtt_info %v", r)
	}
	r = mustInvoke(t, p, "DebugProbe", "rtt_info", map[string]any{"rtt_address": "0x20000400"})
	if r["search"].(map[string]any)["explicit_address"] != true {
		t.Fatalf("rtt_info %v", r)
	}
	w := mustInvoke(t, p, "DebugProbe", "rtt_write", map[string]any{"data": "0123456789ABCDEFGHIJ"})
	if w["written"] != float64(15) || w["dropped"] != float64(5) {
		t.Fatalf("down buffer must take size-1 bytes, got %v", w)
	}
	got, _ := s.Core().Read32(context.Background(), simCB+48+12)
	if got != 15 {
		t.Fatalf("WrOff=%d", got)
	}
	if _, err := invoke(t, p, SimID, "DebugProbe", "rtt_write", map[string]any{"data": "x", "rtt_channel": float64(3)}); err == nil {
		t.Fatal("bad channel must fail")
	}
	if _, err := invoke(t, p, SimID, "DebugProbe", "rtt_info", map[string]any{"rtt_search_size": float64(8)}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("a tiny window must be refused: %v", err)
	}
}

func TestCaptureStreamRule(t *testing.T) {
	for _, tc := range []struct {
		cfg  map[string]any
		want string
	}{
		{nil, "energytrace"},
		{map[string]any{"allow_target_power": true}, "energytrace"},
		{map[string]any{"stream": "energytrace"}, "energytrace"},
		{map[string]any{"stream": "rtt"}, "rtt"},
		{map[string]any{"rtt_channel": 0.0}, "rtt"},
		{map[string]any{"stream": "energytrace", "rtt_channel": 0.0}, ""},
		{map[string]any{"stream": "swo"}, ""},
	} {
		got, err := captureStream(tc.cfg)
		if got != tc.want || (err != nil) != (tc.want == "") {
			t.Errorf("%v: got %q %v, want %q", tc.cfg, got, err, tc.want)
		}
	}
}

func TestRTTCaptureSim(t *testing.T) {
	p, s := simPlugin(t)
	// A session opens the device with the capture's config.
	cfg := map[string]any{"stream": "rtt", "rtt_poll_min_ms": float64(1), "rtt_poll_max_ms": float64(5)}
	openSim(t, p, cfg)
	placeRTT(t, s.Core())
	sink := pluginsdk.NewTestSink(64)
	cancel, c, start := startRTTCapture(t, p, cfg, sink)
	if start.Streams[0].StreamId != "rtt" || start.Streams[0].Attributes["control_block"] != "0x20000400" || start.Streams[0].Attributes["source"] != RTTLogSource {
		t.Fatalf("start %+v", start)
	}
	wr := uint32(0)
	for i, msg := range []string{"boot ok\nsecond li", "ne wraps the ring\n", "ERROR: x\n"} {
		wr = targetWrite(t, p, s.Core(), wr, msg)
		if i == 0 {
			// Debug calls interleave with the capture; it is no EnergyTrace lock.
			mustInvoke(t, p, "DebugProbe", "status", nil)
		}
		time.Sleep(40 * time.Millisecond)
	}
	cancel()
	if err := c.Wait(); err != nil {
		t.Fatal(err)
	}
	lines, levels := rttLines(t, sink)
	want := []string{"boot ok", "second line wraps the ring", "ERROR: x"}
	if strings.Join(lines, "|") != strings.Join(want, "|") || levels[2] != "error" {
		t.Fatalf("lines %q levels %q", lines, levels)
	}
	if st := c.Result().GetStats().GetEvents(); st != 3 {
		t.Fatalf("stats %d", st)
	}
	if p.devs[SimID].rtt != 0 {
		t.Fatal("the capture was not released")
	}
	// the device the capture borrowed must still be open and usable
	mustInvoke(t, p, "DebugProbe", "status", nil)
}

func TestRTTCaptureImpliedByKeysAndNoControlBlock(t *testing.T) {
	p, _ := simPlugin(t)
	openSim(t, p, nil)
	// rtt_* keys select RTT (not EnergyTrace, which the simulator lacks).
	_, _, err := p.StartCapture(context.Background(), pluginsdk.CaptureRequest{DeviceID: SimID, Clock: pluginsdk.NewClock(0),
		Config: map[string]any{"rtt_wait_ms": float64(0)}}, pluginsdk.NewTestSink(4))
	if err == nil || status.Code(err) == codes.Unimplemented || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("want a not-found error, got %v", err)
	}
	if p.devs[SimID].rtt != 0 {
		t.Fatal("a failed start must release the device")
	}
	_, _, err = p.StartCapture(context.Background(), pluginsdk.CaptureRequest{DeviceID: SimID, Clock: pluginsdk.NewClock(0),
		Config: map[string]any{"stream": "rtt", "rtt_chanel": float64(1)}}, pluginsdk.NewTestSink(4))
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("a misspelt key must be refused: %v", err)
	}
}

func TestRTTCaptureOwnsUnopenedDevice(t *testing.T) {
	p, s := simPlugin(t)
	placeRTT(t, s.Core())
	cancel, c, _ := startRTTCapture(t, p, map[string]any{"stream": "rtt"}, pluginsdk.NewTestSink(4))
	if p.devs[SimID] == nil {
		t.Fatal("the capture must hold the device")
	}
	cancel()
	if err := c.Wait(); err != nil {
		t.Fatal(err)
	}
	if p.devs[SimID] != nil {
		t.Fatal("a device the capture opened must be closed with it")
	}
}

func TestRTTCaptureEndsWhenDeviceCloses(t *testing.T) {
	p, s := simPlugin(t)
	openSim(t, p, nil)
	placeRTT(t, s.Core())
	_, c, _ := startRTTCapture(t, p, map[string]any{"stream": "rtt", "rtt_poll_max_ms": float64(5)}, pluginsdk.NewTestSink(4))
	if err := p.CloseDevice(context.Background(), SimID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "closed") {
			t.Fatalf("want a closed-device error, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the capture outlived its device")
	}
}

// On a CC23xx the RTT path must leave SACI like MemoryAccess.read, and a
// capture must follow the target through a reset (which re-enters SACI).
func TestRTTCC23xxLeavesSACIAndFollowsReset(t *testing.T) {
	p, s := ccPlugin(t)
	openSim(t, p, nil)
	placeRTT(t, s.Core())
	if !s.CC23xx().InSACI() {
		t.Fatal("the model must start in SACI")
	}
	if r := mustInvoke(t, p, "DebugProbe", "rtt_info", nil); r["control_block"] != "0x20000400" {
		t.Fatalf("rtt_info %v", r)
	}
	if s.CC23xx().InSACI() {
		t.Fatal("rtt_info must leave SACI")
	}
	sink := pluginsdk.NewTestSink(64)
	cancel, c, _ := startRTTCapture(t, p, map[string]any{"stream": "rtt", "rtt_poll_min_ms": float64(1), "rtt_poll_max_ms": float64(5)}, sink)
	wr := targetWrite(t, p, s.Core(), 0, "before reset\n")
	time.Sleep(40 * time.Millisecond)
	d := p.devs[SimID]
	d.mu.Lock()
	s.CC23xx().PinReset()
	inSACI := s.CC23xx().InSACI()
	d.mu.Unlock()
	if !inSACI {
		t.Fatal("a reset must re-enter SACI")
	}
	targetWrite(t, p, s.Core(), wr, "after reset\n")
	time.Sleep(80 * time.Millisecond)
	cancel()
	if err := c.Wait(); err != nil {
		t.Fatal(err)
	}
	lines, _ := rttLines(t, sink)
	if strings.Join(lines, "|") != "before reset|after reset" {
		t.Fatalf("lines %q", lines)
	}
	if s.CC23xx().InSACI() {
		t.Fatal("the capture must have left SACI again")
	}
}

func TestRTTCC23xxKeepSACI(t *testing.T) {
	p, s := ccPlugin(t)
	openSim(t, p, map[string]any{"saci_exit": "none"})
	placeRTT(t, s.Core())
	_, err := invoke(t, p, SimID, "DebugProbe", "rtt_info", nil)
	wantErr(t, err, codes.FailedPrecondition, "SACI_ACTIVE")
}

// EnergyTrace and RTT share the probe's debug channel: one refuses the other.
func TestRTTAndEnergyTraceExclude(t *testing.T) {
	f := loadET(t)
	tr, data := newETTransport(t, f), &fakeData{reads: fixtureReads(f)}
	p := etPlugin(t, tr, data)
	if err := p.OpenDevice(context.Background(), usbID, map[string]any{"allow_target_power": true}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.CloseDevice(context.Background(), usbID) })
	d := p.devs[usbID]
	d.mu.Lock()
	d.rtt = 1 // an RTT capture is running
	d.mu.Unlock()
	_, _, err := p.StartCapture(context.Background(), pluginsdk.CaptureRequest{DeviceID: usbID, Clock: pluginsdk.NewClock(0)}, pluginsdk.NewTestSink(4))
	wantErr(t, err, codes.FailedPrecondition, "RTT capture is running")
	d.mu.Lock()
	d.rtt = 0
	d.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	c, _, err := p.StartCapture(ctx, pluginsdk.CaptureRequest{DeviceID: usbID, Clock: pluginsdk.NewClock(0)}, pluginsdk.NewTestSink(64))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = p.StartCapture(context.Background(), pluginsdk.CaptureRequest{DeviceID: usbID, Clock: pluginsdk.NewClock(0),
		Config: map[string]any{"stream": "rtt"}}, pluginsdk.NewTestSink(4))
	wantErr(t, err, codes.FailedPrecondition, "EnergyTrace capture is running")
	cancel()
	if err := c.Wait(); err != nil {
		t.Fatal(err)
	}
	if d.rtt != 0 {
		t.Fatal("a refused RTT start must not count as running")
	}
}
