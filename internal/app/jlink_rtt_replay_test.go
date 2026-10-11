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

// The fixture is a real session recorded with examples/jlink-rtt-planted.yaml
// on an nRF52840 DK: the real J-Link RTT engine read a control block in target
// RAM and served it on the RTT telnet port. The block and its text were written
// by the HOST (J-Link memory writes), not by firmware, so this proves the
// probe -> RTT -> hwlab timeline -> session -> replay path, not firmware RTT.
// It must replay with no JLinkExe and no probe.
func TestRealJLinkRTTSessionReplaysWithoutHardware(t *testing.T) {
	ctx := context.Background()
	src := filepath.Join(testutil.ModuleRoot(t), "testdata", "real-jlink-rtt")
	if _, err := os.Stat(src); err != nil {
		t.Skip("fixture missing")
	}
	cfg := config.Config{Home: t.TempDir(), SessionsDir: src}
	a := app.New(cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { a.Close(ctx) })

	check := func(a *app.App, ref string) {
		t.Helper()
		logs, err := a.QueryTimeline(app.TimelineQuery{Session: ref, Types: []string{"log"}})
		if err != nil {
			t.Fatal(err)
		}
		var rtt []string
		var levels []string
		for _, l := range logs {
			if l.Data["log_source"] == "jlink-rtt" {
				rtt = append(rtt, l.Summary)
				levels = append(levels, l.Data["level"].(string))
			}
		}
		want := []string{"boot ok: hwlab rtt test", "<inf> app: sample 1", "<err> app: simulated fault"}
		if len(rtt) != 3 || rtt[0] != want[0] || rtt[1] != want[1] || rtt[2] != want[2] {
			t.Fatalf("rtt lines: %q", rtt)
		}
		if levels[0] != "info" || levels[1] != "info" || levels[2] != "error" {
			t.Errorf("levels: %v", levels)
		}
	}
	check(a, "latest")

	if err := a.UseReplay("latest"); err != nil {
		t.Fatal(err)
	}
	a.Cfg.SessionsDir = t.TempDir()
	rr, err := a.RunExperiment(ctx, &experiment.Experiment{Name: "replay", Duration: experiment.Duration(24 * time.Second),
		Devices: []experiment.DeviceSpec{{Alias: "dbg", Plugin: replay.PluginID, Device: "dbg", Config: map[string]any{}}}}, false)
	if err != nil || rr.Status != "complete" {
		t.Fatalf("replay: %+v err=%v", rr, err)
	}
	check(a, rr.SessionID)
}
