package canplugin

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/faronov/hwlab/internal/can"
	"github.com/faronov/hwlab/internal/pcap"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
)

func hubPlugin(t *testing.T) (*Plugin, *Hub) {
	t.Helper()
	hub := NewHub()
	list := func() ([]IfaceInfo, error) {
		return []IfaceInfo{hub.Info(), {Name: "can9", Backend: "socketcan", Up: true, MTU: can.KernelClassicSize}}, nil
	}
	open := func(name string, o Options) (Bus, error) {
		if name == VirtualName {
			return hub.Open(name, o)
		}
		return nil, os.ErrNotExist
	}
	return NewWith(list, open), hub
}

func invoke(t *testing.T, p *Plugin, dev, method string, args map[string]any) (map[string]any, error) {
	t.Helper()
	s, err := structpb.NewStruct(args)
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.Invoke(context.Background(), &pluginv1.InvokeRequest{DeviceId: dev, Capability: pluginv1.CapCANTransport, Method: method, Args: s})
	if err != nil {
		return nil, err
	}
	return r.Result.AsMap(), nil
}

func TestHubLoopback(t *testing.T) {
	hub := NewHub()
	a, _ := hub.Open(VirtualName, Options{FD: true, Own: true})
	b, _ := hub.Open(VirtualName, Options{FD: false})
	defer a.Close()
	defer b.Close()

	if err := a.Send(can.Frame{ID: 0x123, Data: []byte{1, 2}}); err != nil {
		t.Fatal(err)
	}
	rb, err := b.Recv(time.Second)
	if err != nil || rb.Frame.ID != 0x123 || rb.Frame.Own {
		t.Fatalf("peer: %+v err=%v", rb, err)
	}
	ra, err := a.Recv(time.Second)
	if err != nil || !ra.Frame.Own {
		t.Fatalf("own echo: %+v err=%v", ra, err)
	}
	// An FD frame is not delivered to a classic-only handle, and b cannot send one.
	if err := a.Send(can.Frame{ID: 1, FD: true, Data: make([]byte, 12)}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Recv(30 * time.Millisecond); err != ErrTimeout {
		t.Errorf("classic handle received an FD frame: %v", err)
	}
	if err := b.Send(can.Frame{ID: 1, FD: true, Data: []byte{1}}); err == nil {
		t.Error("FD send on a classic handle succeeded")
	}
	if err := a.Send(can.Frame{ID: 0x800}); err == nil {
		t.Error("an 12-bit standard id was accepted")
	}
}

func TestHubFiltersAndClose(t *testing.T) {
	hub := NewHub()
	f, _ := can.ParseFilter("100:7F0")
	r, _ := hub.Open(VirtualName, Options{Filters: []can.Filter{f}})
	hub.Inject(can.Frame{ID: 0x200})
	hub.Inject(can.Frame{ID: 0x105})
	rx, err := r.Recv(time.Second)
	if err != nil || rx.Frame.ID != 0x105 {
		t.Fatalf("filter: %+v err=%v", rx, err)
	}
	if _, err := r.Recv(20 * time.Millisecond); err != ErrTimeout {
		t.Errorf("0x200 passed the filter: %v", err)
	}
	r.Close()
	r.Close() // idempotent
	if _, err := r.Recv(time.Millisecond); err != ErrClosed {
		t.Errorf("after close: %v", err)
	}
}

func TestParseOptions(t *testing.T) {
	fd := IfaceInfo{Name: "can0", FD: true, MTU: 72}
	classic := IfaceInfo{Name: "can1", MTU: 16}
	o, err := parseOptions(defaultOptions(fd), fd, map[string]any{})
	if err != nil || !o.FD || !o.Own {
		t.Fatalf("defaults on an FD interface: %+v %v", o, err)
	}
	if o, _ = parseOptions(defaultOptions(classic), classic, map[string]any{"fd": "auto"}); o.FD {
		t.Error("auto enabled FD on a classic interface")
	}
	if _, err := parseOptions(defaultOptions(classic), classic, map[string]any{"fd": true}); err == nil || !strings.Contains(err.Error(), "not CAN FD capable") {
		t.Errorf("fd on classic: %v", err)
	}
	o, err = parseOptions(defaultOptions(fd), fd, map[string]any{"fd": false, "own": false, "error_frames": true, "filters": "123:7FF, 200~700"})
	if err != nil || o.FD || o.Own || !o.ErrorFrames || len(o.Filters) != 2 || !o.Filters[1].Invert {
		t.Fatalf("explicit: %+v %v", o, err)
	}
	for _, bad := range []map[string]any{{"fd": "maybe"}, {"own": "yes"}, {"filters": 5.0}, {"filters": "zz"}, {"filters": []any{1.0}}} {
		if _, err := parseOptions(defaultOptions(fd), fd, bad); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
}

func TestBuildFrame(t *testing.T) {
	f, err := buildFrame("c", map[string]any{"id": "123", "data": "de ad be ef"})
	if err != nil || f.ID != 0x123 || f.Extended || !bytes.Equal(f.Data, []byte{0xde, 0xad, 0xbe, 0xef}) {
		t.Fatalf("standard: %+v %v", f, err)
	}
	if f, err = buildFrame("c", map[string]any{"id": "0x1ABCDE00"}); err != nil || !f.Extended {
		t.Errorf("an id above 0x7FF is extended: %+v %v", f, err)
	}
	if f, err = buildFrame("c", map[string]any{"id": "123", "extended": true}); err != nil || !f.Extended {
		t.Errorf("explicit extended: %+v %v", f, err)
	}
	if f, err = buildFrame("c", map[string]any{"id": 16.0, "rtr": true, "dlc": 4.0}); err != nil || !f.RTR || f.DLC != 4 || len(f.Data) != 0 {
		t.Errorf("rtr: %+v %v", f, err)
	}
	if f, err = buildFrame("c", map[string]any{"id": "10", "fd": true, "brs": true, "data": strings.Repeat("ab", 12)}); err != nil || !f.FD || !f.BRS {
		t.Errorf("fd: %+v %v", f, err)
	}
	for name, bad := range map[string]map[string]any{
		"no id":         {},
		"id too big":    {"id": "20000000"},
		"id not hex":    {"id": "xyz"},
		"classic >8":    {"id": "1", "data": strings.Repeat("00", 9)},
		"fd bad length": {"id": "1", "fd": true, "data": strings.Repeat("00", 9)},
		"brs w/o fd":    {"id": "1", "brs": true},
		"bad data":      {"id": "1", "data": "gg"},
		"data type":     {"id": "1", "data": 5.0},
		"rtr dlc":       {"id": "1", "rtr": true, "dlc": 9.0},
	} {
		if _, err := buildFrame("c", bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestInfoDanger(t *testing.T) {
	p, _ := hubPlugin(t)
	danger := map[string]pluginv1.Danger{}
	for _, m := range p.Info().Methods {
		danger[m.Name] = m.Danger
	}
	if danger["send"] != pluginv1.Danger_DANGER_CONFIRM || danger["recv"] != pluginv1.Danger_DANGER_SAFE || danger["status"] != pluginv1.Danger_DANGER_SAFE {
		t.Errorf("danger classes: %v", danger)
	}
}

func TestDevicesAndOpen(t *testing.T) {
	p, _ := hubPlugin(t)
	devs, err := p.ListDevices(context.Background())
	if err != nil || len(devs) != 2 || devs[0].Properties["virtual"] != "true" || devs[1].Properties["fd"] != "false" {
		t.Fatalf("devices: %+v %v", devs, err)
	}
	if err := p.OpenDevice(context.Background(), "nope", nil); status.Code(err) != codes.NotFound {
		t.Errorf("unknown interface: %v", err)
	}
	if err := p.OpenDevice(context.Background(), "can9", nil); status.Code(err) != codes.Unavailable {
		t.Errorf("open failure: %v", err)
	}
	if err := p.OpenDevice(context.Background(), VirtualName, map[string]any{"fd": "x"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("bad option: %v", err)
	}
	if err := p.OpenDevice(context.Background(), VirtualName, nil); err != nil {
		t.Fatal(err)
	}
	st, err := invoke(t, p, VirtualName, "status", nil)
	if err != nil || st["open"] != true || st["capturing"] != false {
		t.Fatalf("status: %v %v", st, err)
	}
	if err := p.CloseDevice(context.Background(), VirtualName); err != nil {
		t.Fatal(err)
	}
	if err := p.CloseDevice(context.Background(), VirtualName); err != nil {
		t.Errorf("second close: %v", err)
	}
}

func TestInject(t *testing.T) {
	p, hub := hubPlugin(t)
	if _, err := invoke(t, p, VirtualName, "inject", map[string]any{"id": "1"}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("inject without the virtual bus enabled: %v", err)
	}
	p.withHub(hub, true)
	if err := p.OpenDevice(context.Background(), VirtualName, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := invoke(t, p, VirtualName, "inject", map[string]any{"id": "123", "data": "01"}); err != nil {
		t.Fatal(err)
	}
	r, err := invoke(t, p, VirtualName, "recv", map[string]any{"timeout_ms": 500.0, "max_frames": 1.0})
	if err != nil || r["count"] != 1.0 || r["frames"].([]any)[0].(map[string]any)["dir"] != "rx" {
		t.Errorf("injected frame must arrive as rx: %v %v", r, err)
	}
	if _, err := invoke(t, p, "can9", "inject", map[string]any{"id": "1"}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("inject on a real interface: %v", err)
	}
}

func TestSendAndRecv(t *testing.T) {
	p, hub := hubPlugin(t)
	if err := p.OpenDevice(context.Background(), VirtualName, map[string]any{"own": false}); err != nil {
		t.Fatal(err)
	}
	defer p.CloseDevice(context.Background(), VirtualName)
	go func() {
		time.Sleep(30 * time.Millisecond)
		hub.Inject(can.Frame{ID: 0x321, Data: []byte{9, 8}})
	}()
	r, err := invoke(t, p, VirtualName, "recv", map[string]any{"max_frames": 1.0, "timeout_ms": 2000.0})
	if err != nil || r["count"] != 1.0 {
		t.Fatalf("recv: %v %v", r, err)
	}
	fr := r["frames"].([]any)[0].(map[string]any)
	if fr["id"] != "0x321" || fr["data"] != "0908" || fr["dir"] != "rx" {
		t.Errorf("frame: %v", fr)
	}
	if r, err = invoke(t, p, VirtualName, "recv", map[string]any{"timeout_ms": 20.0}); err != nil || r["count"] != 0.0 {
		t.Errorf("empty recv: %v %v", r, err)
	}
	s, err := invoke(t, p, VirtualName, "send", map[string]any{"id": "7DF", "data": "0201"})
	if err != nil || s["sent"] == "" {
		t.Fatalf("send: %v %v", s, err)
	}
	if _, err := invoke(t, p, VirtualName, "send", map[string]any{"id": "7DF", "data": "zz"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("bad data: %v", err)
	}
	for _, args := range []map[string]any{{"max_frames": 0.0}, {"max_frames": 9999.0}, {"timeout_ms": "x"}} {
		if _, err := invoke(t, p, VirtualName, "recv", args); status.Code(err) != codes.InvalidArgument {
			t.Errorf("recv %v: %v", args, err)
		}
	}
	if _, err := invoke(t, p, VirtualName, "bogus", nil); status.Code(err) != codes.Unimplemented {
		t.Errorf("unknown method: %v", err)
	}
	st, _ := invoke(t, p, VirtualName, "status", nil)
	if st["frames_tx"] != 1.0 || st["frames_rx"] != 1.0 {
		t.Errorf("counters: %v", st)
	}
}

func TestTransientCalls(t *testing.T) {
	p, hub := hubPlugin(t) // device never opened
	go func() {
		time.Sleep(30 * time.Millisecond)
		hub.Inject(can.Frame{ID: 5})
	}()
	r, err := invoke(t, p, VirtualName, "recv", map[string]any{"max_frames": 1.0, "timeout_ms": 2000.0})
	if err != nil || r["count"] != 1.0 {
		t.Fatalf("transient recv: %v %v", r, err)
	}
	if _, err := invoke(t, p, VirtualName, "send", map[string]any{"id": "5"}); err != nil {
		t.Errorf("transient send: %v", err)
	}
}

func startCap(t *testing.T, p *Plugin, cfg map[string]any, dir string) (pluginsdk.Capture, context.CancelFunc, *pluginsdk.Sink) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	sink := pluginsdk.NewTestSink(64)
	c, start, err := p.StartCapture(ctx, pluginsdk.CaptureRequest{DeviceID: VirtualName, CaptureID: "c1", Config: cfg, ArtifactDir: dir, Clock: pluginsdk.NewClock(1_000)}, sink)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if len(start.Streams) != 1 || start.Streams[0].StreamId != StreamFrames || start.Streams[0].Attributes["synthetic"] != "true" {
		t.Errorf("streams: %+v", start.Streams)
	}
	return c, cancel, sink
}

func TestCaptureProducesEventsAndPcapng(t *testing.T) {
	p, hub := hubPlugin(t)
	if err := p.OpenDevice(context.Background(), VirtualName, nil); err != nil { // own=true
		t.Fatal(err)
	}
	dir := t.TempDir()
	c, cancel, sink := startCap(t, p, nil, dir)

	hub.Inject(can.Frame{ID: 0x100, Data: []byte{1, 2, 3}})
	hub.Inject(can.Frame{ID: 0x1ABCDE00, Extended: true, Data: []byte{0xFF}})
	hub.Inject(can.Frame{ID: 0x10, RTR: true, DLC: 3})
	hub.Inject(can.Frame{ID: 0x55, FD: true, BRS: true, Data: make([]byte, 20)})
	if _, err := invoke(t, p, VirtualName, "send", map[string]any{"id": "7DF", "data": "0201"}); err != nil {
		t.Fatal(err)
	}
	if _, err := invoke(t, p, VirtualName, "recv", nil); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("recv during capture: %v", err)
	}
	if _, _, err := p.StartCapture(context.Background(), pluginsdk.CaptureRequest{DeviceID: VirtualName, Clock: pluginsdk.NewClock(0)}, pluginsdk.NewTestSink(1)); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("second capture: %v", err)
	}
	if _, _, err := p.StartCapture(context.Background(), pluginsdk.CaptureRequest{DeviceID: VirtualName, Config: map[string]any{"own": false}, Clock: pluginsdk.NewClock(0)}, pluginsdk.NewTestSink(1)); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("second capture: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	cancel()
	if err := c.Wait(); err != nil {
		t.Fatal(err)
	}
	res := c.Result()
	evs := sink.Drain()
	if len(evs) != 5 || res.Stats.Events != 5 {
		t.Fatalf("events %d, stats %d", len(evs), res.Stats.Events)
	}
	var dirs []string
	for i, e := range evs {
		pk := e.GetPacket()
		if pk.Protocol != "can" || pk.LinkType != 227 {
			t.Errorf("packet %d: %+v", i, pk)
		}
		f, err := can.FromPacket(pk.Data, pk.Attributes)
		if err != nil {
			t.Fatal(err)
		}
		dirs = append(dirs, f.Dir())
		if e.TimestampNs < 1_000 || (i > 0 && e.TimestampNs < evs[i-1].TimestampNs) {
			t.Errorf("event %d stamp %d not monotonic from the session start", i, e.TimestampNs)
		}
	}
	if strings.Join(dirs, ",") != "rx,rx,rx,rx,tx" {
		t.Errorf("directions %v", dirs)
	}
	if len(res.Artifacts) != 1 {
		t.Fatalf("artifacts: %+v", res.Artifacts)
	}
	a := res.Artifacts[0]
	if a.Kind != "pcap" || a.Metadata["frames"] != "5" || a.Metadata["fd_frames"] != "1" || a.Metadata["rtr_frames"] != "1" || a.Metadata["tx"] != "1" || a.Metadata["synthetic"] != "true" {
		t.Errorf("artifact meta: %+v", a.Metadata)
	}
	raw, err := os.ReadFile(a.Path)
	if err != nil {
		t.Fatal(err)
	}
	ng, err := pcap.ReadNG(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(ng.Packets) != 5 || ng.LinkType != pcap.LinkCANSocket || ng.Received != 5 {
		t.Fatalf("pcapng: %d packets, link %d", len(ng.Packets), ng.LinkType)
	}
	if !bytes.Equal(ng.Packets[0].Data, evs[0].GetPacket().Data) {
		t.Error("pcapng frame differs from the event frame")
	}
	if !strings.Contains(res.Clock.Source, "synthetic") {
		t.Errorf("clock must say synthetic: %+v", res.Clock)
	}
	// The device is idle again: recv works.
	if _, err := invoke(t, p, VirtualName, "recv", map[string]any{"timeout_ms": 10.0}); err != nil {
		t.Errorf("recv after capture: %v", err)
	}
}

func TestCaptureOwnFalseStampsTransmittedFrames(t *testing.T) {
	p, _ := hubPlugin(t)
	if err := p.OpenDevice(context.Background(), VirtualName, map[string]any{"own": false}); err != nil {
		t.Fatal(err)
	}
	c, cancel, sink := startCap(t, p, map[string]any{"own": false}, "")
	if _, err := invoke(t, p, VirtualName, "send", map[string]any{"id": "123", "data": "aa"}); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := c.Wait(); err != nil {
		t.Fatal(err)
	}
	evs := sink.Drain()
	if len(evs) != 1 || evs[0].GetPacket().Attributes["dir"] != "tx" {
		t.Fatalf("tx event: %+v", evs)
	}
	if len(c.Result().Artifacts) != 0 {
		t.Error("no ArtifactDir, no artifact")
	}
}

func TestCaptureFilterAndTransientOpen(t *testing.T) {
	p, hub := hubPlugin(t) // not opened: the capture owns the handle
	ctx, cancel := context.WithCancel(context.Background())
	sink := pluginsdk.NewTestSink(8)
	c, _, err := p.StartCapture(ctx, pluginsdk.CaptureRequest{DeviceID: VirtualName, Config: map[string]any{"filters": "100:7FF"}, Clock: pluginsdk.NewClock(0)}, sink)
	if err != nil {
		t.Fatal(err)
	}
	hub.Inject(can.Frame{ID: 0x101})
	hub.Inject(can.Frame{ID: 0x100})
	time.Sleep(100 * time.Millisecond)
	cancel()
	if err := c.Wait(); err != nil {
		t.Fatal(err)
	}
	evs := sink.Drain()
	if len(evs) != 1 || evs[0].GetPacket().Attributes["id"] != "0x100" {
		t.Fatalf("filtered events: %+v", evs)
	}
	if _, err := p.find(VirtualName); err != nil || p.device(VirtualName) != nil {
		t.Error("the transient capture left the device open")
	}
}

func TestCaptureBadOptions(t *testing.T) {
	p, _ := hubPlugin(t)
	_, _, err := p.StartCapture(context.Background(), pluginsdk.CaptureRequest{DeviceID: VirtualName, Config: map[string]any{"fd": "x"}, Clock: pluginsdk.NewClock(0)}, pluginsdk.NewTestSink(1))
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("bad fd: %v", err)
	}
	_, _, err = p.StartCapture(context.Background(), pluginsdk.CaptureRequest{DeviceID: "ghost", Clock: pluginsdk.NewClock(0)}, pluginsdk.NewTestSink(1))
	if status.Code(err) != codes.NotFound {
		t.Errorf("unknown interface: %v", err)
	}
}

func TestCaptureReportsBusLoss(t *testing.T) {
	list := func() ([]IfaceInfo, error) {
		return []IfaceInfo{{Name: "can0", Backend: "socketcan", Up: true, MTU: 16}}, nil
	}
	p := NewWith(list, func(string, Options) (Bus, error) { return failBus{}, nil })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, _, err := p.StartCapture(ctx, pluginsdk.CaptureRequest{DeviceID: "can0", Clock: pluginsdk.NewClock(0)}, pluginsdk.NewTestSink(1))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Wait(); err == nil || !strings.Contains(err.Error(), "went away") {
		t.Errorf("a vanished interface must fail the capture: %v", err)
	}
}

type failBus struct{}

func (failBus) Recv(time.Duration) (Rx, error) { return Rx{}, errors.New("interface can0 went away") }
func (failBus) Send(can.Frame) error           { return nil }
func (failBus) Close() error                   { return nil }
