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

// The fixture is a real session (examples/dk-uart-rx.yaml): the nRF52840 DK's
// UARTE0 was configured through J-Link memory writes (no flash change) and
// transmitted 23 bytes on P0.06, which the host received on the J-Link OB
// virtual COM port. It is real UART RX, as opposed to the pty RX test in
// plugins/uart. It must replay with no port, probe or plugin binary.
func TestRealUARTRXSessionReplaysWithoutHardware(t *testing.T) {
	ctx := context.Background()
	src := filepath.Join(testutil.ModuleRoot(t), "testdata", "real-uart-rx")
	if _, err := os.Stat(src); err != nil {
		t.Skip("fixture missing")
	}
	cfg := config.Config{Home: t.TempDir(), SessionsDir: src}
	a := app.New(cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { a.Close(ctx) })

	check := func(a *app.App, ref string, withInvokes bool) {
		t.Helper()
		pk, err := a.QueryTimeline(app.TimelineQuery{Session: ref, Types: []string{"packet"}})
		if err != nil || len(pk) != 1 {
			t.Fatalf("packets: %d err=%v", len(pk), err)
		}
		d := pk[0].Data
		if d["attr.dir"] != "rx" || d["hex"] != "68776c6162207561727420727820746573742030310d0a" {
			t.Errorf("unexpected rx packet: %+v", d)
		}
		logs, err := a.QueryTimeline(app.TimelineQuery{Session: ref, Types: []string{"log"}, Contains: "hwlab uart rx test"})
		if err != nil || len(logs) != 1 {
			t.Fatalf("rx line events: %d err=%v", len(logs), err)
		}
		if !withInvokes { // the replay run replays the console device only
			return
		}
		starts, err := a.QueryTimeline(app.TimelineQuery{Session: ref, Types: []string{"invoke"}, Contains: "write"})
		if err != nil || len(starts) == 0 {
			t.Fatalf("invoke events: %d err=%v", len(starts), err)
		}
		// the bytes cannot arrive before the STARTTX write call began
		last := starts[len(starts)-1]
		if pk[0].TimestampNs < last.TimestampNs {
			t.Errorf("rx at %d ns precedes the STARTTX write at %d ns", pk[0].TimestampNs, last.TimestampNs)
		}
	}
	check(a, "latest", true)

	if err := a.UseReplay("latest"); err != nil {
		t.Fatal(err)
	}
	a.Cfg.SessionsDir = t.TempDir()
	rr, err := a.RunExperiment(ctx, &experiment.Experiment{Name: "replay", Duration: experiment.Duration(22 * time.Second),
		Devices: []experiment.DeviceSpec{{Alias: "console", Plugin: replay.PluginID, Device: "console", Config: map[string]any{}}}}, false)
	if err != nil || rr.Status != "complete" {
		t.Fatalf("replay: %+v err=%v", rr, err)
	}
	check(a, rr.SessionID, false)
}
