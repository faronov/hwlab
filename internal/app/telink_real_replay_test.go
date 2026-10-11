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

// The fixture is REAL: examples/telink-real.yaml recorded through the native Go
// SWire backend (CH340 USB-UART programmer, TLSR825x target), read-only apart
// from one write request that the plugin refused. The flash unique id is
// redacted. It must answer timeline queries and replay with no plugin binary
// and no hardware.
func TestRealTelinkSessionReplaysWithoutHardware(t *testing.T) {
	ctx := context.Background()
	src := filepath.Join(testutil.ModuleRoot(t), "testdata", "real-telink")
	if _, err := os.Stat(src); err != nil {
		t.Skip("fixture missing")
	}
	cfg := config.Config{Home: t.TempDir(), SessionsDir: src}
	a := app.New(cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { a.Close(ctx) })

	check := func(a *app.App, ref string) {
		t.Helper()
		evs, err := a.QueryTimeline(app.TimelineQuery{Session: ref, Types: []string{"invoke"}})
		if err != nil || len(evs) != 9 {
			t.Fatalf("invoke events: %d err=%v", len(evs), err)
		}
		want := []any{"info", "halt", "connect", "status", "read", "read", "read", "program", "resume"}
		for i, e := range evs {
			if e.Data["method"] != want[i] {
				t.Fatalf("event %d: %+v, want method %v", i, e.Data, want[i])
			}
			if i != 7 && e.Data["ok"] != true {
				t.Fatalf("event %d must succeed: %+v", i, e.Data)
			}
		}
		res := func(i int) map[string]any { return evs[i].Data["result"].(map[string]any) }
		if res(0)["programmer_chip_id"] != "0x5562" || res(0)["backend"] != "native-go-swire" || res(0)["synthetic"] != nil {
			t.Errorf("info: %v", res(0))
		}
		c := res(2)
		if c["target_chip_id"] != "0x5562" || c["target_revision"] != "0x02" || c["target_variant"] != "undetermined" || c["flash_jedec"] != "0xc86013" {
			t.Errorf("connect: %v", c)
		}
		if fp := c["capabilities"].(map[string]any)["flash_program"].(map[string]any); fp["implemented"] != true || fp["allowed_on_current_target"] != false {
			t.Errorf("flash_program must be disabled on this target: %v", fp)
		}
		if res(4)["hex"] != "1f5c00015af100000011050060013b01" {
			t.Errorf("peripheral read: %v", res(4))
		}
		if res(5)["hex"] != "22800000000000004b4e4c5410018800" {
			t.Errorf("flash head: %v", res(5))
		}
		if res(6)["hex"] != "ffffffffffffffff" {
			t.Errorf("factory block: %v", res(6))
		}
		if !strings.Contains(evs[7].Data["error"].(string), "allowed_on_current_target: false") {
			t.Errorf("program must be a recorded refusal: %+v", evs[7].Data)
		}
		if res(8)["halted"] != false {
			t.Errorf("resume: %v", res(8))
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
