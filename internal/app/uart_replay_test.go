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

// The fixture is a real session from the nRF52840 DK's J-Link OB virtual COM
// port (examples/uart-nrf52840-dk.yaml). It is TX-only: the DK firmware is
// silent, so real RX is not covered by this fixture (RX is validated over a
// pty in plugins/uart). It must replay with no port and no plugin binary.
func TestRealUARTSessionReplaysWithoutHardware(t *testing.T) {
	ctx := context.Background()
	src := filepath.Join(testutil.ModuleRoot(t), "testdata", "real-uart")
	if _, err := os.Stat(src); err != nil {
		t.Skip("fixture missing")
	}
	cfg := config.Config{Home: t.TempDir(), SessionsDir: src}
	a := app.New(cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { a.Close(ctx) })

	check := func(a *app.App, ref string) {
		t.Helper()
		inv, err := a.QueryTimeline(app.TimelineQuery{Session: ref, Types: []string{"invoke"}})
		if err != nil || len(inv) != 1 || inv[0].Data["method"] != "write" {
			t.Fatalf("invoke events: %+v err=%v", inv, err)
		}
		pk, err := a.QueryTimeline(app.TimelineQuery{Session: ref, Types: []string{"packet"}})
		if err != nil || len(pk) != 1 {
			t.Fatalf("packets: %d err=%v", len(pk), err)
		}
		d := pk[0].Data
		if d["protocol"] != "uart" || d["attr.dir"] != "tx" || d["hex"] != "68656c6c6f0d0a" || d["attr.mode"] != "8N1" {
			t.Errorf("unexpected tx packet: %+v", d)
		}
		if pk[0].TimestampNs < inv[0].TimestampNs {
			t.Errorf("tx stamped before its write call started")
		}
	}
	check(a, "latest")

	if err := a.UseReplay("latest"); err != nil {
		t.Fatal(err)
	}
	a.Cfg.SessionsDir = t.TempDir()
	rr, err := a.RunExperiment(ctx, &experiment.Experiment{Name: "replay", Duration: experiment.Duration(6 * time.Second),
		Devices: []experiment.DeviceSpec{{Alias: "console", Plugin: replay.PluginID, Device: "console", Config: map[string]any{}}}}, false)
	if err != nil || rr.Status != "complete" {
		t.Fatalf("replay: %+v err=%v", rr, err)
	}
	check(a, rr.SessionID)
}
