package nrf802154

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	"github.com/faronov/hwlab/plugins/nrf802154/dfu"
	"github.com/faronov/hwlab/plugins/nrf802154/dfu/dfutest"
	"github.com/faronov/hwlab/plugins/nrf802154/protocol"
)

// beaconReq is a MAC beacon request (seq 1): harmless, every router answers.
const beaconReq = "030801ffffffff07"

func radioLabPlugin(t *testing.T, frames ...protocol.SimFrame) (*Plugin, *protocol.Sim) {
	t.Helper()
	sim := protocol.NewSim(frames...)
	sim.RadioLab = true
	p := simPlugin(sim, noTshark())
	p.auditDir = t.TempDir()
	return p, sim
}

func invoke(p *Plugin, capName, method string, args map[string]any) (map[string]any, error) {
	st, err := structpb.NewStruct(args)
	if err != nil {
		return nil, err
	}
	r, err := p.Invoke(context.Background(), &pluginv1.InvokeRequest{DeviceId: "4A6D", Capability: capName, Method: method, Args: st})
	if err != nil {
		return nil, err
	}
	return r.GetResult().AsMap(), nil
}

func readAudit(t *testing.T, p *Plugin) []txAudit {
	t.Helper()
	f, err := os.Open(p.auditPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out []txAudit
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var a txAudit
		if err := json.Unmarshal(sc.Bytes(), &a); err != nil {
			t.Fatalf("audit line %q: %v", sc.Text(), err)
		}
		out = append(out, a)
	}
	return out
}

func codeOf(err error) codes.Code { return status.Code(err) }

func TestTransmitDirect(t *testing.T) {
	p, sim := radioLabPlugin(t)
	res, err := invoke(p, pluginv1.CapPacketInjection, "transmit", map[string]any{
		"psdu": "03 08 01 ff ff ff ff 07", "channel": 26.0, "power_dbm": 4.0, "repeat": 2.0, "interval_ms": 10.0, "note": "beacon request test"})
	if err != nil {
		t.Fatal(err)
	}
	if sim.Channel() != 26 || sim.Power() != 4 {
		t.Fatalf("radio channel %d power %d", sim.Channel(), sim.Power())
	}
	if tx := sim.Transmits(); len(tx) != 1 || tx[0] != "transmit "+beaconReq+" repeat 2 10" {
		t.Fatalf("transmits %q", tx)
	}
	sum := res["summary"].(map[string]any)
	if res["via"] != "direct" || sum["sent"] != 2.0 || sum["result"] != "ok" || len(res["attempts"].([]any)) != 2 {
		t.Fatalf("result %v", res)
	}
	a0 := res["attempts"].([]any)[0].(map[string]any)
	if a0["status"] != "sent" || a0["fcs"] != "132d" || a0["power_dbm"] != 4.0 {
		t.Fatalf("attempt %v", a0)
	}
	if fw := res["firmware"].(map[string]any); fw["transmit"] != true || fw["name"] != protocol.RadioLabName {
		t.Fatalf("firmware %v", fw)
	}
	au := readAudit(t, p)
	if len(au) != 2 || au[0].Phase != "request" || au[1].Phase != "result" || au[0].ID != au[1].ID ||
		au[0].PSDU != beaconReq || au[0].Channel != 26 || *au[0].PowerDbm != 4 || au[0].Note != "beacon request test" ||
		au[1].Summary == nil || au[1].Summary.Sent != 2 || len(au[1].Attempts) != 2 || au[1].Error != "" {
		t.Fatalf("audit %+v", au)
	}
	// The radio was put to sleep and the port released: another call opens it again.
	if cmds := sim.Commands(); cmds[len(cmds)-1] != "sleep" {
		t.Fatalf("radio left awake: %q", cmds)
	}
	sim2 := protocol.NewSim()
	sim2.RadioLab = true
	p.open = sim2.Opener()
	if _, err := invoke(p, pluginv1.CapPacketInjection, "transmit", map[string]any{"psdu": beaconReq, "channel": 15.0}); err != nil {
		t.Fatal(err)
	}
	if len(sim2.Transmits()) != 1 || sim2.Channel() != 15 {
		t.Fatalf("second transmit %q", sim2.Transmits())
	}
}

func TestTransmitRefusedOnNordicFirmware(t *testing.T) {
	sim := protocol.NewSim()
	p := simPlugin(sim, noTshark())
	p.auditDir = t.TempDir()
	_, err := invoke(p, pluginv1.CapPacketInjection, "transmit", map[string]any{"psdu": beaconReq, "channel": 26.0})
	if codeOf(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "receive-only") {
		t.Fatalf("got %v", err)
	}
	if len(sim.Transmits()) != 0 {
		t.Fatal("transmit command sent to receive-only firmware")
	}
	for _, c := range sim.Commands() {
		if strings.HasPrefix(c, "transmit") {
			t.Fatalf("commands %q", sim.Commands())
		}
	}
	if au := readAudit(t, p); len(au) != 2 || au[1].Error == "" || au[1].Firmware != protocol.NordicSnifferName {
		t.Fatalf("audit %+v", au)
	}
}

func TestTransmitArgumentGuards(t *testing.T) {
	long := strings.Repeat("00", protocol.MaxTxPSDU+1)
	cases := []map[string]any{
		{},
		{"psdu": ""},
		{"psdu": "0g"},
		{"psdu": "123"},
		{"psdu": long, "channel": 15.0},
		{"psdu": beaconReq, "channel": 10.0},
		{"psdu": beaconReq, "channel": 27.0},
		{"psdu": beaconReq, "channel": 15.5},
		{"psdu": beaconReq, "channel": 15.0, "repeat": 0.0},
		{"psdu": beaconReq, "channel": 15.0, "repeat": float64(MaxTxRepeat + 1), "interval_ms": 10.0},
		{"psdu": beaconReq, "channel": 15.0, "repeat": 2.0},
		{"psdu": beaconReq, "channel": 15.0, "repeat": 2.0, "interval_ms": 5.0},
		{"psdu": beaconReq, "channel": 15.0, "repeat": 100.0, "interval_ms": 400.0}, // 39.6 s burst
		{"psdu": beaconReq, "channel": 15.0, "power_dbm": 1.0},
		{"psdu": beaconReq, "channel": 15.0, "power_dbm": 9.0},
		{"psdu": beaconReq, "channel": 15.0, "cca": "yes"},
		{"psdu": "0102", "channel": 15.0, "ack": true},
		{"psdu": beaconReq, "channel": 15.0, "note": strings.Repeat("x", maxNote+1)},
	}
	for i, c := range cases {
		if _, err := parseTxArgs(c); codeOf(err) != codes.InvalidArgument {
			t.Errorf("case %d %v: got %v", i, c, err)
		}
	}
	ok, err := parseTxArgs(map[string]any{"psdu": strings.Repeat("ab", protocol.MaxTxPSDU), "channel": 11.0, "cca": true,
		"repeat": float64(MaxTxRepeat), "interval_ms": 300.0, "power_dbm": -20.0})
	if err != nil || ok.channel != 11 || ok.req.Repeat != MaxTxRepeat || !ok.req.CCA || *ok.power != -20 {
		t.Fatalf("%+v %v", ok, err)
	}
	// Without a capture the channel is mandatory.
	p, sim := radioLabPlugin(t)
	if _, err := invoke(p, pluginv1.CapPacketInjection, "transmit", map[string]any{"psdu": beaconReq}); codeOf(err) != codes.InvalidArgument {
		t.Fatalf("got %v", err)
	}
	if len(sim.Commands()) != 0 || len(readAudit(t, p)) != 0 {
		t.Fatal("rejected request touched the device or the audit log")
	}
}

func TestTransmitRateLimit(t *testing.T) {
	var l txLimiter
	t0 := time.Unix(1000, 0)
	for i := range TxBudgetFrames / MaxTxRepeat {
		if err := l.reserve("/dev/a", MaxTxRepeat, t0.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	err := l.reserve("/dev/a", 1, t0.Add(10*time.Second))
	if codeOf(err) != codes.ResourceExhausted || !strings.Contains(err.Error(), "retry in 50s") {
		t.Fatalf("got %v", err)
	}
	if err := l.reserve("/dev/b", 1, t0.Add(10*time.Second)); err != nil {
		t.Fatalf("budget is per port: %v", err)
	}
	// The first burst expires after the window.
	if err := l.reserve("/dev/a", MaxTxRepeat, t0.Add(TxBudgetWindow)); err != nil {
		t.Fatal(err)
	}
	if err := l.reserve("/dev/a", 1, t0.Add(TxBudgetWindow)); err == nil {
		t.Fatal("budget exceeded")
	}

	p, sim := radioLabPlugin(t)
	_ = p.txl.reserve("/dev/ttyACM0", TxBudgetFrames, time.Now())
	if _, err := invoke(p, pluginv1.CapPacketInjection, "transmit", map[string]any{"psdu": beaconReq, "channel": 26.0}); codeOf(err) != codes.ResourceExhausted {
		t.Fatalf("got %v", err)
	}
	if len(sim.Transmits()) != 0 {
		t.Fatal("rate-limited frame was sent")
	}
}

func TestTransmitThroughCapture(t *testing.T) {
	p, sim := radioLabPlugin(t, simFrames()...)
	dir := t.TempDir()
	sink := pluginsdk.NewTestSink(1024)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, start, err := p.StartCapture(ctx, req(dir, map[string]any{"channel": 15.0}), sink)
	if err != nil {
		t.Fatal(err)
	}
	if start.Streams[0].GetAttributes()["transmit"] != "true" {
		t.Fatalf("stream attributes %v", start.Streams[0].GetAttributes())
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := invoke(p, pluginv1.CapPacketInjection, "transmit", map[string]any{"psdu": beaconReq, "channel": 20.0}); codeOf(err) != codes.FailedPrecondition {
		t.Fatalf("retune through a capture: %v", err)
	}
	res, err := invoke(p, pluginv1.CapPacketInjection, "transmit", map[string]any{"psdu": beaconReq, "repeat": 3.0, "interval_ms": 10.0})
	if err != nil {
		t.Fatal(err)
	}
	if res["via"] != "capture" || res["capture_id"] != "c1" || res["channel"] != 15.0 || res["summary"].(map[string]any)["sent"] != 3.0 {
		t.Fatalf("result %v", res)
	}
	info, err := invoke(p, pluginv1.CapPacketCapture, "firmware_info", nil)
	if err != nil || info["via"] != "capture" || info["firmware"].(map[string]any)["transmit"] != true {
		t.Fatalf("firmware_info during capture: %v %v", info, err)
	}
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := c.Wait(); err != nil {
		t.Fatal(err)
	}
	if !sim.Receiving() && len(sim.Transmits()) != 1 {
		t.Fatalf("transmits %q", sim.Transmits())
	}
	r := c.Result()
	var rx, tx int
	var logs []string
	for _, e := range sink.Drain() {
		if pk := e.GetPacket(); pk != nil {
			if pk.GetAttributes()["direction"] == "tx" {
				tx++
				if pk.GetAttributes()["tx_id"] != res["id"] || pk.GetChannel() != 15 || pk.GetAttributes()["fcs"] != "132d" {
					t.Fatalf("tx packet %v", pk)
				}
			} else {
				rx++
			}
		}
		if l := e.GetLog(); l != nil {
			logs = append(logs, l.GetMessage())
		}
	}
	if tx != 3 || rx == 0 {
		t.Fatalf("rx %d tx %d", rx, tx)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "done: sent 3/3") {
		t.Fatalf("logs %q", logs)
	}
	art := r.GetArtifacts()[0]
	if art.GetMetadata()["tx_frames"] != "3" {
		t.Fatalf("meta %v", art.GetMetadata())
	}
	f, err := os.Open(art.GetPath())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	ng, err := pcap.ReadNG(f)
	if err != nil {
		t.Fatal(err)
	}
	var out int
	for _, pk := range ng.Packets {
		if pk.Outbound {
			out++
			if !strings.Contains(pk.Comment, res["id"].(string)) {
				t.Fatalf("comment %q", pk.Comment)
			}
		}
	}
	if out != 3 || len(ng.Packets) != rx+tx || ng.Received != uint64(rx+tx) {
		t.Fatalf("pcapng: %d packets, %d outbound, received %d (rx %d tx %d)", len(ng.Packets), out, ng.Received, rx, tx)
	}
	if au := readAudit(t, p); len(au) != 2 || au[0].Via != "capture" || au[0].CaptureID != "c1" || au[0].Pcap != art.GetPath() {
		t.Fatalf("audit %+v", au)
	}
	if p.activeCapture("/dev/ttyACM0") != nil {
		t.Fatal("capture still registered after it ended")
	}
}

func TestTransmitThroughNordicCaptureIsRefused(t *testing.T) {
	sim := protocol.NewSim(simFrames()...)
	p := simPlugin(sim, noTshark())
	p.auditDir = t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	c, _, err := p.StartCapture(ctx, req(t.TempDir(), map[string]any{"channel": 15.0}), pluginsdk.NewTestSink(1024))
	if err != nil {
		t.Fatal(err)
	}
	_, err = invoke(p, pluginv1.CapPacketInjection, "transmit", map[string]any{"psdu": beaconReq})
	cancel()
	_ = c.Wait()
	if codeOf(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "receive-only") {
		t.Fatalf("got %v", err)
	}
	if len(readAudit(t, p)) != 0 {
		t.Fatal("refused transmit was audited as a request")
	}
}

func TestFirmwareInfoAndDiscovery(t *testing.T) {
	nordic := protocol.NewSim()
	p := simPlugin(nordic, noTshark())
	res, err := invoke(p, pluginv1.CapPacketCapture, "firmware_info", nil)
	if err != nil || res["firmware"].(map[string]any)["name"] != protocol.NordicSnifferName || res["firmware"].(map[string]any)["transmit"] != false {
		t.Fatalf("%v %v", res, err)
	}
	devs, _ := p.ListDevices(context.Background())
	if pluginv1.HasCapability(devs[0].GetCapabilities(), pluginv1.CapPacketInjection) {
		t.Fatal("Nordic firmware advertised PacketInjection")
	}

	p, _ = radioLabPlugin(t)
	if devs, _ := p.ListDevices(context.Background()); pluginv1.HasCapability(devs[0].GetCapabilities(), pluginv1.CapPacketInjection) {
		t.Fatal("PacketInjection advertised before the firmware was probed")
	}
	res, err = invoke(p, pluginv1.CapPacketCapture, "firmware_info", nil)
	if err != nil || res["firmware"].(map[string]any)["transmit"] != true || res["firmware"].(map[string]any)["version"] != "0.1.0" {
		t.Fatalf("%v %v", res, err)
	}
	devs, _ = p.ListDevices(context.Background())
	if !pluginv1.HasCapability(devs[0].GetCapabilities(), pluginv1.CapPacketInjection) || devs[0].GetProperties()["firmware"] != "zigbee-rs-radio-lab 0.1.0" {
		t.Fatalf("devices %v", devs)
	}
}

var bootloader = usbserial.Info{Name: "/dev/ttyACM3", VID: "1915", PID: "521F", SerialNumber: "F1E2D3", Product: "Open DFU Bootloader"}

func TestDiscoveryListsBootloader(t *testing.T) {
	devs, err := NewWith(infos(sniffer, bootloader), nil, &decode.Tshark{}).ListDevices(context.Background())
	if err != nil || len(devs) != 2 {
		t.Fatalf("%v %v", devs, err)
	}
	bl := devs[1]
	if bl.GetId() != "F1E2D3" || bl.GetProperties()["mode"] != "dfu" || !pluginv1.HasCapability(bl.GetCapabilities(), pluginv1.CapFlashProgramming) ||
		pluginv1.HasCapability(bl.GetCapabilities(), pluginv1.CapPacketCapture) {
		t.Fatalf("bootloader %v", bl)
	}
	// Only a bootloader: it is not offered as a sniffer candidate.
	devs, _ = NewWith(infos(bootloader), nil, &decode.Tshark{}).ListDevices(context.Background())
	if len(devs) != 1 || devs[0].GetProperties()["mode"] != "dfu" {
		t.Fatalf("%v", devs)
	}
}

func TestDfuProgram(t *testing.T) {
	fb := &dfutest.Bootloader{MTU: 131, MaxData: 4096}
	p := NewWith(infos(sniffer, bootloader), func(name string) (usbserial.Port, error) {
		if name != bootloader.Name {
			t.Errorf("opened %s", name)
		}
		return fb, nil
	}, &decode.Tshark{})
	p.dfuTimeout = 200 * time.Millisecond
	p.dfuLogDir = t.TempDir()
	zip := filepath.Join("dfu", "testdata", "tiny.zip")
	call := func(dev string, args map[string]any) (map[string]any, error) {
		st, _ := structpb.NewStruct(args)
		r, err := p.Invoke(context.Background(), &pluginv1.InvokeRequest{DeviceId: dev, Capability: pluginv1.CapFlashProgramming, Method: "dfu_program", Args: st})
		if err != nil {
			return nil, err
		}
		return r.GetResult().AsMap(), nil
	}
	if _, err := call("F1E2D3", map[string]any{"path": zip}); codeOf(err) != codes.FailedPrecondition {
		t.Fatalf("missing confirm token: %v", err)
	}
	if _, err := call("4A6D", map[string]any{"path": zip, "confirm_token": dfuConfirm}); codeOf(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "RESET") {
		t.Fatalf("DFU on the running sniffer: %v", err)
	}
	if _, err := call("F1E2D3", map[string]any{"path": "fw.bin", "confirm_token": dfuConfirm}); codeOf(err) != codes.InvalidArgument {
		t.Fatalf("bin: %v", err)
	}
	if len(fb.Ops) != 0 {
		t.Fatal("bootloader touched by a rejected request")
	}
	res, err := call("F1E2D3", map[string]any{"path": zip, "confirm_token": dfuConfirm})
	if err != nil {
		t.Fatal(err)
	}
	if res["bytes"] != 5000.0 || res["objects"] != 2.0 || res["activated_ack"] != true || len(fb.Firmware) != 5000 || !fb.Closed {
		t.Fatalf("result %v, bootloader got %d bytes", res, len(fb.Firmware))
	}
	logPath, _ := res["transcript"].(string)
	if filepath.Dir(logPath) != p.dfuLogDir {
		t.Fatalf("transcript %q", logPath)
	}
	if log, err := os.ReadFile(logPath); err != nil || !strings.Contains(string(log), "done: mtu=131 objects=2") || !strings.Contains(string(log), "progress 5000/5000") {
		t.Fatalf("transcript %v\n%s", err, log)
	}
	if _, err := call("NOPE", map[string]any{"path": zip, "confirm_token": dfuConfirm}); codeOf(err) != codes.NotFound {
		t.Fatalf("missing device: %v", err)
	}
}

func TestDfuProgramStallSavesTranscript(t *testing.T) {
	fb := &dfutest.Bootloader{MTU: 131, MaxData: 4096, Mute: func(op byte) bool { return op == dfu.OpCRCGet }}
	p := NewWith(infos(bootloader), func(string) (usbserial.Port, error) { return fb, nil }, &decode.Tshark{})
	p.dfuTimeout = 50 * time.Millisecond
	p.dfuLogDir = t.TempDir()
	st, _ := structpb.NewStruct(map[string]any{"path": filepath.Join("dfu", "testdata", "tiny.zip"), "confirm_token": dfuConfirm})
	_, err := p.Invoke(context.Background(), &pluginv1.InvokeRequest{DeviceId: "F1E2D3", Capability: pluginv1.CapFlashProgramming, Method: "dfu_program", Args: st})
	if codeOf(err) != codes.Aborted || !strings.Contains(err.Error(), "op 0x03") || !strings.Contains(err.Error(), "last exchange:") ||
		!strings.Contains(err.Error(), "timeout after") || !strings.Contains(err.Error(), "transcript: "+p.dfuLogDir) {
		t.Fatalf("got %v", err)
	}
	t.Log(err)
	logs, _ := filepath.Glob(filepath.Join(p.dfuLogDir, "*-F1E2D3.log"))
	if len(logs) != 1 {
		t.Fatalf("logs %v", logs)
	}
	log, _ := os.ReadFile(logs[0])
	if !strings.Contains(string(log), "port /dev/ttyACM3") || !strings.Contains(string(log), "failed: op 0x03") {
		t.Fatalf("transcript:\n%s", log)
	}
}
