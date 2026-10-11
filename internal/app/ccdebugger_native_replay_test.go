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

// The fixture is a real CC-Debugger + CC2530 session recorded through the
// native USB backend with examples/ccdebugger-native.yaml: reset-attach, debug
// status, registers, CODE and physical flash reads, DMA enable and resume. It
// must answer timeline queries and replay with no plugin binary, no libusb and
// no debugger.
func TestRealNativeCCDebuggerSessionReplaysWithoutHardware(t *testing.T) {
	ctx := context.Background()
	src := filepath.Join(testutil.ModuleRoot(t), "testdata", "real-ccdebugger-native")
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
		want := []any{"info", "attach", "status", "read_registers", "read", "read", "enable_dma", "resume"}
		for i := range want {
			if methods[i] != want[i] {
				t.Fatalf("methods = %v, want %v", methods, want)
			}
		}
		info := evs[0].Data["result"].(map[string]any)
		if info["backend"] != "native" || info["adapter"].(map[string]any)["target_id"] != "0x2530" {
			t.Errorf("info: %v", info)
		}
		att := evs[1].Data["result"].(map[string]any)["status_after"].(map[string]any)
		if att["raw"] != float64(0x22) || att["cpu_halted"] != true {
			t.Errorf("attach status: %v", att)
		}
		code := evs[4].Data["result"].(map[string]any)
		if code["space"] != "code" || len(code["data"].(string)) != 32 {
			t.Errorf("code read: %v", code)
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
