package app_test

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/faronov/hwlab/internal/app"
	"github.com/faronov/hwlab/internal/config"
	"github.com/faronov/hwlab/internal/experiment"
	"github.com/faronov/hwlab/internal/plot"
	"github.com/faronov/hwlab/internal/replay"
	"github.com/faronov/hwlab/internal/testutil"
)

// The fixture is a real Saleae Logic capture (Logic 2 2.4.14, 90 s, CH1-CH4 at
// 4 MS/s) with UART and I2C analyzers on hand-touched, otherwise floating
// lines, so the decoded frames are noise: only the pipeline is verified, not
// the protocols. It must replay with no Logic 2 and no analyzer attached.
func TestRealSaleaeSessionReplaysWithoutHardware(t *testing.T) {
	ctx := context.Background()
	src := filepath.Join(testutil.ModuleRoot(t), "testdata", "real-saleae")
	if _, err := os.Stat(src); err != nil {
		t.Skip("fixture missing")
	}
	cfg := config.Config{Home: t.TempDir(), SessionsDir: src}
	a := app.New(cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { a.Close(ctx) })

	check := func(a *app.App, ref string) {
		t.Helper()
		dec, err := a.QueryTimeline(app.TimelineQuery{Session: ref, Types: []string{"decoded"}})
		if err != nil || len(dec) != 6 {
			t.Fatalf("decoded events: %d err=%v", len(dec), err)
		}
		if dec[0].Data["protocol"] == nil || dec[0].Summary == "" {
			t.Errorf("decoded event lacks protocol/summary: %+v", dec[0])
		}
		edges, err := a.QueryTimeline(app.TimelineQuery{Session: ref, Types: []string{"digital.edge"}, Limit: 1000})
		if err != nil || len(edges) != 46 {
			t.Fatalf("digital edges: %d err=%v", len(edges), err)
		}
		if _, err := a.RenderPlot(ctx, ref, "p", "", plot.Spec{Format: "svg"}); err != nil {
			t.Fatalf("plot: %v", err)
		}
	}
	check(a, "latest")

	if err := a.UseReplay("latest"); err != nil {
		t.Fatal(err)
	}
	a.Cfg.SessionsDir = t.TempDir()
	rr, err := a.RunExperiment(ctx, &experiment.Experiment{Name: "replay", Duration: experiment.Duration(95 * time.Second),
		Devices: []experiment.DeviceSpec{{Alias: "logic", Plugin: replay.PluginID, Device: "logic", Config: map[string]any{}}}}, false)
	if err != nil || rr.Status != "complete" {
		t.Fatalf("replay: %+v err=%v", rr, err)
	}
	check(a, rr.SessionID)
}
