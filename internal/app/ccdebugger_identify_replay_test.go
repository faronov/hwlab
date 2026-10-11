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
// examples/ccdebugger-identify.yaml: chip identification from the hardware
// registers, lock bits and IEEE addresses (the unique part of the primary
// address is redacted). It replays with no plugin binary and no hardware.
func TestRealCCDebuggerIdentifySessionReplaysWithoutHardware(t *testing.T) {
	ctx := context.Background()
	src := filepath.Join(testutil.ModuleRoot(t), "testdata", "real-ccdebugger-identify")
	if _, err := os.Stat(src); err != nil {
		t.Skip("fixture missing")
	}
	cfg := config.Config{Home: t.TempDir(), SessionsDir: src}
	a := app.New(cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { a.Close(ctx) })

	check := func(a *app.App, ref string) {
		t.Helper()
		evs, err := a.QueryTimeline(app.TimelineQuery{Session: ref, Types: []string{"invoke"}})
		if err != nil || len(evs) != 8 {
			t.Fatalf("invoke events: %d err=%v", len(evs), err)
		}
		var methods []any
		for _, e := range evs {
			methods = append(methods, e.Data["method"])
			if e.Data["ok"] != true {
				t.Errorf("recorded call failed: %+v", e.Data)
			}
		}
		want := []any{"attach", "enable_dma", "chip_info", "read_lock", "read_ieee", "read_registers", "read", "resume"}
		for i := range want {
			if methods[i] != want[i] {
				t.Fatalf("methods = %v, want %v", methods, want)
			}
		}
		chip := evs[2].Data["result"].(map[string]any)
		if chip["chip"] != "CC2530" || chip["chip_id"] != "0xA5" || chip["revision"] != "0x24" ||
			chip["flash_size"] != float64(262144) || chip["ram_size"] != float64(8192) || chip["page_size"] != float64(2048) {
			t.Errorf("chip_info: %v", chip)
		}
		lock := evs[3].Data["result"].(map[string]any)
		if lock["debug_locked"] != false || lock["raw"] != "ffffffffffffffffffffffffffffffff" {
			t.Errorf("lock: %v", lock)
		}
		ieee := evs[4].Data["result"].(map[string]any)
		if ieee["primary_present"] != true || ieee["secondary_present"] != false {
			t.Errorf("ieee: %v", ieee)
		}
		run := evs[7].Data["result"].(map[string]any)["status_after"].(map[string]any)
		if run["cpu_halted"] != false {
			t.Errorf("target not left running: %v", run)
		}
	}
	check(a, "latest")

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
