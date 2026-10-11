package jlinknativeplugin

import (
	"context"
	"encoding/binary"
	"strings"
	"testing"
	"time"

	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
	"github.com/faronov/hwlab/plugins/jlinknative/sim"
)

const (
	simCB   = 0x20000400
	simUp   = 0x20000500
	simDown = 0x20000600
)

// placeRTT writes a control block with one 32-byte up and one 16-byte down
// buffer into the simulated SRAM.
func placeRTT(t *testing.T, s *sim.Sim) {
	t.Helper()
	b := make([]byte, 24+48)
	copy(b, "SEGGER RTT")
	binary.LittleEndian.PutUint32(b[16:], 1)
	binary.LittleEndian.PutUint32(b[20:], 1)
	binary.LittleEndian.PutUint32(b[24+4:], simUp)
	binary.LittleEndian.PutUint32(b[24+8:], 32)
	binary.LittleEndian.PutUint32(b[48+4:], simDown)
	binary.LittleEndian.PutUint32(b[48+8:], 16)
	if err := s.Core().WriteBlock(context.Background(), simCB, b); err != nil {
		t.Fatal(err)
	}
}

// targetWrite emulates firmware appending to the up ring.
func targetWrite(t *testing.T, p *Plugin, s *sim.Sim, wr uint32, data string) uint32 {
	t.Helper()
	// The simulated core is not goroutine safe: serialise with the poller.
	d := p.devs[SimID]
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := 0; i < len(data); i++ {
		if err := s.Core().Store(simUp+(wr+uint32(i))%32, 1, uint32(data[i])); err != nil {
			t.Fatal(err)
		}
	}
	wr = (wr + uint32(len(data))) % 32
	if err := s.Core().Store(simCB+24+12, 4, wr); err != nil {
		t.Fatal(err)
	}
	return wr
}

func TestRTTInfoAndWriteSim(t *testing.T) {
	p, s := simPlugin(t)
	openSim(t, p)
	if _, err := invoke(t, p, SimID, "DebugProbe", "rtt_info", nil); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("no control block must be reported, got %v", err)
	}
	placeRTT(t, s)
	r := mustInvoke(t, p, "DebugProbe", "rtt_info", nil)
	if r["control_block"] != "0x20000400" || len(r["up"].([]any)) != 1 || len(r["down"].([]any)) != 1 {
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
}

func TestRTTCaptureSim(t *testing.T) {
	p, s := simPlugin(t)
	openSim(t, p)
	placeRTT(t, s)
	sink := pluginsdk.NewTestSink(64)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cap, start, err := p.StartCapture(ctx, pluginsdk.CaptureRequest{DeviceID: SimID, Clock: pluginsdk.NewClock(0),
		Config: map[string]any{"rtt_poll_min_ms": float64(1), "rtt_poll_max_ms": float64(5)}}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if start.Streams[0].Attributes["control_block"] != "0x20000400" {
		t.Fatalf("start %+v", start)
	}
	wr := uint32(0)
	for _, msg := range []string{"boot ok\nsecond li", "ne wraps the ring\n", "ERROR: x\n"} {
		wr = targetWrite(t, p, s, wr, msg)
		time.Sleep(40 * time.Millisecond)
	}
	cancel()
	if err := cap.Wait(); err != nil {
		t.Fatal(err)
	}
	var lines []string
	var levels []string
	for _, e := range sink.Drain() {
		l := e.GetLog()
		if l.GetSource() != RTTLogSource {
			t.Fatalf("source %q", l.GetSource())
		}
		lines, levels = append(lines, l.Message), append(levels, l.Level)
	}
	want := []string{"boot ok", "second line wraps the ring", "ERROR: x"}
	if strings.Join(lines, "|") != strings.Join(want, "|") || levels[2] != "error" {
		t.Fatalf("lines %q levels %q", lines, levels)
	}
	if st := cap.Result().GetStats().GetEvents(); st != 3 {
		t.Fatalf("stats %d", st)
	}
	// the device the capture borrowed must still be open and usable
	mustInvoke(t, p, "DebugProbe", "status", nil)
}

func TestRTTCaptureNoControlBlock(t *testing.T) {
	p, _ := simPlugin(t)
	openSim(t, p)
	_, _, err := p.StartCapture(context.Background(), pluginsdk.CaptureRequest{DeviceID: SimID, Clock: pluginsdk.NewClock(0),
		Config: map[string]any{"rtt_wait_ms": float64(0)}}, pluginsdk.NewTestSink(4))
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("want a not-found error, got %v", err)
	}
}

var _ = pluginv1.CapRTT
