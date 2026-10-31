package protocol

import (
	"context"
	"encoding/binary"
	"fmt"
	"time"
)

// Kit control commands on ChanKit. Only commands seen from Silicon Labs' own
// tools during read-only operations are used.
const (
	KitCmdInfo   byte = 0x07 // arg 1: firmware version, mainboard id and serial
	KitCmdBoards byte = 0x10 // arg 1: board records (mainboard, radio board, ...)
)

const kitHeaderLen = 10

// KitRequest builds a control request: 01 00000000 cmd arg len16 sum, payload.
func KitRequest(cmd, arg byte, payload []byte) []byte {
	b := make([]byte, kitHeaderLen, kitHeaderLen+len(payload))
	b[0] = 1
	b[5], b[6] = cmd, arg
	binary.LittleEndian.PutUint16(b[7:], uint16(len(payload)))
	b[9] = checksum(b[:9])
	return append(b, payload...)
}

func checksum(b []byte) byte {
	var s byte
	for _, c := range b {
		s += c
	}
	return s
}

// ParseKitHeader checks a 10-byte reply header and returns the status byte
// (1 = ok) and the body length.
func ParseKitHeader(h []byte) (status byte, n int, err error) {
	if len(h) != kitHeaderLen || h[0] != 1 {
		return 0, 0, fmt.Errorf("%w: kit reply header % x", ErrProtocol, h)
	}
	if checksum(h[:9]) != h[9] {
		return 0, 0, fmt.Errorf("%w: kit reply header checksum % x", ErrProtocol, h)
	}
	return h[5], int(binary.LittleEndian.Uint16(h[7:])), nil
}

// TLV is one tag/length/value field of a control reply body.
type TLV struct {
	Tag   byte
	Value []byte
}

// KitReply is a decoded control reply.
type KitReply struct {
	Status   byte
	Cmd, Arg byte // echoed request
	Fields   []TLV
}

// Field returns the value of tag t.
func (r KitReply) Field(t byte) ([]byte, bool) {
	for _, f := range r.Fields {
		if f.Tag == t {
			return f.Value, true
		}
	}
	return nil, false
}

// ParseKitBody decodes a reply body: the echoed command and argument, then
// fields of tag u8, length u16 and value.
func ParseKitBody(status byte, b []byte) (KitReply, error) {
	if len(b) < 2 {
		return KitReply{}, fmt.Errorf("%w: kit reply body % x", ErrProtocol, b)
	}
	r := KitReply{Status: status, Cmd: b[0], Arg: b[1]}
	for p := 2; p < len(b); {
		if p+3 > len(b) {
			return r, fmt.Errorf("%w: truncated field at %d of % x", ErrProtocol, p, b)
		}
		n := int(binary.LittleEndian.Uint16(b[p+1:]))
		if p+3+n > len(b) {
			return r, fmt.Errorf("%w: field %#x of %d bytes overruns the body", ErrProtocol, b[p], n)
		}
		r.Fields = append(r.Fields, TLV{Tag: b[p], Value: b[p+3 : p+3+n]})
		p += 3 + n
	}
	return r, nil
}

// KitCommand sends one control request and reads its reply.
func (e *Emucom) KitCommand(ctx context.Context, cmd, arg byte, payload []byte, timeout time.Duration) (KitReply, error) {
	if err := e.Write(ctx, ChanKit, KitRequest(cmd, arg, payload)); err != nil {
		return KitReply{}, err
	}
	h, err := e.ReadFull(ctx, ChanKit, kitHeaderLen, timeout)
	if err != nil {
		return KitReply{}, err
	}
	st, n, err := ParseKitHeader(h)
	if err != nil {
		return KitReply{}, err
	}
	body, err := e.ReadFull(ctx, ChanKit, n, timeout)
	if err != nil {
		return KitReply{}, err
	}
	r, err := ParseKitBody(st, body)
	if err != nil {
		return r, err
	}
	if r.Cmd != cmd || r.Arg != arg {
		return r, fmt.Errorf("%w: reply to %02x/%02x echoes %02x/%02x", ErrProtocol, cmd, arg, r.Cmd, r.Arg)
	}
	if st != 1 {
		return r, fmt.Errorf("%w: command %02x/%02x status %d", ErrProtocol, cmd, arg, st)
	}
	return r, nil
}

// KitInfo is the decoded reply of KitCmdInfo.
type KitInfo struct {
	Firmware        string // e.g. "1.4.6"
	Build           int    // e.g. 1171
	Mainboard       string // e.g. "BRD4001"
	MainboardSerial uint32
}

// ParseKitInfo decodes the info reply. Field 1 is the firmware version
// (major, minor nibbles, patch byte), 2 the build, 7 the BCD board number and
// 0x0b the board serial, as cross-checked against the admin console's
// "sys ver" and Simplicity Commander's adapter probe on one kit.
func ParseKitInfo(r KitReply) (KitInfo, error) {
	var k KitInfo
	v, ok := r.Field(0x01)
	if !ok || len(v) != 2 {
		return k, fmt.Errorf("%w: info reply without firmware version", ErrProtocol)
	}
	fw := binary.LittleEndian.Uint16(v)
	k.Firmware = fmt.Sprintf("%d.%d.%d", fw>>12, fw>>8&0xf, fw&0xff)
	if v, ok := r.Field(0x02); ok && len(v) == 2 {
		k.Build = int(binary.LittleEndian.Uint16(v))
	}
	if v, ok := r.Field(0x07); ok && len(v) == 2 {
		k.Mainboard = fmt.Sprintf("BRD%04x", binary.LittleEndian.Uint16(v))
	}
	if v, ok := r.Field(0x0b); ok && len(v) == 4 {
		k.MainboardSerial = binary.LittleEndian.Uint32(v)
	}
	return k, nil
}

// Board is one board record of the kit (mainboard, radio board, ...).
type Board struct {
	Part     string // e.g. "BRD4183A"
	Revision string // e.g. "A00"
	Serial   uint32
	Date     string // manufacturing date YYYY-MM-DD
	Name     string // from the static table, "" when unknown
}

// ID is the board id as the admin console prints it ("BRD4183A Rev. A00").
func (b Board) ID() string { return b.Part + " Rev. " + b.Revision }

const boardRecordLen = 20

// ParseBoards decodes the board list reply: field 0 is the count, field 1
// holds 20-byte records: u16 BCD board number, variant, three bytes of which
// the third is taken as the revision letter, revision number, 0xff, u32
// serial, u16 year, month, day, four 0xff bytes. Unused records are 0xff.
func ParseBoards(r KitReply) ([]Board, error) {
	c, ok := r.Field(0x00)
	recs, ok2 := r.Field(0x01)
	if !ok || !ok2 || len(c) != 1 {
		return nil, fmt.Errorf("%w: board list reply without count/records", ErrProtocol)
	}
	n := int(c[0])
	if n*boardRecordLen > len(recs) {
		return nil, fmt.Errorf("%w: %d boards announced, %d record bytes", ErrProtocol, n, len(recs))
	}
	out := make([]Board, 0, n)
	for i := 0; i < n; i++ {
		rec := recs[i*boardRecordLen : (i+1)*boardRecordLen]
		num := binary.LittleEndian.Uint16(rec)
		if num == 0xffff {
			continue
		}
		b := Board{
			Part:     fmt.Sprintf("BRD%04x%c", num, 'A'+rec[2]),
			Revision: fmt.Sprintf("%c%02d", 'A'+rec[5], rec[6]),
			Serial:   binary.LittleEndian.Uint32(rec[8:]),
			Date:     fmt.Sprintf("%04d-%02d-%02d", binary.LittleEndian.Uint16(rec[12:]), rec[14], rec[15]),
		}
		if k, ok := knownBoards[fmt.Sprintf("BRD%04x", num)]; ok {
			b.Name = k
		}
		out = append(out, b)
	}
	return out, nil
}

// knownBoards names the boards seen during validation. Unknown boards are
// reported by part number only.
var knownBoards = map[string]string{
	"BRD4001": "Wireless Starter Kit Mainboard",
	"BRD4183": "EFR32xG22 2.4 GHz 6 dBm Radio Board",
}
