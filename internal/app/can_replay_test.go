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

// The fixture is SYNTHETIC, not a bus recording: it was produced by the CAN
// plugin's in-process virtual bus (examples/can-virtual.yaml). It proves the
// CAN event model, the pcapng artifact and replay; it says nothing about real
// SocketCAN or a physical bus, which are unvalidated on this host (no CAN
// hardware, no vcan kernel module). It must replay with no plugin binary.
func TestSyntheticCANSessionReplaysWithoutHardware(t *testing.T) {
	ctx := context.Background()
	src := filepath.Join(testutil.ModuleRoot(t), "testdata", "synthetic-can")
	if _, err := os.Stat(src); err != nil {
		t.Skip("fixture missing")
	}
	cfg := config.Config{Home: t.TempDir(), SessionsDir: src}
	a := app.New(cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { a.Close(ctx) })

	check := func(a *app.App, ref string) {
		t.Helper()
		pk, err := a.QueryTimeline(app.TimelineQuery{Session: ref, Types: []string{"packet"}})
		if err != nil || len(pk) != 6 {
			t.Fatalf("packets: %d err=%v", len(pk), err)
		}
		wantID := []string{"0x0CF00400", "0x123", "0x321", "0x7DF", "0x7E8", "0x18DAF110"}
		wantKind := []string{"can", "can", "rtr", "can", "can", "can-fd"}
		for i, e := range pk {
			d := e.Data
			if d["protocol"] != "can" || d["attr.id"] != wantID[i] || d["attr.kind"] != wantKind[i] || d["attr.interface"] != "virtual-can0" {
				t.Errorf("packet %d: %+v", i, d)
			}
			if i > 0 && e.TimestampNs <= pk[i-1].TimestampNs {
				t.Errorf("packet %d is not after packet %d", i, i-1)
			}
		}
		if pk[3].Data["attr.dir"] != "tx" || pk[0].Data["attr.dir"] != "rx" {
			t.Errorf("directions: %v / %v", pk[3].Data["attr.dir"], pk[0].Data["attr.dir"])
		}
		if pk[5].Data["attr.brs"] != "true" || pk[5].Data["attr.dlc"] != "24" {
			t.Errorf("FD frame: %+v", pk[5].Data)
		}
		inv, err := a.QueryTimeline(app.TimelineQuery{Session: ref, Types: []string{"invoke"}})
		if err != nil || len(inv) != 6 {
			t.Fatalf("invoke events: %d err=%v", len(inv), err)
		}
	}
	check(a, "latest")

	if err := a.UseReplay("latest"); err != nil {
		t.Fatal(err)
	}
	a.Cfg.SessionsDir = t.TempDir()
	rr, err := a.RunExperiment(ctx, &experiment.Experiment{Name: "replay", Duration: experiment.Duration(4 * time.Second),
		Devices: []experiment.DeviceSpec{{Alias: "bus", Plugin: replay.PluginID, Device: "bus", Config: map[string]any{}}}}, false)
	if err != nil || rr.Status != "complete" {
		t.Fatalf("replay: %+v err=%v", rr, err)
	}
	check(a, rr.SessionID)
}
