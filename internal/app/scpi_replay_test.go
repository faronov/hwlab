package app_test

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
)

// The fixture is SYNTHETIC, not an instrument recording: it was produced by the
// SCPI plugin's in-process fake Rigol (examples/scpi-fake.yaml,
// HWLAB_SCPI_FAKE=rigol). It proves the capture/export path, the analog
// artifacts, plotting and replay; it says nothing about a real Rigol or
// Siglent, which are unvalidated on this host (no instrument available). It
// must replay with no plugin binary and no instrument.
func TestSyntheticSCPISessionReplaysWithoutHardware(t *testing.T) {
	ctx := context.Background()
	src := filepath.Join(testutil.ModuleRoot(t), "testdata", "synthetic-scpi")
	if _, err := os.Stat(src); err != nil {
		t.Skip("fixture missing")
	}
	cfg := config.Config{Home: t.TempDir(), SessionsDir: src}
	a := app.New(cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { a.Close(ctx) })

	check := func(a *app.App, ref string) {
		t.Helper()
		inv, err := a.QueryTimeline(app.TimelineQuery{Session: ref, Types: []string{"invoke"}})
		if err != nil || len(inv) != 2 {
			t.Fatalf("invoke events: %d err=%v", len(inv), err)
		}
		if !strings.Contains(inv[0].Summary, "SCPI.identify") || !strings.Contains(inv[1].Summary, "Oscilloscope.measure") {
			t.Errorf("invoke summaries: %q, %q", inv[0].Summary, inv[1].Summary)
		}
		res, _ := inv[1].Data["result"].(map[string]any)
		if v, _ := res["value"].(float64); v != 1 {
			t.Errorf("replayed vpp = %v, want the recorded 1 V", res["value"])
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
	rr, err := a.RunExperiment(ctx, &experiment.Experiment{Name: "replay", Duration: experiment.Duration(2 * time.Second),
		Devices: []experiment.DeviceSpec{{Alias: "scope", Plugin: replay.PluginID, Device: "scope", Config: map[string]any{}}}}, false)
	if err != nil || rr.Status != "complete" {
		t.Fatalf("replay: %+v err=%v", rr, err)
	}
	if rr.Samples != 2400 {
		t.Errorf("replayed samples = %d, want 2400", rr.Samples)
	}
	check(a, rr.SessionID)
}
