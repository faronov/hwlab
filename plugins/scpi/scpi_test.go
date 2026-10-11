package scpiplugin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/faronov/hwlab/internal/scpi"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
)

func fakePlugin(t *testing.T, profile string) (*Plugin, *scpi.Fake, string) {
	t.Helper()
	pr, ok := scpi.ProfileByID(profile)
	if !ok {
		t.Fatalf("no profile %s", profile)
	}
	f := scpi.NewFake(pr)
	t.Cleanup(f.Close)
	id := "fake-" + profile
	list := func() ([]Entry, error) {
		return []Entry{{ID: id, Source: "fake", Fake: f}, {ID: "lab", Source: "env", Addr: scpi.Addr{Kind: scpi.KindTCP, Target: "192.0.2.1:5025"}}}, nil
	}
	open := func(e Entry, _ time.Duration) (scpi.Transport, error) {
		if e.Fake != nil {
			return e.Fake.Dial(), nil
		}
		return nil, os.ErrNotExist
	}
	return NewWith(list, open), f, id
}

func invoke(t *testing.T, p *Plugin, dev, capName, method string, args map[string]any) (map[string]any, error) {
	t.Helper()
	s, err := structpb.NewStruct(args)
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.Invoke(context.Background(), &pluginv1.InvokeRequest{DeviceId: dev, Capability: capName, Method: method, Args: s})
	if err != nil {
		return nil, err
	}
	return r.Result.AsMap(), nil
}

func TestParseHosts(t *testing.T) {
	es := parseHosts("scope=tcp://10.0.0.5, psu=serial:/dev/ttyUSB1@19200,bad,=x")
	if len(es) != 2 || es[0].ID != "scope" || es[0].Addr.Target != "10.0.0.5:5025" || es[1].Addr.Baud != 19200 {
		t.Fatalf("%+v", es)
	}
}

func TestListDevices(t *testing.T) {
	p, _, id := fakePlugin(t, "rigol")
	ds, err := p.ListDevices(context.Background())
	if err != nil || len(ds) != 2 {
		t.Fatalf("%v %v", ds, err)
	}
	if ds[0].Id != id || ds[0].Properties["synthetic"] != "true" || ds[0].Transport != "fake" {
		t.Errorf("fake not marked synthetic: %+v", ds[0])
	}
	if ds[1].Properties["synthetic"] != "" {
		t.Errorf("real entry marked synthetic: %+v", ds[1])
	}
}

func TestIdentifyAndProfiles(t *testing.T) {
	for _, prof := range []string{"rigol", "siglent"} {
		p, _, id := fakePlugin(t, prof)
		if err := p.OpenDevice(context.Background(), id, nil); err != nil {
			t.Fatal(err)
		}
		r, err := invoke(t, p, id, pluginv1.CapSCPI, "identify", nil)
		if err != nil {
			t.Fatal(err)
		}
		if r["profile"] != prof {
			t.Errorf("%s detected as %v", prof, r["profile"])
		}
		if r["profile_validated"] != false {
			t.Errorf("profile must be reported unvalidated: %v", r)
		}
		_ = p.CloseDevice(context.Background(), id)
	}
	p, _, id := fakePlugin(t, "rigol")
	if err := p.OpenDevice(context.Background(), id, map[string]any{"profile": "generic"}); err != nil {
		t.Fatal(err)
	}
	if _, err := invoke(t, p, id, pluginv1.CapOscilloscope, "channels", nil); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("generic profile has no scope semantics: %v", err)
	}
	if err := p.OpenDevice(context.Background(), id, map[string]any{"profile": "nope"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("unknown profile: %v", err)
	}
	if err := p.OpenDevice(context.Background(), id, map[string]any{"bogus": 1}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("unknown key: %v", err)
	}
}

func TestQueryCommandSafety(t *testing.T) {
	p, f, id := fakePlugin(t, "rigol")
	if _, err := invoke(t, p, id, pluginv1.CapSCPI, "query", map[string]any{"command": "*IDN?"}); err != nil { // transient lease
		t.Fatal(err)
	}
	before := len(f.Log())
	bad := []string{":RUN", "*IDN?\n:RUN"}
	for _, c := range bad {
		if _, err := invoke(t, p, id, pluginv1.CapSCPI, "query", map[string]any{"command": c}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("query accepted %q: %v", c, err)
		}
	}
	if _, err := invoke(t, p, id, pluginv1.CapSCPI, "command", map[string]any{"command": "*IDN?"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("command accepted a query: %v", err)
	}
	// The transient connection identifies itself; nothing else may be sent.
	for _, l := range f.Log()[before:] {
		if !strings.HasPrefix(l, "*IDN") {
			t.Errorf("rejected request reached the instrument: %q", l)
		}
	}
	if _, err := invoke(t, p, id, pluginv1.CapSCPI, "command", map[string]any{"command": ":RUN"}); err != nil {
		t.Errorf("valid command: %v", err)
	}

	info := p.Info()
	danger := map[string]pluginv1.Danger{}
	for _, ms := range info.Methods {
		danger[ms.Capability+"."+ms.Name] = ms.Danger
	}
	for _, name := range []string{"SCPI.command", "Oscilloscope.configure_channel", "Oscilloscope.set_timebase", "Oscilloscope.set_trigger", "Oscilloscope.acquire"} {
		if d, ok := danger[name]; !ok || d != pluginv1.Danger_DANGER_CONFIRM {
			t.Errorf("%s danger = %v (present=%v), want CONFIRM", name, d, ok)
		}
	}
	for _, name := range []string{"SCPI.query", "SCPI.query_binary", "Oscilloscope.waveform", "Oscilloscope.measure"} {
		if d, ok := danger[name]; !ok || d != pluginv1.Danger_DANGER_SAFE {
			t.Errorf("%s danger = %v (present=%v), want SAFE", name, d, ok)
		}
	}
}

func TestWaveformIsSummarised(t *testing.T) {
	for _, prof := range []string{"rigol", "siglent"} {
		p, _, id := fakePlugin(t, prof)
		r, err := invoke(t, p, id, pluginv1.CapOscilloscope, "waveform", map[string]any{"channel": 2, "points": 100})
		if err != nil {
			t.Fatalf("%s: %v", prof, err)
		}
		ws, _ := r["waveforms"].([]any)
		if len(ws) != 1 {
			t.Fatalf("%s: waveforms %v", prof, r)
		}
		envm, _ := ws[0].(map[string]any)["envelope"].(map[string]any)
		env, _ := envm["min_v"].([]any)
		if len(env) == 0 || len(env) > 100 {
			t.Errorf("%s: envelope size %d", prof, len(env))
		}
		if _, err := invoke(t, p, id, pluginv1.CapOscilloscope, "waveform", map[string]any{"channel": 2, "points": 1e6}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: huge point count accepted: %v", prof, err)
		}
		if _, err := invoke(t, p, id, pluginv1.CapOscilloscope, "waveform", map[string]any{"channel": 9}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: channel 9 accepted: %v", prof, err)
		}
	}
}

func startCap(t *testing.T, p *Plugin, id string, cfg map[string]any, dir string) (pluginsdk.Capture, *pluginsdk.Sink, error) {
	t.Helper()
	sink := pluginsdk.NewTestSink(64)
	c, start, err := p.StartCapture(context.Background(), pluginsdk.CaptureRequest{DeviceID: id, CaptureID: "c1", Config: cfg, ArtifactDir: dir, Clock: pluginsdk.NewClock(1_000)}, sink)
	if err == nil && (len(start.Streams) > 0 && start.Streams[0].Attributes["synthetic"] != "true") {
		t.Errorf("stream not marked synthetic: %+v", start.Streams)
	}
	return c, sink, err
}

func TestCaptureSingleTrigger(t *testing.T) {
	for _, prof := range []string{"rigol", "siglent"} {
		p, _, id := fakePlugin(t, prof)
		dir := t.TempDir()
		c, sink, err := startCap(t, p, id, map[string]any{"channels": []any{1.0, 2.0}, "trigger": "single", "screenshot": true}, dir)
		if err != nil {
			t.Fatalf("%s: %v", prof, err)
		}
		if err := c.Wait(); err != nil {
			t.Fatalf("%s: %v", prof, err)
		}
		res := c.Result()
		evs := sink.Drain()
		if len(evs) != 2 || res.Stats.Events != 2 || res.Stats.Samples == 0 {
			t.Fatalf("%s: events=%d stats=%+v", prof, len(evs), res.Stats)
		}
		a := evs[0].GetAnalog()
		if a == nil || a.Unit != "V" || a.SamplePeriodNs < 1 || a.Channels[0] != "CH1" {
			t.Errorf("%s: analog batch %+v", prof, a)
		}
		names := map[string]bool{}
		for _, ar := range res.Artifacts {
			names[ar.Name] = true
			if _, err := os.Stat(ar.Path); err != nil {
				t.Errorf("%s: artifact %s: %v", prof, ar.Name, err)
			}
			if ar.Metadata["profile_validated"] == "true" {
				t.Errorf("%s: artifact claims validated profile", prof)
			}
		}
		for _, want := range []string{"waveform-ch1.csv", "waveform-ch2.csv"} {
			if !names[want] {
				t.Errorf("%s: missing %s in %v", prof, want, names)
			}
		}
		if !names["screenshot.png"] && !names["screenshot.bmp"] {
			t.Errorf("%s: no screenshot in %v", prof, names)
		}
		if !strings.Contains(res.Clock.Source, "synthetic") || res.Clock.Note == "" {
			t.Errorf("%s: clock must say synthetic: %+v", prof, res.Clock)
		}
		csv, _ := os.ReadFile(filepath.Join(dir, "waveform-ch1.csv"))
		if !strings.HasPrefix(string(csv), "time_s,ch1_volts") {
			t.Errorf("%s: csv header %.40q", prof, csv)
		}
	}
}

func TestCaptureValidation(t *testing.T) {
	p, f, id := fakePlugin(t, "rigol")
	dir := t.TempDir()
	bad := []map[string]any{
		{"channels": []any{0.0}},
		{"channels": []any{5.0}},
		{"channels": []any{1.5}},
		{"channels": []any{}},
		{"trigger": "auto"},
		{"trigger_timeout_s": -1.0},
		{"waveform": false},
		{"nonsense": 1.0},
	}
	for _, cfg := range bad {
		before := len(f.Log())
		_, _, err := startCap(t, p, id, cfg, dir)
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("%v: want InvalidArgument, got %v", cfg, err)
		}
		if len(f.Log()) != before {
			// the transient connect sends *IDN? only; nothing else may follow
			for _, l := range f.Log()[before:] {
				if !strings.HasPrefix(l, "*IDN") {
					t.Errorf("%v: unexpected traffic %q", cfg, l)
				}
			}
		}
	}
	if _, _, err := p.StartCapture(context.Background(), pluginsdk.CaptureRequest{DeviceID: id, Clock: pluginsdk.NewClock(0)}, pluginsdk.NewTestSink(1)); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("no artifact dir: %v", err)
	}
	if _, _, err := startCap(t, p, "ghost", nil, dir); err == nil {
		t.Error("unknown device accepted")
	}
}

func TestCaptureCancelDuringTriggerWait(t *testing.T) {
	p, f, id := fakePlugin(t, "rigol")
	f.NeverTrigger()
	ctx, cancel := context.WithCancel(context.Background())
	sink := pluginsdk.NewTestSink(8)
	c, _, err := p.StartCapture(ctx, pluginsdk.CaptureRequest{DeviceID: id, Config: map[string]any{"trigger": "single", "trigger_timeout_s": 30.0}, ArtifactDir: t.TempDir(), Clock: pluginsdk.NewClock(0)}, sink)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	cancel()
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("capture did not stop on cancel")
	}
	if n := len(sink.Drain()); n != 0 {
		t.Errorf("%d events after cancel", n)
	}
}

func TestSecondCaptureRejectedWhileRunning(t *testing.T) {
	p, f, id := fakePlugin(t, "rigol")
	f.NeverTrigger()
	if err := p.OpenDevice(context.Background(), id, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, _, err := p.StartCapture(ctx, pluginsdk.CaptureRequest{DeviceID: id, Config: map[string]any{"trigger": "single", "trigger_timeout_s": 30.0}, ArtifactDir: t.TempDir(), Clock: pluginsdk.NewClock(0)}, pluginsdk.NewTestSink(8))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.StartCapture(ctx, pluginsdk.CaptureRequest{DeviceID: id, ArtifactDir: t.TempDir(), Clock: pluginsdk.NewClock(0)}, pluginsdk.NewTestSink(8)); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("second capture: %v", err)
	}
	cancel()
	_ = c.Wait()
}

func TestTriggerTimeoutIsAnError(t *testing.T) {
	p, f, id := fakePlugin(t, "rigol")
	f.NeverTrigger()
	c, _, err := startCap(t, p, id, map[string]any{"trigger": "single", "trigger_timeout_s": 0.2}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Wait(); err == nil {
		t.Error("a trigger that never fired must fail the capture")
	}
}
