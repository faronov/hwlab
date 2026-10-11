package modbus

import (
	"encoding/binary"
	"fmt"
	"time"
)

// EncodeRTU frames a PDU: unit, PDU, CRC16 (low byte first).
func EncodeRTU(unit byte, pdu []byte) []byte {
	b := make([]byte, 0, len(pdu)+3)
	b = append(append(b, unit), pdu...)
	return binary.LittleEndian.AppendUint16(b, CRC16(b))
}

// DecodeRTU checks and splits one RTU frame.
func DecodeRTU(frame []byte) (unit byte, pdu []byte, err error) {
	if len(frame) < 4 {
		return 0, nil, ErrShort
	}
	body := frame[:len(frame)-2]
	if CRC16(body) != binary.LittleEndian.Uint16(frame[len(frame)-2:]) {
		return 0, nil, ErrCRC
	}
	return body[0], body[1:], nil
}

// RTUSilence is the minimum idle time between RTU frames: 3.5 character times
// (11 bits each) at or below 19200 baud and the fixed 1.75 ms above.
func RTUSilence(baud int) time.Duration {
	if baud <= 0 || baud > 19200 {
		return 1750 * time.Microsecond
	}
	return time.Duration(3.5 * 11 / float64(baud) * float64(time.Second))
}

// Role says what an RTU byte stream carries when it is being cut into frames.
type Role int

const (
	// RoleAny accepts both requests and responses (a sniffer on a shared bus).
	RoleAny Role = iota
	RoleRequest
	RoleResponse
)

type lenState int

const (
	lenKnown lenState = iota
	lenNeedMore
	lenUnknown
)

// pduLen is the length of the PDU that starts at b (b[0] is the function
// code) for the given role, if it can be told from the bytes seen so far.
func pduLen(role Role, b []byte) (int, lenState) {
	if len(b) == 0 {
		return 0, lenNeedMore
	}
	fc := b[0]
	if role == RoleResponse && fc&0x80 != 0 {
		return 2, lenKnown
	}
	switch role {
	case RoleRequest:
		switch fc {
		case FCReadCoils, FCReadDiscreteInputs, FCReadHoldingRegisters, FCReadInputRegisters, FCWriteSingleCoil, FCWriteSingleRegister:
			return 5, lenKnown
		case FCWriteMultipleCoils, FCWriteMultipleRegisters:
			if len(b) < 6 {
				return 0, lenNeedMore
			}
			return 6 + int(b[5]), lenKnown
		case FCMaskWriteRegister:
			return 7, lenKnown
		case FCReadWriteRegisters:
			if len(b) < 10 {
				return 0, lenNeedMore
			}
			return 10 + int(b[9]), lenKnown
		}
	case RoleResponse:
		switch fc {
		case FCReadCoils, FCReadDiscreteInputs, FCReadHoldingRegisters, FCReadInputRegisters, FCReadWriteRegisters:
			if len(b) < 2 {
				return 0, lenNeedMore
			}
			return 2 + int(b[1]), lenKnown
		case FCWriteSingleCoil, FCWriteSingleRegister, FCWriteMultipleCoils, FCWriteMultipleRegisters:
			return 5, lenKnown
		case FCMaskWriteRegister:
			return 7, lenKnown
		}
	}
	return 0, lenUnknown
}

// RTUFrame is one frame cut out of a byte stream.
type RTUFrame struct {
	Offset int // index of the first byte in the input
	Raw    []byte
	Unit   byte
	PDU    []byte
	// Role is the role the frame was recognised as (never RoleAny).
	Role   Role
	BadCRC bool
}

// unmodelled reports whether no role knows the length of function code fc.
func unmodelled(fc byte, roles []Role) bool {
	for _, r := range roles {
		if _, st := pduLen(r, []byte{fc, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}); st != lenUnknown {
			return false
		}
	}
	return true
}

// boundaryOK says whether b can follow a frame whose CRC failed: it is empty
// or starts a frame with a good CRC. Without this check a stray byte could make
// the extractor swallow the start of the next good frame as one long "bad CRC"
// frame. The price is that a bad frame followed by trailing noise is reported
// as junk instead.
func boundaryOK(b []byte, roles []Role) bool {
	if len(b) == 0 {
		return true
	}
	for _, r := range roles {
		n, st := pduLen(r, b[1:])
		if st != lenKnown || n < 1 || n > MaxPDU {
			continue
		}
		total := 1 + n + 2
		if total <= len(b) && CRC16(b[:total-2]) == binary.LittleEndian.Uint16(b[total-2:total]) {
			return true
		}
	}
	return false
}

// ExtractRTU cuts complete frames out of buf, trying the roles in role. A
// frame is recognised by the length its function code implies and a valid CRC;
// bytes that fit no frame are skipped and counted as junk. A frame whose
// length fits but whose CRC fails is returned with BadCRC and consumed. With
// final false an incomplete frame at the end is left in rest for more data;
// with final true it counts as junk.
func ExtractRTU(buf []byte, role Role, final bool) (frames []RTUFrame, rest []byte, junk int) {
	roles := []Role{role}
	if role == RoleAny {
		roles = []Role{RoleRequest, RoleResponse}
	}
	pos := 0
	for pos < len(buf) {
		if len(buf)-pos < 4 {
			break
		}
		type cand struct {
			total int
			role  Role
		}
		var good *cand
		var bad *cand
		needMore := false
		for _, r := range roles {
			n, st := pduLen(r, buf[pos+1:])
			switch st {
			case lenNeedMore:
				needMore = true
			case lenKnown:
				if n < 1 || n > MaxPDU {
					continue
				}
				total := 1 + n + 2
				if pos+total > len(buf) {
					needMore = true
					continue
				}
				if CRC16(buf[pos:pos+total-2]) == binary.LittleEndian.Uint16(buf[pos+total-2:pos+total]) {
					if good == nil {
						good = &cand{total, r}
					}
				} else if bad == nil {
					bad = &cand{total, r}
				}
			}
		}
		switch {
		case good != nil:
			raw := buf[pos : pos+good.total]
			frames = append(frames, RTUFrame{Offset: pos, Raw: raw, Unit: raw[0], PDU: raw[1 : len(raw)-2], Role: good.role})
			pos += good.total
		case needMore && !final:
			return frames, buf[pos:], junk
		case bad != nil && !needMore && boundaryOK(buf[pos+bad.total:], roles):
			raw := buf[pos : pos+bad.total]
			frames = append(frames, RTUFrame{Offset: pos, Raw: raw, Unit: raw[0], PDU: raw[1 : len(raw)-2], Role: bad.role, BadCRC: true})
			pos += bad.total
		default:
			// An unmodelled function: the rest of the buffer is one frame only if
			// its CRC checks out; otherwise resynchronise one byte on.
			if unmodelled(buf[pos+1], roles) && len(buf)-pos >= 4 && CRC16(buf[pos:len(buf)-2]) == binary.LittleEndian.Uint16(buf[len(buf)-2:]) {
				raw := buf[pos:]
				r := role
				if r == RoleAny {
					r = RoleRequest
				}
				frames = append(frames, RTUFrame{Offset: pos, Raw: raw, Unit: raw[0], PDU: raw[1 : len(raw)-2], Role: r})
				pos = len(buf)
				continue
			}
			junk++
			pos++
		}
	}
	if pos < len(buf) {
		if final {
			junk += len(buf) - pos
			return frames, nil, junk
		}
		rest = buf[pos:]
	}
	return frames, rest, junk
}

// MBAP is the Modbus TCP application header.
const mbapLen = 7

// EncodeTCP frames a PDU with an MBAP header (protocol id 0).
func EncodeTCP(tid uint16, unit byte, pdu []byte) []byte {
	b := make([]byte, 0, mbapLen+len(pdu))
	b = binary.BigEndian.AppendUint16(b, tid)
	b = binary.BigEndian.AppendUint16(b, 0)
	b = binary.BigEndian.AppendUint16(b, uint16(len(pdu)+1))
	return append(append(b, unit), pdu...)
}

// DecodeTCP splits one MBAP frame.
func DecodeTCP(frame []byte) (tid uint16, unit byte, pdu []byte, err error) {
	if len(frame) < mbapLen+1 {
		return 0, 0, nil, ErrShort
	}
	if be16(frame[2:]) != 0 {
		return 0, 0, nil, fmt.Errorf("%w: protocol id %d is not 0", ErrProtocol, be16(frame[2:]))
	}
	if int(be16(frame[4:])) != len(frame)-6 {
		return 0, 0, nil, fmt.Errorf("%w: MBAP length %d does not match the %d bytes that follow", ErrProtocol, be16(frame[4:]), len(frame)-6)
	}
	return be16(frame), frame[6], frame[7:], nil
}
