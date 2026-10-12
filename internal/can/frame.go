// Package can is the vendor-independent CAN frame model of hwlab.
//
// Every CAN source (SocketCAN, a USB-CAN adapter, a replayed session) produces
// [Frame] values. On the plugin protocol a frame travels as a Packet event with
// protocol "can", link type 227 (LINKTYPE_CAN_SOCKETCAN) and the SocketCAN
// pcap layout in Packet.data, so it can be written to a pcap(ng) file and
// decoded by Wireshark unchanged. Analyzers (ISO-TP, UDS, CANopen, J1939)
// consume Frames, never a vendor structure.
package can

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Protocol is the Packet.protocol value of CAN events.
const Protocol = "can"

// LinkType is the pcap link type of the Packet.data layout (SocketCAN).
const LinkType uint32 = 227

// CAN identifier flags as they appear in the SocketCAN can_id word.
const (
	flagEFF uint32 = 0x80000000
	flagRTR uint32 = 0x40000000
	flagERR uint32 = 0x20000000
	flagInv uint32 = 0x20000000 // CAN_INV_FILTER shares the ERR bit in filters

	maskSFF uint32 = 0x7FF
	maskEFF uint32 = 0x1FFFFFFF
)

// CAN FD flags (second header byte).
const (
	fdBRS byte = 0x01
	fdESI byte = 0x02
	fdFDF byte = 0x04
)

// Maximum payload sizes.
const (
	MaxClassic = 8
	MaxFD      = 64
)

// Frame is one CAN or CAN-FD frame.
type Frame struct {
	// Interface is the bus/interface name, e.g. "can0".
	Interface string
	// ID is the 11-bit (standard) or 29-bit (extended) identifier, without flags.
	ID       uint32
	Extended bool
	// RTR marks a remote transmission request (classic CAN only). The payload
	// is empty and DLC carries the requested length.
	RTR bool
	// Error marks a controller/bus error frame; ID and Data hold the kernel's
	// error class and detail bytes.
	Error bool
	FD    bool
	// BRS and ESI are the CAN-FD bit-rate-switch and error-state-indicator flags.
	BRS, ESI bool
	// DLC is the requested length of an RTR frame; ignored otherwise.
	DLC  uint8
	Data []byte
	// Own is true for a frame this host transmitted (seen by the receiver as an echo).
	Own bool
	// TimestampNs is the session time; set by the producer, not part of Encode.
	TimestampNs int64
}

// validFDLen reports whether n is a legal CAN-FD payload length.
func validFDLen(n int) bool {
	switch {
	case n <= 8:
		return true
	case n <= 24:
		return n%4 == 0
	default:
		return n == 32 || n == 48 || n == 64
	}
}

// Validate checks identifier range and payload length rules.
func (f Frame) Validate() error {
	if f.Extended {
		if f.ID > maskEFF {
			return fmt.Errorf("extended identifier 0x%X exceeds 29 bits", f.ID)
		}
	} else if f.ID > maskSFF && !f.Error {
		return fmt.Errorf("standard identifier 0x%X exceeds 11 bits (set extended)", f.ID)
	}
	if f.FD {
		if f.RTR {
			return errors.New("CAN FD has no remote frames")
		}
		if len(f.Data) > MaxFD || !validFDLen(len(f.Data)) {
			return fmt.Errorf("CAN FD payload of %d bytes is not a valid length (0-8,12,16,20,24,32,48,64)", len(f.Data))
		}
		return nil
	}
	if f.BRS || f.ESI {
		return errors.New("BRS/ESI are CAN FD flags: set fd")
	}
	if f.RTR {
		if len(f.Data) != 0 || f.DLC > MaxClassic {
			return errors.New("a remote frame carries no data and a DLC of at most 8")
		}
		return nil
	}
	if len(f.Data) > MaxClassic {
		return fmt.Errorf("classic CAN payload of %d bytes exceeds 8 (use fd)", len(f.Data))
	}
	return nil
}

// Len is the payload length announced on the bus (DLC for remote frames).
func (f Frame) Len() int {
	if f.RTR {
		return int(f.DLC)
	}
	return len(f.Data)
}

// Encode returns the SocketCAN pcap layout: the identifier word with flags in
// network byte order, the length, the FD flags, two reserved bytes, then the
// payload. Classic frames are padded to the kernel's 16-byte can_frame.
func (f Frame) Encode() []byte {
	id := f.ID
	if f.Extended {
		id |= flagEFF
	}
	if f.RTR {
		id |= flagRTR
	}
	if f.Error {
		id |= flagERR
	}
	size := 8 + len(f.Data)
	if !f.FD && size < 16 {
		size = 16
	}
	b := make([]byte, size)
	binary.BigEndian.PutUint32(b, id)
	b[4] = byte(f.Len())
	if f.FD {
		b[5] = fdFDF
		if f.BRS {
			b[5] |= fdBRS
		}
		if f.ESI {
			b[5] |= fdESI
		}
	}
	copy(b[8:], f.Data)
	return b
}

// Decode parses the layout produced by [Frame.Encode]. Interface and
// TimestampNs are left empty.
func Decode(b []byte) (Frame, error) {
	if len(b) < 8 {
		return Frame{}, fmt.Errorf("CAN frame of %d bytes is shorter than its 8-byte header", len(b))
	}
	id := binary.BigEndian.Uint32(b)
	n := int(b[4])
	fl := b[5]
	f := Frame{
		Extended: id&flagEFF != 0,
		RTR:      id&flagRTR != 0,
		Error:    id&flagERR != 0,
		FD:       fl&fdFDF != 0 || n > MaxClassic,
		BRS:      fl&fdBRS != 0,
		ESI:      fl&fdESI != 0,
	}
	if f.Extended || f.Error {
		f.ID = id & maskEFF
	} else {
		f.ID = id & maskSFF
	}
	if f.RTR {
		f.DLC = uint8(n)
		return f, nil
	}
	if len(b) < 8+n {
		return Frame{}, fmt.Errorf("CAN frame announces %d payload bytes but carries %d", n, len(b)-8)
	}
	f.Data = append([]byte(nil), b[8:8+n]...)
	return f, nil
}

// Kind names the frame type: "can", "can-fd", "rtr" or "error".
func (f Frame) Kind() string {
	switch {
	case f.Error:
		return "error"
	case f.RTR:
		return "rtr"
	case f.FD:
		return "can-fd"
	}
	return "can"
}

// IDString renders the identifier as candump does: 3 hex digits for standard,
// 8 for extended.
func (f Frame) IDString() string {
	if f.Extended {
		return fmt.Sprintf("%08X", f.ID)
	}
	return fmt.Sprintf("%03X", f.ID)
}

// Dir is "tx" for frames this host sent, else "rx".
func (f Frame) Dir() string {
	if f.Own {
		return "tx"
	}
	return "rx"
}

// Summary is a one-line candump-style description.
func (f Frame) Summary() string {
	var data string
	switch {
	case f.RTR:
		data = "remote"
	default:
		parts := make([]string, len(f.Data))
		for i, b := range f.Data {
			parts[i] = fmt.Sprintf("%02X", b)
		}
		data = strings.Join(parts, " ")
	}
	id := f.IDString()
	if f.Error {
		id = fmt.Sprintf("error 0x%X", f.ID)
	}
	return strings.TrimRight(fmt.Sprintf("%s %s %s [%d] %s", f.Dir(), f.Interface, id, f.Len(), data), " ")
}

// Attributes are the Packet.attributes of a CAN event (copied to the timeline
// as attr.<key>). The frame bytes themselves are in Packet.data.
func (f Frame) Attributes() map[string]string {
	a := map[string]string{
		"interface": f.Interface,
		"dir":       f.Dir(),
		"kind":      f.Kind(),
		"id":        "0x" + f.IDString(),
		"extended":  strconv.FormatBool(f.Extended),
		"dlc":       strconv.Itoa(f.Len()),
		"payload":   hex.EncodeToString(f.Data),
	}
	if f.FD {
		a["brs"], a["esi"] = strconv.FormatBool(f.BRS), strconv.FormatBool(f.ESI)
	}
	return a
}

// FromPacket rebuilds a Frame from a CAN Packet event.
func FromPacket(data []byte, attrs map[string]string) (Frame, error) {
	f, err := Decode(data)
	if err != nil {
		return Frame{}, err
	}
	f.Interface = attrs["interface"]
	f.Own = attrs["dir"] == "tx"
	return f, nil
}

// Filter is a receive filter in candump notation: id:mask matches frames
// where (frame.id & mask) == (id & mask); id~mask matches the opposite.
type Filter struct {
	ID       uint32
	Mask     uint32
	Extended bool
	Invert   bool
}

// ParseFilter parses "123:7FF", "123~7FF" or an 8-digit extended form such as
// "12345678:1FFFFFFF". A bare "123" means an exact match of that identifier.
func ParseFilter(s string) (Filter, error) {
	s = strings.TrimSpace(s)
	var f Filter
	idStr, maskStr := s, ""
	if i := strings.IndexAny(s, ":~"); i >= 0 {
		idStr, maskStr = s[:i], s[i+1:]
		f.Invert = s[i] == '~'
	}
	if idStr == "" || len(idStr) > 8 {
		return Filter{}, fmt.Errorf("filter %q: identifier must be 1-8 hex digits", s)
	}
	id, err := strconv.ParseUint(idStr, 16, 32)
	if err != nil {
		return Filter{}, fmt.Errorf("filter %q: bad identifier: %w", s, err)
	}
	f.Extended = len(idStr) == 8
	f.ID = uint32(id)
	if f.Extended {
		f.ID &= maskEFF
	} else if f.ID > maskSFF {
		return Filter{}, fmt.Errorf("filter %q: standard identifier above 0x7FF needs 8 hex digits", s)
	}
	f.Mask = maskSFF
	if f.Extended {
		f.Mask = maskEFF
	}
	if maskStr != "" {
		m, err := strconv.ParseUint(maskStr, 16, 32)
		if err != nil || len(maskStr) > 8 {
			return Filter{}, fmt.Errorf("filter %q: bad mask", s)
		}
		f.Mask = uint32(m) & maskEFF
	}
	return f, nil
}

// Match applies the filter in software. The extended/standard format is part
// of the match, as in the kernel.
func (f Filter) Match(fr Frame) bool {
	if fr.Extended != f.Extended {
		return f.Invert
	}
	hit := fr.ID&f.Mask == f.ID&f.Mask
	return hit != f.Invert
}

// KernelPair returns the can_id and can_mask words of the kernel's can_filter.
func (f Filter) KernelPair() (id, mask uint32) {
	id, mask = f.ID, f.Mask|flagEFF
	if f.Extended {
		id |= flagEFF
	}
	if f.Invert {
		id |= flagInv
	}
	return id, mask
}

// MatchAny reports whether any filter matches, or true when there are none.
func MatchAny(filters []Filter, fr Frame) bool {
	if len(filters) == 0 {
		return true
	}
	for _, f := range filters {
		if f.Match(fr) {
			return true
		}
	}
	return false
}
