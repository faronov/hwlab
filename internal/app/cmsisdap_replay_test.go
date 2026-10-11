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

// The fixture is SYNTHETIC, not a probe recording: it was produced by the
// CMSIS-DAP plugin against its in-process simulated probe and Cortex-M target
// (examples/cmsisdap-sim.yaml, HWLAB_CMSISDAP_SIM=1). It proves the DebugProbe
// and MemoryAccess result shapes, the timeline and replay; it says nothing
// about any real CMSIS-DAP probe or target, which are unvalidated on this host
// (no probe available). It must replay with no plugin binary and no probe.
func TestSyntheticCMSISDAPSessionReplaysWithoutHardware(t *testing.T) {
	ctx := context.Background()
	src := filepath.Join(testutil.ModuleRoot(t), "testdata", "synthetic-cmsisdap")
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
		want := []any{"info", "connect", "read", "halt", "read_registers", "step", "reset", "resume"}
		for i, e := range evs {
			if e.Data["method"] != want[i] || e.Data["ok"] != true {
				t.Fatalf("event %d: %+v, want method %v", i, e.Data, want[i])
			}
		}
		res := func(i int) map[string]any { return evs[i].Data["result"].(map[string]any) }
		if res(0)["synthetic"] != true || res(0)["validated"] != false {
			t.Errorf("info must say synthetic and unvalidated: %v", res(0))
		}
		if res(1)["core"] != "Cortex-M4" || res(1)["dpidr"] != "0x2ba01477" {
			t.Errorf("connect: %v", res(1))
		}
		if res(2)["hex"] != "0000012001010000020000a5030000a5" {
			t.Errorf("vector table read: %v", res(2))
		}
		if res(3)["halted"] != true || res(5)["pc"] != "0x00000102" || res(6)["halted"] != true || res(7)["halted"] != false {
			t.Errorf("run-control sequence: %v %v %v %v", res(3), res(5), res(6), res(7))
		}
		if regs := res(4)["registers"].(map[string]any); len(regs) != 20 {
			t.Errorf("registers: %v", regs)
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
