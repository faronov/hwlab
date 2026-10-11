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

// The fixture is SYNTHETIC, not a device recording: it was produced by the
// modbus plugin's in-process slave (examples/modbus-fake.yaml,
// HWLAB_MODBUS_FAKE=1). It proves the polling capture, the transaction events,
// plotting and replay; it says nothing about any real Modbus device, none of
// which was available. It must replay with no plugin binary and no device.
func TestSyntheticModbusSessionReplaysWithoutHardware(t *testing.T) {
	ctx := context.Background()
	src := filepath.Join(testutil.ModuleRoot(t), "testdata", "synthetic-modbus")
	if _, err := os.Stat(src); err != nil {
		t.Skip("fixture missing")
	}
	cfg := config.Config{Home: t.TempDir(), SessionsDir: src}
	a := app.New(cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { a.Close(ctx) })

	check := func(a *app.App, ref string) {
		t.Helper()
		dec, err := a.QueryTimeline(app.TimelineQuery{Session: ref, Types: []string{"decoded"}, Limit: 1000})
		if err != nil || len(dec) != 18 {
			t.Fatalf("decoded events: %d err=%v", len(dec), err)
		}
		var temp bool
		for _, e := range dec {
			f, _ := e.Data["fields"].(map[string]any)
			if f["poll"] == "temp" {
				v, _ := f["values"].([]any)
				if len(v) != 1 || v[0] != 23.5 {
					t.Errorf("temp poll values = %v, want the recorded 23.5", f["values"])
				}
				temp = true
			}
		}
		if !temp {
			t.Error("no temp poll in the replay")
		}
		pk, err := a.QueryTimeline(app.TimelineQuery{Session: ref, Types: []string{"packet"}, Limit: 1000})
		if err != nil || len(pk) != 36 {
			t.Fatalf("packets: %d err=%v", len(pk), err)
		}
		if pk[0].Data["protocol"] != "modbus-rtu" {
			t.Errorf("packet protocol = %v", pk[0].Data["protocol"])
		}
		inv, err := a.QueryTimeline(app.TimelineQuery{Session: ref, Types: []string{"invoke"}})
		if err != nil || len(inv) != 3 {
			t.Fatalf("invoke events: %d err=%v", len(inv), err)
		}
		if !strings.Contains(inv[2].Summary, "illegal_data_address") {
			t.Errorf("exception step summary = %q", inv[2].Summary)
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
	rr, err := a.RunExperiment(ctx, &experiment.Experiment{Name: "replay", Duration: experiment.Duration(4 * time.Second),
		Devices: []experiment.DeviceSpec{{Alias: "plc", Plugin: replay.PluginID, Device: "plc", Config: map[string]any{}}}}, false)
	if err != nil || rr.Status != "complete" {
		t.Fatalf("replay: %+v err=%v", rr, err)
	}
	check(a, rr.SessionID)
}
