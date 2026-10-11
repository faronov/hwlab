package app_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/faronov/hwlab/internal/app"
	"github.com/faronov/hwlab/internal/config"
	"github.com/faronov/hwlab/internal/correlation"
	"github.com/faronov/hwlab/internal/testutil"
)

// REAL hardware fixture (examples/ppk2-saleae-shared-signal.yaml): one 100 Hz
// square wave from an nRF52840 DK (flash untouched) fed to PPK2 digital input
// D0 and Saleae Logic CH0 in ONE experiment/session. The session replays here
// with neither device nor any plugin. Device serials are replaced by
// placeholders; the timeline is otherwise as recorded.
func TestRealPPK2SaleaeSessionReplaysOffline(t *testing.T) {
	ctx := context.Background()
	src := filepath.Join(testutil.ModuleRoot(t), "testdata", "real-ppk2-saleae")
	sessions := filepath.Join(src, "2026-10-08T164412Z-ppk2-saleae-shared-signal-long")
	if _, err := os.Stat(sessions); err != nil {
		t.Skip("fixture missing")
	}
	cfg := config.Config{Home: t.TempDir(), SessionsDir: src}
	a := app.New(cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { a.Close(ctx) })

	info, err := a.SessionInfo("latest")
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Meta.Devices) != 3 {
		t.Fatalf("devices: %d", len(info.Meta.Devices))
	}
	if len(info.Meta.ClockCalibrations) != 1 {
		t.Fatalf("stored calibrations: %d", len(info.Meta.ClockCalibrations))
	}
	stored := info.Meta.ClockCalibrations[0]

	// Recompute from the stored timeline: the measurement must be reproducible.
	r, err := a.CalibrateClocks(app.ClockCalibrationQuery{Session: "latest", Ref: "logic", Dev: "ppk2", RefChan: "SHARED", DevChan: "D0"})
	if err != nil {
		t.Fatal(err)
	}
	c := r.Calibration
	if c.Pairs < 1500 {
		t.Errorf("only %d matched edges", c.Pairs)
	}
	if math.Abs(c.DriftPPM-stored.DriftPPM) > 0.001 || c.UncertaintyNs != stored.UncertaintyNs {
		t.Errorf("recomputed %+v differs from stored %+v", c, stored)
	}
	// Measured on real hardware: the PPK2 clock (sample count x 10 us) runs
	// about 93 ppm off the Saleae clock; a fixed offset cannot absorb that.
	if c.DriftPPM < 80 || c.DriftPPM > 105 {
		t.Errorf("drift %.2f ppm outside the measured 80..105 ppm", c.DriftPPM)
	}
	if c.DriftPPM < 3*c.DriftStdErrPPM {
		t.Errorf("drift %.2f ppm is not significant against %.3f ppm", c.DriftPPM, c.DriftStdErrPPM)
	}
	if c.ResidualP95Ns > 20_000 || c.UncertaintyNs > 200_000 {
		t.Errorf("residual p95 %d ns, uncertainty %d ns", c.ResidualP95Ns, c.UncertaintyNs)
	}
	if c.OffsetOnlyP95Ns < 10*c.ResidualP95Ns {
		t.Errorf("fixed offset p95 %d ns vs affine %d ns: the rate term should matter", c.OffsetOnlyP95Ns, c.ResidualP95Ns)
	}

	// Selected-window analysis: after mapping, every shared edge of the window
	// agrees within the claimed uncertainty (the raw timeline is ~4 ms apart).
	s, err := a.OpenSession("latest")
	if err != nil {
		t.Fatal(err)
	}
	m := correlation.Affine{RefDeviceNs: c.RefDeviceNs, RefSessionNs: c.RefSessionNs, Rate: c.Rate}
	ref := correlation.EdgesOf(s.Timeline(), "logic", "SHARED")
	dev := correlation.EdgesOf(s.Timeline(), "ppk2", "D0")
	match, err := correlation.MatchEdges(ref, dev, 0, 2*20_000)
	if err != nil {
		t.Fatal(err)
	}
	var worst, rawWorst int64
	for _, p := range match.Pairs {
		if p.Ref < 3_000_000_000 || p.Ref > 3_500_000_000 {
			continue
		}
		if d := absNs(p.Ref - m.Apply(p.Dev)); d > worst {
			worst = d
		}
		if d := absNs(p.Ref - p.Dev); d > rawWorst {
			rawWorst = d
		}
	}
	if worst == 0 || worst > c.UncertaintyNs {
		t.Errorf("window residual %d ns vs claimed uncertainty %d ns", worst, c.UncertaintyNs)
	}
	if rawWorst < 10*worst {
		t.Errorf("uncalibrated window offset %d ns should dwarf the calibrated %d ns", rawWorst, worst)
	}

	// Timeline queries and the combined plot work from the stored data only.
	ev, err := a.QueryTimeline(app.TimelineQuery{Session: "latest", Types: []string{"digital.edge"}, FromNs: 3_000_000_000, ToNs: 3_030_000_000})
	if err != nil || len(ev) < 6 {
		t.Fatalf("window events: %d err=%v", len(ev), err)
	}
	srcs := map[string]bool{}
	for _, e := range ev {
		srcs[e.SourceID] = true
	}
	if !srcs["ppk2"] || !srcs["logic"] {
		t.Errorf("window should hold edges of both devices: %v", srcs)
	}

	// The 300 s run (timeline not stored: 120k edges) keeps its measurement.
	b, err := os.ReadFile(filepath.Join(src, "long-300s-calibration.json"))
	if err != nil {
		t.Fatal(err)
	}
	var long struct {
		Synthetic bool `json:"synthetic"`
		Cals      []struct {
			Pairs           int     `json:"pairs"`
			SpanNs          int64   `json:"span_ns"`
			DriftPPM        float64 `json:"drift_ppm"`
			DriftStdErr     float64 `json:"drift_stderr_ppm"`
			ResidualP95Ns   int64   `json:"residual_p95_ns"`
			OffsetOnlyP95Ns int64   `json:"offset_only_p95_ns"`
			UncertaintyNs   int64   `json:"uncertainty_ns"`
		} `json:"clock_calibrations"`
	}
	if err := json.Unmarshal(b, &long); err != nil || len(long.Cals) != 1 || long.Synthetic {
		t.Fatalf("long calibration: %v %+v", err, long)
	}
	l := long.Cals[0]
	if l.SpanNs < 290e9 || l.Pairs < 50_000 {
		t.Errorf("long run too small: %+v", l)
	}
	if math.Abs(l.DriftPPM-c.DriftPPM) > 1 {
		t.Errorf("drift differs between runs: %.2f vs %.2f ppm", l.DriftPPM, c.DriftPPM)
	}
	if l.OffsetOnlyP95Ns < 5_000_000 {
		t.Errorf("a fixed offset should leave > 5 ms over 300 s, got %d ns", l.OffsetOnlyP95Ns)
	}
}

func absNs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
