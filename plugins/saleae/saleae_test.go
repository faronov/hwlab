package saleae

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
	"github.com/faronov/hwlab/plugins/saleae/internal/fakelogic2"
)

func digitalCfg(extra map[string]any) map[string]any {
	m := map[string]any{
		"digital_channels":    []any{0.0, 1.0, 2.0},
		"digital_sample_rate": 10e6,
		"channel_names":       map[string]any{"0": "CLK", "1": "MOSI", "2": "IRQ"},
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

type runResult struct {
	events []*pluginv1.Event
	res    *pluginv1.StopCaptureResponse
	err    error
}

// runCapture starts a capture like the SDK would: the context is cancelled
// when the duration elapses or when stop fires.
func runCapture(t *testing.T, p *Plugin, cfg map[string]any, dur time.Duration, cancelAfter time.Duration) runResult {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sink := pluginsdk.NewTestSink(4096)
	c, _, err := p.StartCapture(ctx, pluginsdk.CaptureRequest{DeviceID: "F4241", CaptureID: "c1", Config: cfg,
		Duration: dur, ArtifactDir: t.TempDir(), Clock: pluginsdk.NewClock(1_000_000)}, sink)
	if err != nil {
		return runResult{err: err}
	}
	stop := dur
	if cancelAfter > 0 {
		stop = cancelAfter
	}
	time.AfterFunc(stop, cancel)
	werr := c.Wait()
	return runResult{events: sink.Drain(), res: c.Result(), err: werr}
}

func TestDiscovery(t *testing.T) {
	_, addr := fakelogic2.Start(t)
	devs, err := NewWithAddr(addr).ListDevices(context.Background())
	if err != nil || len(devs) != 1 {
		t.Fatalf("devices: %v err=%v", devs, err)
	}
	d := devs[0]
	if d.Id != "F4241" || d.Name != "Saleae Logic Pro 16" || d.Properties["logic2_version"] != "fake-2.4.0" {
		t.Errorf("device: %+v", d)
	}
	for _, c := range d.Capabilities {
		if c.Id == pluginv1.CapLiveRawStream {
			t.Error("Logic 2 must not advertise LiveRawStream")
		}
	}
	if err := NewWithAddr(addr).OpenDevice(context.Background(), "F4241", nil); err != nil {
		t.Errorf("open: %v", err)
	}
	if err := NewWithAddr(addr).OpenDevice(context.Background(), "NOPE", nil); err == nil {
		t.Error("opening an unlisted device must fail")
	}
}

func TestDiscoveryWithoutLogic2(t *testing.T) {
	// Nothing listens here: discovery must be empty and fast, not an error.
	p := NewWithAddr("127.0.0.1:1")
	start := time.Now()
	devs, err := p.ListDevices(context.Background())
	if err != nil || len(devs) != 0 {
		t.Fatalf("devs=%v err=%v", devs, err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("discovery took %v", time.Since(start))
	}
	if err := p.OpenDevice(context.Background(), "x", nil); err == nil || !strings.Contains(err.Error(), "not reachable") {
		t.Errorf("open: %v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	srv, addr := fakelogic2.Start(t)
	p := NewWithAddr(addr)
	for name, cfg := range map[string]map[string]any{
		"no channels":        {},
		"missing rate":       {"digital_channels": []any{0.0}},
		"missing analograte": {"analog_channels": []any{0.0}},
		"bad channel":        {"digital_channels": []any{40.0}, "digital_sample_rate": 1e6},
		"bad rate":           {"digital_channels": []any{0.0}, "digital_sample_rate": -1.0},
		"bad analyzer":       digitalCfg(map[string]any{"analyzers": []any{map[string]any{"type": "foo"}}}),
		"bad role":           digitalCfg(map[string]any{"analyzers": []any{map[string]any{"type": "spi", "channels": map[string]any{"xyz": 1.0}}}}),
	} {
		_, _, err := p.StartCapture(context.Background(), pluginsdk.CaptureRequest{DeviceID: "F4241", Config: cfg,
			Duration: time.Second, ArtifactDir: t.TempDir(), Clock: pluginsdk.NewClock(0)}, pluginsdk.NewTestSink(8))
		if err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if calls := srv.Calls(); slices.Contains(calls, "StartCapture") {
		t.Error("invalid config must be rejected before talking to Logic 2")
	}
}

func TestTimedDigitalCapture(t *testing.T) {
	srv, addr := fakelogic2.Start(t)
	r := runCapture(t, NewWithAddr(addr), digitalCfg(nil), 300*time.Millisecond, 0)
	if r.err != nil {
		t.Fatal(r.err)
	}
	req := srv.StartRequest(1)
	if tm := req.GetCaptureConfiguration().GetTimedCaptureMode(); tm == nil || tm.DurationSeconds != 0.3 {
		t.Errorf("capture mode: %v", req.GetCaptureConfiguration())
	}
	dc := req.GetLogicDeviceConfiguration()
	if !slices.Equal(dc.GetLogicChannels().GetDigitalChannels(), []uint32{0, 1, 2}) || dc.DigitalSampleRate != 10_000_000 {
		t.Errorf("device config: %v", dc)
	}
	calls := srv.Calls()
	if slices.Contains(calls, "StopCapture") {
		t.Errorf("a timed capture that ends by itself must not be stopped: %v", calls)
	}
	if calls[len(calls)-1] != "CloseCapture" {
		t.Errorf("capture not closed: %v", calls)
	}

	var edges []*pluginv1.Event
	for _, e := range r.events {
		if e.GetDigital() != nil {
			edges = append(edges, e)
		}
	}
	if len(edges) != 1 {
		t.Fatalf("digital events: %d (%v)", len(edges), r.events)
	}
	db := edges[0].GetDigital()
	if !slices.Equal(db.Channels, []string{"CLK", "MOSI", "IRQ"}) {
		t.Errorf("channels: %v", db.Channels)
	}
	e := db.GetEdges()
	if e.InitialState != 0 || !slices.Equal(e.OffsetNs, []int64{100_000, 200_000, 200_500, 201_000, 300_000}) ||
		!slices.Equal(e.State, []uint32{4, 7, 6, 5, 1}) {
		t.Errorf("edges: %+v", e)
	}
	if edges[0].TimestampNs < 1_000_000 {
		t.Errorf("timestamp %d not anchored after the clock origin", edges[0].TimestampNs)
	}

	names := map[string]string{}
	for _, a := range r.res.Artifacts {
		names[a.Name] = a.Kind
		if !filepath.IsAbs(a.Path) {
			t.Errorf("artifact path not absolute: %s", a.Path)
		}
		if _, err := os.Stat(a.Path); err != nil {
			t.Errorf("artifact missing: %v", err)
		}
	}
	if names["capture.sal"] != "native-capture" || names["digital.csv"] != "csv" {
		t.Errorf("artifacts: %v", names)
	}
	ck := r.res.Clock
	if ck.ErrorMinBounded || ck.ErrorMaxBounded {
		t.Errorf("must not claim timing bounds that are not measured: %+v", ck)
	}
	if ck.ResolutionNs != 100 || !strings.Contains(ck.Note, "not measured") {
		t.Errorf("clock info: %+v", ck)
	}
}

func TestManualCaptureStopsOnCancel(t *testing.T) {
	srv, addr := fakelogic2.Start(t)
	r := runCapture(t, NewWithAddr(addr), digitalCfg(nil), 0, 100*time.Millisecond)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if srv.StartRequest(1).GetCaptureConfiguration().GetManualCaptureMode() == nil {
		t.Error("no duration must use manual capture mode")
	}
	calls := srv.Calls()
	if slices.Contains(calls, "WaitCapture") || !slices.Contains(calls, "StopCapture") {
		t.Errorf("manual capture: WaitCapture is illegal, StopCapture required: %v", calls)
	}
	if len(r.events) == 0 {
		t.Error("data must still be exported after the stop")
	}
}

func TestEarlyCancelOfTimedCaptureStopsImmediately(t *testing.T) {
	srv, addr := fakelogic2.Start(t)
	start := time.Now()
	r := runCapture(t, NewWithAddr(addr), digitalCfg(nil), 30*time.Second, 100*time.Millisecond)
	if r.err != nil {
		t.Fatal(r.err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("early cancel took %v; the timer grace must not apply", d)
	}
	if !slices.Contains(srv.Calls(), "StopCapture") {
		t.Errorf("calls: %v", srv.Calls())
	}
}

func TestAnalogAndAnalyzers(t *testing.T) {
	srv, addr := fakelogic2.Start(t)
	cfg := digitalCfg(map[string]any{
		"analog_channels":    []any{0.0},
		"analog_sample_rate": 100000.0,
		"analyzers": []any{map[string]any{"type": "spi", "label": "radio-spi",
			"channels": map[string]any{"clk": 0.0, "mosi": 1.0, "cs": 2.0}}},
	})
	r := runCapture(t, NewWithAddr(addr), cfg, 200*time.Millisecond, 0)
	if r.err != nil {
		t.Fatal(r.err)
	}
	an := srv.Analyzers()
	if len(an) != 1 || an[0].AnalyzerName != "SPI" || an[0].AnalyzerLabel != "radio-spi" ||
		an[0].Settings["Clock"].GetInt64Value() != 0 || an[0].Settings["MOSI"].GetInt64Value() != 1 || an[0].Settings["Enable"].GetInt64Value() != 2 {
		t.Errorf("analyzers: %+v", an)
	}

	var analog *pluginv1.AnalogBatch
	var decoded []*pluginv1.DecodedProtocolEvent
	for _, e := range r.events {
		if a := e.GetAnalog(); a != nil {
			analog = a
		}
		if d := e.GetDecoded(); d != nil {
			decoded = append(decoded, d)
		}
	}
	if analog == nil || analog.SamplePeriodNs != 10_000 || analog.Unit != "V" || len(analog.Samples) != 4 {
		t.Fatalf("analog: %+v", analog)
	}
	if len(decoded) != 2 || decoded[0].Protocol != "spi" || decoded[0].DurationNs != 100_000 {
		t.Fatalf("decoded: %+v", decoded)
	}
	f := decoded[0].Fields.AsMap()
	if f["mosi"] != "0xA5" || f["analyzer"] != "radio-spi" || !strings.Contains(decoded[0].Summary, "mosi=0xA5") {
		t.Errorf("fields: %v summary %q", f, decoded[0].Summary)
	}
	if r.res.Stats.Samples != 4 {
		t.Errorf("stats: %+v", r.res.Stats)
	}
}

func TestAnalyzerFailureIsAWarning(t *testing.T) {
	srv, addr := fakelogic2.Start(t)
	srv.FailTable = true
	cfg := digitalCfg(map[string]any{"analyzers": []any{map[string]any{"type": "i2c", "channels": map[string]any{"sda": 1.0, "scl": 0.0}}}})
	r := runCapture(t, NewWithAddr(addr), cfg, 150*time.Millisecond, 0)
	if r.err != nil {
		t.Fatalf("a failed analyzer must not fail the capture: %v", r.err)
	}
	var warn, digital bool
	for _, e := range r.events {
		if l := e.GetLog(); l != nil && l.Level == "warn" && strings.Contains(l.Message, "ExportDataTableCsv") {
			warn = true
		}
		if e.GetDigital() != nil {
			digital = true
		}
	}
	if !warn || !digital {
		t.Errorf("warn=%v digital=%v", warn, digital)
	}
}

func TestExportFailureWithNothingSavedFails(t *testing.T) {
	srv, addr := fakelogic2.Start(t)
	srv.FailExport = true
	r := runCapture(t, NewWithAddr(addr), digitalCfg(map[string]any{"save_native": false}), 100*time.Millisecond, 0)
	if r.err == nil || !strings.Contains(r.err.Error(), "ExportRawDataCsv") {
		t.Fatalf("want an export error, got %v", r.err)
	}
}

func TestSecondCaptureOnBusyDeviceRefused(t *testing.T) {
	_, addr := fakelogic2.Start(t)
	p := NewWithAddr(addr)
	ctx, cancel := context.WithCancel(context.Background())
	sink := pluginsdk.NewTestSink(1024)
	c, _, err := p.StartCapture(ctx, pluginsdk.CaptureRequest{DeviceID: "F4241", Config: digitalCfg(nil),
		ArtifactDir: t.TempDir(), Clock: pluginsdk.NewClock(0)}, sink)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = p.StartCapture(context.Background(), pluginsdk.CaptureRequest{DeviceID: "F4241", Config: digitalCfg(nil),
		ArtifactDir: t.TempDir(), Clock: pluginsdk.NewClock(0)}, pluginsdk.NewTestSink(8))
	if err == nil || !strings.Contains(err.Error(), "already") {
		t.Errorf("second capture: %v", err)
	}
	cancel()
	if err := c.Wait(); err != nil {
		t.Fatal(err)
	}
	// The lock is released once the capture is done.
	c2, _, err := p.StartCapture(context.Background(), pluginsdk.CaptureRequest{DeviceID: "F4241", Config: digitalCfg(nil),
		Duration: 50 * time.Millisecond, ArtifactDir: t.TempDir(), Clock: pluginsdk.NewClock(0)}, pluginsdk.NewTestSink(1024))
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	_ = c2.Wait()
}

func TestAppInfo(t *testing.T) {
	_, addr := fakelogic2.Start(t)
	resp, err := NewWithAddr(addr).Invoke(context.Background(), &pluginv1.InvokeRequest{Capability: pluginv1.CapDigitalCapture, Method: "app_info"})
	if err != nil {
		t.Fatal(err)
	}
	m := resp.Result.AsMap()
	if m["application_version"] != "fake-2.4.0" || m["api_version"] != "1.0.0" {
		t.Errorf("app_info: %v", m)
	}
	if _, err := NewWithAddr(addr).Invoke(context.Background(), &pluginv1.InvokeRequest{Capability: pluginv1.CapDigitalCapture, Method: "nope"}); err == nil {
		t.Error("unknown method must fail")
	}
}

func TestReadDigitalCSVMergesAndBatches(t *testing.T) {
	var b strings.Builder
	b.WriteString("Time [s],Channel 0\n0.0,0\n")
	// Two changes in the same nanosecond collapse to the later state; then a
	// long alternating run that must be split into several batches.
	b.WriteString("0.000000100,1\n0.000000100,0\n")
	for i := 0; i < 2*maxEdgesPerBatch+10; i++ {
		ns := 1000 + i*10
		level := (i + 1) % 2
		fmt.Fprintf(&b, "0.%09d,%d\n", ns, level)
	}
	var batches []*digitalEdges
	err := readDigitalCSV(strings.NewReader(b.String()), 1, func(e *digitalEdges) error {
		cp := *e
		batches = append(batches, &cp)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 3 {
		t.Fatalf("batches: %d", len(batches))
	}
	total := 0
	for i, e := range batches {
		total += len(e.Offsets)
		// The first batch is anchored at the initial-state row (time 0), so a
		// capture without any edge still has a time extent; later batches start
		// at their first change.
		if i > 0 && e.Offsets[0] != 0 {
			t.Errorf("batch %d does not start at offset 0", i)
		}
		for j := 1; j < len(e.Offsets); j++ {
			if e.Offsets[j] <= e.Offsets[j-1] {
				t.Fatalf("batch %d: offsets not strictly increasing", i)
			}
		}
	}
	if total != 1+2*maxEdgesPerBatch+10 {
		t.Errorf("edges: %d", total)
	}
	if batches[0].States[0] != 0 || batches[0].StartNs != 0 || batches[0].Offsets[0] != 100 {
		t.Errorf("collapsed change: %+v", batches[0])
	}
	if batches[1].Initial != batches[0].States[len(batches[0].States)-1] {
		t.Errorf("batch 1 initial state %d does not continue batch 0", batches[1].Initial)
	}
}

func TestReadDigitalCSVFlatCaptureKeepsExtent(t *testing.T) {
	// Real Logic 2 output for a capture with no activity: the initial state and
	// the end-of-capture row. The batch must span the capture.
	in := "Time [s],Channel 0,Channel 1\n0.000000000,1,1\n3.072000000,1,1\n"
	var got []*digitalEdges
	if err := readDigitalCSV(strings.NewReader(in), 2, func(e *digitalEdges) error { cp := *e; got = append(got, &cp); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].StartNs != 0 || got[0].Initial != 3 || !slices.Equal(got[0].Offsets, []int64{3_072_000_000}) {
		t.Fatalf("flat capture: %+v", got)
	}
}

func TestReadersRejectMalformedInput(t *testing.T) {
	if err := readDigitalCSV(strings.NewReader("Time [s],Channel 0\n0.0,0,1\n"), 1, nil); err == nil {
		t.Error("wrong column count must fail")
	}
	if err := readDigitalCSV(strings.NewReader("Time [s],Channel 0\n0.0,x\n"), 1, nil); err == nil {
		t.Error("bad level must fail")
	}
	if err := readAnalogCSV(strings.NewReader("Time [s],Channel 0\n0.0,abc\n"), 1, func(*analogFrames) error { return nil }); err == nil {
		t.Error("bad analog value must fail")
	}
	if err := readTableCSV(strings.NewReader("a,b\n1,2\n"), func([]decodedRow) error { return nil }); err == nil {
		t.Error("table without start time must fail")
	}
	if err := readDigitalCSV(strings.NewReader(""), 1, nil); err != nil {
		t.Errorf("empty file: %v", err)
	}
}
