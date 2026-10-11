package app_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"time"

	"github.com/faronov/hwlab/internal/analysis"
	"github.com/faronov/hwlab/internal/app"
	"github.com/faronov/hwlab/internal/experiment"
	"github.com/faronov/hwlab/internal/plot"
	"github.com/faronov/hwlab/internal/replay"
	"github.com/faronov/hwlab/internal/session"
	"github.com/faronov/hwlab/internal/timeline"
)

func TestPlotShowsNumberedSpikesAndPackets(t *testing.T) {
	a := buildExplainSession(t, nil)
	svg, err := a.RenderPlot(context.Background(), "latest", "p", "", plot.Spec{Format: "svg"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(svg.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{">#1<", ">#2<", "radio packets (3)"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("plot lacks %q", want)
		}
	}
	if strings.Contains(string(b), "6188aa") {
		t.Error("packet payload must not be rendered")
	}
	if out := os.Getenv("HWLAB_TEST_PLOT_OUT"); out != "" {
		if _, err := a.RenderPlot(context.Background(), "latest", "p", out, plot.Spec{Format: "png", Title: "synthetic combined session"}); err != nil {
			t.Fatal(err)
		}
	}
}

// buildExplainSession writes a synthetic combined session: a 5 µA baseline with
// two 1 ms, 5 mA spikes (at 100 ms and 400 ms) and radio packets around them.
func buildExplainSession(t *testing.T, radio *session.ClockMeta) *app.App {
	t.Helper()
	a := newApp(t)
	s, err := session.Create(a.Cfg.SessionsDir, "explain", "test")
	if err != nil {
		t.Fatal(err)
	}
	w, sm, err := s.CreateStream(session.StreamMeta{ID: "current", Kind: session.KindPower, Alias: "power", CaptureID: "power"})
	if err != nil {
		t.Fatal(err)
	}
	const period = 10_000
	var cur []int32
	for i := 0; i < 100_000; i++ { // 1 s
		ts := int64(i) * period
		v := int32(5_000)
		if (ts >= 100_000_000 && ts < 101_000_000) || (ts >= 400_000_000 && ts < 401_000_000) {
			v = 5_000_000
		}
		cur = append(cur, v)
	}
	for off := 0; off < len(cur); off += 1000 {
		if err := w.WritePower(&session.PowerChunk{StartNs: int64(off) * period, PeriodNs: period, CurrentNA: cur[off : off+1000]}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	pk := func(ts int64, hex string) {
		_, err := s.AppendEvent(timeline.Event{TimestampNs: ts, SourceID: "radio", Type: timeline.TypePacket,
			Data: map[string]any{"protocol": "ieee802154", "hex": hex, "len": len(hex) / 2, "rssi_dbm": -60.0, "lqi": 200.0}})
		if err != nil {
			t.Fatal(err)
		}
	}
	pk(98_000_000, "6188aa")  // data, before spike 1
	pk(400_900_000, "6188bb") // data, after spike 2 start
	pk(401_200_000, "02aabb") // ack
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	s.Update(func(m *session.Meta) {
		m.Devices = []session.DeviceMeta{{Alias: "power", DeviceID: "power", PluginID: "fake"}, {Alias: "radio", DeviceID: "radio", PluginID: "fake"}}
		m.Captures = []session.CaptureMeta{
			{ID: "power", Alias: "power", Streams: []session.StreamMeta{sm}, Clock: &session.ClockMeta{
				Source: "ppk2", ErrorMinNs: -2_000_000, ErrorMinBounded: true, ErrorMaxNs: 50_000, ErrorMaxBounded: true}},
			{ID: "radio", Alias: "radio", Clock: radio},
		}
	})
	if _, err := analysis.AnnotatePower(context.Background(), s, 3.3, analysis.SpikeOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	return a
}

func TestExplainSpikeWithDataAndAck(t *testing.T) {
	a := buildExplainSession(t, &session.ClockMeta{Source: "sniffer", ErrorMinBounded: true})
	x, err := a.Explain(context.Background(), app.ExplainQuery{Session: "latest", Spike: 2, NominalVoltage: 3.3})
	if err != nil {
		t.Fatal(err)
	}
	if x.Spike == nil || x.Spike.Total != 2 || x.Spike.Index != 2 {
		t.Fatalf("spike: %+v", x.Spike)
	}
	if d := x.Spike.StartNs - 400_000_000; d < -60_000 || d > 60_000 {
		t.Errorf("spike start %d not near 400 ms", x.Spike.StartNs)
	}
	if x.Spike.DurationNs < 900_000 || x.Spike.DurationNs > 1_200_000 {
		t.Errorf("duration %d", x.Spike.DurationNs)
	}
	if x.FirstTx == nil || x.FirstTx.Kind != "data" || x.AckAfter == nil || x.AckAfter.Kind != "ack" {
		t.Fatalf("data/ack: %+v %+v", x.FirstTx, x.AckAfter)
	}
	if x.DataAck == nil || *x.DataAck != 300_000 {
		t.Fatalf("data->ack %v", x.DataAck)
	}
	if x.InBurst["data"] != 1 || x.InBurst["ack"] != 0 {
		t.Errorf("burst packets: %v", x.InBurst)
	}
	if len(x.Before) != 0 {
		t.Errorf("packet 300 ms earlier is outside the window: %+v", x.Before)
	}
	if !x.Energy.HasEnergy || x.Energy.BurstUJ < 14 || x.Energy.BurstUJ > 20 { // 5 mA * 3.3 V * 1 ms = 16.5 µJ
		t.Errorf("burst energy %.2f µJ", x.Energy.BurstUJ)
	}
	if x.Energy.GapNs == 0 || x.Energy.GapMeanNA > 100_000 {
		t.Errorf("gap since previous spike: %+v", x.Energy)
	}
	// The sniffer side has no upper bound: the delta must say so and the
	// order of events so close to the spike start must not be asserted.
	if x.Timing == nil || x.Timing.LowBounded || x.Timing.Order != "undetermined" {
		t.Fatalf("timing: %+v", x.Timing)
	}
	if !strings.Contains(x.Timing.Statement, "no known bound") {
		t.Errorf("statement: %s", x.Timing.Statement)
	}
}

func TestExplainBoundedClocksGiveInterval(t *testing.T) {
	a := buildExplainSession(t, &session.ClockMeta{Source: "bounded", ErrorMinNs: 0, ErrorMinBounded: true, ErrorMaxNs: 1_000_000, ErrorMaxBounded: true})
	x, err := a.Explain(context.Background(), app.ExplainQuery{Session: "latest", Spike: 2})
	if err != nil {
		t.Fatal(err)
	}
	tm := x.Timing
	if tm == nil || !tm.LowBounded || !tm.HighBounded || tm.TrueLowNs > tm.DeltaNs || tm.TrueHighNs < tm.DeltaNs-1 {
		t.Fatalf("timing: %+v", tm)
	}
	// radio late by 0..1 ms and power early by 0.05..2 ms: width 1+2.05 ms.
	if w := tm.TrueHighNs - tm.TrueLowNs; w != 3_050_000 {
		t.Errorf("width = %d", w)
	}
}

func TestExplainPacketAndTimeAnchors(t *testing.T) {
	a := buildExplainSession(t, nil)
	x, err := a.Explain(context.Background(), app.ExplainQuery{Session: "latest", Packet: 1})
	if err != nil {
		t.Fatal(err)
	}
	if x.Anchor != "packet" || x.Spike == nil || x.Spike.Index != 1 || x.Spike.DistanceNs == 0 {
		t.Fatalf("packet anchor: %+v", x)
	}
	if x.Timing == nil || x.Timing.LowBounded || !strings.Contains(x.Timing.Statement, "missing") {
		t.Errorf("missing radio clock must be said: %+v", x.Timing)
	}
	y, err := a.Explain(context.Background(), app.ExplainQuery{Session: "latest", AtNs: 400_500_000, HasAt: true})
	if err != nil {
		t.Fatal(err)
	}
	if y.Spike == nil || y.Spike.Index != 2 || y.Spike.DistanceNs != 0 || y.Nearest == nil {
		t.Fatalf("time anchor: %+v", y)
	}
}

func TestExplainErrors(t *testing.T) {
	a := buildExplainSession(t, nil)
	for name, q := range map[string]app.ExplainQuery{
		"no anchor":  {Session: "latest"},
		"bad spike":  {Session: "latest", Spike: 9},
		"bad packet": {Session: "latest", Packet: 9},
	} {
		if _, err := a.Explain(context.Background(), q); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// A recorded session replays without any device and keeps its clock model, so
// that `explain` gives the same uncertainty on the replayed session.
func TestReplayKeepsClockAndExplain(t *testing.T) {
	ctx := context.Background()
	radio := &session.ClockMeta{Source: "sniffer", ErrorMinBounded: true, OffsetAppliedNs: -1_000_000, ErrorMinNs: -1_000_000, ErrorMaxNs: 4_000_000, ErrorMaxBounded: true}
	a := buildExplainSession(t, radio)
	orig, err := a.Explain(ctx, app.ExplainQuery{Session: "latest", Spike: 2})
	if err != nil {
		t.Fatal(err)
	}
	ms, _ := a.ListSessions()
	srcID := ms[0].ID

	b := newApp(t)
	b.Cfg.SessionsDir = a.Cfg.SessionsDir
	if err := b.UseReplay(srcID); err != nil {
		t.Fatal(err)
	}
	var devs []experiment.DeviceSpec
	for _, d := range ms[0].Devices {
		devs = append(devs, experiment.DeviceSpec{Alias: d.Alias, Plugin: replay.PluginID, Device: d.Alias, Config: map[string]any{}})
	}
	if len(devs) == 0 {
		t.Skip("synthetic session has no device records")
	}
	rr, err := b.RunExperiment(ctx, &experiment.Experiment{Name: "replay", Duration: experiment.Duration(time.Second), Devices: devs}, false)
	if err != nil || rr.Status != "complete" {
		t.Fatalf("replay: %+v err=%v", rr, err)
	}
	again, err := b.Explain(ctx, app.ExplainQuery{Session: rr.SessionID, Spike: 2})
	if err != nil {
		t.Fatal(err)
	}
	if again.Timing == nil || orig.Timing == nil || again.Timing.Statement != orig.Timing.Statement {
		t.Fatalf("replayed timing differs:\n%+v\n%+v", again.Timing, orig.Timing)
	}
}
