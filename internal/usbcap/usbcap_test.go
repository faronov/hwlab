package usbcap

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	in := Packet{TimestampNs: 12_345_678_000, URBID: 0xffff8800deadbeef, Event: Complete, Transfer: Bulk, Bus: 3, Device: 12, Endpoint: 2, In: true,
		Status: -32, Length: 512, Data: []byte{1, 2, 3, 4, 5}}
	b := in.Encode()
	if len(b) != HeaderLen+5 {
		t.Fatalf("encoded %d bytes", len(b))
	}
	// Layout checked against the usbmon documentation (struct usbmon_packet).
	if b[8] != 'C' || b[9] != 3 || b[10] != 0x82 || b[11] != 12 || binary.LittleEndian.Uint16(b[12:]) != 3 {
		t.Fatalf("header %x", b[:16])
	}
	if binary.LittleEndian.Uint64(b[16:]) != 12 || binary.LittleEndian.Uint32(b[24:]) != 345678 {
		t.Fatalf("timestamp fields %x", b[16:28])
	}
	if int32(binary.LittleEndian.Uint32(b[28:])) != -32 || binary.LittleEndian.Uint32(b[32:]) != 512 || binary.LittleEndian.Uint32(b[36:]) != 5 {
		t.Fatalf("status/len %x", b[28:40])
	}
	out, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if out.TimestampNs != in.TimestampNs || out.URBID != in.URBID || out.Status != -32 || out.Endpoint != 2 || !out.In ||
		out.Bus != 3 || out.Device != 12 || !bytes.Equal(out.Data, in.Data) || out.Length != 512 || out.HasSetup {
		t.Fatalf("round trip %+v", out)
	}
}

func TestSetupRoundTrip(t *testing.T) {
	in := Packet{Event: Submit, Transfer: Control, Bus: 1, Device: 2, In: true, HasSetup: true, Setup: setup(0x80, 6, 0x0100, 0, 18), Length: 18}
	out, err := Decode(in.Encode())
	if err != nil || !out.HasSetup || out.Setup != in.Setup || !out.In {
		t.Fatalf("%+v %v", out, err)
	}
	if got := out.Summary(); !strings.Contains(got, "GET_DESCRIPTOR(DEVICE)") {
		t.Errorf("summary %q", got)
	}
}

func TestDecodeRejectsGarbage(t *testing.T) {
	if _, err := Decode(make([]byte, 10)); err == nil {
		t.Error("short buffer accepted")
	}
	b := make([]byte, HeaderLen)
	b[8] = 'X'
	if _, err := Decode(b); err == nil {
		t.Error("unknown event accepted")
	}
	b[8], b[9] = 'S', 9
	if _, err := Decode(b); err == nil {
		t.Error("unknown transfer type accepted")
	}
	// A header claiming more data than present must not panic or over-read.
	b[9] = 3
	binary.LittleEndian.PutUint32(b[36:], 1000)
	if p, err := Decode(b); err != nil || len(p.Data) != 0 {
		t.Errorf("%v %d", err, len(p.Data))
	}
}

func TestTextParserDocumentedLines(t *testing.T) {
	var tp TextParser
	p, ok, err := tp.Parse("d5ea89a0 3575914555 S Ci:1:001:0 s a3 00 0000 0003 0004 4 <")
	if err != nil || !ok {
		t.Fatal(err)
	}
	if p.Event != Submit || p.Transfer != Control || !p.In || p.Bus != 1 || p.Device != 1 || p.Endpoint != 0 || !p.HasSetup ||
		p.Setup != setup(0xa3, 0, 0, 3, 4) || p.Length != 4 || p.URBID != 0xd5ea89a0 || p.TimestampNs != 3575914555*1000 {
		t.Errorf("%+v", p)
	}
	p, _, err = tp.Parse("d5ea89a0 3575914560 C Ci:1:001:0 0 4 = 01050000")
	if err != nil || p.Event != Complete || p.Status != 0 || !bytes.Equal(p.Data, []byte{1, 5, 0, 0}) || p.Length != 4 {
		t.Errorf("%+v %v", p, err)
	}
	p, _, err = tp.Parse("dd0f5a80 3575915000 S Bo:2:005:2 -115 31 = 55534243 01000000 24000000 80000c12")
	if err != nil || p.Transfer != Bulk || p.In || p.Status != -115 || len(p.Data) != 16 || p.Length != 31 {
		t.Errorf("%+v %v", p, err)
	}
	p, _, err = tp.Parse("dd0f5a80 3575915100 C Ii:2:005:1 -32:8 0")
	if err != nil || p.Transfer != Interrupt || p.Status != -32 || p.Length != 0 || len(p.Data) != 0 {
		t.Errorf("%+v %v", p, err)
	}
	// Setup packet the kernel could not copy.
	p, _, err = tp.Parse("dd0f5a80 3575915200 S Co:2:005:0 Z __ __ ____ ____ ____ 0")
	if err != nil || p.HasSetup || p.Length != 0 {
		t.Errorf("%+v %v", p, err)
	}
	// Isochronous: per-frame descriptors are skipped.
	p, _, err = tp.Parse("dd0f5a80 3575915300 C Zi:2:005:3 0:1:0 #2 0:0:0:4 1:0:4:4 8 = 01020304 05060708")
	if err != nil || p.Transfer != Isochronous || p.Length != 8 || len(p.Data) != 8 {
		t.Errorf("%+v %v", p, err)
	}
}

func TestTextParserErrors(t *testing.T) {
	var tp TextParser
	if _, ok, err := tp.Parse("   "); ok || err != nil {
		t.Errorf("blank line: %v %v", ok, err)
	}
	for _, bad := range []string{
		"zz 1 S Ci:1:1:0 0 0 <",
		"d5 x S Ci:1:1:0 0 0 <",
		"d5 1 Q Ci:1:1:0 0 0 <",
		"d5 1 S Xi:1:1:0 0 0 <",
		"d5 1 S Ci:1:1 0 0 <",
		"d5 1 S Cx:1:1:0 0 0 <",
		"d5 1 S Ci:1:1:0 s a3 00 <",
		"d5 1 S Bi:1:1:0 0 4 = 0g",
		"short line",
	} {
		if _, ok, err := tp.Parse(bad); ok || err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestTextTimestampUnwrap(t *testing.T) {
	var tp TextParser
	line := func(ts string) Packet {
		p, ok, err := tp.Parse("a " + ts + " S Bi:1:2:1 -115 8 <")
		if !ok || err != nil {
			t.Fatal(err)
		}
		return p
	}
	a := line("4294967000") // just before the 2^32 µs wrap
	b := line("500")        // just after
	c := line("4294967100") // a straggler from before the wrap, arriving late
	if b.TimestampNs-a.TimestampNs != (500+296)*1000 {
		t.Errorf("wrap not unwrapped: %d -> %d", a.TimestampNs, b.TimestampNs)
	}
	if c.TimestampNs >= b.TimestampNs || c.TimestampNs <= a.TimestampNs {
		t.Errorf("straggler misplaced: %d %d %d", a.TimestampNs, c.TimestampNs, b.TimestampNs)
	}
}

func TestFormatTextRoundTrip(t *testing.T) {
	var tp TextParser
	for _, p := range Synthetic(6) {
		line := FormatText(p)
		q, ok, err := tp.Parse(line)
		if err != nil || !ok {
			t.Fatalf("%q: %v", line, err)
		}
		// The text API holds 32 bytes of data and microsecond stamps.
		want := p.Data
		if len(want) > 32 {
			want = want[:32]
		}
		if !bytes.Equal(q.Data, want) || q.Length != p.Length || q.Event != p.Event || q.Device != p.Device || (!p.HasSetup && q.Status != p.Status) ||
			q.Endpoint != p.Endpoint || q.In != p.In || q.HasSetup != p.HasSetup || q.Setup != p.Setup {
			t.Fatalf("%q -> %+v, want %+v", line, q, p)
		}
	}
}

func TestParseDescriptors(t *testing.T) {
	d, ok := ParseDevice(devDesc(2, 0x1209, 0x0003))
	if !ok || d.VID != 0x1209 || d.PID != 3 || d.Class != 2 || d.USB != 0x200 || !d.Full {
		t.Errorf("%+v", d)
	}
	short, ok := ParseDevice(devDesc(0, 1, 2)[:8])
	if !ok || short.Full || short.MaxPacket0 != 64 {
		t.Errorf("8-byte read %+v", short)
	}
	if _, ok := ParseDevice([]byte{18, 2, 0, 0, 0, 0, 0, 0}); ok {
		t.Error("configuration descriptor taken for a device descriptor")
	}
	cfg := cfgDesc(2, iface(0, 1, ClassCDC, 2, 1), ep(0x83, 3, 16, 16), iface(1, 2, ClassCDCData, 0, 0), ep(0x82, 2, 64, 0), ep(0x01, 2, 64, 0))
	c, ok := ParseConfig(cfg)
	if !ok || c.Truncated || len(c.Interfaces) != 2 || len(c.Interfaces[1].Endpoints) != 2 {
		t.Fatalf("%+v", c)
	}
	if e := c.Interfaces[1].Endpoints[0]; e.Addr != 0x82 || !e.In() || e.Transfer != Bulk || e.MaxPacket != 64 {
		t.Errorf("%+v", e)
	}
	// usbmon's text API cuts data at 32 bytes.
	cut, ok := ParseConfig(cfg[:32])
	if !ok || !cut.Truncated || len(cut.Interfaces) != 1 {
		t.Errorf("truncated %+v", cut)
	}
	// Malformed lengths must stop the walk, not loop or panic.
	bad := append([]byte(nil), cfg...)
	bad[9] = 0
	if c, ok := ParseConfig(bad); !ok || !c.Truncated {
		t.Errorf("zero-length descriptor: %+v", c)
	}
	if _, ok := ParseConfig([]byte{9, 2, 0}); ok {
		t.Error("short buffer accepted")
	}
}

func TestClassDecoders(t *testing.T) {
	if s, ok := LineCoding([]byte{0x00, 0xc2, 0x01, 0, 0, 0, 8}); !ok || s != "115200 baud 8N1" {
		t.Errorf("%q", s)
	}
	if s, _ := LineCoding([]byte{0x80, 0x25, 0, 0, 2, 2, 7}); s != "9600 baud 7E2" {
		t.Errorf("%q", s)
	}
	if s, _ := DFUStatus([]byte{0, 0x64, 0, 0, 5, 0}); s != "status ok, dfuDNLOAD-IDLE, poll 100 ms" {
		t.Errorf("%q", s)
	}
	if s, _ := DFUStatus([]byte{0x0b, 0, 0, 0, 10, 0}); !strings.Contains(s, "error 0x0b") || !strings.Contains(s, "dfuERROR") {
		t.Errorf("%q", s)
	}
	cbw := make([]byte, 31)
	copy(cbw, "USBC")
	binary.LittleEndian.PutUint32(cbw[4:], 7)
	binary.LittleEndian.PutUint32(cbw[8:], 512)
	cbw[12], cbw[14], cbw[15] = 0x80, 10, 0x28
	binary.BigEndian.PutUint32(cbw[17:], 2048)
	binary.BigEndian.PutUint16(cbw[22:], 4)
	s, ok := MSCCommandBlock(cbw)
	if !ok || !strings.Contains(s, "READ(10)") || !strings.Contains(s, "LBA 2048 x4") || !strings.Contains(s, "512 bytes in") {
		t.Errorf("%q", s)
	}
	if _, ok := MSCCommandBlock([]byte("USBS0123456789012")); ok {
		t.Error("CSW taken for CBW")
	}
	if s, ok := MSCStatusWrapper(append([]byte("USBS\x07\x00\x00\x00\x00\x02\x00\x00"), 1)); !ok || !strings.Contains(s, "failed") || !strings.Contains(s, "residue 512") {
		t.Errorf("%q", s)
	}
}

func TestDescribeSetup(t *testing.T) {
	cases := []struct {
		s    [8]byte
		ifc  *Interface
		want string
	}{
		{setup(0x80, 6, 0x0200, 0, 9), nil, "GET_DESCRIPTOR(CONFIGURATION #0)"},
		{setup(0x80, 6, 0x0302, 0x409, 255), nil, "GET_DESCRIPTOR(STRING #2)"},
		{setup(0x00, 9, 1, 0, 0), nil, "SET_CONFIGURATION"},
		{setup(0x21, 0x20, 0, 0, 7), &Interface{Class: ClassCDC}, "CDC SET_LINE_CODING"},
		{setup(0xa1, 0xfe, 0, 0, 1), &Interface{Class: ClassMSC}, "mass-storage GET_MAX_LUN"},
		{setup(0xa1, 3, 0, 0, 6), &Interface{Class: ClassApp, Subclass: 1}, "DFU DFU_GETSTATUS"},
		{setup(0x21, 0x0a, 0, 0, 0), &Interface{Class: ClassHID}, "HID SET_IDLE"},
		{setup(0x21, 0x0a, 0, 0, 0), nil, "class request 0x0a"},
		{setup(0xc0, 0x42, 1, 2, 4), nil, "vendor request 0x42 value 0x0001 index 0x0002"},
	}
	for _, c := range cases {
		if got := DescribeSetup(ParseSetup(c.s), c.ifc); got != c.want {
			t.Errorf("%x: %q, want %q", c.s, got, c.want)
		}
	}
}

func find(a *Analysis, substr string) []Transaction {
	var out []Transaction
	for _, t := range a.Transactions {
		if strings.Contains(t.Name, substr) {
			out = append(out, t)
		}
	}
	return out
}

func TestAnalyzeSynthetic(t *testing.T) {
	a := Analyze(Synthetic(20))
	if a.Stats.Errors != 2 || a.Stats.UnpairedComplete != 0 || a.Stats.StillPending != 0 || a.Stats.Transactions*2 != a.Stats.Packets {
		t.Fatalf("stats %+v", a.Stats)
	}
	if len(a.Devices) != 3 {
		t.Fatalf("devices %+v", a.Devices)
	}
	byDev := map[string]DeviceInfo{}
	for _, d := range a.Devices {
		byDev[d.Device] = d
	}
	if d := byDev["1:5"]; d.VID != "1209" || d.PID != "0001" || len(d.Interfaces) != 1 || d.Interfaces[0] != "if0 HID" || d.Errors != 2 {
		t.Errorf("hid %+v", d)
	}
	if d := byDev["1:7"]; len(d.Interfaces) != 2 || d.Interfaces[0] != "if0 CDC" || d.Interfaces[1] != "if1 CDC-data" || d.Class != "CDC" {
		t.Errorf("cdc %+v", d)
	}
	if d := byDev["1:6"]; len(d.Interfaces) != 1 || d.Interfaces[0] != "if0 mass-storage" {
		t.Errorf("msc %+v", d)
	}

	if lc := find(a, "SET_LINE_CODING"); len(lc) != 1 || lc[0].Detail != "115200 baud 8N1" {
		t.Errorf("line coding %+v", lc)
	}
	if cl := find(a, "SET_CONTROL_LINE_STATE"); len(cl) != 1 || cl[0].Detail != "DTR=1 RTS=1" {
		t.Errorf("control line state %+v", cl)
	}
	if g := find(a, "GET_DESCRIPTOR(DEVICE)"); len(g) != 3 || !strings.Contains(g[0].Detail, "1209:0001") {
		t.Errorf("device descriptors %+v", g)
	}
	if g := find(a, "GET_DESCRIPTOR(CONFIGURATION"); len(g) != 6 || g[0].Detail != "configuration 1 header: 34 bytes in total" ||
		!strings.HasPrefix(g[1].Detail, "configuration 1: 1 interface(s): HID") || strings.Contains(g[1].Detail, "truncated") {
		t.Errorf("configuration descriptors %+v", g)
	}
	if h := find(a, "HID report in"); len(h) != 20 || h[0].Detail != "0000ff00" {
		t.Errorf("hid report detail %+v", h[0])
	}
	cmd := find(a, "MSC command")
	if len(cmd) != 2 || !strings.Contains(cmd[0].Detail, "INQUIRY") || !strings.Contains(cmd[1].Detail, "READ(10)") {
		t.Errorf("msc commands %+v", cmd)
	}
	if st := find(a, "MSC status"); len(st) != 2 || !strings.Contains(st[0].Detail, "passed") {
		t.Errorf("msc status %+v", st)
	}
	if cdc := find(a, "CDC data"); len(cdc) != 2 || cdc[0].Detail != `"AT\r\n"` || cdc[1].Detail != `"OK\r\n"` {
		t.Errorf("cdc data %+v", cdc)
	}
	if h := find(a, "HID report in"); len(h) != 20 {
		t.Errorf("hid reports: %d", len(h))
	}
	// The stalled GET_PROTOCOL and the mid-capture interrupt bus error.
	got := map[int32]int{}
	for _, e := range a.ErrorCounts {
		got[e.Status] = e.Count
	}
	if got[-32] != 1 || got[-71] != 1 {
		t.Errorf("error counts %+v", a.ErrorCounts)
	}
	for _, e := range a.Endpoints {
		if e.URBs == 0 || e.MeanLatUs <= 0 || e.MaxLatUs < e.MeanLatUs {
			t.Errorf("endpoint stats %+v", e)
		}
	}
	// The mouse polls every ~8 ms: the interrupt endpoint's latency is the report latency.
	for _, e := range a.Endpoints {
		if e.Device == "1:5" && e.Transfer == "interrupt" && (e.URBs != 20 || e.Errors != 1 || e.Class != "HID") {
			t.Errorf("hid endpoint %+v", e)
		}
	}
}

func TestAnalyzeWithoutEnumerationDoesNotGuess(t *testing.T) {
	var all []Packet
	for _, p := range Synthetic(3) {
		if p.Device == 5 && p.Transfer != Interrupt {
			continue // capture started after enumeration
		}
		all = append(all, p)
	}
	a := Analyze(all)
	h := find(a, "interrupt in")
	if len(h) != 3 {
		t.Fatalf("%+v", a.Transactions)
	}
	if h[0].Class != "" || len(a.Devices[0].Interfaces) != 0 {
		t.Errorf("class invented without a descriptor: %+v", h[0])
	}
}

func TestAnalyzePairingEdgeCases(t *testing.T) {
	sub := Packet{URBID: 1, Event: Submit, Transfer: Bulk, Bus: 1, Device: 2, Endpoint: 1, In: true, Length: 8, Status: -115, TimestampNs: 1000}
	done := sub
	done.Event, done.Status, done.TimestampNs = Complete, 0, 5000
	orphan := done
	orphan.URBID = 99
	pend := sub
	pend.URBID = 7
	a := Analyze([]Packet{orphan, sub, done, pend})
	if a.Stats.UnpairedComplete != 1 || a.Stats.StillPending != 1 || a.Stats.Transactions != 1 {
		t.Errorf("%+v", a.Stats)
	}
	if a.Transactions[0].LatencyNs != 4000 {
		t.Errorf("latency %d", a.Transactions[0].LatencyNs)
	}
	if e := Analyze(nil); e.Stats.Packets != 0 || len(e.Transactions) != 0 {
		t.Errorf("empty %+v", e)
	}
}

func TestSetAddressMovesDevice(t *testing.T) {
	d0 := func(urb uint64, e byte, ts int64, s [8]byte, data []byte, dev int, st int32) Packet {
		return Packet{URBID: urb, Event: e, Transfer: Control, Bus: 1, Device: dev, In: s[0]&0x80 != 0, HasSetup: e == Submit, Setup: s,
			Status: st, TimestampNs: ts, Length: uint32(len(data)), Data: data}
	}
	cfg := cfgDesc(1, iface(0, 1, ClassHID, 1, 1), ep(0x81, 3, 8, 10))
	pk := []Packet{
		d0(1, Submit, 1, setup(0x80, 6, 0x0200, 0, uint16(len(cfg))), nil, 0, -115),
		d0(1, Complete, 2, setup(0x80, 6, 0x0200, 0, uint16(len(cfg))), cfg, 0, 0),
		d0(2, Submit, 3, setup(0, ReqSetAddress, 9, 0, 0), nil, 0, -115),
		d0(2, Complete, 4, setup(0, ReqSetAddress, 9, 0, 0), nil, 0, 0),
		{URBID: 3, Event: Submit, Transfer: Interrupt, Bus: 1, Device: 9, Endpoint: 1, In: true, Length: 8, Status: -115, TimestampNs: 5},
		{URBID: 3, Event: Complete, Transfer: Interrupt, Bus: 1, Device: 9, Endpoint: 1, In: true, Length: 8, Data: []byte{1, 2, 3, 4, 5, 6, 7, 8}, TimestampNs: 6},
	}
	a := Analyze(pk)
	h := find(a, "HID report in")
	if len(h) != 1 || h[0].Device != "1:9" {
		t.Errorf("address change lost the device's interfaces: %+v", a.Transactions)
	}
}
