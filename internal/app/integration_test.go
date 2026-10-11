package app_test

import (
	"bytes"
	"context"
	"image/png"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/faronov/hwlab/internal/app"
	"github.com/faronov/hwlab/internal/config"
	"github.com/faronov/hwlab/internal/experiment"
	"github.com/faronov/hwlab/internal/plot"
	"github.com/faronov/hwlab/internal/policy"
	"github.com/faronov/hwlab/internal/replay"
	"github.com/faronov/hwlab/internal/testutil"
	"github.com/faronov/hwlab/internal/timeline"
)

func newApp(t *testing.T) *app.App {
	t.Helper()
	cfg := config.Config{Home: t.TempDir(), PluginPaths: []string{testutil.BuildFakePlugin(t)}}
	cfg.SessionsDir = filepath.Join(cfg.Home, "sessions")
	a := app.New(cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { a.Close(context.Background()) })
	return a
}

func loadExample(t *testing.T) *experiment.Experiment {
	t.Helper()
	e, err := experiment.Load(filepath.Join(testutil.ModuleRoot(t), "examples", "fake-experiment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func count(evs []timeline.Event, typ string) int {
	n := 0
	for _, e := range evs {
		if e.Type == typ {
			n++
		}
	}
	return n
}

// TestFakeExperimentPipeline is the Milestone 1 definition-of-done test:
// fake power + fake packets -> unified timeline -> correlation -> plot, and
// then replay of the stored session without the plugin.
func TestFakeExperimentPipeline(t *testing.T) {
	ctx := context.Background()
	a := newApp(t)

	res, err := a.RunExperiment(ctx, loadExample(t), true)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Status != "complete" || res.Error != "" {
		t.Fatalf("status=%q error=%q warnings=%v", res.Status, res.Error, res.Warnings)
	}
	if res.Samples != 100_000 {
		t.Errorf("samples=%d, want 100000 (1 s at 100 kSa/s)", res.Samples)
	}
	// 1 s of simulated time, content ends slightly after 1 s because the
	// capture is anchored after device open.
	if d := time.Duration(res.DurationNs); d < time.Second || d > 1100*time.Millisecond {
		t.Errorf("duration=%v", d)
	}

	// Session layout.
	for _, f := range []string{"session.json", "timeline.jsonl", "experiment.yaml", "plots/overview.png", "plots/overview.svg", "plots/first-tx.png", "artifacts/radio/radio0.pcap"} {
		if st, err := os.Stat(filepath.Join(res.Dir, f)); err != nil || st.Size() == 0 {
			t.Errorf("session file %s missing or empty: %v", f, err)
		}
	}
	if raw, err := os.ReadFile(filepath.Join(res.Dir, "plots", "overview.svg")); err != nil || !strings.Contains(string(raw), "<svg") {
		t.Errorf("overview.svg is not an SVG: %v", err)
	}
	pngBytes, err := os.ReadFile(filepath.Join(res.Dir, "plots", "overview.png"))
	if err != nil {
		t.Fatal(err)
	}
	if img, err := png.Decode(bytes.NewReader(pngBytes)); err != nil || img.Bounds().Dx() < 400 {
		t.Errorf("overview.png does not decode: %v", err)
	}

	// Unified timeline holds events from all four sources.
	evs, err := a.QueryTimeline(app.TimelineQuery{Session: res.SessionID, Limit: 10000})
	if err != nil {
		t.Fatal(err)
	}
	for typ, want := range map[string]int{"packet": 20, "log": 20, "marker": 10, "digital.edge": 40, "analysis.power_rise": 10, "analysis.power_fall": 10} {
		if got := count(evs, typ); got != want {
			t.Errorf("%s events = %d, want %d", typ, got, want)
		}
	}
	sources := map[string]bool{}
	for i, e := range evs {
		sources[e.SourceID] = true
		if i > 0 && e.TimestampNs < evs[i-1].TimestampNs {
			t.Fatalf("timeline not sorted at %d", i)
		}
	}
	for _, s := range []string{"power", "logic", "radio", "debug"} {
		if !sources[s] {
			t.Errorf("no events from source %q", s)
		}
	}

	// Correlation: every power rise is preceded by the radio TX packet.
	cor, err := a.Correlate(app.CorrelateQuery{
		Session: res.SessionID, WindowNs: int64(time.Millisecond),
		A: timeline.Filter{Types: []string{"analysis.power_rise"}}, B: timeline.Filter{Types: []string{"packet"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cor.Anchors != 10 || cor.Matched != 10 {
		t.Errorf("anchors=%d matched=%d, want 10/10", cor.Anchors, cor.Matched)
	}
	rise := evs[0]
	for _, e := range evs {
		if e.Type == "analysis.power_rise" {
			rise = e
			break
		}
	}
	near, err := a.Nearest(res.SessionID, rise.TimestampNs, timeline.Filter{Types: []string{"packet"}})
	if err != nil || !near.Found || near.Delta > int64(time.Millisecond) || near.Delta < -int64(time.Millisecond) {
		t.Errorf("nearest packet: %+v err=%v", near, err)
	}

	// Analysis and an on-demand plot.
	an, err := a.Analyze(ctx, app.AnalyzeOptions{Session: res.SessionID, Spikes: true, NominalVoltage: 3.3})
	if err != nil || len(an) != 1 || len(an[0].Spikes) != 10 {
		t.Fatalf("analyze: %+v err=%v", an, err)
	}
	pr, err := a.RenderPlot(ctx, res.SessionID, "custom", "", plot.Spec{Format: "png", StartNs: 10_000_000, EndNs: 18_000_000})
	if err != nil || pr.Bytes == 0 || pr.Tracks < 2 {
		t.Errorf("render plot: %+v err=%v", pr, err)
	}

	// Replay: no plugin process involved; same events come out of the pipeline.
	b := newApp(t)
	b.Cfg.SessionsDir = a.Cfg.SessionsDir
	if err := b.UseReplay(res.SessionID); err != nil {
		t.Fatal(err)
	}
	var devs []experiment.DeviceSpec
	info, err := b.SessionInfo(res.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range info.Meta.Devices {
		devs = append(devs, experiment.DeviceSpec{Alias: d.Alias, Plugin: replay.PluginID, Device: d.Alias, Config: map[string]any{}})
	}
	rr, err := b.RunExperiment(ctx, &experiment.Experiment{Name: "replay", Duration: experiment.Duration(time.Duration(res.DurationNs)), Devices: devs}, false)
	if err != nil || rr.Status != "complete" {
		t.Fatalf("replay run: %+v err=%v", rr, err)
	}
	if rr.Samples != res.Samples {
		t.Errorf("replayed samples=%d, want %d", rr.Samples, res.Samples)
	}
	for typ, want := range res.Events {
		if rr.Events[typ] != want {
			t.Errorf("replayed %s events = %d, want %d", typ, rr.Events[typ], want)
		}
	}
}

func TestDangerousConfigNeedsConfirmation(t *testing.T) {
	a := newApp(t)
	e := loadExample(t)
	// supply_mv is declared dangerous by the fake plugin.
	e.Devices[0].Config["supply_mv"] = 3000
	_, err := a.RunExperiment(context.Background(), e, false)
	if !policy.IsConfirmationRequired(err) {
		t.Fatalf("want confirmation-required error, got %v", err)
	}
	if ss, _ := a.ListSessions(); len(ss) != 0 {
		t.Errorf("a refused run must not create a session, found %d", len(ss))
	}
}

func TestInvokeRespectsDeclaredMethodsAndDanger(t *testing.T) {
	ctx := context.Background()
	a := newApp(t)
	_, err := a.Invoke(ctx, app.InvokeRequest{Plugin: "fake", Device: "power0", Capability: "PowerSource", Method: "set_voltage", Args: map[string]any{"volts": 3.0}})
	if !policy.IsConfirmationRequired(err) {
		t.Fatalf("set_voltage without confirm: %v", err)
	}
	out, err := a.Invoke(ctx, app.InvokeRequest{Plugin: "fake", Device: "power0", Capability: "PowerSource", Method: "set_voltage", Args: map[string]any{"volts": 3.0}, Confirm: true})
	if err != nil || out["volts"] != 3.0 {
		t.Fatalf("confirmed set_voltage: %v %v", out, err)
	}
	if _, err := a.Invoke(ctx, app.InvokeRequest{Plugin: "fake", Device: "power0", Capability: "PowerSource", Method: "explode"}); err == nil {
		t.Error("undeclared methods must be refused")
	}
}
