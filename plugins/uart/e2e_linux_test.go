//go:build linux

package uart

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/faronov/hwlab/internal/app"
	"github.com/faronov/hwlab/internal/config"
	"github.com/faronov/hwlab/internal/experiment"
	"github.com/faronov/hwlab/internal/replay"
	"github.com/faronov/hwlab/internal/testutil"
)

// The built plugin binary, a pty standing in for a device, and the whole
// hwlab pipeline: run -> timeline -> replay with no plugin and no port.
func TestSerialCaptureThroughCore(t *testing.T) {
	ctx := context.Background()
	master, slave := openPTY(t)
	t.Setenv(ExtraPortsEnv, "pty0="+slave)

	// The attached device announces itself, then answers AT with OK. It
	// waits for the plugin to put the line into raw mode, or the pty's
	// cooked discipline would rewrite the bytes.
	go func() {
		for i := 0; i < 400; i++ {
			if tio, err := unix.IoctlGetTermios(int(master.Fd()), unix.TCGETS); err == nil && tio.Iflag&unix.ICRNL == 0 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond)
		master.WriteString("boot ok\r\n")
		time.Sleep(100 * time.Millisecond)
		master.WriteString("temp=21.5\r\n")
		buf := make([]byte, 64)
		for {
			n, err := master.Read(buf)
			if err != nil {
				return
			}
			if strings.Contains(string(buf[:n]), "AT") {
				master.WriteString("OK\r\n")
			}
		}
	}()

	cfg := config.Config{Home: t.TempDir(), PluginPaths: []string{testutil.BuildPlugin(t, "uart")}}
	cfg.SessionsDir = filepath.Join(cfg.Home, "sessions")
	a := app.New(cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { a.Close(ctx) })

	exp := &experiment.Experiment{
		Name:     "uart-pty",
		Duration: experiment.Duration(900 * time.Millisecond),
		Devices: []experiment.DeviceSpec{{
			Alias: "console", Plugin: "uart", Device: "pty0",
			Requires: []string{"SerialTransport"},
			Config:   map[string]any{"baud": 115200, "lines": true, "idle_gap_ms": 20, "flush_input": false},
		}},
		Steps: []experiment.Step{{Name: "ping", At: experiment.Duration(450 * time.Millisecond), Device: "console",
			Call: "SerialTransport.write", Args: map[string]any{"data": "AT\r\n"}}},
	}
	res, err := a.RunExperiment(ctx, exp, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "complete" || res.Error != "" {
		t.Fatalf("run: %+v", res)
	}

	check := func(a *app.App, ref string) {
		t.Helper()
		pk, err := a.QueryTimeline(app.TimelineQuery{Session: ref, Types: []string{"packet"}})
		if err != nil {
			t.Fatal(err)
		}
		var rx, tx []string
		var txAt, okAt int64
		for _, e := range pk {
			dir, _ := e.Data["attr.dir"].(string)
			hexs, _ := e.Data["hex"].(string)
			if e.Data["protocol"] != "uart" {
				t.Errorf("protocol %v", e.Data["protocol"])
			}
			switch dir {
			case "tx":
				tx = append(tx, hexs)
				txAt = e.TimestampNs
			case "rx":
				rx = append(rx, hexs)
				if strings.HasSuffix(hexs, "4f4b0d0a") {
					okAt = e.TimestampNs
				}
			}
		}
		if len(tx) != 1 || tx[0] != "41540d0a" {
			t.Errorf("tx %v", tx)
		}
		all := strings.Join(rx, "")
		if !strings.Contains(all, "626f6f74206f6b0d0a") || !strings.HasSuffix(all, "4f4b0d0a") { // "boot ok\r\n" ... "OK\r\n"
			t.Errorf("rx %v", rx)
		}
		if okAt <= txAt || txAt == 0 {
			t.Errorf("the answer (%d) must come after the request (%d) on one timeline", okAt, txAt)
		}
		logs, err := a.QueryTimeline(app.TimelineQuery{Session: ref, Types: []string{"log"}})
		if err != nil {
			t.Fatal(err)
		}
		var msgs []string
		for _, l := range logs {
			msgs = append(msgs, l.Summary)
		}
		if strings.Join(msgs, "|") != "boot ok|temp=21.5|OK" {
			t.Errorf("lines %q", msgs)
		}
		inv, _ := a.QueryTimeline(app.TimelineQuery{Session: ref, Types: []string{"invoke"}})
		if len(inv) != 1 {
			t.Errorf("invoke events %d", len(inv))
		}
	}
	check(a, res.SessionID)

	// Replay: the same session served by the replay plugin; no port needed.
	if err := a.UseReplay(res.SessionID); err != nil {
		t.Fatal(err)
	}
	a.Cfg.SessionsDir = t.TempDir()
	rr, err := a.RunExperiment(ctx, &experiment.Experiment{Name: "replay", Duration: exp.Duration,
		Devices: []experiment.DeviceSpec{{Alias: "console", Plugin: replay.PluginID, Device: "console", Config: map[string]any{}}}}, false)
	if err != nil || rr.Status != "complete" {
		t.Fatalf("replay: %+v err=%v", rr, err)
	}
	check(a, rr.SessionID)
}
