package wstk

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/faronov/hwlab/internal/pcap"
	iusb "github.com/faronov/hwlab/internal/usb"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
	jusb "github.com/faronov/hwlab/plugins/jlinknative/usb"
	"github.com/faronov/hwlab/plugins/wstk/protocol"
)

// fakeKit is a kit on the "network": an admin console and a debug channel
// on loopback listeners.
type fakeKit struct {
	admin, dch net.Listener
	records    [][]byte // DCH frames sent to each debug-channel client
	mu         sync.Mutex
	commands   []string
}

func newFakeKit(t *testing.T) *fakeKit {
	t.Helper()
	k := &fakeKit{}
	var err error
	if k.admin, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	if k.dch, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k.admin.Close(); _ = k.dch.Close() })
	go k.serveAdmin()
	go k.serveDCH()
	return k
}

func (k *fakeKit) serveAdmin() {
	for {
		c, err := k.admin.Accept()
		if err != nil {
			return
		}
		go func() {
			defer func() { _ = c.Close() }()
			r := bufio.NewReader(c)
			_, _ = c.Write([]byte(protocol.AdminPrompt))
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				cmd := strings.TrimSpace(l)
				k.mu.Lock()
				k.commands = append(k.commands, cmd)
				k.mu.Unlock()
				out := ""
				switch cmd {
				case "":
				case "dbg info":
					out = "------ Debug Interface Status ------\r\nTarget Voltage           : 3.35 V\r\nAEM current              : 581.39 uA\r\n"
				case "pti info":
					out = "PTI mode                  : EFR UART\r\nPackets received          : 0\r\n"
				case "sys ver":
					out = "1.4.6b1171\r\n"
				default:
					out = "Unknown command\r\n"
				}
				_, _ = c.Write([]byte(cmd + "\r\n" + out + protocol.AdminPrompt))
			}
		}()
	}
}

func (k *fakeKit) serveDCH() {
	for {
		c, err := k.dch.Accept()
		if err != nil {
			return
		}
		k.mu.Lock()
		records := k.records
		k.mu.Unlock()
		for _, r := range records {
			_, _ = c.Write(r)
		}
		go func() { // hold the connection until the client leaves
			defer func() { _ = c.Close() }()
			_, _ = c.Read(make([]byte, 1))
		}()
	}
}

func (k *fakeKit) sent() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.commands...)
}

func dchFrame(devNs uint64, typ uint16, payload []byte) []byte {
	m := []byte{3, 0}
	m = binary.LittleEndian.AppendUint64(m, devNs)
	m = binary.LittleEndian.AppendUint16(m, typ)
	m = binary.LittleEndian.AppendUint32(m, 0)
	m = binary.LittleEndian.AppendUint16(m, 1)
	m = append(m, payload...)
	b := []byte{'[', byte(len(m)), byte(len(m) >> 8)}
	return append(append(b, m...), ']')
}

type testEnv struct {
	sim   *protocol.SimKit
	opens int
	kit   *fakeKit
}

func newTestPlugin(t *testing.T) (*Plugin, *testEnv) {
	te := &testEnv{sim: protocol.NewSimKit(), kit: newFakeKit(t)}
	env := Env{
		ScanUSB: func() []USBKit {
			return []USBKit{{Serial: "440123456", Manufacturer: "Silicon Labs", Product: "J-Link Pro OB"}}
		},
		OpenUSB: func(USBKit) (KitConn, error) { te.opens++; return nopCloser{te.sim}, nil },
		Hosts:   func() map[string]string { return map[string]string{"lab": "kit.local"} },
		Dial: func(ctx context.Context, addr string) (net.Conn, error) {
			ln := te.kit.admin
			if strings.HasSuffix(addr, ":4905") {
				ln = te.kit.dch
			}
			var d net.Dialer
			return d.DialContext(ctx, "tcp", ln.Addr().String())
		},
		VCOM: func(s string) string { return "/dev/cu.usbmodem000" + s + "1" },
	}
	return NewWith(env), te
}

func invoke(t *testing.T, p *Plugin, id, capID, method string, args map[string]any) map[string]any {
	t.Helper()
	st, _ := structpb.NewStruct(args)
	r, err := p.Invoke(context.Background(), &pluginv1.InvokeRequest{DeviceId: id, Capability: capID, Method: method, Args: st})
	if err != nil {
		t.Fatalf("%s.%s: %v", capID, method, err)
	}
	return r.GetResult().AsMap()
}

func TestScanUSBKeepsSiliconLabsKits(t *testing.T) {
	infos := []jusb.Info{
		{Serial: "440123456", Device: iusb.Device{Manufacturer: "Silicon Labs", Product: "J-Link Pro OB"}},
		{Serial: "440012345", Device: iusb.Device{Manufacturer: "SEGGER", Product: "J-Link OB-SAM3U128-V2-NordicSemi"}},
		{Serial: "440099999", Device: iusb.Device{Manufacturer: "Energy Micro AS", Product: "J-Link EnergyMicro"}},
	}
	kits := ScanUSB(infos)
	if len(kits) != 2 || kits[0].Serial != "440123456" || kits[1].Serial != "440099999" {
		t.Fatalf("kits = %+v", kits)
	}
}

func TestParseHosts(t *testing.T) {
	h := ParseHosts(" lab = 10.0.0.5 , 192.168.1.9,,bad=")
	if len(h) != 2 || h["lab"] != "10.0.0.5" || h["192.168.1.9"] != "192.168.1.9" {
		t.Fatalf("hosts = %v", h)
	}
}

func TestListDoesNotOpenAndInfoFillsBoards(t *testing.T) {
	p, te := newTestPlugin(t)
	devs, err := p.ListDevices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 2 || devs[0].GetId() != "wstk:440123456" || devs[1].GetId() != "wstk-net:lab" || te.opens != 0 {
		t.Fatalf("devices %v, opens %d", devs, te.opens)
	}
	if pr := devs[0].GetProperties(); pr["vcom"] != "/dev/cu.usbmodem0004401234561" || pr["uart_device"] != "uart/cu.usbmodem0004401234561" ||
		pr["jlink_devices"] != "jlink/440123456, jlinknative/jlink-native:440123456" {
		t.Fatalf("props %v", devs[0].GetProperties())
	}
	res := invoke(t, p, "wstk:440123456", pluginv1.CapPowerMeasurement, "info", nil)
	if res["firmware"] != "1.4.6" || res["boards"] != "BRD4001A Rev. A02, BRD4183A Rev. A00" || te.opens != 1 {
		t.Fatalf("info %v", res)
	}
	devs, _ = p.ListDevices(context.Background())
	if !strings.Contains(devs[0].GetName(), "BRD4001A + BRD4183A") || devs[0].GetProperties()["mainboard_serial"] != "200000001" {
		t.Fatalf("after info: %s %v", devs[0].GetName(), devs[0].GetProperties())
	}
}

func TestListIdentifiesOnceAndBacksOff(t *testing.T) {
	p, te := newTestPlugin(t)
	p.env.IdentifyOnList = true
	open := p.env.OpenUSB
	fail := true
	p.env.OpenUSB = func(k USBKit) (KitConn, error) {
		if fail {
			te.opens++
			return nil, errors.New("busy")
		}
		return open(k)
	}
	_, _ = p.ListDevices(context.Background())
	devs, _ := p.ListDevices(context.Background())
	if te.opens != 1 || devs[0].GetProperties()["boards"] != "unknown until info or a capture has run" {
		t.Fatalf("opens %d, props %v", te.opens, devs[0].GetProperties())
	}
	fail = false
	p.tried = map[string]time.Time{} // the retry interval has passed
	_, _ = p.ListDevices(context.Background())
	devs, _ = p.ListDevices(context.Background())
	if te.opens != 2 || devs[0].GetProperties()["boards"] != "BRD4001A Rev. A02, BRD4183A Rev. A00" {
		t.Fatalf("opens %d, props %v", te.opens, devs[0].GetProperties())
	}
}

func TestReadCurrentUSB(t *testing.T) {
	p, _ := newTestPlugin(t)
	res := invoke(t, p, "wstk:440123456", pluginv1.CapPowerMeasurement, "read_current", nil)
	if m := res["current_mean_ma"].(float64); m < 1.5 || m > 1.8 || res["current_min_ma"].(float64) != 1.5 {
		t.Fatalf("read_current %v", res)
	}
	if v := res["voltage_v"].(float64); v < 3.29 || v > 3.31 {
		t.Fatalf("voltage %v", v)
	}
}

func TestUSBAEMCapture(t *testing.T) {
	p, _ := newTestPlugin(t)
	sink := pluginsdk.NewTestSink(1024)
	c, st, err := p.StartCapture(context.Background(), pluginsdk.CaptureRequest{DeviceID: "wstk:440123456", Duration: 100 * time.Millisecond,
		Clock: pluginsdk.NewClock(0)}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if st.Streams[0].GetStreamId() != "current" || st.Streams[0].GetAttributes()["sample_period_ns"] != "100000" {
		t.Fatalf("streams %v", st.Streams)
	}
	if err := c.Wait(); err != nil {
		t.Fatal(err)
	}
	res := c.Result()
	if res.GetStats().GetSamples() != 1000 || !res.GetClock().GetErrorMinBounded() {
		t.Fatalf("result %v", res)
	}
	var n int
	var prevEnd int64 = -1 << 62
	for _, ev := range sink.Drain() {
		pb := ev.GetPower()
		if pb == nil {
			continue
		}
		if ev.GetTimestampNs() < prevEnd || pb.GetSamplePeriodNs() != 100_000 || len(pb.GetVoltageUv()) != len(pb.GetCurrentNa()) {
			t.Fatalf("batch at %d (prev end %d) %v", ev.GetTimestampNs(), prevEnd, pb.GetSamplePeriodNs())
		}
		if pb.GetCurrentNa()[0] != 1_500_000 || pb.GetVoltageUv()[0] != 3_300_000 {
			t.Fatalf("first sample %d nA %d uV", pb.GetCurrentNa()[0], pb.GetVoltageUv()[0])
		}
		prevEnd = ev.GetTimestampNs() + int64(len(pb.GetCurrentNa()))*100_000
		n += len(pb.GetCurrentNa())
	}
	if n != 1000 {
		t.Fatalf("%d samples in events", n)
	}
	// The probe is released: a second capture can start.
	if _, err := p.Invoke(context.Background(), &pluginv1.InvokeRequest{DeviceId: "wstk:440123456", Capability: pluginv1.CapPowerMeasurement, Method: "info"}); err != nil {
		t.Fatal(err)
	}
}

func TestPTIOnUSBIsRefused(t *testing.T) {
	p, _ := newTestPlugin(t)
	_, _, err := p.StartCapture(context.Background(), pluginsdk.CaptureRequest{DeviceID: "wstk:440123456", Config: map[string]any{"mode": "pti"},
		Clock: pluginsdk.NewClock(0)}, pluginsdk.NewTestSink(8))
	if err == nil || !strings.Contains(err.Error(), "Ethernet") {
		t.Fatalf("err = %v", err)
	}
}

func TestNetAdminMethods(t *testing.T) {
	p, te := newTestPlugin(t)
	res := invoke(t, p, "wstk-net:lab", pluginv1.CapPowerMeasurement, "read_current", nil)
	if res["current_ma"].(float64) != 0.58139 || res["command"] != "dbg info" || res["voltage_v"].(float64) != 3.35 {
		t.Fatalf("read_current %v", res)
	}
	res = invoke(t, p, "wstk-net:lab", pluginv1.CapPacketCapture, "pti_status", nil)
	if res["fields"].(map[string]any)["PTI mode"] != "EFR UART" {
		t.Fatalf("pti_status %v", res)
	}
	st, _ := structpb.NewStruct(map[string]any{"command": "target reset"})
	if _, err := p.Invoke(context.Background(), &pluginv1.InvokeRequest{DeviceId: "wstk-net:lab", Capability: pluginv1.CapPowerMeasurement,
		Method: "admin_read", Args: st}); err == nil {
		t.Fatal("target reset accepted")
	}
	for _, c := range te.kit.sent() {
		if !protocol.AdminAllowed(c) && c != "" {
			t.Fatalf("sent %q to the kit", c)
		}
	}
}

func TestNetAEMPolling(t *testing.T) {
	p, _ := newTestPlugin(t)
	sink := pluginsdk.NewTestSink(256)
	c, _, err := p.StartCapture(context.Background(), pluginsdk.CaptureRequest{DeviceID: "wstk-net:lab", Duration: 150 * time.Millisecond,
		Config: map[string]any{"poll_ms": 20.0}, Clock: pluginsdk.NewClock(0)}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Wait(); err != nil {
		t.Fatal(err)
	}
	var n int
	for _, ev := range sink.Drain() {
		if pb := ev.GetPower(); pb != nil {
			if pb.GetCurrentNa()[0] != 581_390 || pb.GetVoltageUv()[0] != 3_350_000 {
				t.Fatalf("sample %v", pb)
			}
			n++
		}
	}
	if n < 2 {
		t.Fatalf("%d samples", n)
	}
}

func TestNetPTICapture(t *testing.T) {
	p, te := newTestPlugin(t)
	mpdu := []byte{0x41, 0x88, 0x01, 0xcd, 0xab, 0xff, 0xff, 0x00, 0x00, 0x09}
	ota := append(append([]byte{byte(len(mpdu) + 2)}, mpdu...), 0x12, 0x34)
	rx := append(append([]byte{0xf8}, ota...), 0xf9, 0xc4, 0x04, protocol.PTIProtoZigbee, 0x40|1<<3)
	te.kit.mu.Lock()
	te.kit.records = [][]byte{
		dchFrame(5_000_000_000, protocol.DCHEFRRX, rx),
		dchFrame(5_000_100_000, 0x000c, []byte{1, 2}), // not a trace record
		dchFrame(5_000_200_000, protocol.DCHEFRRX, []byte{0xf8, 0x40, 0x02, 0xf9, 0xc4, 0x25, protocol.PTIProtoBLE, 0x40 | 1<<3}),
	}
	te.kit.mu.Unlock()
	dir := t.TempDir()
	sink := pluginsdk.NewTestSink(64)
	c, _, err := p.StartCapture(context.Background(), pluginsdk.CaptureRequest{DeviceID: "wstk-net:lab", Duration: 300 * time.Millisecond,
		Config: map[string]any{"mode": "pti"}, ArtifactDir: dir, Clock: pluginsdk.NewClock(0)}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Wait(); err != nil {
		t.Fatal(err)
	}
	res := c.Result()
	if len(res.GetArtifacts()) != 1 || res.GetArtifacts()[0].GetMetadata()["packets"] != "1" || res.GetArtifacts()[0].GetMetadata()["non_802154_records"] != "1" {
		t.Fatalf("result %v", res)
	}
	var pkts int
	for _, ev := range sink.Drain() {
		if pk := ev.GetPacket(); pk != nil {
			pkts++
			if pk.GetChannel() != 15 || pk.GetRssiDbm() != -60 || string(pk.GetData()) != string(mpdu) || pk.GetLinkType() != pcap.LinkIEEE802154NoFCS {
				t.Fatalf("packet %v", pk)
			}
		}
	}
	if pkts != 1 {
		t.Fatalf("%d packet events", pkts)
	}
	f, err := os.Open(filepath.Join(dir, "wstk-pti.pcapng"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	ng, err := pcap.ReadNG(f)
	if err != nil || ng.LinkType != pcap.LinkIEEE802154TAP || len(ng.Packets) != 1 {
		t.Fatalf("pcapng %+v %v", ng, err)
	}
}
