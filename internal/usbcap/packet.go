// Package usbcap is the vendor-independent USB packet model of hwlab.
//
// A USB packet source (Linux usbmon, a hardware analyzer, a vendor tool, a
// replayed session) produces [Packet] values: one per URB event as the host
// stack sees it. On the plugin protocol a packet travels as a Packet event with
// protocol "usb", link type 220 (LINKTYPE_USB_LINUX_MMAPPED) and the usbmon
// 64-byte header in Packet.data, so it can be written to a pcap(ng) file and
// decoded by Wireshark unchanged. [Analyze] consumes Packets, never a vendor
// structure.
//
// The model is transaction level (URBs), not bus level: it has no SOF, token,
// handshake or bit-timing information. A source that can only provide that
// level (a hardware analyzer) needs its own link type; this package does not
// pretend to cover it.
package usbcap

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
)

// Protocol is the Packet.protocol value of USB events.
const Protocol = "usb"

// LinkType is the pcap link type of the Packet.data layout.
const LinkType uint32 = 220

// HeaderLen is the length of the usbmon "mmapped" header.
const HeaderLen = 64

// Transfer is the USB transfer type, with the usbmon numbering.
type Transfer uint8

// Transfer types.
const (
	Isochronous Transfer = 0
	Interrupt   Transfer = 1
	Control     Transfer = 2
	Bulk        Transfer = 3
)

func (t Transfer) String() string {
	switch t {
	case Isochronous:
		return "isochronous"
	case Interrupt:
		return "interrupt"
	case Control:
		return "control"
	case Bulk:
		return "bulk"
	}
	return "transfer" + strconv.Itoa(int(t))
}

// Event types of a URB.
const (
	Submit   byte = 'S'
	Complete byte = 'C'
	Error    byte = 'E'
)

// Packet is one URB event.
type Packet struct {
	TimestampNs int64
	URBID       uint64 // pairs a submission with its completion
	Event       byte   // Submit, Complete or Error
	Transfer    Transfer
	Bus         int
	Device      int
	Endpoint    int  // number without the direction bit
	In          bool // device-to-host
	Status      int32
	Length      uint32 // URB length (requested on submit, actual on completion)
	HasSetup    bool   // control submissions only
	Setup       [8]byte
	Data        []byte // captured payload; may be shorter than Length
}

// DeviceKey is "bus:device".
func (p Packet) DeviceKey() string { return fmt.Sprintf("%d:%d", p.Bus, p.Device) }

// EndpointAddr is the USB endpoint address (direction in bit 7).
func (p Packet) EndpointAddr() int {
	if p.In {
		return p.Endpoint | 0x80
	}
	return p.Endpoint
}

// Dir is "in" or "out".
func (p Packet) Dir() string {
	if p.In {
		return "in"
	}
	return "out"
}

// Encode returns the usbmon mmapped layout (header + captured data).
func (p Packet) Encode() []byte {
	b := make([]byte, HeaderLen+len(p.Data))
	le := binary.LittleEndian
	le.PutUint64(b[0:], p.URBID)
	b[8] = p.Event
	b[9] = byte(p.Transfer)
	b[10] = byte(p.EndpointAddr())
	b[11] = byte(p.Device)
	le.PutUint16(b[12:], uint16(p.Bus))
	if p.HasSetup {
		b[14] = 0
		copy(b[40:48], p.Setup[:])
	} else {
		b[14] = '-'
	}
	if len(p.Data) > 0 {
		b[15] = 0
	} else if p.In {
		b[15] = '<'
	} else {
		b[15] = '>'
	}
	le.PutUint64(b[16:], uint64(p.TimestampNs/1e9))
	le.PutUint32(b[24:], uint32((p.TimestampNs%1e9)/1000))
	le.PutUint32(b[28:], uint32(p.Status))
	le.PutUint32(b[32:], p.Length)
	le.PutUint32(b[36:], uint32(len(p.Data)))
	copy(b[HeaderLen:], p.Data)
	return b
}

// Decode parses the usbmon mmapped layout. The timestamp has microsecond
// resolution; callers that know a better one overwrite TimestampNs.
func Decode(b []byte) (Packet, error) {
	if len(b) < HeaderLen {
		return Packet{}, errors.New("usb packet shorter than the 64-byte usbmon header")
	}
	le := binary.LittleEndian
	p := Packet{
		URBID:    le.Uint64(b[0:]),
		Event:    b[8],
		Transfer: Transfer(b[9]),
		In:       b[10]&0x80 != 0,
		Endpoint: int(b[10] & 0x7f),
		Device:   int(b[11]),
		Bus:      int(le.Uint16(b[12:])),
		Status:   int32(le.Uint32(b[28:])),
		Length:   le.Uint32(b[32:]),
	}
	switch p.Event {
	case Submit, Complete, Error:
	default:
		return Packet{}, fmt.Errorf("usb packet: unknown event type 0x%02x", p.Event)
	}
	if p.Transfer > Bulk {
		return Packet{}, fmt.Errorf("usb packet: unknown transfer type %d", p.Transfer)
	}
	if b[14] == 0 {
		p.HasSetup = true
		copy(p.Setup[:], b[40:48])
	}
	p.TimestampNs = int64(le.Uint64(b[16:]))*1e9 + int64(le.Uint32(b[24:]))*1000
	n := int(le.Uint32(b[36:]))
	if n > len(b)-HeaderLen {
		n = len(b) - HeaderLen
	}
	if n > 0 {
		p.Data = append([]byte(nil), b[HeaderLen:HeaderLen+n]...)
	}
	return p, nil
}

// StatusName names a URB status (a negative errno, 0 = success).
func StatusName(s int32) string {
	switch s {
	case 0:
		return "ok"
	case -2:
		return "ENOENT (unlinked)"
	case -19:
		return "ENODEV (disconnected)"
	case -32:
		return "EPIPE (stall)"
	case -62:
		return "ETIME"
	case -71:
		return "EPROTO (bus error)"
	case -75:
		return "EOVERFLOW (babble)"
	case -84:
		return "EILSEQ (CRC / timeout)"
	case -104:
		return "ECONNRESET (unlinked)"
	case -108:
		return "ESHUTDOWN"
	case -110:
		return "ETIMEDOUT"
	case -115:
		return "EINPROGRESS"
	case -121:
		return "EREMOTEIO (short)"
	}
	return "errno " + strconv.Itoa(int(-s))
}

// Summary is a one-line description.
func (p Packet) Summary() string {
	dir := "Out"
	if p.In {
		dir = "In"
	}
	s := fmt.Sprintf("%c %s %s %s:%03d ep%d", p.Event, p.Transfer, dir, strconv.Itoa(p.Bus), p.Device, p.Endpoint)
	if p.HasSetup {
		s += " " + DescribeSetup(ParseSetup(p.Setup), nil)
	}
	if p.Event != Submit && p.Status != 0 {
		s += " status=" + StatusName(p.Status)
	}
	s += fmt.Sprintf(" len=%d", p.Length)
	if n := len(p.Data); n > 0 {
		s += " " + previewHex(p.Data, 8)
	}
	return s
}

// Attributes are the flat, queryable fields of a packet event.
func (p Packet) Attributes() map[string]string {
	m := map[string]string{
		"bus": strconv.Itoa(p.Bus), "dev": strconv.Itoa(p.Device), "device": p.DeviceKey(),
		"ep": strconv.Itoa(p.Endpoint), "dir": p.Dir(), "transfer": p.Transfer.String(),
		"event": string(rune(p.Event)), "status": strconv.Itoa(int(p.Status)), "len": strconv.Itoa(int(p.Length)),
		"urb": strconv.FormatUint(p.URBID, 16),
	}
	if p.HasSetup {
		m["setup"] = fmt.Sprintf("%x", p.Setup)
	}
	return m
}

func previewHex(b []byte, max int) string {
	if len(b) <= max {
		return fmt.Sprintf("%x", b)
	}
	return fmt.Sprintf("%x…", b[:max])
}
