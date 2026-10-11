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

// ftdiReplay opens a recorded FTDI/MPSSE session twice, directly and again
// re-run through the replay pipeline, with no plugin binary and no hardware.
func ftdiReplay(t *testing.T, fixture string, check func(t *testing.T, a *app.App, ref string)) {
	t.Helper()
	aliasReplay(t, fixture, "jtag", check)
}

// aliasReplay is ftdiReplay for a fixture whose device alias is not "jtag".
func aliasReplay(t *testing.T, fixture, alias string, check func(t *testing.T, a *app.App, ref string)) {
	t.Helper()
	ctx := context.Background()
	src := filepath.Join(testutil.ModuleRoot(t), "testdata", fixture)
	if _, err := os.Stat(src); err != nil {
		t.Skip("fixture missing")
	}
	a := app.New(config.Config{Home: t.TempDir(), SessionsDir: src}, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { a.Close(ctx) })
	check(t, a, "latest")

	if err := a.UseReplay("latest"); err != nil {
		t.Fatal(err)
	}
	a.Cfg.SessionsDir = t.TempDir()
	rr, err := a.RunExperiment(ctx, &experiment.Experiment{Name: "replay", Duration: experiment.Duration(2 * time.Second),
		Devices: []experiment.DeviceSpec{{Alias: alias, Plugin: replay.PluginID, Device: alias, Config: map[string]any{}}}}, false)
	if err != nil || rr.Status != "complete" {
		t.Fatalf("replay: %+v err=%v", rr, err)
	}
	check(t, a, rr.SessionID)
}

func ftdiInvokes(t *testing.T, a *app.App, ref string) []map[string]any {
	t.Helper()
	evs, err := a.QueryTimeline(app.TimelineQuery{Session: ref, Types: []string{"invoke"}})
	if err != nil || len(evs) != 7 {
		t.Fatalf("invoke events: %d err=%v", len(evs), err)
	}
	want := []string{"info", "selftest", "tap_reset", "scan", "scan", "scan", "identify"}
	var out []map[string]any
	for i, e := range evs {
		if e.Data["method"] != want[i] || e.Data["ok"] != true {
			t.Fatalf("event %d: %+v", i, e.Data)
		}
		out = append(out, e.Data["result"].(map[string]any))
	}
	return out
}

// REAL: a Sipeed RV-Debugger-BL702 (FT2232D emulator) with NO JTAG target
// attached. The scans honestly report "no-device"; no target was verified.
func TestRealFTDISipeedSessionReplaysWithoutHardware(t *testing.T) {
	ftdiReplay(t, "real-ftdi-sipeed", func(t *testing.T, a *app.App, ref string) {
		r := ftdiInvokes(t, a, ref)
		info := r[0]
		if info["profile"] != "sipeed-bl702" || info["implementation"] != "compatible-emulator" || info["synthetic"] != nil {
			t.Errorf("info: %v", info)
		}
		ad := info["adapter"].(map[string]any)
		if ad["detached_kernel_driver"] != "ftdi_sio" || ad["clock_fixed_by_firmware"] != true || ad["chip"] != "FT2232C/D" {
			t.Errorf("adapter: %v", ad)
		}
		if lb := r[1]["loopback"].(map[string]any); lb["supported"] != false {
			t.Errorf("loopback: %v", lb)
		}
		for i := 3; i <= 5; i++ {
			if r[i]["status"] != "no-device" || r[i]["count"] != float64(0) || r[i]["target_verified"] != false {
				t.Errorf("scan %d: %v", i, r[i])
			}
		}
		if r[6]["count"] != float64(0) || r[6]["target_verified"] != false {
			t.Errorf("identify: %v", r[6])
		}
	})
}

// SYNTHETIC: the in-process adapter with one fabricated TAP.
func TestSyntheticFTDISessionReplaysWithoutHardware(t *testing.T) {
	ftdiReplay(t, "synthetic-ftdi", func(t *testing.T, a *app.App, ref string) {
		r := ftdiInvokes(t, a, ref)
		if r[0]["synthetic"] != true {
			t.Errorf("not marked synthetic: %v", r[0])
		}
		for i := 3; i <= 5; i++ {
			devs := r[i]["devices"].([]any)
			if r[i]["status"] != "ok" || len(devs) != 1 || r[i]["synthetic"] != true {
				t.Fatalf("scan %d: %v", i, r[i])
			}
			if devs[0].(map[string]any)["idcode"].(map[string]any)["raw"] != float64(0x1BA01477) {
				t.Errorf("idcode: %v", devs[0])
			}
		}
		if r[6]["targets"].([]any)[0].(map[string]any)["idcode"] != "0x1ba01477" {
			t.Errorf("identify: %v", r[6])
		}
	})
}
