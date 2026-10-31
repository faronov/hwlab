package protocol

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/hex"
	"math"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestKitRequestMatchesTrace(t *testing.T) {
	for _, c := range []struct {
		cmd, arg byte
		want     string
	}{
		{KitCmdInfo, 1, "01000000000701000009"},
		{KitCmdBoards, 1, "01000000001001000012"},
		{0x08, 1, "0100000000080100000a"},
	} {
		if got := hexs(KitRequest(c.cmd, c.arg, nil)); got != c.want {
			t.Errorf("KitRequest(%02x,%02x) = %s, want %s", c.cmd, c.arg, got, c.want)
		}
	}
	st, n, err := ParseKitHeader(unhex(t, "01000000000100400042"))
	if err != nil || st != 1 || n != 0x40 {
		t.Fatalf("ParseKitHeader = %d %d %v", st, n, err)
	}
	if _, _, err := ParseKitHeader(unhex(t, "01000000000100400043")); err == nil {
		t.Fatal("bad checksum accepted")
	}
}

func TestKitInfoAndBoardsOverSimKit(t *testing.T) {
	for _, combined := range []bool{false, true} {
		sim := NewSimKit()
		sim.Combined = combined
		e := &Emucom{L: sim, Poll: time.Millisecond}
		ctx := context.Background()
		r, err := e.KitCommand(ctx, KitCmdInfo, 1, nil, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		info, err := ParseKitInfo(r)
		if err != nil {
			t.Fatal(err)
		}
		if info != (KitInfo{Firmware: "1.4.6", Build: 1171, Mainboard: "BRD4001", MainboardSerial: 200000001}) {
			t.Fatalf("info = %+v", info)
		}
		r, err = e.KitCommand(ctx, KitCmdBoards, 1, nil, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		boards, err := ParseBoards(r)
		if err != nil {
			t.Fatal(err)
		}
		if len(boards) != 2 || boards[0].ID() != "BRD4001A Rev. A02" || boards[1].ID() != "BRD4183A Rev. A00" ||
			boards[0].Serial != 200000001 || boards[1].Date != "2020-11-20" || boards[1].Name == "" {
			t.Fatalf("boards = %+v", boards)
		}
		if _, err := e.KitCommand(ctx, 0x11, 0, nil, time.Second); err == nil {
			t.Fatal("status 2 accepted")
		}
	}
}

// aem_stream.bin holds the AEM channel bytes of a recorded session (config
// reply, then four data messages of which the last re-sends the third);
// aem_commander.csv is Simplicity Commander's CSV of the same samples.
func TestAEMRecordedStreamMatchesCommanderCSV(t *testing.T) {
	raw, err := os.ReadFile("testdata/aem_stream.bin")
	if err != nil {
		t.Fatal(err)
	}
	var sp AEMSplitter
	var msgs []AEMMessage
	for i := 0; i < len(raw); i += 100 { // arbitrary read boundaries
		msgs = append(msgs, sp.Feed(raw[i:min(i+100, len(raw))])...)
	}
	if len(msgs) != 5 || sp.Skipped != 0 {
		t.Fatalf("%d messages, %d skipped", len(msgs), sp.Skipped)
	}
	cfg, err := ParseAEMConfig(msgs[0])
	if err != nil || cfg.SampleRate != 10000 || cfg.Version != 1 {
		t.Fatalf("config %+v %v", cfg, err)
	}
	var tl AEMTimeline
	var got [][3]float64
	for _, m := range msgs[1:] {
		b, err := ParseAEMData(m)
		if err != nil {
			t.Fatal(err)
		}
		start, fresh := tl.Place(b)
		if !fresh {
			continue
		}
		for i, c := range b.CurrentmA {
			got = append(got, [3]float64{float64(start) + float64(i)*1e6/float64(b.SampleRate), float64(c), float64(b.VoltageV)})
		}
	}
	if tl.Duplicates != 1 || len(got) != 768 {
		t.Fatalf("duplicates %d, %d samples", tl.Duplicates, len(got))
	}
	want := readCSV(t, "testdata/aem_commander.csv")
	if len(want) != len(got) {
		t.Fatalf("csv has %d rows, stream %d samples", len(want), len(got))
	}
	for i, w := range want {
		g := got[i]
		// Commander stamps the middle of each 100 us interval and prints
		// 6 significant digits.
		if math.Abs(g[0]+50-w[0]) > 1 || math.Abs(g[1]-w[1]) > 1e-4 || math.Abs(g[2]-w[2]) > 1e-4 {
			t.Fatalf("sample %d = %v, commander %v", i, g, w)
		}
	}
}

func TestAEMTimelineUnwrapsMicroseconds(t *testing.T) {
	var tl AEMTimeline
	// 71.6 min after boot the 32-bit us counter has wrapped; the ms
	// header still carries the full time.
	ms := uint32(4_400_000)
	start := uint32(uint64(ms)*1000 - 25_000) // truncated to 32 bits
	got, fresh := tl.Place(AEMBuffer{KitMs: ms, StartUs: start})
	if !fresh || got != int64(ms)*1000-25_000 {
		t.Fatalf("Place = %d %v", got, fresh)
	}
	if _, fresh := tl.Place(AEMBuffer{KitMs: ms - 30, StartUs: start - 25_600}); fresh || tl.Stale != 1 {
		t.Fatal("older buffer accepted")
	}
}

func TestAEMSplitterResyncs(t *testing.T) {
	sim := NewSimKit()
	sim.handle(ChanAEM, AEMStartRequest())
	stream := append([]byte{1, 2, 3, 0xea}, sim.queues[ChanAEM]...)
	stream = append(stream, sim.aemBuffer()...)
	var sp AEMSplitter
	msgs := sp.Feed(stream)
	if len(msgs) != 2 || sp.Skipped != 4 || msgs[1].Type != AEMTypeData {
		t.Fatalf("%d messages, %d skipped", len(msgs), sp.Skipped)
	}
}

func TestAEMStartRequest(t *testing.T) {
	if got := hexs(AEMStartRequest()); got != "eaeaaeae00000000108020000000000000000000000000000000000000000000" {
		t.Fatal(got)
	}
}

func TestDCHFramerV2V3AndResync(t *testing.T) {
	v3 := []byte{3, 0}
	v3 = binary.LittleEndian.AppendUint64(v3, 123456789)
	v3 = binary.LittleEndian.AppendUint16(v3, DCHEFRRX)
	v3 = binary.LittleEndian.AppendUint32(v3, 0)
	v3 = binary.LittleEndian.AppendUint16(v3, 7)
	v3 = append(v3, 0xaa, 0xbb)
	v2 := []byte{2, 0, 0x10, 0x27, 0, 0, 0, 0, 0x29, 0, 9, 0xcc}
	frame := func(m []byte) []byte {
		b := []byte{'[', byte(len(m)), byte(len(m) >> 8)}
		return append(append(b, m...), ']')
	}
	stream := append([]byte("xx[\x05\x00garbage"), frame(v3)...)
	stream = append(stream, frame(v2)...)
	var f DCHFramer
	var got []DCHMessage
	for _, c := range stream {
		got = append(got, f.Feed([]byte{c})...)
	}
	if len(got) != 2 {
		t.Fatalf("%d messages, skipped %d, bad %d", len(got), f.Skipped, f.Bad)
	}
	if m := got[0]; m.Version != 3 || m.TimeNs != 123456789 || m.Type != DCHEFRRX || m.Seq != 7 || hexs(m.Payload) != "aabb" {
		t.Fatalf("v3 = %+v", m)
	}
	if m := got[1]; m.Version != 2 || m.TimeNs != 10_000_000 || m.Type != DCHEFRTX || m.Seq != 9 || hexs(m.Payload) != "cc" {
		t.Fatalf("v2 = %+v", m)
	}
}

func TestParsePTI(t *testing.T) {
	mpdu := []byte{0x41, 0x88, 0x01, 0xcd, 0xab, 0xff, 0xff, 0x00, 0x00, 0x09} // data frame
	fcs := []byte{0x12, 0x34}
	ota := append(append([]byte{byte(len(mpdu) + 2)}, mpdu...), fcs...)
	// rx, var_len 1 (rssi), version 0; channel index 4 (ch 15), Zigbee
	rx := append(append([]byte{0xf8}, ota...), 0xf9, 0xc4, 0x04, PTIProtoZigbee, 0x40|1<<3)
	f, err := ParsePTI(rx)
	if err != nil {
		t.Fatal(err)
	}
	if !f.RX || f.Aborted || !f.HasRSSI || f.RSSIdBm != -60 || f.Channel != 15 || !f.IEEE802154 || hexs(f.MPDU()) != hexs(mpdu) {
		t.Fatalf("rx = %+v", f)
	}
	// tx, var_len 0, version 1, channel index 0 (ch 11), EmberPHY
	tx := append(append([]byte{0xfc}, ota...), 0xfd, 0x00, PTIProtoEmberPHY, 0x01)
	if f, err = ParsePTI(tx); err != nil || f.RX || f.HasRSSI || f.Channel != 11 || hexs(f.MPDU()) != hexs(mpdu) {
		t.Fatalf("tx = %+v %v", f, err)
	}
	// rx with version 1 RSSI offset, sync word and a 1-byte radio config
	rx = append(append([]byte{0xf8}, ota...), 0xfa, 0x32+0xc4, 1, 2, 3, 4, 0x00, 0x0f, PTIProtoThread, 0x40|6<<3|1)
	if f, err = ParsePTI(rx); err != nil || !f.Aborted || f.RSSIdBm != -60 || f.Channel != 26 {
		t.Fatalf("rx v1 = %+v %v", f, err)
	}
	// BLE frames are decoded but are not 802.15.4
	ble := []byte{0xf8, 0x40, 0x02, 0x01, 0x02, 0xf9, 0xc4, 0x25, PTIProtoBLE, 0x40 | 1<<3}
	if f, err = ParsePTI(ble); err != nil || f.IEEE802154 || f.MPDU() != nil || f.Channel != 37 {
		t.Fatalf("ble = %+v %v", f, err)
	}
	for _, bad := range [][]byte{{0xf8, 1, 2}, {0xf8, 0xf9, 0x00, 0x05, 0x40}, {0xf8, 1, 0xf9, 0x00, 0xf5, 0x48}} {
		if _, err := ParsePTI(bad); err == nil {
			t.Errorf("ParsePTI(% x) accepted", bad)
		}
	}
}

func TestAdminAllowList(t *testing.T) {
	for _, ok := range []string{"dbg info", " AEM  current ", "pti info"} {
		if !AdminAllowed(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"target status", "target reset", "sys reset", "dbg mode mcu", "aem calibrate", "pti config"} {
		if AdminAllowed(bad) {
			t.Errorf("%q allowed", bad)
		}
	}
}

// The reply text below is the kit's "dbg info" output as recorded.
const dbgInfo = "dbg info\r\n------ Debug Interface Status ------\r\nSet mode                 : MCU\r\nActive mode              : MCU\r\n" +
	"Target Voltage           : 3.35 V\r\nExternal cable state     : Disconnected\r\n# debug connections      : 1\r\nAEM current              : 581.39 uA\r\n"

func TestAdminOverPipe(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		kit, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = kit.Close() }()
		r := bufio.NewReader(kit)
		_, _ = kit.Write([]byte("Welcome\r\n" + AdminPrompt))
		for {
			l, err := r.ReadString('\n')
			if err != nil {
				return
			}
			switch strings.TrimSpace(l) {
			case "dbg info":
				_, _ = kit.Write([]byte(dbgInfo + AdminPrompt))
			default:
				_, _ = kit.Write([]byte("\r\n" + AdminPrompt))
			}
		}
	}()
	host, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = host.Close() }()
	a := NewAdmin(host)
	ctx := context.Background()
	if err := a.Sync(ctx, time.Second); err != nil {
		t.Fatal(err)
	}
	out, err := a.Command(ctx, "dbg info", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(out, "dbg info") {
		t.Fatalf("echo kept: %q", out)
	}
	i, ok := ParseAdminCurrent(out)
	if !ok || math.Abs(i-581.39e-6) > 1e-12 {
		t.Fatalf("current %v %v", i, ok)
	}
	v, ok := ParseAdminVoltage(out)
	if !ok || v != 3.35 {
		t.Fatalf("voltage %v %v", v, ok)
	}
	if _, err := a.Command(ctx, "target reset", time.Second); err == nil {
		t.Fatal("target reset sent")
	}
}

func TestParseQuantityUnits(t *testing.T) {
	for s, want := range map[string]float64{"12.5 mA": 12.5e-3, "Current: 800 nA": 800e-9, "1.2e-3 A": 1.2e-3, "3 µA": 3e-6} {
		if got, ok := ParseQuantity(s, CurrentUnits...); !ok || math.Abs(got-want) > 1e-15 {
			t.Errorf("ParseQuantity(%q) = %v %v", s, got, ok)
		}
	}
	if _, ok := ParseQuantity("0 packets", CurrentUnits...); ok {
		t.Error("unitless number parsed")
	}
}

func readCSV(t *testing.T, path string) [][3]float64 {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out [][3]float64
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n")[1:] {
		var r [3]float64
		for i, f := range strings.Split(strings.TrimSpace(l), ",") {
			if r[i], err = strconv.ParseFloat(f, 64); err != nil {
				t.Fatal(err)
			}
		}
		out = append(out, r)
	}
	return out
}

func hexs(b []byte) string { return hex.EncodeToString(b) }

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
