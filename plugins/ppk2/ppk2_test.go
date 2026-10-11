package ppk2

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/faronov/hwlab/internal/usbserial"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
	"github.com/faronov/hwlab/plugins/ppk2/protocol"
)

const simMetadata = "Calibrated: 1\nR0: 1000\nR1: 100\nR2: 10\nR3: 1\nR4: 0.05\nHW: 2\nEND\n"

// simPort simulates the PPK2 command interface: it answers GET_META_DATA and
// produces a constant sample stream between AVERAGE_START and AVERAGE_STOP.
type simPort struct {
	mu        sync.Mutex
	pending   []byte
	streaming bool
	written   [][]byte
	closed    bool
	failWrite bool // simulates an unplugged device
}

func (s *simPort) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failWrite || s.closed {
		return 0, io.ErrClosedPipe
	}
	s.written = append(s.written, append([]byte(nil), b...))
	switch b[0] {
	case protocol.CmdGetMetaData:
		s.pending = append(s.pending, simMetadata...)
	case protocol.CmdAverageStart:
		s.streaming = true
	case protocol.CmdAverageStop:
		s.streaming = false
	}
	return len(b), nil
}

func (s *simPort) Read(p []byte) (int, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return 0, io.EOF
	}
	if s.streaming {
		// 1001 samples per read; the odd byte cap below splits samples
		// across reads to exercise reassembly.
		for i := 0; i < 1001; i++ {
			s.pending = append(s.pending, 0xE8, 0x03, 0x00, 0x05) // adc=1000, range 0, logic=0x05
		}
	}
	n := copy(p[:min(len(p), 4001)], s.pending)
	s.pending = s.pending[n:]
	s.mu.Unlock()
	if n == 0 {
		time.Sleep(time.Millisecond)
	}
	return n, nil
}

func (s *simPort) SetReadTimeout(time.Duration) error { return nil }
func (s *simPort) Close() error                       { s.mu.Lock(); s.closed = true; s.mu.Unlock(); return nil }

func (s *simPort) sent(op byte) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, w := range s.written {
		if w[0] == op {
			n++
		}
	}
	return n
}

// lastDUT returns the argument of the last DUT-power command, or -1.
func (s *simPort) lastDUT() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	last := -1
	for _, w := range s.written {
		if w[0] == protocol.CmdDeviceRunningSet {
			last = int(w[1])
		}
	}
	return last
}

func newSim() (*Plugin, *simPort) {
	sim := &simPort{}
	list := func() ([]usbserial.Info, error) {
		return []usbserial.Info{
			{Name: "/dev/ttyACM0", VID: "1915", PID: "C00A", SerialNumber: "ABC123"},
			{Name: "/dev/ttyACM1", VID: "1915", PID: "c00a", SerialNumber: "ABC123"},
			{Name: "/dev/ttyUSB0", VID: "0403", PID: "6001", SerialNumber: "OTHER"},
		}, nil
	}
	return NewWith(list, func(string) (usbserial.Port, error) { return sim, nil }), sim
}

func TestDiscovery(t *testing.T) {
	p, _ := newSim()
	devs, err := p.ListDevices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 1 || devs[0].GetId() != "ABC123" {
		t.Fatalf("devices = %v", devs)
	}
	if got := devs[0].GetProperties()["ports"]; got != "/dev/ttyACM0,/dev/ttyACM1" {
		t.Errorf("ports = %q", got)
	}
}

func TestCaptureSimulated(t *testing.T) {
	p, sim := newSim()
	ctx := context.Background()
	if err := p.OpenDevice(ctx, "ABC123", map[string]any{"mode": "source", "supply_mv": float64(3300)}); err != nil {
		t.Fatal(err)
	}
	if sim.sent(protocol.CmdGetMetaData) == 0 {
		t.Fatal("metadata was not requested")
	}
	sink := pluginsdk.NewTestSink(64)
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cap, start, err := p.StartCapture(cctx, pluginsdk.CaptureRequest{
		DeviceID: "ABC123", Duration: 100 * time.Millisecond, Clock: pluginsdk.NewClock(5_000),
	}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if len(start.Streams) != 1 || start.Streams[0].GetKind() != "power" {
		t.Fatalf("streams = %v", start.Streams)
	}
	if err := cap.Wait(); err != nil {
		t.Fatal(err)
	}

	var samples int
	var nextTs int64 = -1
	for _, ev := range sink.Drain() {
		pb := ev.GetPower()
		if nextTs >= 0 && ev.GetTimestampNs() != nextTs {
			t.Fatalf("gap: ts %d, want %d", ev.GetTimestampNs(), nextTs)
		}
		if nextTs < 0 && ev.GetTimestampNs() < 5_000 {
			t.Fatalf("first ts %d precedes the clock anchor", ev.GetTimestampNs())
		}
		samples += len(pb.GetCurrentNa())
		nextTs = ev.GetTimestampNs() + int64(len(pb.GetCurrentNa()))*protocol.SamplePeriod
		if pb.GetDigital()[0] != 0x05 || pb.GetCurrentNa()[0] <= 0 {
			t.Fatalf("unexpected sample: %v / %v", pb.GetCurrentNa()[0], pb.GetDigital()[0])
		}
	}
	if samples != 10_000 {
		t.Errorf("samples = %d, want 10000 (100 ms at 100 kSa/s)", samples)
	}
	if res := cap.Result(); res.GetStats().GetSamples() != 10_000 {
		t.Errorf("stats samples = %d", res.GetStats().GetSamples())
	}
	if sim.sent(protocol.CmdAverageStop) < 2 || sim.sent(protocol.CmdRegulatorSet) != 1 || sim.sent(protocol.CmdDeviceRunningSet) != 2 {
		t.Errorf("unexpected command counts: stop=%d reg=%d dut=%d", sim.sent(protocol.CmdAverageStop), sim.sent(protocol.CmdRegulatorSet), sim.sent(protocol.CmdDeviceRunningSet))
	}
	if err := p.CloseDevice(ctx, "ABC123"); err != nil {
		t.Fatal(err)
	}
}

func TestAmpereModeDoesNotDriveSupply(t *testing.T) {
	p, sim := newSim()
	ctx := context.Background()
	if err := p.OpenDevice(ctx, "ABC123", nil); err != nil {
		t.Fatal(err)
	}
	sink := pluginsdk.NewTestSink(64)
	cap, _, err := p.StartCapture(ctx, pluginsdk.CaptureRequest{DeviceID: "ABC123", Duration: 10 * time.Millisecond, Clock: pluginsdk.NewClock(0)}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := cap.Wait(); err != nil {
		t.Fatal(err)
	}
	if sim.sent(protocol.CmdRegulatorSet) != 0 || sim.sent(protocol.CmdDeviceRunningSet) != 0 {
		t.Error("ampere mode must not set the regulator or switch the DUT supply")
	}
}

func TestInvokeValidation(t *testing.T) {
	p, _ := newSim()
	_, err := p.Invoke(context.Background(), &pluginv1.InvokeRequest{DeviceId: "ABC123", Capability: pluginv1.CapPowerSource, Method: "set_voltage"})
	if err == nil {
		t.Error("set_voltage without mv must fail")
	}
}

func TestOptionValidation(t *testing.T) {
	for _, cfg := range []map[string]any{
		{"mode": "bogus"}, {"supply_mv": float64(100)}, {"supply_mv": float64(9000)}, {"vdd_mv": float64(-1)},
	} {
		if _, err := parseOptions(cfg); err == nil {
			t.Errorf("parseOptions(%v) should fail", cfg)
		}
	}
}

func runCapture(t *testing.T, p *Plugin, cfg map[string]any) {
	t.Helper()
	ctx := context.Background()
	if err := p.OpenDevice(ctx, "ABC123", cfg); err != nil {
		t.Fatal(err)
	}
	cap, _, err := p.StartCapture(ctx, pluginsdk.CaptureRequest{DeviceID: "ABC123", Duration: 10 * time.Millisecond, Clock: pluginsdk.NewClock(0)}, pluginsdk.NewTestSink(64))
	if err != nil {
		t.Fatal(err)
	}
	if err := cap.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestCaptureSwitchesDUTSupplyOffAtEnd(t *testing.T) {
	p, sim := newSim()
	runCapture(t, p, map[string]any{"mode": "source", "supply_mv": float64(3300)})
	if got := sim.lastDUT(); got != 0 {
		t.Errorf("last DUT power command = %d, want 0 (off)", got)
	}
}

func TestKeepDUTPowerLeavesSupplyOn(t *testing.T) {
	p, sim := newSim()
	runCapture(t, p, map[string]any{"mode": "source", "keep_dut_power": true})
	if got := sim.lastDUT(); got != 1 {
		t.Errorf("last DUT power command = %d, want 1 (kept on)", got)
	}
}

func TestReconnectAfterPortFailure(t *testing.T) {
	var opens int
	var cur *simPort
	list := func() ([]usbserial.Info, error) {
		return []usbserial.Info{{Name: "/dev/ttyACM0", VID: "1915", PID: "c00a", SerialNumber: "ABC123"}}, nil
	}
	p := NewWith(list, func(string) (usbserial.Port, error) { opens++; cur = &simPort{}; return cur, nil })
	ctx := context.Background()
	if err := p.OpenDevice(ctx, "ABC123", nil); err != nil {
		t.Fatal(err)
	}
	first := cur
	first.mu.Lock()
	first.failWrite = true // unplugged
	first.mu.Unlock()
	cap, _, err := p.StartCapture(ctx, pluginsdk.CaptureRequest{DeviceID: "ABC123", Duration: 10 * time.Millisecond, Clock: pluginsdk.NewClock(0)}, pluginsdk.NewTestSink(64))
	if err != nil {
		t.Fatalf("capture on a replugged device: %v", err)
	}
	if err := cap.Wait(); err != nil {
		t.Fatal(err)
	}
	if opens != 2 || cur == first {
		t.Errorf("opens=%d, want a fresh port after the failure", opens)
	}
}
