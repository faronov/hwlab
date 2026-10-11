// Package pcap writes classic libpcap files (nanosecond resolution) so packet
// captures can be opened in Wireshark or decoded with tshark.
package pcap

import (
	"encoding/binary"
	"io"
)

// Link types (DLT) used by hwlab.
const (
	LinkIEEE802154WithFCS uint32 = 195
	LinkIEEE802154NoFCS   uint32 = 230
	LinkEthernet          uint32 = 1
	LinkCANSocket         uint32 = 227
)

// Writer writes a pcap stream.
type Writer struct {
	w   io.Writer
	buf [16]byte
}

// NewWriter writes the global header immediately.
func NewWriter(w io.Writer, linkType uint32) (*Writer, error) {
	var h [24]byte
	le := binary.LittleEndian
	le.PutUint32(h[0:], 0xa1b23c4d) // nanosecond-resolution magic
	le.PutUint16(h[4:], 2)
	le.PutUint16(h[6:], 4)
	le.PutUint32(h[16:], 262144)
	le.PutUint32(h[20:], linkType)
	if _, err := w.Write(h[:]); err != nil {
		return nil, err
	}
	return &Writer{w: w}, nil
}

// WritePacket appends a packet. tsNs is nanoseconds since the pcap epoch; the
// session timeline origin is used, so Wireshark shows 1970-01-01 + offset.
func (p *Writer) WritePacket(tsNs int64, data []byte) error {
	le := binary.LittleEndian
	if tsNs < 0 {
		tsNs = 0
	}
	le.PutUint32(p.buf[0:], uint32(tsNs/1_000_000_000))
	le.PutUint32(p.buf[4:], uint32(tsNs%1_000_000_000))
	le.PutUint32(p.buf[8:], uint32(len(data)))
	le.PutUint32(p.buf[12:], uint32(len(data)))
	if _, err := p.w.Write(p.buf[:]); err != nil {
		return err
	}
	_, err := p.w.Write(data)
	return err
}

// FCS16 computes the IEEE 802.15.4 frame check sequence (CRC-16/KERMIT).
func FCS16(data []byte) uint16 {
	var crc uint16
	for _, b := range data {
		crc ^= uint16(b)
		for i := 0; i < 8; i++ {
			if crc&1 != 0 {
				crc = crc>>1 ^ 0x8408
			} else {
				crc >>= 1
			}
		}
	}
	return crc
}

// AppendFCS returns frame followed by its little-endian FCS.
func AppendFCS(frame []byte) []byte {
	f := FCS16(frame)
	return append(frame, byte(f), byte(f>>8))
}
