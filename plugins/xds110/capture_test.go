package xds110plugin

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/internal/usb"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
	"github.com/faronov/hwlab/plugins/xds110/xds"
)

type etFixture struct {
	Segments []struct {
		URBs     []string `json:"urbs"`
		Expected []int64  `json:"expected_current_na"`
	} `json:"segments"`
	Start []struct {
		Cmd, Args string
		Status    int32
		Data      string
	} `json:"start_sequence"`
}

func loadET(t *testing.T) etFixture {
	t.Helper()
	b, err := os.ReadFile("energytrace/testdata/real-lpxds110et/fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	var f etFixture
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

// etTransport answers the command channel the way the fixture's probe did and
// records every command.
type etTransport struct {
	mu      sync.Mutex
	replies map[string][]byte
	seen    []byte
	failCmd map[byte]int32
	closed  bool
}

func newETTransport(t *testing.T, f etFixture) *etTransport {
	tr := &etTransport{replies: map[string][]byte{}, failCmd: map[byte]int32{}}
	for _, s := range f.Start {
		d, _ := hex.DecodeString(s.Data)
		tr.replies[s.Cmd+s.Args] = d
	}
	return tr
}

func (e *etTransport) Exchange(req []byte, _ time.Duration) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	cmd := req[3]
	e.seen = append(e.seen, cmd)
	st := e.failCmd[cmd]
	key := hex.EncodeToString(req[3:])
	out := make([]byte, 4)
	binary.LittleEndian.PutUint32(out, uint32(st))
	if cmd == 0x03 && e.replies[key] == nil {
		return append(out, 0x32, 0, 0, 3, 0x23, 0), nil
	}
	return append(out, e.replies[key]...), nil
}
func (e *etTransport) Close() error     { e.closed = true; return nil }
func (e *etTransport) Describe() string { return "fake et" }
func (e *etTransport) commands() []byte {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]byte(nil), e.seen...)
}

type fakeData struct {
	reads  [][]byte
	mu     sync.Mutex
	i      int
	err    error
	closed bool
}

func (f *fakeData) BulkIn(ctx context.Context, ep uint8, n int, d time.Duration) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.i < len(f.reads) {
		f.i++
		return f.reads[f.i-1], nil
	}
	if f.err != nil {
		return nil, f.err
	}
	f.mu.Unlock()
	time.Sleep(2 * time.Millisecond)
	f.mu.Lock()
	return nil, usb.ErrTimeout
}
func (f *fakeData) Close() error { f.closed = true; return nil }

func etPlugin(t *testing.T, tr *etTransport, data *fakeData) *Plugin {
	t.Helper()
	u := &usb.Device{VID: 0x0451, PID: 0xbef3, Serial: "SER1", Product: "XDS110 (test)"}
	p := NewWith(func() []Entry { return []Entry{{ID: usbID, USB: u}} },
		func(Entry) (xds.Transport, error) { return tr, nil })
	p.SetDataOpener(func(Entry) (DataLink, error) { return data, nil })
	return p
}

func fixtureReads(f etFixture) [][]byte {
	var out [][]byte
	for _, u := range f.Segments[0].URBs {
		b, _ := hex.DecodeString(u)
		out = append(out, b)
	}
	return out
}

func TestEnergyTraceCaptureReplaysFixture(t *testing.T) {
	f := loadET(t)
	tr, data := newETTransport(t, f), &fakeData{reads: fixtureReads(f)}
	p := etPlugin(t, tr, data)
	if err := p.OpenDevice(context.Background(), usbID, map[string]any{"allow_target_power": true}); err != nil {
		t.Fatal(err)
	}
	sink := pluginsdk.NewTestSink(64)
	clock := pluginsdk.NewClock(0)
	c, start, err := p.StartCapture(context.Background(), pluginsdk.CaptureRequest{
		DeviceID: usbID, Duration: 2 * time.Millisecond, Clock: clock}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if len(start.Streams) != 1 || start.Streams[0].Kind != "power" || start.Streams[0].Attributes["sample_rate_hz"] != "256000" {
		t.Fatalf("stream %+v", start.Streams)
	}
	if err := c.Wait(); err != nil {
		t.Fatal(err)
	}
	res := c.Result()
	evs := sink.Drain()
	var cur []int32
	var first int64 = -1
	for _, e := range evs {
		pb := e.GetPower()
		if pb == nil || pb.SamplePeriodNs != 3906 {
			t.Fatalf("bad event %+v", e)
		}
		if first < 0 {
			first = e.TimestampNs
		}
		cur = append(cur, pb.CurrentNa...)
	}
	if len(cur) != 512 || res.Stats.Samples != 512 {
		t.Fatalf("got %d samples, stats %+v (2 ms is 512 samples)", len(cur), res.Stats)
	}
	if first < 1_000_000 || first > 1_000_000+int64(time.Second) {
		t.Fatalf("first timestamp %d: the first sample is 256 periods after the start", first)
	}
	want := f.Segments[0].Expected
	for i := range cur {
		if math.Abs(float64(cur[i])-float64(want[i])) > 2 {
			t.Fatalf("sample %d: %d, oracle %d", i, cur[i], want[i])
		}
	}
	if res.Clock == nil || !res.Clock.ErrorMaxBounded {
		t.Fatalf("clock info %+v", res.Clock)
	}
	// OpenDevice connects (01 03); the capture then replays the observed
	// sequence and finally reconnects to restore the debug session.
	wantCmds := []byte{0x01, 0x03, 0x02, 0x28, 0x03, 0x04, 0x46, 0x03, 0x30, 0x31, 0x1d, 0x1e, 0x1e, 0x1e, 0x1f, 0x20, 0x21, 0x02, 0x01, 0x03}
	if got := tr.commands(); string(got) != string(wantCmds) {
		t.Fatalf("commands % x\nwant     % x", got, wantCmds)
	}
	if !data.closed {
		t.Fatal("the data interface was not released")
	}
	// The probe is usable for debug again.
	if _, err := invoke(t, p, usbID, pluginv1.CapDebugProbe, "info", nil); err != nil {
		t.Fatalf("debug after capture: %v", err)
	}
}

func TestEnergyTraceNeedsAllowTargetPower(t *testing.T) {
	f := loadET(t)
	tr, data := newETTransport(t, f), &fakeData{}
	p := etPlugin(t, tr, data)
	if err := p.OpenDevice(context.Background(), usbID, nil); err != nil {
		t.Fatal(err)
	}
	before := len(tr.commands())
	_, _, err := p.StartCapture(context.Background(), pluginsdk.CaptureRequest{DeviceID: usbID, Clock: pluginsdk.NewClock(0)}, pluginsdk.NewTestSink(4))
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("err %v", err)
	}
	if len(tr.commands()) != before {
		t.Fatalf("commands were sent despite the refusal: % x", tr.commands())
	}
}

func TestEnergyTraceNotOnSimulator(t *testing.T) {
	p, _ := simPlugin(t)
	openSim(t, p, map[string]any{"allow_target_power": true})
	_, _, err := p.StartCapture(context.Background(), pluginsdk.CaptureRequest{DeviceID: SimID, Clock: pluginsdk.NewClock(0)}, pluginsdk.NewTestSink(4))
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("err %v", err)
	}
}

func TestEnergyTraceStopsOnCancelAndBlocksDebugMeanwhile(t *testing.T) {
	f := loadET(t)
	tr, data := newETTransport(t, f), &fakeData{reads: fixtureReads(f)}
	p := etPlugin(t, tr, data)
	_ = p.OpenDevice(context.Background(), usbID, map[string]any{"allow_target_power": true})
	ctx, cancel := context.WithCancel(context.Background())
	c, _, err := p.StartCapture(ctx, pluginsdk.CaptureRequest{DeviceID: usbID, Clock: pluginsdk.NewClock(0)}, pluginsdk.NewTestSink(64))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := invoke(t, p, usbID, pluginv1.CapDebugProbe, "status", nil); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("a debug call during capture must be refused: %v", err)
	}
	if _, _, err := p.StartCapture(context.Background(), pluginsdk.CaptureRequest{DeviceID: usbID, Clock: pluginsdk.NewClock(0)}, pluginsdk.NewTestSink(4)); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("a second capture must be refused: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := c.Wait(); err != nil {
		t.Fatal(err)
	}
	cmds := tr.commands()
	if cmds[len(cmds)-5] != 0x20 || cmds[len(cmds)-4] != 0x21 {
		t.Fatalf("stop sequence missing: % x", cmds)
	}
}

func TestEnergyTraceDeviceGoneIsReported(t *testing.T) {
	f := loadET(t)
	tr, data := newETTransport(t, f), &fakeData{reads: fixtureReads(f)[:2], err: usb.ErrDisconnected}
	p := etPlugin(t, tr, data)
	_ = p.OpenDevice(context.Background(), usbID, map[string]any{"allow_target_power": true})
	sink := pluginsdk.NewTestSink(64)
	c, _, err := p.StartCapture(context.Background(), pluginsdk.CaptureRequest{DeviceID: usbID, Clock: pluginsdk.NewClock(0)}, sink)
	if err != nil {
		t.Fatal(err)
	}
	werr := c.Wait()
	if werr == nil || !IsDeviceGone(errors.Unwrap(werr)) && !IsDeviceGone(werr) {
		t.Fatalf("want a device-gone error, got %v", werr)
	}
	// The SDK hides the plugin error when a result exists, so the timeline
	// must carry it.
	var logged bool
	for _, e := range sink.Drain() {
		if l := e.GetLog(); l != nil && l.Level == "error" && l.Source == "energytrace" {
			logged = true
		}
	}
	if !logged {
		t.Fatal("device loss not reported as an error log event")
	}
	for _, x := range tr.commands() {
		if x == 0x20 {
			t.Fatal("stop commands were sent to a probe that is gone")
		}
	}
}

func TestEnergyTraceStartFailureReleasesEverything(t *testing.T) {
	f := loadET(t)
	tr, data := newETTransport(t, f), &fakeData{}
	tr.failCmd[0x30] = -261
	p := etPlugin(t, tr, data)
	_ = p.OpenDevice(context.Background(), usbID, map[string]any{"allow_target_power": true})
	_, _, err := p.StartCapture(context.Background(), pluginsdk.CaptureRequest{DeviceID: usbID, Clock: pluginsdk.NewClock(0)}, pluginsdk.NewTestSink(4))
	if err == nil {
		t.Fatal("expected failure")
	}
	if !data.closed {
		t.Fatal("data interface leaked")
	}
	for _, x := range tr.commands() {
		if x == 0x1f {
			t.Fatal("the stream was started after a failed setup")
		}
	}
}

func TestEnergyTraceInfo(t *testing.T) {
	p := etPlugin(t, newETTransport(t, loadET(t)), &fakeData{})
	res, err := invoke(t, p, usbID, pluginv1.CapPowerMeasurement, "info", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res["status"] != "PROTOCOL_PARTIAL" || res["supported"] != true {
		t.Fatalf("%v", res)
	}
}
