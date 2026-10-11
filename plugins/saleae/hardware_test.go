//go:build hardware

package saleae

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/faronov/hwlab/pkg/pluginsdk"
)

// Opt-in: needs Logic 2 running with the automation server enabled and a
// Saleae analyzer (or HWLAB_SALEAE_SIMULATION=1 for a Logic 2 simulation
// device). Captures 1 s on digital channels 0-3 (or HWLAB_SALEAE_CHANNELS as a
// count) at HWLAB_SALEAE_RATE (default 10 MS/s) and logs what came back.
func TestHardwareDigitalCapture(t *testing.T) {
	p := New()
	devs, _ := p.ListDevices(context.Background())
	if len(devs) == 0 {
		t.Skip("Logic 2 not reachable or no Saleae device attached")
	}
	n, _ := strconv.Atoi(os.Getenv("HWLAB_SALEAE_CHANNELS"))
	if n <= 0 {
		n = 4
	}
	rate := 10_000_000.0
	if s := os.Getenv("HWLAB_SALEAE_RATE"); s != "" {
		rate, _ = strconv.ParseFloat(s, 64)
	}
	var ch []any
	for i := 0; i < n; i++ {
		ch = append(ch, float64(i))
	}
	sink := pluginsdk.NewTestSink(65536)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c, _, err := p.StartCapture(ctx, pluginsdk.CaptureRequest{DeviceID: devs[0].Id,
		Config:   map[string]any{"digital_channels": ch, "digital_sample_rate": rate},
		Duration: time.Second, ArtifactDir: t.TempDir(), Clock: pluginsdk.NewClock(0)}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Wait(); err != nil {
		t.Fatal(err)
	}
	res := c.Result()
	evs := sink.Drain()
	t.Logf("device=%s events=%d artifacts=%d clock=%q", devs[0].Name, len(evs), len(res.Artifacts), res.Clock.Source)
	for _, a := range res.Artifacts {
		t.Logf("artifact %s (%s) %s", a.Name, a.Kind, a.Path)
	}
}
