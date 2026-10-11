package pcap

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"
)

func TestTAPHeaderLayout(t *testing.T) {
	frame := []byte{0x02, 0x00, 0x64}
	b := EncodeTAP15_4(TAPMeta{Channel: 15, RSSIdBm: -60, LQI: 0x40}, frame)
	le := binary.LittleEndian
	if b[0] != 0 || b[1] != 0 {
		t.Fatalf("version/reserved = %x %x", b[0], b[1])
	}
	hl := int(le.Uint16(b[2:]))
	if hl%4 != 0 || !bytes.Equal(b[hl:], frame) {
		t.Fatalf("header length %d, tail %x", hl, b[hl:])
	}
	// Walk the TLVs and check RSSI, channel and LQI.
	seen := map[uint16][]byte{}
	for o := 4; o < hl; {
		typ, l := le.Uint16(b[o:]), int(le.Uint16(b[o+2:]))
		seen[typ] = b[o+4 : o+4+l]
		o += 4 + pad4(l)
	}
	if got := math.Float32frombits(le.Uint32(seen[tapTLVRSSI])); got != -60 {
		t.Errorf("rssi = %v", got)
	}
	if le.Uint16(seen[tapTLVChannel]) != 15 || seen[tapTLVChannel][2] != 0 {
		t.Errorf("channel tlv = %v", seen[tapTLVChannel])
	}
	if seen[tapTLVLQI][0] != 0x40 {
		t.Errorf("lqi = %v", seen[tapTLVLQI])
	}
}

func TestNGRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	w, err := NewNGWriter(&buf, "hwlab-test", NGInterface{LinkType: LinkIEEE802154TAP, Name: "ch15", Description: "d"})
	if err != nil {
		t.Fatal(err)
	}
	pk := [][]byte{{1}, {1, 2, 3, 4, 5}, bytes.Repeat([]byte{9}, 64)}
	ts := []int64{0, 1_500_000_123, 40_000_000_000}
	for i := range pk {
		if err := w.WritePacket(ts[i], pk[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(50_000_000_000, 3, 1, "clock: test"); err != nil {
		t.Fatal(err)
	}
	if buf.Len()%4 != 0 {
		t.Fatalf("file length %d not 32-bit aligned", buf.Len())
	}
	f, err := ReadNG(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if f.LinkType != LinkIEEE802154TAP || len(f.Packets) != 3 || f.Received != 3 || f.Dropped != 1 || f.Comment != "clock: test" {
		t.Fatalf("file = %+v", f)
	}
	for i := range pk {
		if f.Packets[i].TsNs != ts[i] || !bytes.Equal(f.Packets[i].Data, pk[i]) {
			t.Errorf("packet %d = %+v", i, f.Packets[i])
		}
	}
}

func TestReadNGRejectsTruncated(t *testing.T) {
	var buf bytes.Buffer
	w, _ := NewNGWriter(&buf, "x", NGInterface{LinkType: 1})
	_ = w.WritePacket(1, []byte{1, 2, 3})
	if _, err := ReadNG(bytes.NewReader(buf.Bytes()[:buf.Len()-3])); err == nil {
		t.Fatal("expected error for truncated file")
	}
}
