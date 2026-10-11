package saleae_test

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/faronov/hwlab/internal/app"
	"github.com/faronov/hwlab/internal/config"
	"github.com/faronov/hwlab/internal/experiment"
	"github.com/faronov/hwlab/internal/plot"
	"github.com/faronov/hwlab/internal/replay"
	"github.com/faronov/hwlab/internal/testutil"
	"github.com/faronov/hwlab/plugins/saleae"
	"github.com/faronov/hwlab/plugins/saleae/internal/fakelogic2"
)

func newApp(t *testing.T, plugins ...string) *app.App {
	t.Helper()
	cfg := config.Config{Home: t.TempDir(), PluginPaths: plugins}
	cfg.SessionsDir = filepath.Join(cfg.Home, "sessions")
	a := app.New(cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { a.Close(context.Background()) })
	return a
}

// The built plugin binary, a fake Logic 2 server, and the whole hwlab
// pipeline: run -> timeline -> plot, then replay with no plugin.
func TestSaleaeCaptureThroughCore(t *testing.T) {
	ctx := context.Background()
	_, addr := fakelogic2.Start(t)
	t.Setenv(saleae.AddrEnv, addr)
	a := newApp(t, testutil.BuildPlugin(t, "saleae"))

	exp := &experiment.Experiment{
		Name:     "saleae-fake",
		Duration: experiment.Duration(500 * time.Millisecond),
		Devices: []experiment.DeviceSpec{{
			Alias: "logic", Plugin: "saleae", Device: "F4241",
			Requires: []string{"DigitalCapture"},
			Config: map[string]any{
				"digital_channels":    []any{0, 1, 2},
				"digital_sample_rate": 10000000,
				"channel_names":       map[string]any{"0": "CLK", "1": "MOSI", "2": "IRQ"},
				"analog_channels":     []any{0},
				"analog_sample_rate":  100000,
				"analyzers": []any{map[string]any{"type": "spi", "label": "spi",
					"channels": map[string]any{"clk": 0, "mosi": 1, "cs": 2}}},
			},
		}},
	}
	res, err := a.RunExperiment(ctx, exp, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "complete" || res.Error != "" {
		t.Fatalf("status=%q error=%q warnings=%v", res.Status, res.Error, res.Warnings)
	}
	if res.Events["digital.edge"] != 7 || res.Events["decoded"] != 2 || res.Samples != 4 {
		t.Errorf("events=%v samples=%d", res.Events, res.Samples)
	}
	for _, f := range []string{"capture.sal", "raw/digital.csv", "raw/analog.csv"} {
		matches, _ := filepath.Glob(filepath.Join(res.Dir, "artifacts", "logic", f))
		if len(matches) == 0 {
			t.Errorf("artifact %s not in the session", f)
		}
	}

	check := func(a *app.App, ref string) {
		t.Helper()
		edges, err := a.QueryTimeline(app.TimelineQuery{Session: ref, Types: []string{"digital.edge"}, Limit: 100})
		if err != nil || len(edges) != 7 {
			t.Fatalf("digital edges: %d err=%v", len(edges), err)
		}
		dec, err := a.QueryTimeline(app.TimelineQuery{Session: ref, Types: []string{"decoded"}, Limit: 100})
		if err != nil || len(dec) != 2 || !strings.Contains(dec[0].Summary, "mosi=0xA5") {
			t.Fatalf("decoded: %+v err=%v", dec, err)
		}
		pr, err := a.RenderPlot(ctx, ref, "saleae", "", plot.Spec{Format: "svg"})
		if err != nil || pr.Tracks < 3 {
			t.Fatalf("plot: %+v err=%v", pr, err)
		}
		raw := string(pr.Data)
		for _, want := range []string{"CLK", "MOSI", "IRQ"} {
			if !strings.Contains(raw, want) {
				t.Errorf("plot has no %s track", want)
			}
		}
	}
	check(a, res.SessionID)

	// Replay needs neither the plugin binary nor Logic 2.
	b := newApp(t)
	b.Cfg.SessionsDir = a.Cfg.SessionsDir
	if err := b.UseReplay(res.SessionID); err != nil {
		t.Fatal(err)
	}
	rr, err := b.RunExperiment(ctx, &experiment.Experiment{Name: "replay", Duration: experiment.Duration(time.Duration(res.DurationNs)),
		Devices: []experiment.DeviceSpec{{Alias: "logic", Plugin: replay.PluginID, Device: "logic", Config: map[string]any{}}}}, false)
	if err != nil || rr.Status != "complete" {
		t.Fatalf("replay: %+v err=%v", rr, err)
	}
	if rr.Events["digital.edge"] != 7 || rr.Events["decoded"] != 2 || rr.Samples != res.Samples {
		t.Errorf("replayed events=%v samples=%d", rr.Events, rr.Samples)
	}
	check(b, rr.SessionID)
}
