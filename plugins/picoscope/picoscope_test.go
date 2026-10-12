package picoscopeplugin

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
	"github.com/faronov/hwlab/plugins/picoscope/backend"
)

func fakePlugin(t *testing.T) *Plugin {
	t.Helper()
	t.Setenv(FakeEnv, "1")
	return New()
}

func opened(t *testing.T, cfg map[string]any) *Plugin {
	t.Helper()
	p := fakePlugin(t)
	if err := p.OpenDevice(context.Background(), FakeID, cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.CloseDevice(context.Background(), FakeID) })
	return p
}

func invoke(t *testing.T, p *Plugin, method string, args map[string]any) (map[string]any, error) {
	t.Helper()
	s, err := structpb.NewStruct(args)
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.Invoke(context.Background(), &pluginv1.InvokeRequest{DeviceId: FakeID, Capability: pluginv1.CapOscilloscope, Method: method, Args: s})
	if err != nil {
		return nil, err
	}
	return r.Result.AsMap(), nil
}

func must(t *testing.T, p *Plugin, method string, args map[string]any) map[string]any {
	t.Helper()
	r, err := invoke(t, p, method, args)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	return r
}

func near(t *testing.T, what string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %g, want %g ± %g", what, got, want, tol)
	}
}

func meas(t *testing.T, r map[string]any, channel string) map[string]any {
	t.Helper()
	for _, c := range r["channels"].([]any) {
		cm := c.(map[string]any)
		if cm["channel"] == channel {
			return cm["measurements"].(map[string]any)
		}
	}
	t.Fatalf("no channel %s in %v", channel, r)
	return nil
}

const fastCfgRate = 20e6

func TestNoDeviceWithoutEnv(t *testing.T) {
	t.Setenv(FakeEnv, "")
	ds, err := New().ListDevices(context.Background())
	if err != nil || len(ds) != 0 {
		t.Fatalf("devices = %v, %v; want none", ds, err)
	}
}

func TestListingIsMarkedSyntheticAndUnvalidated(t *testing.T) {
	ds, err := fakePlugin(t).ListDevices(context.Background())
	if err != nil || len(ds) != 1 {
		t.Fatalf("%v %v", ds, err)
	}
	d := ds[0]
	if d.Properties["synthetic"] != "true" || d.Properties["validated"] != "false" {
		t.Errorf("props %v", d.Properties)
	}
	have := map[string]bool{}
	for _, c := range d.Capabilities {
		have[c.Id] = true
		if c.Attributes["streaming"] != "not-implemented" {
			t.Errorf("%s: streaming must be reported as not implemented", c.Id)
		}
	}
	for _, id := range []string{pluginv1.CapOscilloscope, pluginv1.CapAnalogCapture, pluginv1.CapTimedCapture, pluginv1.CapArtifactExport} {
		if !have[id] {
			t.Errorf("missing %s", id)
		}
	}
}

func TestInfoIsHonest(t *testing.T) {
	r := must(t, fakePlugin(t), "info", nil)
	if r["validated"] != false || r["synthetic"] != true || r["streaming"] != "not-implemented" {
		t.Errorf("%v", r)
	}
	if len(r["channels"].([]any)) != 4 {
		t.Errorf("channels %v", r["channels"])
	}
}

func TestMethodDangerLevels(t *testing.T) {
	info := fakePlugin(t).Info()
	if len(info.Methods) == 0 {
		t.Fatal("no methods")
	}
	for _, m := range info.Methods {
		if m.Capability != pluginv1.CapOscilloscope {
			t.Errorf("%s: capability %s", m.Name, m.Capability)
		}
		// nothing here drives a target or writes instrument non-volatile state
		if m.Danger != pluginv1.Danger_DANGER_SAFE {
			t.Errorf("%s: danger %v", m.Name, m.Danger)
		}
	}
}

func TestKnownAnswerMeasurements(t *testing.T) {
	p := opened(t, map[string]any{"channels": []any{"A", "B", "C"}})
	r := must(t, p, "acquire", map[string]any{"sample_rate_hz": fastCfgRate, "samples": 200000.0})
	a := meas(t, r, "A") // 1 kHz square, 0..3.3 V, 50 %, rise 2 us, fall 4 us
	near(t, "A freq", a["frequency_hz"].(float64), 1000, 5)
	near(t, "A duty", a["duty"].(float64), 0.5, 0.02)
	near(t, "A vpp", a["vpp"].(float64), 3.3, 0.05)
	near(t, "A rise", a["rise_time_s"].(float64), 1.6e-6, 0.3e-6)
	near(t, "A fall", a["fall_time_s"].(float64), 3.2e-6, 0.4e-6)
	b := meas(t, r, "B") // 10 kHz sine, +-1 V
	near(t, "B freq", b["frequency_hz"].(float64), 10000, 50)
	near(t, "B rms", b["rms"].(float64), math.Sqrt2/2, 0.02)
	c := meas(t, r, "C")                              // DC 1.2 V
	near(t, "C mean", c["mean"].(float64), 1.2, 0.04) // 8-bit on a 5 V range: LSB 0.039 V
	if r["synthetic"] != true {
		t.Error("acquire result not marked synthetic")
	}
}

func TestOverflowIsReported(t *testing.T) {
	p := opened(t, map[string]any{"channels": []any{"D"}, "range_v": 5.0})
	r := must(t, p, "acquire", map[string]any{"samples": 20000.0, "sample_rate_hz": 100e3})
	if r["channels"].([]any)[0].(map[string]any)["overflow"] != true {
		t.Errorf("50 Hz +-15 V on a 5 V range must overflow: %v", r)
	}
	must(t, p, "configure_channel", map[string]any{"channel": "D", "range_v": 20.0})
	r = must(t, p, "acquire", map[string]any{"samples": 20000.0, "sample_rate_hz": 100e3})
	if r["channels"].([]any)[0].(map[string]any)["overflow"] == true {
		t.Errorf("20 V range must not overflow: %v", r)
	}
}

func TestTrigger(t *testing.T) {
	p := opened(t, nil)
	must(t, p, "set_trigger", map[string]any{"trigger_source": "A", "trigger_level_v": 1.65, "trigger_slope": "rising"})
	r := must(t, p, "acquire", map[string]any{"sample_rate_hz": fastCfgRate, "samples": 20000.0, "pre_trigger": 0.25})
	if r["triggered"] != true {
		t.Fatalf("not triggered: %v", r)
	}
	near(t, "trigger index", r["trigger_index"].(float64), 5000, 2)

	// at a trigger point the signal passes the level, so the first sample after is above it
	w := must(t, p, "waveform", map[string]any{"channel": "A", "from_s": 0.0, "to_s": 2e-6, "points": 4.0})
	if min := w["channels"].([]any)[0].(map[string]any)["min"].(float64); min < 1 {
		t.Errorf("after a rising trigger the waveform should be high, min = %g", min)
	}

	// a level above the signal never triggers
	must(t, p, "set_trigger", map[string]any{"trigger_level_v": 4.5, "trigger_timeout_s": 0.05})
	_, err := invoke(t, p, "acquire", map[string]any{"samples": 1000.0})
	if status.Code(err) != codes.DeadlineExceeded {
		t.Errorf("no-trigger code = %v (%v)", status.Code(err), err)
	}
	// auto mode returns an untriggered block instead
	r = must(t, p, "acquire", map[string]any{"samples": 1000.0, "trigger_auto": true})
	if r["triggered"] != false || !strings.Contains(r["note"].(string), "untriggered") {
		t.Errorf("auto: %v", r)
	}
}

func TestFallingTrigger(t *testing.T) {
	p := opened(t, nil)
	r := must(t, p, "acquire", map[string]any{"trigger_source": "A", "trigger_level_v": 1.65, "trigger_slope": "falling", "sample_rate_hz": fastCfgRate, "samples": 20000.0, "pre_trigger": 0.5})
	if r["triggered"] != true {
		t.Fatalf("%v", r)
	}
	w := must(t, p, "waveform", map[string]any{"from_s": 0.0, "to_s": 2e-6, "points": 4.0})
	if max := w["channels"].([]any)[0].(map[string]any)["max"].(float64); max > 2.5 {
		t.Errorf("after a falling trigger the waveform should be low, max = %g", max)
	}
}

func TestSettingsValidation(t *testing.T) {
	p := opened(t, nil)
	for name, tc := range map[string]struct {
		method string
		args   map[string]any
	}{
		"unknown channel":   {"configure_channel", map[string]any{"channel": "Z"}},
		"missing channel":   {"configure_channel", map[string]any{"enabled": true}},
		"unknown arg":       {"configure_channel", map[string]any{"channel": "A", "bogus": 1.0}},
		"bad range":         {"configure_channel", map[string]any{"channel": "A", "range_v": 3.0}},
		"bad coupling":      {"configure_channel", map[string]any{"channel": "A", "coupling": "gnd"}},
		"offset too large":  {"configure_channel", map[string]any{"channel": "A", "offset_v": 100.0}},
		"enabled not bool":  {"configure_channel", map[string]any{"channel": "A", "enabled": "yes"}},
		"unknown key":       {"set_timebase", map[string]any{"samplerate": 1.0}},
		"zero samples":      {"set_timebase", map[string]any{"samples": 0.0}},
		"bad resolution":    {"set_timebase", map[string]any{"resolution_bits": 10.0}},
		"bad pre trigger":   {"set_timebase", map[string]any{"pre_trigger": 1.5}},
		"huge block":        {"set_timebase", map[string]any{"samples": 7e6}},
		"bad trigger src":   {"set_trigger", map[string]any{"trigger_source": "Q"}},
		"bad trigger slope": {"set_trigger", map[string]any{"trigger_slope": "sideways"}},
		"bad window":        {"measure", map[string]any{"from_s": "x"}},
	} {
		_, err := invoke(t, p, tc.method, tc.args)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if tc.method == "measure" {
			continue // no block yet: precondition comes first
		}
		if c := status.Code(err); c != codes.InvalidArgument {
			t.Errorf("%s: code %v (%v)", name, c, err)
		}
	}
	// the failed calls changed nothing
	tb := must(t, p, "timebase", nil)
	near(t, "samples", tb["samples"].(float64), 10000, 0)
}

func TestRateIsQuantisedAndReported(t *testing.T) {
	p := opened(t, nil)
	r := must(t, p, "set_timebase", map[string]any{"sample_rate_hz": 3e6})
	if r["rate_adjusted"] != true {
		t.Errorf("3 MS/s is not on the grid of a 1 GS/s clock; %v", r)
	}
	got := r["sample_rate_hz"].(float64)
	if got == 3e6 || math.Abs(got-3e6) > 1e6 {
		t.Errorf("achieved rate %g", got)
	}
	r = must(t, p, "set_timebase", map[string]any{"sample_rate_hz": 5e9})
	near(t, "clamped rate", r["sample_rate_hz"].(float64), r["max_sample_rate_hz"].(float64), 1)
	if r["note"] == nil {
		t.Error("a limited rate must be explained")
	}
}

func TestLimitsDependOnSetup(t *testing.T) {
	p := opened(t, nil)
	one := must(t, p, "info", nil)["max_sample_rate_hz"].(float64)
	must(t, p, "configure_channel", map[string]any{"channel": "B", "enabled": true})
	must(t, p, "configure_channel", map[string]any{"channel": "C", "enabled": true})
	three := must(t, p, "info", nil)["max_sample_rate_hz"].(float64)
	if three >= one {
		t.Errorf("rate with three channels (%g) should be below one channel (%g)", three, one)
	}
	r := must(t, p, "set_timebase", map[string]any{"resolution_bits": 12.0, "sample_rate_hz": 1e9})
	if r["sample_rate_hz"].(float64) > 250e6 {
		t.Errorf("12-bit rate %v", r["sample_rate_hz"])
	}
}

func TestLastChannelCannotBeDisabled(t *testing.T) {
	p := opened(t, nil)
	if _, err := invoke(t, p, "configure_channel", map[string]any{"channel": "A", "enabled": false}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("disabling the only channel: %v", err)
	}
}

func TestTransientCallsCannotMutate(t *testing.T) {
	p := fakePlugin(t) // not opened
	for _, m := range []struct {
		name string
		args map[string]any
	}{{"configure_channel", map[string]any{"channel": "A", "range_v": 1.0}}, {"set_timebase", map[string]any{"samples": 100.0}}, {"set_trigger", map[string]any{"trigger_source": "none"}}} {
		_, err := invoke(t, p, m.name, m.args)
		if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "open the scope") {
			t.Errorf("%s: %v", m.name, err)
		}
	}
	// but one transient acquire is self-contained
	r := must(t, p, "acquire", map[string]any{"channels": []any{"C"}, "samples": 1000.0})
	near(t, "C", meas(t, r, "C")["mean"].(float64), 1.2, 0.04)
	// and nothing survives it
	if _, err := invoke(t, p, "measure", nil); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("measure without a block: %v", err)
	}
}

func TestMeasureAndWaveformUseTheLastBlock(t *testing.T) {
	p := opened(t, map[string]any{"channels": []any{"A", "B"}})
	if _, err := invoke(t, p, "waveform", nil); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("waveform before acquire: %v", err)
	}
	must(t, p, "acquire", map[string]any{"sample_rate_hz": fastCfgRate, "samples": 200000.0})

	// 0-0.5 ms is the high half of the first square-wave cycle
	m := must(t, p, "measure", map[string]any{"channel": "A", "from_s": 0.0001, "to_s": 0.0004})
	near(t, "window mean", m["measurements"].(map[string]any)["mean"].(float64), 3.3, 0.05)

	w := must(t, p, "waveform", map[string]any{"channels": []any{"A", "B"}, "points": 50.0})
	chs := w["channels"].([]any)
	if len(chs) != 2 {
		t.Fatalf("%v", w)
	}
	env := chs[0].(map[string]any)["envelope"].(map[string]any)
	if n := len(env["min"].([]any)); n < 1 || n > 50 {
		t.Errorf("envelope has %d buckets, asked for at most 50", n)
	}
	if _, err := invoke(t, p, "waveform", map[string]any{"points": 5000.0}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("points above the cap: %v", err)
	}
	if _, err := invoke(t, p, "waveform", map[string]any{"channel": "D"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("channel not in block: %v", err)
	}
	if _, err := invoke(t, p, "waveform", map[string]any{"from_s": 99.0, "to_s": 100.0}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("empty window: %v", err)
	}
}

func TestWaveformNeverReturnsRawSamples(t *testing.T) {
	p := opened(t, nil)
	must(t, p, "set_timebase", map[string]any{"sample_rate_hz": fastCfgRate, "samples": 200000.0})
	r := must(t, p, "acquire", nil)
	w := must(t, p, "waveform", map[string]any{"points": 1000.0})
	for _, rr := range []map[string]any{r, w} {
		b, _ := structpb.NewStruct(rr)
		if n := len(b.String()); n > 100_000 {
			t.Errorf("result is %d bytes: raw samples must not travel through the RPC", n)
		}
	}
}

func startCap(t *testing.T, p *Plugin, cfg map[string]any, dir string) (pluginsdk.Capture, *pluginsdk.Sink, error) {
	t.Helper()
	sink := pluginsdk.NewTestSink(256)
	c, start, err := p.StartCapture(context.Background(), pluginsdk.CaptureRequest{DeviceID: FakeID, CaptureID: "c1", Config: cfg, ArtifactDir: dir, Clock: pluginsdk.NewClock(1_000_000)}, sink)
	if err == nil {
		if len(start.Streams) != 1 || start.Streams[0].Attributes["synthetic"] != "true" || start.Streams[0].Kind != "analog" {
			t.Errorf("stream %+v", start.Streams)
		}
	}
	return c, sink, err
}

func TestCaptureEventsAndArtifact(t *testing.T) {
	for _, open := range []bool{false, true} {
		var p *Plugin
		if open {
			p = opened(t, nil)
		} else {
			p = fakePlugin(t)
		}
		dir := t.TempDir()
		c, sink, err := startCap(t, p, map[string]any{"channels": []any{"A", "B"}, "sample_rate_hz": 1e6, "samples": 120000.0,
			"trigger_source": "A", "trigger_level_v": 1.65, "pre_trigger": 0.1}, dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Wait(); err != nil {
			t.Fatal(err)
		}
		res := c.Result()
		evs := sink.Drain()
		if len(evs) != 3 {
			t.Fatalf("open=%v: %d events, want 3 chunks of at most %d frames", open, len(evs), maxChunk)
		}
		var frames int
		for i, ev := range evs {
			a := ev.GetAnalog()
			if a == nil || len(a.Channels) != 2 || a.Unit != "V" || a.SamplePeriodNs != 1000 {
				t.Fatalf("event %d: %+v", i, ev)
			}
			if len(a.Samples)%2 != 0 {
				t.Errorf("event %d: samples not interleaved by 2", i)
			}
			if i > 0 && ev.TimestampNs-evs[i-1].TimestampNs != int64(maxChunk)*1000 {
				t.Errorf("event %d: spacing %d ns", i, ev.TimestampNs-evs[i-1].TimestampNs)
			}
			frames += len(a.Samples) / 2
		}
		if frames != 120000 || res.Stats.Samples != 120000 || res.Stats.Events != 3 {
			t.Errorf("frames %d stats %+v", frames, res.Stats)
		}
		if !strings.Contains(res.Clock.Source, "synthetic") || res.Clock.Note == "" {
			t.Errorf("clock must say synthetic: %+v", res.Clock)
		}
		if len(res.Artifacts) != 1 || res.Artifacts[0].Metadata["synthetic"] != "true" {
			t.Fatalf("artifacts %+v", res.Artifacts)
		}
		b, err := os.ReadFile(filepath.Join(dir, "waveform.csv"))
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		if lines[0] != "time_s,A,B" || len(lines) != 120001 {
			t.Errorf("csv header %q lines %d", lines[0], len(lines))
		}
		// time zero is the trigger point: 10 % of the block comes before it
		if !strings.HasPrefix(lines[12001], "0,") {
			t.Errorf("trigger row: %q", lines[12001])
		}
		// the last block stays available for measure after the capture
		if open {
			must(t, p, "measure", map[string]any{"channel": "B"})
		}
	}
}

func TestCaptureChunkTimestampsPreservePreTriggerSpacing(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ref       int64
		triggered bool
	}{
		{"pretrigger before session epoch", 1_000_000, true},
		{"pretrigger after session epoch", 100_000_000, true},
		{"untriggered", 1_000_000, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const frames = 120_000
			const trigger = 60_000
			const period = int64(1000)
			acq := &acquisition{blk: &backend.Block{
				SampleIntervalS: 1e-6, Samples: frames,
				Triggered: tc.triggered, TriggerIndex: trigger,
				Channels: []backend.ChannelData{{Name: "A", ADC: make([]int16, frames), MaxADC: 32767, RangeV: 5}},
			}}
			sink := pluginsdk.NewTestSink(3)
			c := &capture{sink: sink}
			if err := c.emit(context.Background(), acq, tc.ref); err != nil {
				t.Fatal(err)
			}
			evs := sink.Drain()
			if len(evs) != 3 || c.samples != frames || c.events != 3 {
				t.Fatalf("events=%d stats=%d/%d", len(evs), c.samples, c.events)
			}
			first := tc.ref
			if tc.triggered {
				first -= trigger * period
			}
			var seen int
			for i, ev := range evs {
				want := first + int64(seen)*period
				if ev.TimestampNs != want || ev.GetAnalog().SamplePeriodNs != period {
					t.Errorf("chunk %d: timestamp=%d period=%d, want %d/%d",
						i, ev.TimestampNs, ev.GetAnalog().SamplePeriodNs, want, period)
				}
				seen += len(ev.GetAnalog().Samples)
			}
			if seen != frames {
				t.Errorf("sample count=%d, want %d", seen, frames)
			}
		})
	}
}

func TestCaptureValidation(t *testing.T) {
	p := opened(t, nil)
	for name, cfg := range map[string]map[string]any{
		"unknown key": {"samplerate": 1.0},
		"bad channel": {"channels": []any{"Q"}},
		"bad rate":    {"sample_rate_hz": -1.0},
		"too big":     {"samples": 9e9},
	} {
		if _, _, err := p.StartCapture(context.Background(), pluginsdk.CaptureRequest{DeviceID: FakeID, Config: cfg, ArtifactDir: t.TempDir(), Clock: pluginsdk.NewClock(0)}, pluginsdk.NewTestSink(1)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, _, err := p.StartCapture(context.Background(), pluginsdk.CaptureRequest{DeviceID: FakeID, Clock: pluginsdk.NewClock(0)}, pluginsdk.NewTestSink(1)); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("no artifact dir: %v", err)
	}
	// a rejected start must not leave the device busy
	c, _, err := startCap(t, p, nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestCaptureCancelDuringTriggerWait(t *testing.T) {
	p := opened(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	c, _, err := p.StartCapture(ctx, pluginsdk.CaptureRequest{DeviceID: FakeID, ArtifactDir: t.TempDir(), Clock: pluginsdk.NewClock(0),
		Config: map[string]any{"trigger_source": "A", "trigger_level_v": 4.5, "trigger_timeout_s": 30.0}}, pluginsdk.NewTestSink(8))
	if err != nil {
		t.Fatal(err)
	}
	// busy while running: instrument access is refused, reads of settings are not
	if _, err := invoke(t, p, "acquire", nil); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("acquire during capture: %v", err)
	}
	if _, err := invoke(t, p, "set_timebase", map[string]any{"samples": 100.0}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("set_timebase during capture: %v", err)
	}
	must(t, p, "info", nil)
	must(t, p, "trigger", nil)
	if _, _, err := p.StartCapture(ctx, pluginsdk.CaptureRequest{DeviceID: FakeID, ArtifactDir: t.TempDir(), Clock: pluginsdk.NewClock(0)}, pluginsdk.NewTestSink(8)); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("second capture: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	cancel()
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("a cancelled capture is not an error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not stop the capture")
	}
	must(t, p, "info", nil) // device is usable again
	must(t, p, "acquire", nil)
}

func TestTriggerTimeoutFailsCapture(t *testing.T) {
	p := opened(t, nil)
	c, _, err := startCap(t, p, map[string]any{"trigger_source": "A", "trigger_level_v": 4.5, "trigger_timeout_s": 0.05}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Wait(); err == nil {
		t.Error("an untriggered capture must fail, not silently produce nothing")
	}
}

func TestUnknownMethodAndCapability(t *testing.T) {
	p := fakePlugin(t)
	if _, err := invoke(t, p, "frobnicate", nil); status.Code(err) != codes.Unimplemented {
		t.Errorf("%v", err)
	}
	if _, err := p.Invoke(context.Background(), &pluginv1.InvokeRequest{DeviceId: FakeID, Capability: pluginv1.CapDigitalCapture, Method: "x"}); status.Code(err) != codes.Unimplemented {
		t.Errorf("%v", err)
	}
	if _, err := p.Invoke(context.Background(), &pluginv1.InvokeRequest{DeviceId: "nope", Capability: pluginv1.CapOscilloscope, Method: "info"}); err == nil {
		t.Error("unknown device accepted")
	}
}
