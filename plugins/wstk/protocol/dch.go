package protocol

import (
	"encoding/binary"
	"fmt"
)

// Debug Channel (DCH) framing, as served on the kit's TCP port 4905: each
// frame is '[' u16 LE length, message of that length, ']'. A message starts
// with a u16 version:
//
//	v2: version, 6-byte timestamp in us, u16 type, u8 sequence, payload
//	v3: version, 8-byte timestamp in ns, u16 type, u32 flags, u16 sequence, payload
//
// Layout per Silicon Labs' public debug-channel documentation.

// DCH message types used by hwlab.
const (
	DCHTimeSync uint16 = 0x0000
	DCHEFRTX    uint16 = 0x0029 // EFR32 packet trace, transmitted frame
	DCHEFRRX    uint16 = 0x002A // EFR32 packet trace, received frame
	DCHEFROther uint16 = 0x002B
)

// DCHMessage is one decoded debug-channel message.
type DCHMessage struct {
	Version int
	// TimeNs is the kit timestamp in ns (v2 microseconds are scaled).
	TimeNs  uint64
	Type    uint16
	Flags   uint32
	Seq     uint16
	Payload []byte
}

// ParseDCHMessage decodes the bytes between '[' len and ']'.
func ParseDCHMessage(b []byte) (DCHMessage, error) {
	if len(b) < 2 {
		return DCHMessage{}, fmt.Errorf("%w: DCH message of %d bytes", ErrProtocol, len(b))
	}
	le := binary.LittleEndian
	switch v := le.Uint16(b); v {
	case 2:
		if len(b) < 11 {
			break
		}
		var ts [8]byte
		copy(ts[:], b[2:8])
		return DCHMessage{Version: 2, TimeNs: le.Uint64(ts[:]) * 1000, Type: le.Uint16(b[8:]), Seq: uint16(b[10]), Payload: b[11:]}, nil
	case 3:
		if len(b) < 18 {
			break
		}
		return DCHMessage{Version: 3, TimeNs: le.Uint64(b[2:]), Type: le.Uint16(b[10:]), Flags: le.Uint32(b[12:]), Seq: le.Uint16(b[16:]), Payload: b[18:]}, nil
	default:
		return DCHMessage{}, fmt.Errorf("%w: DCH version %d", ErrProtocol, v)
	}
	return DCHMessage{}, fmt.Errorf("%w: short DCH v%d message (%d bytes)", ErrProtocol, le.Uint16(b), len(b))
}

// DCHFramer cuts a DCH byte stream into messages, resynchronising on bad
// framing by skipping one byte.
type DCHFramer struct {
	buf []byte
	// Skipped counts bytes discarded while resynchronising; Bad counts
	// well-framed messages that failed to decode.
	Skipped, Bad int
}

// Feed appends data and returns the complete, decodable messages.
func (f *DCHFramer) Feed(data []byte) []DCHMessage {
	f.buf = append(f.buf, data...)
	var out []DCHMessage
	for len(f.buf) > 0 {
		if f.buf[0] != '[' {
			f.buf = f.buf[1:]
			f.Skipped++
			continue
		}
		if len(f.buf) < 3 {
			break
		}
		n := int(binary.LittleEndian.Uint16(f.buf[1:]))
		if len(f.buf) < 3+n+1 {
			break
		}
		if f.buf[3+n] != ']' {
			f.buf = f.buf[1:]
			f.Skipped++
			continue
		}
		m, err := ParseDCHMessage(append([]byte(nil), f.buf[3:3+n]...))
		f.buf = f.buf[3+n+1:]
		if err != nil {
			f.Bad++
			continue
		}
		out = append(out, m)
	}
	return out
}
