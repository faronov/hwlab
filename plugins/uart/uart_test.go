package uart

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
)

// fakePort is an in-memory serial line: push feeds the receive side, written
// collects the transmit side.
type fakePort struct {
	mu       sync.Mutex
	rx       chan []byte
	left     []byte
	done     chan struct{}
	timeout  time.Duration
	written  bytes.Buffer
	mode     Mode
	dtr, rts *bool
	resets   int
	closed   bool
}

func newFakePort(m Mode) *fakePort {
	return &fakePort{rx: make(chan []byte, 64), done: make(chan struct{}), timeout: 10 * time.Millisecond, mode: m}
}

func (f *fakePort) push(b string) { f.rx <- []byte(b) }

func (f *fakePort) Read(p []byte) (int, error) {
	f.mu.Lock()
	if len(f.left) > 0 {
		n := copy(p, f.left)
		f.left = f.left[n:]
		f.mu.Unlock()
		return n, nil
	}
	to := f.timeout
	f.mu.Unlock()
	select {
	case b := <-f.rx:
		n := copy(p, b)
		f.mu.Lock()
		f.left = append(f.left, b[n:]...)
		f.mu.Unlock()
		return n, nil
	case <-f.done:
		return 0, io.EOF
	case <-time.After(to):
		return 0, nil
	}
}

func (f *fakePort) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.written.Write(p)
}

func (f *fakePort) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.done)
	}
	return nil
}

func (f *fakePort) SetReadTimeout(d time.Duration) error {
	f.mu.Lock()
	f.timeout = d
	f.mu.Unlock()
	return nil
}
func (f *fakePort) SetMode(m Mode) error { f.mu.Lock(); f.mode = m; f.mu.Unlock(); return nil }
func (f *fakePort) SetDTR(on bool) error { f.mu.Lock(); f.dtr = &on; f.mu.Unlock(); return nil }
func (f *fakePort) SetRTS(on bool) error { f.mu.Lock(); f.rts = &on; f.mu.Unlock(); return nil }
func (f *fakePort) ResetInput() error    { f.mu.Lock(); f.resets++; f.mu.Unlock(); return nil }
func (f *fakePort) sent() string         { f.mu.Lock(); defer f.mu.Unlock(); return f.written.String() }
func (f *fakePort) isClosed() bool       { f.mu.Lock(); defer f.mu.Unlock(); return f.closed }

func newFakePlugin(t *testing.T) (*Plugin, *fakePort) {
	t.Helper()
	fp := newFakePort(DefaultMode())
	lister := func() ([]Info, error) {
		return []Info{
			{Path: "/dev/ttyACM0", VID: "1366", PID: "1025", Serial: "900000002", Product: "J-Link", USB: true},
			{Path: "/dev/ttyS0"},
		}, nil
	}
	opener := func(path string, m Mode) (Port, error) {
		if path == "/dev/missing" {
			return nil, errors.New("no such file")
		}
		fp.mu.Lock()
		fp.mode = m
		fp.mu.Unlock()
		return fp, nil
	}
	return NewWith(lister, opener), fp
}

func invoke(t *testing.T, p *Plugin, id, method string, args map[string]any, bin []byte) (map[string]any, []byte, error) {
	t.Helper()
	s, err := structpb.NewStruct(args)
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.Invoke(context.Background(), &pluginv1.InvokeRequest{DeviceId: id, Capability: pluginv1.CapSerialTransport, Method: method, Args: s, Binary: bin})
	if err != nil {
		return nil, nil, err
	}
	return r.GetResult().AsMap(), r.GetBinary(), nil
}

func TestListDevices(t *testing.T) {
	p, _ := newFakePlugin(t)
	devs, err := p.ListDevices(context.Background())
	if err != nil || len(devs) != 2 {
		t.Fatalf("devices %v err %v", devs, err)
	}
	usb := devs[0]
	if usb.Id != "ttyACM0" || usb.Transport != "usb" || usb.Serial != "900000002" || usb.Properties["vid"] != "1366" || usb.Properties["path"] != "/dev/ttyACM0" {
		t.Errorf("usb device: %+v", usb)
	}
	if devs[1].Id != "ttyS0" || devs[1].Transport != "serial" {
		t.Errorf("plain device: %+v", devs[1])
	}
	if !pluginv1.HasCapability(usb.Capabilities, pluginv1.CapSerialTransport) {
		t.Error("device must advertise SerialTransport")
	}
}

func TestInfoDangerClassification(t *testing.T) {
	info := New().Info()
	got := map[string]pluginv1.Danger{}
	for _, m := range info.Methods {
		got[m.Name] = m.Danger
	}
	for _, name := range []string{"write", "set_lines"} {
		if got[name] != pluginv1.Danger_DANGER_CONFIRM {
			t.Errorf("%s must require confirmation", name)
		}
	}
	for _, name := range []string{"status", "configure", "read", "flush"} {
		if got[name] != pluginv1.Danger_DANGER_SAFE {
			t.Errorf("%s must be safe", name)
		}
	}
	if len(info.DangerousConfigKeys) != 2 {
		t.Errorf("dtr/rts must be dangerous config keys: %v", info.DangerousConfigKeys)
	}
	for _, id := range []string{pluginv1.CapSerialTransport, pluginv1.CapTimedCapture, pluginv1.CapLiveRawStream} {
		if !pluginv1.HasCapability(info.Capabilities, id) {
			t.Errorf("missing capability %s", id)
		}
	}
}

func TestParseMode(t *testing.T) {
	m, err := parseMode(Mode{}, map[string]any{"baud": 9600.0, "data_bits": "7", "parity": "E", "stop_bits": 2.0, "flow_control": "rtscts", "dtr": false})
	if err != nil {
		t.Fatal(err)
	}
	if m.Baud != 9600 || m.DataBits != 7 || m.Parity != "even" || m.StopBits != "2" || m.Flow != FlowRTSCTS || m.DTR == nil || *m.DTR || m.RTS != nil {
		t.Errorf("mode %+v", m)
	}
	if m.Framing() != "7E2" {
		t.Errorf("framing %s", m.Framing())
	}
	def, _ := parseMode(Mode{}, nil)
	if def.String() != "115200 8N1 flow=none" {
		t.Errorf("default %s", def)
	}
	half, err := parseMode(Mode{}, map[string]any{"stop_bits": "1.5"})
	if err != nil || half.StopBits != "1.5" {
		t.Errorf("1.5 stop bits: %v %v", half, err)
	}
	for _, bad := range []map[string]any{
		{"baud": 0.0}, {"baud": 1.5}, {"baud": "fast"}, {"data_bits": 9.0}, {"parity": "weird"},
		{"stop_bits": 3.0}, {"flow_control": "dsrdtr"}, {"dtr": "yes"},
	} {
		if _, err := parseMode(Mode{}, bad); err == nil {
			t.Errorf("config %v must be rejected", bad)
		}
	}
}

func TestOpenDeviceErrors(t *testing.T) {
	p, fp := newFakePlugin(t)
	ctx := context.Background()
	if err := p.OpenDevice(ctx, "nope", nil); status.Code(err) != codes.NotFound {
		t.Errorf("unknown port: %v", err)
	}
	if err := p.OpenDevice(ctx, "ttyACM0", map[string]any{"baud": -1.0}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("bad baud: %v", err)
	}
	if err := p.OpenDevice(ctx, "x", map[string]any{"path": "/dev/missing"}); status.Code(err) != codes.Unavailable {
		t.Errorf("open failure: %v", err)
	}
	if err := p.OpenDevice(ctx, "ttyACM0", map[string]any{"baud": 57600.0}); err != nil {
		t.Fatal(err)
	}
	fp.mu.Lock()
	baud := fp.mode.Baud
	fp.mu.Unlock()
	if baud != 57600 {
		t.Errorf("opener saw baud %d", baud)
	}
	res, _, err := invoke(t, p, "ttyACM0", "status", nil, nil)
	if err != nil || res["open"] != true || res["baud"] != 57600.0 || res["path"] != "/dev/ttyACM0" {
		t.Errorf("status %v %v", res, err)
	}
	if err := p.CloseDevice(ctx, "ttyACM0"); err != nil || !fp.isClosed() {
		t.Errorf("close: %v closed=%v", err, fp.isClosed())
	}
	res, _, _ = invoke(t, p, "ttyACM0", "status", nil, nil)
	if res["open"] != false {
		t.Errorf("status after close: %v", res)
	}
}

func TestReadWriteOnOpenPort(t *testing.T) {
	p, fp := newFakePlugin(t)
	if err := p.OpenDevice(context.Background(), "ttyACM0", nil); err != nil {
		t.Fatal(err)
	}
	res, _, err := invoke(t, p, "ttyACM0", "write", map[string]any{"data": "AT", "eol": "crlf"}, nil)
	if err != nil || res["written"] != 4.0 || fp.sent() != "AT\r\n" {
		t.Fatalf("write: %v %v sent=%q", res, err, fp.sent())
	}
	if _, _, err := invoke(t, p, "ttyACM0", "write", map[string]any{"hex": "de ad BE ef"}, nil); err != nil || !strings.HasSuffix(fp.sent(), "\xde\xad\xbe\xef") {
		t.Fatalf("hex write: %v sent=%q", err, fp.sent())
	}
	if _, _, err := invoke(t, p, "ttyACM0", "write", nil, []byte{1, 2}); err != nil || !strings.HasSuffix(fp.sent(), "\x01\x02") {
		t.Fatalf("binary write: %v", err)
	}

	go func() {
		time.Sleep(20 * time.Millisecond)
		fp.push("OK\r")
		time.Sleep(5 * time.Millisecond)
		fp.push("\n")
	}()
	res, bin, err := invoke(t, p, "ttyACM0", "read", map[string]any{"timeout_ms": 2000.0, "idle_gap_ms": 100.0}, nil)
	if err != nil || string(bin) != "OK\r\n" || res["n"] != 4.0 || res["hex"] != "4f4b0d0a" {
		t.Fatalf("read: %v %q %v", res, bin, err)
	}
	// Nothing arrives: the call times out empty, not with an error.
	res, bin, err = invoke(t, p, "ttyACM0", "read", map[string]any{"timeout_ms": 30.0}, nil)
	if err != nil || len(bin) != 0 || res["n"] != 0.0 {
		t.Fatalf("empty read: %v %v", res, err)
	}
	fp.push("0123456789")
	_, bin, _ = invoke(t, p, "ttyACM0", "read", map[string]any{"max_bytes": 4.0, "timeout_ms": 500.0}, nil)
	if string(bin) != "0123" {
		t.Errorf("max_bytes: %q", bin)
	}
}

func TestWriteValidation(t *testing.T) {
	p, _ := newFakePlugin(t)
	if err := p.OpenDevice(context.Background(), "ttyACM0", nil); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string]map[string]any{
		"nothing":  {},
		"two":      {"data": "a", "hex": "41"},
		"bad hex":  {"hex": "zz"},
		"bad eol":  {"data": "a", "eol": "nl"},
		"not text": {"data": 5.0},
	} {
		if _, _, err := invoke(t, p, "ttyACM0", "write", args, nil); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: want InvalidArgument, got %v", name, err)
		}
	}
	if _, _, err := invoke(t, p, "ttyACM0", "write", map[string]any{"data": strings.Repeat("x", maxWriteBytes+1)}, nil); status.Code(err) != codes.InvalidArgument {
		t.Errorf("oversized write: %v", err)
	}
	if _, _, err := invoke(t, p, "ttyACM0", "frobnicate", nil, nil); status.Code(err) != codes.Unimplemented {
		t.Errorf("unknown method: %v", err)
	}
}

func TestTransientWriteAndRead(t *testing.T) {
	p, fp := newFakePlugin(t)
	// The device is not open: write opens the port for this one call.
	if _, _, err := invoke(t, p, "ttyACM0", "write", map[string]any{"data": "x", "baud": 9600.0}, nil); err != nil {
		t.Fatal(err)
	}
	if fp.sent() != "x" || !fp.isClosed() {
		t.Errorf("transient write: sent=%q closed=%v", fp.sent(), fp.isClosed())
	}
	for _, m := range []string{"configure", "set_lines", "flush"} {
		if _, _, err := invoke(t, p, "ttyACM0", m, map[string]any{"dtr": true}, nil); status.Code(err) != codes.FailedPrecondition {
			t.Errorf("%s without an open port: %v", m, err)
		}
	}
}

func TestConfigureAndLines(t *testing.T) {
	p, fp := newFakePlugin(t)
	if err := p.OpenDevice(context.Background(), "ttyACM0", nil); err != nil {
		t.Fatal(err)
	}
	res, _, err := invoke(t, p, "ttyACM0", "configure", map[string]any{"baud": 9600.0, "parity": "odd"}, nil)
	if err != nil || res["mode"] != "9600 8O1 flow=none" {
		t.Fatalf("configure: %v %v", res, err)
	}
	fp.mu.Lock()
	m := fp.mode
	fp.mu.Unlock()
	if m.Baud != 9600 || m.Parity != "odd" {
		t.Errorf("port mode %+v", m)
	}
	if _, _, err := invoke(t, p, "ttyACM0", "set_lines", map[string]any{}, nil); status.Code(err) != codes.InvalidArgument {
		t.Errorf("empty set_lines: %v", err)
	}
	if _, _, err := invoke(t, p, "ttyACM0", "set_lines", map[string]any{"dtr": false, "rts": true}, nil); err != nil {
		t.Fatal(err)
	}
	fp.mu.Lock()
	ok := fp.dtr != nil && !*fp.dtr && fp.rts != nil && *fp.rts
	fp.mu.Unlock()
	if !ok {
		t.Error("lines not driven")
	}
	if _, _, err := invoke(t, p, "ttyACM0", "flush", nil, nil); err != nil || fp.resets != 1 {
		t.Errorf("flush: %v resets=%d", err, fp.resets)
	}
}

func startCapture(t *testing.T, p *Plugin, cfg map[string]any) (pluginsdk.Capture, context.CancelFunc, *pluginsdk.Sink, *pluginsdk.CaptureStart) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	sink := pluginsdk.NewTestSink(256)
	c, start, err := p.StartCapture(ctx, pluginsdk.CaptureRequest{DeviceID: "ttyACM0", CaptureID: "c1", Config: cfg, Clock: pluginsdk.NewClock(1_000)}, sink)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	return c, cancel, sink, start
}

func TestCaptureChunksTxAndLines(t *testing.T) {
	p, fp := newFakePlugin(t)
	if err := p.OpenDevice(context.Background(), "ttyACM0", map[string]any{"baud": 9600.0}); err != nil {
		t.Fatal(err)
	}
	c, cancel, sink, start := startCapture(t, p, map[string]any{"lines": true, "idle_gap_ms": 20.0})
	ids := map[string]string{}
	for _, s := range start.Streams {
		ids[s.StreamId] = s.Kind
	}
	if ids[StreamRX] != "packet" || ids[StreamTX] != "packet" || ids[StreamLines] != "log" {
		t.Fatalf("streams %v", ids)
	}
	if fp.resets != 1 {
		t.Errorf("stale input must be discarded at capture start, resets=%d", fp.resets)
	}

	fp.push("hello\r\nwor")
	time.Sleep(80 * time.Millisecond) // longer than the idle gap: a chunk boundary
	fp.push("ld\n")
	time.Sleep(10 * time.Millisecond)
	if _, _, err := invoke(t, p, "ttyACM0", "write", map[string]any{"data": "AT\r\n"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := invoke(t, p, "ttyACM0", "read", nil, nil); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("read during capture: %v", err)
	}
	if _, _, err := invoke(t, p, "ttyACM0", "configure", map[string]any{"baud": 1200.0}, nil); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("configure during capture: %v", err)
	}
	if _, _, err := p.StartCapture(context.Background(), pluginsdk.CaptureRequest{DeviceID: "ttyACM0", Clock: pluginsdk.NewClock(0)}, pluginsdk.NewTestSink(1)); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("second capture on a busy port: %v", err)
	}
	time.Sleep(60 * time.Millisecond)
	cancel()
	if err := c.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if fp.isClosed() {
		t.Error("a capture on an opened device must leave the port open")
	}

	var rx, tx []string
	var lines []string
	var rxAt []int64
	for _, e := range sink.Drain() {
		switch pl := e.Payload.(type) {
		case *pluginv1.Event_Packet:
			if pl.Packet.Protocol != Protocol || pl.Packet.Attributes["port"] != "/dev/ttyACM0" || pl.Packet.Attributes["baud"] != "9600" {
				t.Errorf("packet attrs %+v", pl.Packet)
			}
			if e.StreamId == StreamTX {
				tx = append(tx, string(pl.Packet.Data))
			} else {
				rx = append(rx, string(pl.Packet.Data))
				rxAt = append(rxAt, e.TimestampNs)
			}
		case *pluginv1.Event_Log:
			lines = append(lines, pl.Log.Message)
			if pl.Log.Source != LogSource || pl.Log.Attributes["dir"] != "rx" {
				t.Errorf("log %+v", pl.Log)
			}
		}
	}
	if strings.Join(rx, "") != "hello\r\nworld\n" || len(rx) != 2 {
		t.Errorf("rx chunks %q", rx)
	}
	if len(rxAt) == 2 && (rxAt[0] < 1_000 || rxAt[1] <= rxAt[0]) {
		t.Errorf("chunk timestamps must start at the session offset and increase: %v", rxAt)
	}
	if len(tx) != 1 || tx[0] != "AT\r\n" {
		t.Errorf("tx %q", tx)
	}
	if strings.Join(lines, "|") != "hello|world" {
		t.Errorf("lines %q", lines)
	}

	res := c.Result()
	if res.Stats.Events != 5 {
		t.Errorf("events %d", res.Stats.Events)
	}
	if res.Clock.ErrorMinBounded {
		t.Error("with tx traffic the stamp may be early; the lower bound must not be claimed")
	}
	if !strings.Contains(res.Clock.Note, "rx 13 bytes, tx 4 bytes") {
		t.Errorf("clock note %q", res.Clock.Note)
	}
	if st, _, _ := invoke(t, p, "ttyACM0", "status", nil, nil); st["capturing"] != false {
		t.Errorf("capture flag must clear: %v", st)
	}
}

func TestCaptureRxOnlyClockIsNeverEarly(t *testing.T) {
	p, fp := newFakePlugin(t)
	c, cancel, sink, _ := startCapture(t, p, nil) // device not opened: the capture owns the port
	fp.push("ping")
	time.Sleep(40 * time.Millisecond)
	cancel()
	if err := c.Wait(); err != nil {
		t.Fatal(err)
	}
	if !fp.isClosed() {
		t.Error("a capture that opened the port must close it")
	}
	if len(sink.Drain()) != 1 {
		t.Error("want one rx chunk")
	}
	ci := c.Result().Clock
	if !ci.ErrorMinBounded || ci.ErrorMinNs != 0 || ci.ErrorMaxBounded {
		t.Errorf("rx-only clock must be one-sided: %+v", ci)
	}
}

func TestCaptureSplitsContinuousTraffic(t *testing.T) {
	p, fp := newFakePlugin(t)
	c, cancel, sink, _ := startCapture(t, p, map[string]any{"max_chunk_bytes": 8.0, "idle_gap_ms": 500.0})
	fp.push(strings.Repeat("a", 20))
	time.Sleep(60 * time.Millisecond)
	cancel()
	_ = c.Wait()
	total, n := 0, 0
	for _, e := range sink.Drain() {
		if pk := e.GetPacket(); pk != nil {
			total += len(pk.Data)
			n++
		}
	}
	if total != 20 || n < 2 {
		t.Errorf("20 bytes in %d chunks (%d total); max_chunk_bytes must bound a chunk", n, total)
	}
}

func TestCaptureReportsDisconnect(t *testing.T) {
	p, fp := newFakePlugin(t)
	c, cancel, sink, _ := startCapture(t, p, nil)
	defer cancel()
	fp.push("before")
	time.Sleep(30 * time.Millisecond)
	fp.Close() // the device vanished
	err := c.Wait()
	if err == nil || !strings.Contains(err.Error(), "disconnected") {
		t.Fatalf("a vanished device must fail the capture: %v", err)
	}
	if evs := sink.Drain(); len(evs) != 1 {
		t.Errorf("data read before the failure must be kept: %d events", len(evs))
	}
}

func TestCaptureConfigValidation(t *testing.T) {
	p, _ := newFakePlugin(t)
	for _, cfg := range []map[string]any{
		{"idle_gap_ms": 0.0}, {"max_chunk_bytes": 0.0}, {"max_chunk_ms": -1.0}, {"lines": "yes"}, {"baud": 0.0},
	} {
		_, _, err := p.StartCapture(context.Background(), pluginsdk.CaptureRequest{DeviceID: "ttyACM0", Config: cfg, Clock: pluginsdk.NewClock(0)}, pluginsdk.NewTestSink(1))
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("config %v: %v", cfg, err)
		}
	}
}

func TestLineSplitterEdgeCases(t *testing.T) {
	c := &capture{path: "/dev/x"}
	l := &lineSplitter{c: c}
	var got []string
	collect := func(evs []*pluginv1.Event) {
		for _, e := range evs {
			got = append(got, e.GetLog().Message)
		}
	}
	collect(l.feed([]byte("\r\n\n  \nab"), 10))
	collect(l.feed([]byte("c\r\nbad\xffbyte\n"), 20))
	collect(l.feed([]byte("tail"), 30))
	collect(l.finish())
	want := "abc|bad\uFFFDbyte|tail"
	if strings.Join(got, "|") != want {
		t.Errorf("lines %q, want %q", got, want)
	}
	long := strings.Repeat("z", maxLineLen*2+5)
	got = nil
	collect(l.feed([]byte(long), 40))
	collect(l.finish())
	if len(got) != 3 || len(got[0]) != maxLineLen {
		t.Errorf("a stream without newlines must be cut at %d bytes: %d lines", maxLineLen, len(got))
	}
}

func TestPreview(t *testing.T) {
	if got := preview([]byte("a\r\n\x00"), 32); got != `"a\r\n\x00"` {
		t.Errorf("preview %s", got)
	}
	if got := preview([]byte(strings.Repeat("a", 40)), 8); got != `"aaaaaaaa"…` {
		t.Errorf("truncated preview %s", got)
	}
}
