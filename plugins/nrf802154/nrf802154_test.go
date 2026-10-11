package nrf802154

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/faronov/hwlab/internal/decode"
	"github.com/faronov/hwlab/internal/pcap"
	"github.com/faronov/hwlab/internal/usbserial"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
	"github.com/faronov/hwlab/plugins/nrf802154/protocol"
)

func infos(extra ...usbserial.Info) usbserial.Lister {
	return func() ([]usbserial.Info, error) {
		return append([]usbserial.Info{
			{Name: "/dev/ttyACM1", VID: "1915", PID: "C00A", SerialNumber: "PPK"},
			{Name: "/dev/ttyUSB0", VID: "0403", PID: "6001"},
		}, extra...), nil
	}
}

var sniffer = usbserial.Info{Name: "/dev/ttyACM0", VID: "1915", PID: "154B", SerialNumber: "4A6D", Product: "nRF 802154 Sniffer"}

func simFrames() []protocol.SimFrame {
	return []protocol.SimFrame{
		{Data: []byte{0x02, 0x00, 0x64}, RSSI: -60, LQI: 200},
		{Data: []byte{0x61, 0x88, 0x01, 0xe9, 0x2f, 0x00, 0x00, 0x01, 0x00}, RSSI: -75, LQI: 120},
	}
}

func simPlugin(sim *protocol.Sim, tshark *decode.Tshark) *Plugin {
	p := NewWith(infos(sniffer), sim.Opener(), tshark)
	p.quiet = 10 * time.Millisecond
	return p
}

func noTshark() *decode.Tshark {
	return &decode.Tshark{Path: "/nonexistent/tshark", Run: func(context.Context, string, ...string) ([]byte, error) {
		return nil, os.ErrNotExist
	}}
}

func req(dir string, cfg map[string]any) pluginsdk.CaptureRequest {
	return pluginsdk.CaptureRequest{DeviceID: "4A6D", CaptureID: "c1", Config: cfg, ArtifactDir: dir, Clock: pluginsdk.NewClock(1_000_000_000)}
}

func TestDiscoveryPrefersConfirmedSniffer(t *testing.T) {
	cand := usbserial.Info{Name: "/dev/ttyACM2", VID: "1915", PID: "522A", SerialNumber: "D1"}
	p := NewWith(infos(sniffer, cand), nil, &decode.Tshark{})
	devs, err := p.ListDevices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 1 || devs[0].GetId() != "4A6D" || devs[0].GetProperties()["confirmed"] != "true" || devs[0].GetProperties()["port"] != "/dev/ttyACM0" {
		t.Fatalf("devices = %v", devs)
	}
}

func TestDiscoveryFallsBackToCandidates(t *testing.T) {
	cand := usbserial.Info{Name: "/dev/ttyACM2", VID: "1915", PID: "522A", SerialNumber: "D1"}
	devs, err := NewWith(infos(cand), nil, &decode.Tshark{}).ListDevices(context.Background())
	if err != nil || len(devs) != 1 || devs[0].GetProperties()["candidate"] != "true" || devs[0].GetProperties()["confirmed"] != "false" {
		t.Fatalf("devices = %v, err = %v", devs, err)
	}
}

func TestChannelValidation(t *testing.T) {
	for _, ok := range []float64{11, 15, 26} {
		if _, err := ValidateChannel(ok); err != nil {
			t.Errorf("channel %v: %v", ok, err)
		}
	}
	for _, bad := range []any{10.0, 27.0, 12.5, "15", nil} {
		if _, err := ValidateChannel(bad); err == nil {
			t.Errorf("channel %v should be rejected", bad)
		}
	}
	if err := New().OpenDevice(context.Background(), "x", map[string]any{"channel": 99.0}); err == nil {
		t.Error("OpenDevice must validate channel")
	}
}

func TestParseChannels(t *testing.T) {
	got, err := ParseChannels("11-13,20,13")
	if err != nil || len(got) != 4 || got[0] != 11 || got[3] != 20 {
		t.Fatalf("got %v err %v", got, err)
	}
	if all, _ := ParseChannels(""); len(all) != 16 {
		t.Errorf("default = %v", all)
	}
	for _, bad := range []string{"10", "27", "a", "15-12", "11-30"} {
		if _, err := ParseChannels(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}

func TestCaptureProducesTimelineEventsAndPcapng(t *testing.T) {
	sim := protocol.NewSim(simFrames()...)
	sim.Garble = 7
	p := simPlugin(sim, noTshark())
	dir := t.TempDir()
	sink := pluginsdk.NewTestSink(256)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, start, err := p.StartCapture(ctx, req(dir, map[string]any{"channel": 15.0}), sink)
	if err != nil {
		t.Fatal(err)
	}
	if len(start.Streams) != 1 || start.Streams[0].GetKind() != "packet" {
		t.Fatalf("streams = %v", start.Streams)
	}
	time.Sleep(400 * time.Millisecond)
	cancel()
	if err := c.Wait(); err != nil {
		t.Fatal(err)
	}
	res := c.Result()
	events := sink.Drain()

	var packets []*pluginv1.Packet
	var tss []int64
	for _, e := range events {
		if pk := e.GetPacket(); pk != nil {
			packets = append(packets, pk)
			tss = append(tss, e.GetTimestampNs())
		}
	}
	if len(packets) < 30 {
		t.Fatalf("only %d packets in 400 ms at 5 ms interval", len(packets))
	}
	for i, pk := range packets {
		if pk.GetChannel() != 15 || pk.GetProtocol() != "ieee802154" || pk.GetLinkType() != pcap.LinkIEEE802154NoFCS {
			t.Fatalf("packet %d = %v", i, pk)
		}
		if pk.GetAttributes()["dev_ts_us"] == "" || pk.GetAttributes()["latency_ns"] == "" {
			t.Fatalf("packet %d lacks clock attributes: %v", i, pk.GetAttributes())
		}
		if i > 0 && tss[i] < tss[i-1] {
			t.Fatalf("timestamps not monotonic at %d", i)
		}
	}
	if tss[0] < 1_000_000_000 {
		t.Errorf("first timestamp %d precedes session anchor", tss[0])
	}
	if packets[0].GetRssiDbm() != -60 || packets[0].GetLqi() != 200 || len(packets[0].GetData()) != 3 {
		t.Errorf("first packet = %v", packets[0])
	}

	if len(res.GetArtifacts()) != 1 {
		t.Fatalf("artifacts = %v", res.GetArtifacts())
	}
	art := res.GetArtifacts()[0]
	if art.GetKind() != "pcap" || filepath.Dir(art.GetPath()) != dir || art.GetMetadata()["clock.latency_max_ns"] == "" {
		t.Fatalf("artifact = %v", art)
	}
	if art.GetMetadata()["decode"] == "" {
		t.Error("missing tshark decode status in artifact metadata")
	}
	f, err := os.Open(art.GetPath())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ng, err := pcap.ReadNG(f)
	if err != nil {
		t.Fatal(err)
	}
	if ng.LinkType != pcap.LinkIEEE802154TAP || len(ng.Packets) != len(packets) || ng.Received != uint64(len(packets)) {
		t.Fatalf("pcapng: link %d, %d packets (events %d), received %d", ng.LinkType, len(ng.Packets), len(packets), ng.Received)
	}
	if ng.Packets[0].TsNs != tss[0] {
		t.Errorf("pcapng ts %d != event ts %d", ng.Packets[0].TsNs, tss[0])
	}
	if res.GetStats().GetEvents() != uint64(len(packets)) {
		t.Errorf("stats events = %d", res.GetStats().GetEvents())
	}
	cmds := sim.Commands()
	if cmds[len(cmds)-1] != "sleep" || sim.Receiving() {
		t.Errorf("radio not put to sleep; commands = %q", cmds)
	}
}

func TestCaptureEmitsDecodedEventsWhenTsharkIsAvailable(t *testing.T) {
	sim := protocol.NewSim(simFrames()...)
	tshark := &decode.Tshark{Path: "tshark", Run: func(context.Context, string, ...string) ([]byte, error) {
		return []byte("1\t0.0\twpan:wpan-tap\tAck\n2\t0.1\twpan:wpan-tap:zbee_nwk\tData, Dst: 0x0000\n3\t0.2\twpan:wpan-tap:zbee_nwk:zbee_aps\tZCL: Report\n"), nil
	}}
	p := simPlugin(sim, tshark)
	sink := pluginsdk.NewTestSink(256)
	ctx, cancel := context.WithCancel(context.Background())
	c, _, err := p.StartCapture(ctx, req(t.TempDir(), map[string]any{"channel": 20.0}), sink)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	cancel()
	_ = c.Wait()
	var dec []*pluginv1.DecodedProtocolEvent
	for _, e := range sink.Drain() {
		if d := e.GetDecoded(); d != nil {
			dec = append(dec, d)
		}
	}
	if len(dec) != 2 || dec[0].GetProtocol() != "zbee_nwk" || dec[1].GetProtocol() != "zbee_aps" {
		t.Fatalf("decoded = %v", dec)
	}
}

func TestStartCaptureErrors(t *testing.T) {
	sim := protocol.NewSim(simFrames()...)
	p := simPlugin(sim, &decode.Tshark{})
	sink := pluginsdk.NewTestSink(4)
	r := req("", map[string]any{})
	if _, _, err := p.StartCapture(context.Background(), r, sink); status.Code(err) != codes.InvalidArgument {
		t.Errorf("missing channel: %v", err)
	}
	r.Config = map[string]any{"channel": 5.0}
	if _, _, err := p.StartCapture(context.Background(), r, sink); status.Code(err) != codes.InvalidArgument {
		t.Errorf("bad channel: %v", err)
	}
	r.Config, r.DeviceID = map[string]any{"channel": 15.0}, "NOPE"
	if _, _, err := p.StartCapture(context.Background(), r, sink); status.Code(err) != codes.NotFound {
		t.Errorf("unknown device: %v", err)
	}
}

func TestSecondCaptureOnSamePortIsRejected(t *testing.T) {
	sim := protocol.NewSim(simFrames()...)
	p := simPlugin(sim, &decode.Tshark{})
	ctx, cancel := context.WithCancel(context.Background())
	c, _, err := p.StartCapture(ctx, req("", map[string]any{"channel": 15.0}), pluginsdk.NewTestSink(64))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = p.StartCapture(context.Background(), req("", map[string]any{"channel": 15.0}), pluginsdk.NewTestSink(4))
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("second capture: %v", err)
	}
	cancel()
	_ = c.Wait()
}

func TestDeviceFailureDuringCaptureIsReported(t *testing.T) {
	sim := protocol.NewSim(simFrames()...)
	sim.FailAfter = 10
	p := simPlugin(sim, noTshark())
	sink := pluginsdk.NewTestSink(256)
	c, _, err := p.StartCapture(context.Background(), req(t.TempDir(), map[string]any{"channel": 15.0}), sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Wait(); err == nil {
		t.Fatal("Wait should report the read failure")
	}
	var sawErr bool
	for _, e := range sink.Drain() {
		if l := e.GetLog(); l != nil && l.GetLevel() == "error" {
			sawErr = true
		}
	}
	if !sawErr {
		t.Error("no error log event on the timeline")
	}
	if len(c.Result().GetArtifacts()) != 1 {
		t.Error("the partial capture must still be kept as an artifact")
	}
}

func TestCaptureStopLeavesNoGoroutines(t *testing.T) {
	before := runtime.NumGoroutine()
	for i := 0; i < 5; i++ {
		sim := protocol.NewSim(simFrames()...)
		p := simPlugin(sim, noTshark())
		ctx, cancel := context.WithCancel(context.Background())
		c, _, err := p.StartCapture(ctx, req(t.TempDir(), map[string]any{"channel": 11.0 + float64(i)}), pluginsdk.NewTestSink(64))
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(40 * time.Millisecond)
		cancel()
		if err := c.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("goroutines: %d before, %d after", before, after)
	}
}

func TestScanReportsActiveChannels(t *testing.T) {
	sim := protocol.NewSim(simFrames()...)
	p := simPlugin(sim, &decode.Tshark{})
	args, _ := structpb.NewStruct(map[string]any{"channels": "15,16", "dwell_ms": 150.0, "min_packets": 5.0})
	resp, err := p.Invoke(context.Background(), &pluginv1.InvokeRequest{DeviceId: "4A6D", Capability: pluginv1.CapPacketCapture, Method: "scan", Args: args})
	if err != nil {
		t.Fatal(err)
	}
	m := resp.GetResult().AsMap()
	rows := m["channels"].([]any)
	if len(rows) != 2 {
		t.Fatalf("rows = %v", rows)
	}
	first := rows[0].(map[string]any)
	if first["channel"].(float64) != 15 || first["packets"].(float64) < 5 || first["rssi_min_dbm"].(float64) != -75 || first["rssi_max_dbm"].(float64) != -60 {
		t.Errorf("first = %v", first)
	}
	if active := m["likely_active"].([]any); len(active) == 0 {
		t.Errorf("likely_active empty: %v", m)
	}
	cmds := sim.Commands()
	if last := cmds[len(cmds)-1]; last != "sleep" {
		t.Errorf("scan left radio in %q", last)
	}
	if sim.Channel() != 16 {
		t.Errorf("last channel = %d", sim.Channel())
	}
}

func TestScanHonoursCancellation(t *testing.T) {
	sim := protocol.NewSim(simFrames()...)
	p := simPlugin(sim, &decode.Tshark{})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	args, _ := structpb.NewStruct(map[string]any{"dwell_ms": 5000.0})
	start := time.Now()
	_, err := p.Invoke(ctx, &pluginv1.InvokeRequest{DeviceId: "4A6D", Capability: pluginv1.CapPacketCapture, Method: "scan", Args: args})
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("scan ignored cancellation for %v", time.Since(start))
	}
}

func TestTsharkStatusAndDecodePcap(t *testing.T) {
	p := NewWith(infos(), nil, &decode.Tshark{Path: "/usr/bin/tshark", Run: func(context.Context, string, ...string) ([]byte, error) {
		return []byte("1\t0.0\twpan:wpan-tap\tAck\n2\t0.1\twpan:wpan-tap:zbee_nwk\tData\n"), nil
	}})
	resp, err := p.Invoke(context.Background(), &pluginv1.InvokeRequest{Capability: pluginv1.CapProtocolDecode, Method: "tshark_status"})
	if err != nil || resp.GetResult().AsMap()["available"] != true {
		t.Fatalf("resp=%v err=%v", resp, err)
	}
	args, _ := structpb.NewStruct(map[string]any{"path": "x.pcapng"})
	resp, err = p.Invoke(context.Background(), &pluginv1.InvokeRequest{Capability: pluginv1.CapProtocolDecode, Method: "decode_pcap", Args: args})
	if err != nil {
		t.Fatal(err)
	}
	m := resp.GetResult().AsMap()
	if m["frames"].(float64) != 2 || m["top_layer_counts"].(map[string]any)["zbee_nwk"].(float64) != 1 {
		t.Errorf("decode result = %v", m)
	}
}
