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
	"github.com/faronov/hwlab/internal/replay"
	"github.com/faronov/hwlab/internal/testutil"
)

// The fixture is a real session from an nRF52840 DK's on-board J-Link OB,
// recorded with examples/jlink-nrf52840-dk.yaml (RTT banner only: the target
// firmware has no RTT output). It must replay with no JLinkExe and no probe.
func TestRealJLinkSessionReplaysWithoutHardware(t *testing.T) {
	ctx := context.Background()
	src := filepath.Join(testutil.ModuleRoot(t), "testdata", "real-jlink")
	if _, err := os.Stat(src); err != nil {
		t.Skip("fixture missing")
	}
	cfg := config.Config{Home: t.TempDir(), SessionsDir: src}
	a := app.New(cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { a.Close(ctx) })

	check := func(a *app.App, ref string) {
		t.Helper()
		evs, err := a.QueryTimeline(app.TimelineQuery{Session: ref, Types: []string{"invoke"}})
		if err != nil || len(evs) != 2 {
			t.Fatalf("invoke events: %d err=%v", len(evs), err)
		}
		mem := evs[1].Data["result"].(map[string]any)
		if evs[0].Data["method"] != "info" || mem["hex"] != "80cd0020497d00008b910100357d0000" {
			t.Errorf("unexpected recorded calls: %+v", evs)
		}
		logs, err := a.QueryTimeline(app.TimelineQuery{Session: ref, Types: []string{"log"}})
		if err != nil || len(logs) != 3 {
			t.Fatalf("rtt banner logs: %d err=%v", len(logs), err)
		}
	}
	check(a, "latest")

	if err := a.UseReplay("latest"); err != nil {
		t.Fatal(err)
	}
	a.Cfg.SessionsDir = t.TempDir()
	rr, err := a.RunExperiment(ctx, &experiment.Experiment{Name: "replay", Duration: experiment.Duration(6 * time.Second),
		Devices: []experiment.DeviceSpec{{Alias: "dbg", Plugin: replay.PluginID, Device: "dbg", Config: map[string]any{}}}}, false)
	if err != nil || rr.Status != "complete" {
		t.Fatalf("replay: %+v err=%v", rr, err)
	}
	check(a, rr.SessionID)
}
