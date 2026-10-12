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
	"github.com/faronov/hwlab/internal/session"
	"github.com/faronov/hwlab/internal/testutil"
)

// The fixture is a real PPK2 + nRF52840 sniffer session (channel 15, no DUT
// attached, so the current is baseline noise). Power is decimated 10:1 and
// radio bytes are synthetic; recorded times and clock bounds are retained.
// It must open and replay with no plugin binaries and no devices.
func TestRealSessionReplaysWithoutHardware(t *testing.T) {
	ctx := context.Background()
	src := filepath.Join(testutil.ModuleRoot(t), "testdata", "real-session")
	if _, err := os.Stat(src); err != nil {
		t.Skip("fixture missing")
	}
	home := t.TempDir()
	cfg := config.Config{Home: home, SessionsDir: src} // no PluginPaths: nothing to spawn
	a := app.New(cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { a.Close(ctx) })

	info, err := a.SessionInfo("latest")
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Meta.Devices) != 2 || len(info.Meta.Captures) != 2 {
		t.Fatalf("devices=%d captures=%d", len(info.Meta.Devices), len(info.Meta.Captures))
	}
	var pc session.CaptureMeta
	for _, c := range info.Meta.Captures {
		if c.Alias == "power" {
			pc = c
		}
	}
	if pc.Clock == nil || !pc.Clock.ErrorMinBounded || !pc.Clock.ErrorMaxBounded {
		t.Fatalf("recorded PPK2 clock should be bounded on both sides: %+v", pc.Clock)
	}
	if pc.Samples != 99_962 || pc.Clock.ResolutionNs != 100_000 {
		t.Fatalf("expected bounded 10:1 decimated fixture, samples=%d clock=%+v", pc.Samples, pc.Clock)
	}
	if pc.Clock.ErrorMinNs >= 0 || pc.Clock.ErrorMinNs < -50_000_000 || pc.Clock.ErrorMaxNs > 10_000_000 {
		t.Errorf("implausible PPK2 start bound [%d, %d] ns", pc.Clock.ErrorMinNs, pc.Clock.ErrorMaxNs)
	}

	if err := a.UseReplay("latest"); err != nil {
		t.Fatal(err)
	}
	var devs []experiment.DeviceSpec
	for _, d := range info.Meta.Devices {
		devs = append(devs, experiment.DeviceSpec{Alias: d.Alias, Plugin: replay.PluginID, Device: d.Alias, Config: map[string]any{}})
	}
	a.Cfg.SessionsDir = t.TempDir() // new sessions go elsewhere, the fixture stays untouched
	rr, err := a.RunExperiment(ctx, &experiment.Experiment{Name: "replay", Duration: experiment.Duration(12 * time.Second), Devices: devs}, false)
	if err != nil || rr.Status != "complete" {
		t.Fatalf("replay: %+v err=%v", rr, err)
	}
	if rr.Samples != pc.Samples {
		t.Errorf("replayed samples=%d, recorded %d", rr.Samples, pc.Samples)
	}
	got, err := a.SessionInfo(rr.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var gc session.CaptureMeta
	for _, c := range got.Meta.Captures {
		if c.Alias == "power" {
			gc = c
		}
	}
	if gc.Clock == nil || gc.Clock.ErrorMinNs != pc.Clock.ErrorMinNs {
		t.Errorf("clock lost in replay: %+v", gc.Clock)
	}
}
