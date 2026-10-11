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
	"github.com/faronov/hwlab/internal/replay"
	"github.com/faronov/hwlab/internal/testutil"
)

// The fixture is SYNTHETIC, not a programmer recording: it was produced by the
// Telink plugin against its in-process simulated SWire programmer and TLSR8258
// target (examples/telink-sim.yaml, HWLAB_TELINK_SIM=1). It proves the result
// shapes, the timeline, replay and the refused factory-data erase; it says
// nothing about any real SWire programmer or Telink chip, which are
// unvalidated on this host. It must replay with no plugin binary and no
// hardware.
func TestSyntheticTelinkSessionReplaysWithoutHardware(t *testing.T) {
	ctx := context.Background()
	src := filepath.Join(testutil.ModuleRoot(t), "testdata", "synthetic-telink")
	if _, err := os.Stat(src); err != nil {
		t.Skip("fixture missing")
	}
	cfg := config.Config{Home: t.TempDir(), SessionsDir: src}
	a := app.New(cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { a.Close(ctx) })

	check := func(a *app.App, ref string) {
		t.Helper()
		evs, err := a.QueryTimeline(app.TimelineQuery{Session: ref, Types: []string{"invoke"}})
		if err != nil || len(evs) != 12 {
			t.Fatalf("invoke events: %d err=%v", len(evs), err)
		}
		want := []any{"info", "connect", "attach", "status", "step", "read_registers", "read", "read", "read", "set_breakpoint", "resume", "erase"}
		for i, e := range evs {
			if e.Data["method"] != want[i] {
				t.Fatalf("event %d: %+v, want method %v", i, e.Data, want[i])
			}
			if i < 11 && e.Data["ok"] != true {
				t.Fatalf("event %d must succeed: %+v", i, e.Data)
			}
		}
		res := func(i int) map[string]any { return evs[i].Data["result"].(map[string]any) }
		if res(0)["synthetic"] != true || res(0)["programmer_chip"] != "TLSR825x" {
			t.Errorf("info: %v", res(0))
		}
		if res(1)["flash_jedec"] != "0xef4013" || res(1)["flash_matches_layout"] != true {
			t.Errorf("connect: %v", res(1))
		}
		if res(2)["halted"] != true || res(2)["mode"] != "hard" || res(4)["pc"] != "0x00001006" {
			t.Errorf("attach/step: %v %v", res(2), res(4))
		}
		if regs := res(5)["registers"].(map[string]any); regs["pc"] != "0x00001006" {
			t.Errorf("registers: %v", regs)
		}
		if res(8)["hex"] != "deadbeef00112233" {
			t.Errorf("factory EUI read: %v", res(8))
		}
		if res(9)["hit"] != true || res(10)["halted"] != false {
			t.Errorf("breakpoint/resume: %v %v", res(9), res(10))
		}
		if evs[11].Data["ok"] != false || !strings.Contains(evs[11].Data["error"].(string), "factory") {
			t.Errorf("factory erase must be a recorded refusal: %+v", evs[11].Data)
		}
	}
	check(a, "latest")

	if err := a.UseReplay("latest"); err != nil {
		t.Fatal(err)
	}
	a.Cfg.SessionsDir = t.TempDir()
	rr, err := a.RunExperiment(ctx, &experiment.Experiment{Name: "replay", Duration: experiment.Duration(5 * time.Second),
		Devices: []experiment.DeviceSpec{{Alias: "pgm", Plugin: replay.PluginID, Device: "pgm", Config: map[string]any{}}}}, false)
	if err != nil || rr.Status != "complete" {
		t.Fatalf("replay: %+v err=%v", rr, err)
	}
	check(a, rr.SessionID)
}
