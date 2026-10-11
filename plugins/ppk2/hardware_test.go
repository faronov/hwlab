//go:build hardware

package ppk2

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/faronov/hwlab/pkg/pluginsdk"
)

// Run with: go test -tags=hardware ./plugins/ppk2/ -run Hardware -v
// Needs a PPK2 (1915:c00a). Only ampere mode is exercised unless
// HWLAB_TEST_PPK2_SOURCE=1, which drives 3.3 V on VOUT: make sure nothing
// that cannot take 3.3 V is attached.

func hardwareCapture(t *testing.T, cfg map[string]any, d time.Duration) (samples int, drops uint64) {
	t.Helper()
	p := New()
	ctx := context.Background()
	devs, err := p.ListDevices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) == 0 {
		t.Skip("no PPK2 connected")
	}
	id := devs[0].GetId()
	if err := p.OpenDevice(ctx, id, cfg); err != nil {
		t.Fatal(err)
	}
	defer p.CloseDevice(ctx, id)
	sink := pluginsdk.NewTestSink(4096)
	cap, _, err := p.StartCapture(ctx, pluginsdk.CaptureRequest{DeviceID: id, Duration: d, Clock: pluginsdk.NewClock(0)}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := cap.Wait(); err != nil {
		t.Fatal(err)
	}
	for _, ev := range sink.Drain() {
		samples += len(ev.GetPower().GetCurrentNa())
	}
	return samples, sink.Dropped()
}

func TestHardwarePPK2AmpereCapture(t *testing.T) {
	want := 100_000
	got, drops := hardwareCapture(t, map[string]any{"mode": "ampere"}, time.Second)
	if got != want || drops != 0 {
		t.Errorf("samples=%d (want %d), dropped batches=%d", got, want, drops)
	}
}

func TestHardwarePPK2SourceCapture(t *testing.T) {
	if os.Getenv("HWLAB_TEST_PPK2_SOURCE") != "1" {
		t.Skip("set HWLAB_TEST_PPK2_SOURCE=1 to drive VOUT")
	}
	got, drops := hardwareCapture(t, map[string]any{"mode": "source", "supply_mv": float64(3300)}, time.Second)
	if got != 100_000 || drops != 0 {
		t.Errorf("samples=%d, dropped batches=%d", got, drops)
	}
}

// TestHardwarePPK2Replug is interactive: it asks for the PPK2 to be unplugged
// and plugged back in, then expects a capture on the same plugin instance.
// Enable with HWLAB_TEST_PPK2_REPLUG=1.
func TestHardwarePPK2Replug(t *testing.T) {
	if os.Getenv("HWLAB_TEST_PPK2_REPLUG") != "1" {
		t.Skip("set HWLAB_TEST_PPK2_REPLUG=1 (interactive)")
	}
	p := New()
	ctx := context.Background()
	devs, err := p.ListDevices(ctx)
	if err != nil || len(devs) == 0 {
		t.Skip("no PPK2 connected")
	}
	id := devs[0].GetId()
	if err := p.OpenDevice(ctx, id, map[string]any{"mode": "ampere"}); err != nil {
		t.Fatal(err)
	}
	defer p.CloseDevice(ctx, id)
	capture := func() int {
		cap, _, err := p.StartCapture(ctx, pluginsdk.CaptureRequest{DeviceID: id, Duration: 500 * time.Millisecond, Clock: pluginsdk.NewClock(0)}, pluginsdk.NewTestSink(4096))
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		if err := cap.Wait(); err != nil {
			t.Fatalf("wait: %v", err)
		}
		return int(cap.Result().GetStats().GetSamples())
	}
	if n := capture(); n != 50_000 {
		t.Fatalf("baseline capture: %d samples", n)
	}
	waitFor := func(present bool, what string) {
		t.Logf(">>> %s", what)
		for end := time.Now().Add(90 * time.Second); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
			if d, _ := p.ListDevices(ctx); (len(d) > 0) == present {
				return
			}
		}
		t.Fatalf("timeout: %s", what)
	}
	waitFor(false, "UNPLUG the PPK2 USB cable now")
	waitFor(true, "PLUG the PPK2 back in now")
	time.Sleep(2 * time.Second) // let the CDC ports settle
	if n := capture(); n != 50_000 {
		t.Errorf("capture after replug: %d samples", n)
	}
}
