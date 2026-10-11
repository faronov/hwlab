package app_test

import (
	"context"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/faronov/hwlab/internal/app"
	"github.com/faronov/hwlab/internal/config"
	"github.com/faronov/hwlab/internal/experiment"
	"github.com/faronov/hwlab/internal/plot"
	"github.com/faronov/hwlab/internal/replay"
	"github.com/faronov/hwlab/internal/testutil"
)

// The fixture is SYNTHETIC, not an instrument recording: it was produced by the
// picoscope plugin's built-in synthetic scope (examples/picoscope-fake.yaml,
// HWLAB_PICOSCOPE_FAKE=1). It proves the block-capture path, the analog
// artifact, the waveform analysis, plotting and replay; it says nothing about a
// real PicoScope, which has not been run on this host. It must replay with no
// plugin binary and no instrument.
func TestSyntheticPicoScopeSessionReplaysWithoutHardware(t *testing.T) {
	ctx := context.Background()
	src := filepath.Join(testutil.ModuleRoot(t), "testdata", "synthetic-picoscope")
	if _, err := os.Stat(src); err != nil {
		t.Skip("fixture missing")
	}
	cfg := config.Config{Home: t.TempDir(), SessionsDir: src}
	a := app.New(cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { a.Close(ctx) })

	near := func(what string, got *float64, want, tol float64) {
		t.Helper()
		if got == nil || math.Abs(*got-want) > tol {
			t.Errorf("%s = %v, want %g ± %g", what, got, want, tol)
		}
	}
	check := func(a *app.App, ref string) {
		t.Helper()
		inv, err := a.QueryTimeline(app.TimelineQuery{Session: ref, Types: []string{"invoke"}})
		if err != nil || len(inv) != 3 {
			t.Fatalf("invoke events: %d err=%v", len(inv), err)
		}
		for i, m := range []string{"info", "measure", "waveform"} {
			if inv[i].Data["method"] != m || inv[i].Data["ok"] != true {
				t.Errorf("invoke %d: %+v", i, inv[i].Data)
			}
		}
		if r, _ := inv[0].Data["result"].(map[string]any); r["synthetic"] != true || r["validated"] != false {
			t.Errorf("info must say synthetic and unvalidated: %v", r)
		}

		rep, err := a.MeasureWaveform(ctx, app.WaveformQuery{Session: ref, Source: "scope", Channel: "A", Points: 50})
		if err != nil {
			t.Fatal(err)
		}
		if len(rep.Streams) != 1 || len(rep.Streams[0].Segments) != 1 || rep.Streams[0].Unit != "V" {
			t.Fatalf("report %+v", rep)
		}
		sg := rep.Streams[0].Segments[0]
		if sg.Samples != 8000 || sg.PeriodNs != 500 || sg.Measurements == nil {
			t.Fatalf("segment %+v", sg)
		}
		m := sg.Measurements
		near("frequency", m.FrequencyHz, 1000, 5)
		near("duty", m.Duty, 0.5, 0.02)
		near("rise", m.RiseTimeS, 1.6e-6, 0.3e-6)
		near("fall", m.FallTimeS, 3.2e-6, 0.5e-6)
		if len(sg.Envelope.Min) > 50 {
			t.Errorf("envelope has %d buckets, asked for 50", len(sg.Envelope.Min))
		}

		b, err := a.MeasureWaveform(ctx, app.WaveformQuery{Session: ref, Channel: "b"})
		if err != nil {
			t.Fatal(err)
		}
		bm := b.Streams[0].Segments[0].Measurements
		near("B frequency", bm.FrequencyHz, 10000, 50)
		near("B rms", &bm.RMS, math.Sqrt2/2, 0.03)

		// the segment starts 400 us (10 % pre-trigger) before the rising edge,
		// so +500..+800 us lies inside the first high half
		w, err := a.MeasureWaveform(ctx, app.WaveformQuery{Session: ref, Channel: "A", FromNs: sg.StartNs + 500_000, ToNs: sg.StartNs + 800_000})
		if err != nil {
			t.Fatal(err)
		}
		if got := w.Streams[0].Segments[0].Samples; got != 600 {
			t.Errorf("window holds %d samples, want 600", got)
		}
		near("window mean", &w.Streams[0].Segments[0].Measurements.Mean, 3.3, 0.05)

		if _, err := a.MeasureWaveform(ctx, app.WaveformQuery{Session: ref, Channel: "Z"}); err == nil {
			t.Error("unknown channel accepted")
		}
		if _, err := a.MeasureWaveform(ctx, app.WaveformQuery{Session: ref, Points: 5000}); err == nil {
			t.Error("points above the cap accepted")
		}
		if _, err := a.MeasureWaveform(ctx, app.WaveformQuery{Session: ref, Source: "nope"}); err == nil {
			t.Error("unknown source accepted")
		}
		if _, err := a.MeasureWaveform(ctx, app.WaveformQuery{Session: ref, FromNs: sg.EndNs + 1_000_000_000}); err == nil {
			t.Error("a window past the data must be an error, not an empty success")
		}
		if _, err := a.RenderPlot(ctx, ref, "p", "", plot.Spec{Format: "svg"}); err != nil {
			t.Fatalf("plot: %v", err)
		}
	}
	check(a, "latest")

	if err := a.UseReplay("latest"); err != nil {
		t.Fatal(err)
	}
	a.Cfg.SessionsDir = t.TempDir()
	rr, err := a.RunExperiment(ctx, &experiment.Experiment{Name: "replay", Duration: experiment.Duration(3 * time.Second),
		Devices: []experiment.DeviceSpec{{Alias: "scope", Plugin: replay.PluginID, Device: "scope", Config: map[string]any{}}}}, false)
	if err != nil || rr.Status != "complete" {
		t.Fatalf("replay: %+v err=%v", rr, err)
	}
	if rr.Samples != 8000 {
		t.Errorf("replayed samples = %d, want 8000", rr.Samples)
	}
	check(a, rr.SessionID)
}

func TestWaveformNeedsAnAnalogStream(t *testing.T) {
	ctx := context.Background()
	src := filepath.Join(testutil.ModuleRoot(t), "testdata", "synthetic-telink")
	if _, err := os.Stat(src); err != nil {
		t.Skip("fixture missing")
	}
	a := app.New(config.Config{Home: t.TempDir(), SessionsDir: src}, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { a.Close(ctx) })
	if _, err := a.MeasureWaveform(ctx, app.WaveformQuery{Session: "latest"}); err == nil {
		t.Error("a session without analog streams must be an error")
	}
}
