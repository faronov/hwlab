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

// The fixture is a real CC-Debugger + CC2530 session recorded with
// examples/ccdebugger-info.yaml (target EUI-64 redacted). It must answer
// timeline queries and replay with no plugin binary and no debugger.
func TestRealCCDebuggerSessionReplaysWithoutHardware(t *testing.T) {
	ctx := context.Background()
	src := filepath.Join(testutil.ModuleRoot(t), "testdata", "real-ccdebugger")
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
		chip := evs[0].Data["result"].(map[string]any)["chip"].(map[string]any)
		if chip["name"] != "CC2530" || evs[0].Data["method"] != "info" || evs[1].Data["method"] != "read_mac" {
			t.Errorf("unexpected recorded calls: %+v", evs)
		}
	}
	check(a, "latest")

	info, err := a.SessionInfo("latest")
	if err != nil || len(info.Meta.Devices) != 1 || len(info.Meta.Captures) != 0 {
		t.Fatalf("info: %+v err=%v", info, err)
	}
	if err := a.UseReplay("latest"); err != nil {
		t.Fatal(err)
	}
	a.Cfg.SessionsDir = t.TempDir()
	rr, err := a.RunExperiment(ctx, &experiment.Experiment{Name: "replay", Duration: experiment.Duration(2 * time.Second),
		Devices: []experiment.DeviceSpec{{Alias: "dbg", Plugin: replay.PluginID, Device: "dbg", Config: map[string]any{}}}}, false)
	if err != nil || rr.Status != "complete" {
		t.Fatalf("replay: %+v err=%v", rr, err)
	}
	check(a, rr.SessionID)
}
