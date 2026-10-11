//go:build hardware && linux

package xds110plugin

import (
	"context"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/faronov/hwlab/pkg/pluginsdk"
)

// EnergyTrace real-hardware tests, opt-in with HWLAB_XDS110_ENERGYTRACE_HW=1.
// They run on an LP-XDS110ET with no target attached and do not need a load:
// they check stream integrity (no lost packets, exact sample count, timing,
// clean stop) and a plausible idle reading, not accuracy. Accuracy needs a
// calibrated load and is not claimed.

func etHW(t *testing.T) (*Plugin, string) {
	t.Helper()
	if os.Getenv("HWLAB_XDS110_ENERGYTRACE_HW") != "1" {
		t.Skip("set HWLAB_XDS110_ENERGYTRACE_HW=1 to run against a real LP-XDS110ET")
	}
	p := New()
	return p, hwID(t, p)
}

func etCaptureHW(t *testing.T, p *Plugin, id string, d time.Duration) (*pluginsdk.Sink, uint64, int64) {
	t.Helper()
	if err := p.OpenDevice(context.Background(), id, map[string]any{"allow_target_power": true}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.CloseDevice(context.Background(), id) })
	sink := pluginsdk.NewTestSink(1 << 16)
	c, _, err := p.StartCapture(context.Background(), pluginsdk.CaptureRequest{DeviceID: id, Duration: d, Clock: pluginsdk.NewClock(0)}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Wait(); err != nil {
		t.Fatal(err)
	}
	res := c.Result()
	return sink, res.Stats.DroppedBatches, int64(res.Stats.Samples)
}

func TestHWEnergyTraceCapture(t *testing.T) {
	p, id := etHW(t)
	sink, dropped, samples := etCaptureHW(t, p, id, 2*time.Second)
	if dropped != 0 {
		t.Errorf("%d batches or packets lost", dropped)
	}
	if want := int64(2 * 256000); samples != want {
		t.Errorf("samples %d, want %d", samples, want)
	}
	var cur []int32
	var prevEnd int64 = -1
	for _, e := range sink.Drain() {
		pb := e.GetPower()
		if prevEnd >= 0 && e.TimestampNs < prevEnd-2000 {
			t.Fatalf("timestamps go backwards: %d after %d", e.TimestampNs, prevEnd)
		}
		prevEnd = e.TimestampNs + int64(len(pb.CurrentNa))*3906
		cur = append(cur, pb.CurrentNa...)
	}
	sort.Slice(cur, func(i, j int) bool { return cur[i] < cur[j] })
	med := cur[len(cur)/2]
	t.Logf("median %d nA over %d samples (min %d, max %d)", med, len(cur), cur[0], cur[len(cur)-1])
	if med < -1_000_000 || med > 100_000_000 {
		t.Errorf("median %d nA is not a plausible idle reading", med)
	}
	// The debug path still works afterwards.
	if _, err := hwInvoke(p, id, "DebugProbe", "info", nil); err != nil {
		t.Errorf("debug info after capture: %v", err)
	}
}

func TestHWEnergyTraceRepeatedCaptures(t *testing.T) {
	p, id := etHW(t)
	for i := 0; i < 5; i++ {
		_, dropped, samples := etCaptureHW(t, p, id, 300*time.Millisecond)
		if dropped != 0 || samples != 300*256 {
			t.Fatalf("run %d: samples %d dropped %d", i, samples, dropped)
		}
	}
}

func TestHWEnergyTraceCancelStopsCleanly(t *testing.T) {
	p, id := etHW(t)
	if err := p.OpenDevice(context.Background(), id, map[string]any{"allow_target_power": true}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.CloseDevice(context.Background(), id) })
	ctx, cancel := context.WithCancel(context.Background())
	sink := pluginsdk.NewTestSink(1 << 16)
	c, _, err := p.StartCapture(ctx, pluginsdk.CaptureRequest{DeviceID: id, Clock: pluginsdk.NewClock(0)}, sink)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(700 * time.Millisecond)
	cancel()
	if err := c.Wait(); err != nil {
		t.Fatal(err)
	}
	if n := c.Result().Stats.Samples; n < 100_000 {
		t.Errorf("only %d samples in 0.7 s", n)
	}
	if _, err := hwInvoke(p, id, "DebugProbe", "info", nil); err != nil {
		t.Errorf("debug info after cancel: %v", err)
	}
	// A new capture works right after a cancelled one.
	_, dropped, samples := etCaptureHW(t, p, id, 200*time.Millisecond)
	if dropped != 0 || samples != 200*256 {
		t.Errorf("follow-up capture: samples %d dropped %d", samples, dropped)
	}
}

func TestHWEnergyTraceRefusedWithoutPermission(t *testing.T) {
	p, id := etHW(t)
	if err := p.OpenDevice(context.Background(), id, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.CloseDevice(context.Background(), id) })
	if _, _, err := p.StartCapture(context.Background(), pluginsdk.CaptureRequest{DeviceID: id, Clock: pluginsdk.NewClock(0)}, pluginsdk.NewTestSink(4)); err == nil {
		t.Fatal("capture without allow_target_power must be refused")
	}
}
