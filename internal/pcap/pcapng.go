package pcap

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

// LinkIEEE802154TAP is DLT 283, "IEEE 802.15.4 TAP": a TLV header carrying
// channel, RSSI and LQI in front of the frame, understood by Wireshark >= 3.6.
const LinkIEEE802154TAP uint32 = 283

// TAP TLV types used by hwlab (see the IEEE 802.15.4 TAP specification).
const (
	tapTLVFCSType = 0
	tapTLVRSSI    = 1
	tapTLVChannel = 3
	tapTLVLQI     = 10
)

// TAPMeta is the per-frame radio metadata stored in a TAP header.
type TAPMeta struct {
	Channel uint16  // 2.4 GHz channel number (11-26)
	Page    uint8   // channel page (0 for 2.4 GHz O-QPSK)
	RSSIdBm float32 // received signal strength
	LQI     uint8
}

// EncodeTAP15_4 prepends an IEEE 802.15.4 TAP header to a frame that carries
// no FCS. The header declares "no FCS" so dissectors do not expect one.
func EncodeTAP15_4(m TAPMeta, frame []byte) []byte {
	le := binary.LittleEndian
	b := make([]byte, 0, 32+len(frame))
	b = append(b, 0, 0, 0, 0) // version, reserved, total length (patched below)
	tlv := func(typ uint16, val []byte) {
		var h [4]byte
		le.PutUint16(h[0:], typ)
		le.PutUint16(h[2:], uint16(len(val)))
		b = append(b, h[:]...)
		b = append(b, val...)
		for len(b)%4 != 0 {
			b = append(b, 0)
		}
	}
	tlv(tapTLVFCSType, []byte{0}) // 0 = no FCS present
	var f [4]byte
	le.PutUint32(f[:], math.Float32bits(m.RSSIdBm))
	tlv(tapTLVRSSI, f[:])
	tlv(tapTLVChannel, []byte{byte(m.Channel), byte(m.Channel >> 8), m.Page})
	tlv(tapTLVLQI, []byte{m.LQI})
	le.PutUint16(b[2:], uint16(len(b)))
	return append(b, frame...)
}

// pcapng block types and option codes (IETF draft-ietf-opsawg-pcapng).
const (
	blockSHB uint32 = 0x0A0D0D0A
	blockIDB uint32 = 1
	blockEPB uint32 = 6
	blockISB uint32 = 5

	optEnd         = 0
	optComment     = 1
	optSHBUserAppl = 4
	optIFName      = 2
	optIFDesc      = 3
	optIFTsresol   = 9
	optISBRecv     = 4
	optISBDrop     = 5
)

// NGWriter writes a single-interface pcapng file with nanosecond timestamps.
// Packet timestamps are nanoseconds on the hwlab session timeline.
type NGWriter struct {
	w io.Writer
}

// NGInterface describes the one capture interface of a file.
type NGInterface struct {
	LinkType    uint32
	Name        string
	Description string
}

func pad4(n int) int { return (n + 3) &^ 3 }

func appendOpt(b []byte, code uint16, val []byte) []byte {
	var h [4]byte
	binary.LittleEndian.PutUint16(h[0:], code)
	binary.LittleEndian.PutUint16(h[2:], uint16(len(val)))
	b = append(b, h[:]...)
	b = append(b, val...)
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return b
}

func (n *NGWriter) block(typ uint32, body []byte) error {
	total := 12 + pad4(len(body))
	buf := make([]byte, total)
	le := binary.LittleEndian
	le.PutUint32(buf[0:], typ)
	le.PutUint32(buf[4:], uint32(total))
	copy(buf[8:], body)
	le.PutUint32(buf[total-4:], uint32(total))
	_, err := n.w.Write(buf)
	return err
}

// NewNGWriter writes the section header and interface description blocks.
func NewNGWriter(w io.Writer, app string, ifc NGInterface) (*NGWriter, error) {
	n := &NGWriter{w: w}
	le := binary.LittleEndian
	shb := make([]byte, 16)
	le.PutUint32(shb[0:], 0x1A2B3C4D)
	le.PutUint16(shb[4:], 1)
	le.PutUint16(shb[6:], 0)
	le.PutUint64(shb[8:], ^uint64(0)) // section length unknown
	shb = appendOpt(shb, optSHBUserAppl, []byte(app))
	shb = appendOpt(shb, optEnd, nil)
	if err := n.block(blockSHB, shb); err != nil {
		return nil, err
	}
	idb := make([]byte, 8)
	le.PutUint16(idb[0:], uint16(ifc.LinkType))
	le.PutUint32(idb[4:], 262144)
	if ifc.Name != "" {
		idb = appendOpt(idb, optIFName, []byte(ifc.Name))
	}
	if ifc.Description != "" {
		idb = appendOpt(idb, optIFDesc, []byte(ifc.Description))
	}
	idb = appendOpt(idb, optIFTsresol, []byte{9}) // 10^-9 s
	idb = appendOpt(idb, optEnd, nil)
	if err := n.block(blockIDB, idb); err != nil {
		return nil, err
	}
	return n, nil
}

// WritePacket appends an enhanced packet block.
func (n *NGWriter) WritePacket(tsNs int64, data []byte) error {
	if tsNs < 0 {
		tsNs = 0
	}
	le := binary.LittleEndian
	body := make([]byte, 20, 20+len(data))
	le.PutUint32(body[4:], uint32(uint64(tsNs)>>32))
	le.PutUint32(body[8:], uint32(tsNs))
	le.PutUint32(body[12:], uint32(len(data)))
	le.PutUint32(body[16:], uint32(len(data)))
	body = append(body, data...)
	return n.block(blockEPB, body)
}

// Close appends an interface statistics block carrying the final counters and
// a free-form comment (hwlab stores clock metadata there), then returns.
func (n *NGWriter) Close(tsNs int64, received, dropped uint64, comment string) error {
	le := binary.LittleEndian
	body := make([]byte, 12)
	le.PutUint32(body[4:], uint32(uint64(tsNs)>>32))
	le.PutUint32(body[8:], uint32(tsNs))
	if comment != "" {
		body = appendOpt(body, optComment, []byte(comment))
	}
	var v [8]byte
	le.PutUint64(v[:], received)
	body = appendOpt(body, optISBRecv, v[:])
	le.PutUint64(v[:], dropped)
	body = appendOpt(body, optISBDrop, v[:])
	body = appendOpt(body, optEnd, nil)
	return n.block(blockISB, body)
}

// NGPacket is one packet read back from a pcapng file.
type NGPacket struct {
	TsNs int64
	Data []byte
}

// NGFile is the parsed content of a hwlab pcapng file.
type NGFile struct {
	LinkType uint32
	Packets  []NGPacket
	Comment  string // ISB comment
	Received uint64
	Dropped  uint64
}

// ReadNG parses a little-endian single-interface pcapng file as written by
// NGWriter. It exists for tests and replay; Wireshark is the general reader.
func ReadNG(r io.Reader) (*NGFile, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	le := binary.LittleEndian
	f := &NGFile{}
	for off := 0; off < len(raw); {
		if len(raw)-off < 12 {
			return nil, fmt.Errorf("pcapng: truncated block at %d", off)
		}
		typ := le.Uint32(raw[off:])
		total := int(le.Uint32(raw[off+4:]))
		if total < 12 || total%4 != 0 || off+total > len(raw) || le.Uint32(raw[off+total-4:]) != uint32(total) {
			return nil, fmt.Errorf("pcapng: bad block length at %d", off)
		}
		body := raw[off+8 : off+total-4]
		switch typ {
		case blockSHB:
			if le.Uint32(body) != 0x1A2B3C4D {
				return nil, fmt.Errorf("pcapng: unsupported byte order")
			}
		case blockIDB:
			f.LinkType = uint32(le.Uint16(body))
		case blockEPB:
			ts := int64(le.Uint32(body[4:]))<<32 | int64(le.Uint32(body[8:]))
			capLen := int(le.Uint32(body[12:]))
			if 20+capLen > len(body) {
				return nil, fmt.Errorf("pcapng: packet overruns block")
			}
			f.Packets = append(f.Packets, NGPacket{TsNs: ts, Data: bytes.Clone(body[20 : 20+capLen])})
		case blockISB:
			for o := 12; o+4 <= len(body); {
				code, l := le.Uint16(body[o:]), int(le.Uint16(body[o+2:]))
				if code == optEnd || o+4+l > len(body) {
					break
				}
				val := body[o+4 : o+4+l]
				switch {
				case code == optComment:
					f.Comment = string(val)
				case code == optISBRecv && l == 8:
					f.Received = le.Uint64(val)
				case code == optISBDrop && l == 8:
					f.Dropped = le.Uint64(val)
				}
				o += 4 + pad4(l)
			}
		}
		off += total
	}
	return f, nil
}
