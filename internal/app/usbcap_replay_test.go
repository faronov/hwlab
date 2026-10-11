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

// The fixture is SYNTHETIC, not a bus recording: it was produced by the usbcap
// plugin's built-in synthetic source (examples/usbcap-synth.yaml,
// HWLAB_USBCAP_SYNTH=1). It proves the packet path, the pcapng artifact, the
// URB analysis and replay; it says nothing about the Linux usbmon backend,
// which has not run on this host. It must replay with no plugin and no USB.
func TestSyntheticUSBSessionReplaysWithoutHardware(t *testing.T) {
	ctx := context.Background()
	src := filepath.Join(testutil.ModuleRoot(t), "testdata", "synthetic-usbcap")
	if _, err := os.Stat(src); err != nil {
		t.Skip("fixture missing")
	}
	a := app.New(config.Config{Home: t.TempDir(), SessionsDir: src}, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { a.Close(ctx) })

	check := func(a *app.App, ref string) {
		t.Helper()
		pk, err := a.QueryTimeline(app.TimelineQuery{Session: ref, Types: []string{"packet"}, Limit: 1000})
		if err != nil || len(pk) != 92 {
			t.Fatalf("packets: %d err=%v", len(pk), err)
		}
		if pk[0].Data["protocol"] != "usb" {
			t.Errorf("packet protocol %v", pk[0].Data["protocol"])
		}

		rep, err := a.AnalyzeUSB(app.USBQuery{Session: ref, Limit: 1000})
		if err != nil || len(rep.Streams) != 1 {
			t.Fatalf("analyze: %+v err=%v", rep, err)
		}
		s := rep.Streams[0]
		if s.Source != "usb" || s.Stats.Packets != 92 || s.Stats.Transactions != 46 || s.Stats.Errors != 2 ||
			s.Stats.UnpairedComplete != 0 || s.Stats.StillPending != 0 {
			t.Errorf("stats %+v", s.Stats)
		}
		classes := map[string]string{}
		for _, d := range s.Devices {
			classes[d.Device] = strings.Join(d.Interfaces, ",")
		}
		if len(classes) != 3 || !strings.Contains(classes["1:5"], "HID") || !strings.Contains(classes["1:6"], "mass-storage") || !strings.Contains(classes["1:7"], "CDC") {
			t.Errorf("devices %+v", classes)
		}
		var line, cbw bool
		for _, tr := range s.Transactions {
			line = line || tr.Name == "CDC SET_LINE_CODING" && tr.Detail == "115200 baud 8N1"
			cbw = cbw || strings.Contains(tr.Detail, "INQUIRY")
		}
		if !line || !cbw {
			t.Errorf("missing SET_LINE_CODING (%v) or INQUIRY CBW (%v)", line, cbw)
		}

		dev, err := a.AnalyzeUSB(app.USBQuery{Session: ref, Device: "1:7"})
		if err != nil || dev.Streams[0].Matching != 8 || len(dev.Streams[0].Transactions) != 8 {
			t.Fatalf("device filter: %+v err=%v", dev, err)
		}
		for _, tr := range dev.Streams[0].Transactions {
			if tr.Device != "1:7" {
				t.Errorf("device filter leaked %s", tr.Device)
			}
		}
		if dev.Streams[0].Stats.Packets != 92 {
			t.Error("stats must stay global under a device filter")
		}

		er, err := a.AnalyzeUSB(app.USBQuery{Session: ref, OnlyErrors: true})
		if err != nil || len(er.Streams[0].Transactions) != 2 {
			t.Fatalf("errors: %+v err=%v", er, err)
		}
		if _, err := a.AnalyzeUSB(app.USBQuery{Session: ref, Source: "nope"}); err == nil {
			if r, _ := a.AnalyzeUSB(app.USBQuery{Session: ref, Source: "nope"}); len(r.Streams) != 0 {
				t.Error("unknown source returned data")
			}
		}
	}
	check(a, "latest")

	if err := a.UseReplay("latest"); err != nil {
		t.Fatal(err)
	}
	a.Cfg.SessionsDir = t.TempDir()
	rr, err := a.RunExperiment(ctx, &experiment.Experiment{Name: "replay", Duration: experiment.Duration(2 * time.Second),
		Devices: []experiment.DeviceSpec{{Alias: "usb", Plugin: replay.PluginID, Device: "usb", Config: map[string]any{}}}}, false)
	if err != nil || rr.Status != "complete" {
		t.Fatalf("replay: %+v err=%v", rr, err)
	}
	check(a, rr.SessionID)
}
