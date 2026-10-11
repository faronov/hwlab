package pcap

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestWriterLayout(t *testing.T) {
	var buf bytes.Buffer
	w, err := NewWriter(&buf, LinkIEEE802154WithFCS)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.WritePacket(2_500_000_123, []byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	le := binary.LittleEndian
	if len(b) != 24+16+3 {
		t.Fatalf("len = %d", len(b))
	}
	if le.Uint32(b[0:]) != 0xa1b23c4d || le.Uint32(b[20:]) != LinkIEEE802154WithFCS {
		t.Errorf("bad global header: magic=%#x link=%d", le.Uint32(b[0:]), le.Uint32(b[20:]))
	}
	if le.Uint32(b[24:]) != 2 || le.Uint32(b[28:]) != 500_000_123 || le.Uint32(b[32:]) != 3 {
		t.Errorf("bad record header: sec=%d nsec=%d len=%d", le.Uint32(b[24:]), le.Uint32(b[28:]), le.Uint32(b[32:]))
	}
}

func TestFCS16(t *testing.T) {
	// 802.15.4 ACK frame for sequence number 0x07: FCF 0x0002, seq 0x07.
	frame := []byte{0x02, 0x00, 0x07}
	with := AppendFCS(frame)
	if len(with) != 5 {
		t.Fatalf("AppendFCS length %d", len(with))
	}
	if got := FCS16(with[:3]); got != uint16(with[3])|uint16(with[4])<<8 {
		t.Errorf("appended FCS does not match FCS16: %#x", got)
	}
	// Known CRC-16/KERMIT check value for "123456789" is 0x2189.
	if got := FCS16([]byte("123456789")); got != 0x2189 {
		t.Errorf("FCS16(123456789) = %#x, want 0x2189", got)
	}
}
