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

// REAL (not synthetic): two SEGGER J-Link probes driven by one session.
// probe 90000001 (SAM-ICE) was wired to an EFR32MG1P, probe 900000002 (the
// nRF52840 DK's on-board J-Link OB) to its own nRF52840. The test proves the
// recorded session keeps each call tied to the exact probe serial and target.
func TestRealTwoJLinkSessionReplaysWithoutHardware(t *testing.T) {
	ctx := context.Background()
	src := filepath.Join(testutil.ModuleRoot(t), "testdata", "real-jlink-multi")
	if _, err := os.Stat(src); err != nil {
		t.Skip("fixture missing")
	}
	a := app.New(config.Config{Home: t.TempDir(), SessionsDir: src}, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { a.Close(ctx) })

	want := map[string]struct{ serial, target string }{
		"efr": {"90000001", "EFR32MG1PxxxF256"},
		"nrf": {"900000002", "NRF52840_XXAA"},
	}
	check := func(a *app.App, ref string, replayed bool) {
		t.Helper()
		evs, err := a.QueryTimeline(app.TimelineQuery{Session: ref, Types: []string{"invoke"}})
		if err != nil || len(evs) != 18 {
			t.Fatalf("invoke events: %d err=%v", len(evs), err)
		}
		perProbe := map[string]int{}
		for _, e := range evs {
			w, ok := want[e.SourceID]
			if !ok || e.Data["ok"] != true {
				t.Fatalf("unexpected event %+v", e)
			}
			if !replayed && e.DeviceID != w.serial { // a replay is keyed by alias
				t.Errorf("%s event recorded for device %s, want serial %s", e.SourceID, e.DeviceID, w.serial)
			}
			res := e.Data["result"].(map[string]any)
			if got := res["probe_serial"]; got != nil && (got != w.serial || res["target"] != w.target) {
				t.Errorf("%s answered as %v/%v, want %s/%s", e.SourceID, got, res["target"], w.serial, w.target)
			}
			if got := res["serial"]; got != nil && got != w.serial {
				t.Errorf("%s info answered from %v, want %s", e.SourceID, got, w.serial)
			}
			perProbe[e.SourceID]++
			switch e.Data["method"] {
			case "info":
				if e.SourceID == "efr" && (res["model"] != "SAM-ICE" || res["nickname"] != "jlinkv9") ||
					e.SourceID == "nrf" && res["model"] != "J-Link OB-SAM3U128-V2-NordicSem" {
					t.Errorf("%s model: %v", e.SourceID, res)
				}
			case "halt":
				if res["halted"] != true {
					t.Errorf("%s halt not confirmed: %v", e.SourceID, res)
				}
			case "resume", "reset":
				if res["halted"] != false {
					t.Errorf("%s %v left the CPU halted: %v", e.SourceID, e.Data["method"], res)
				}
			case "connect":
				if res["core"] != "Cortex-M4" || res["dp_id"] != "0x2BA01477" {
					t.Errorf("%s connect: %v", e.SourceID, res)
				}
			case "read":
				switch e.Data["args"].(map[string]any)["address"] {
				case "0x0":
					wantHex := map[string]string{"efr": "002000207d050000c9050000c9050000", "nrf": "80cd0020497d00008b910100357d0000"}[e.SourceID]
					if res["hex"] != wantHex {
						t.Errorf("%s vector table %v", e.SourceID, res["hex"])
					}
				case "0x0FE081F8":
					if res["hex"] != "000120008400109e" { // 256 kB flash, 32 kB RAM, family EFR32MG1P
						t.Errorf("EFR32 DEVINFO %v", res["hex"])
					}
				case "0x10000100":
					if res["hex"] != "40280500" { // FICR INFO.PART = 0x00052840
						t.Errorf("nRF FICR PART %v", res["hex"])
					}
				}
			}
		}
		if perProbe["efr"] != 9 || perProbe["nrf"] != 9 {
			t.Errorf("calls per probe: %v", perProbe)
		}
		logs, err := a.QueryTimeline(app.TimelineQuery{Session: ref, Types: []string{"log"}})
		if err != nil || len(logs) != 3 {
			t.Fatalf("rtt banner logs: %d err=%v", len(logs), err)
		}
		for _, l := range logs {
			if l.SourceID != "efr" {
				t.Errorf("only the SAM-ICE ran RTT: %+v", l)
			}
		}
	}
	check(a, "latest", false)

	if err := a.UseReplay("latest"); err != nil {
		t.Fatal(err)
	}
	a.Cfg.SessionsDir = t.TempDir()
	rr, err := a.RunExperiment(ctx, &experiment.Experiment{Name: "replay", Duration: experiment.Duration(16 * time.Second),
		Devices: []experiment.DeviceSpec{
			{Alias: "efr", Plugin: replay.PluginID, Device: "efr", Config: map[string]any{}},
			{Alias: "nrf", Plugin: replay.PluginID, Device: "nrf", Config: map[string]any{}}}}, false)
	if err != nil || rr.Status != "complete" {
		t.Fatalf("replay: %+v err=%v", rr, err)
	}
	check(a, rr.SessionID, true)
}
