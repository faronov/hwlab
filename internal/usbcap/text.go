package usbcap

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// TextParser parses lines of the Linux usbmon text API
// (/sys/kernel/debug/usb/usbmon/Nu), for example
//
//	d5ea89a0 3575914555 S Ci:1:001:0 s a3 00 0000 0003 0004 4 <
//	d5ea89a0 3575914560 C Ci:1:001:0 0 4 = 01050000
//
// The kernel timestamp is a 32-bit microsecond counter that wraps every ~71
// minutes; the parser unwraps it. Packet.TimestampNs is that kernel-clock
// time, not session time: the caller anchors it. The text API keeps at most 32
// bytes of data per URB, so longer payloads arrive truncated (Length keeps the
// real size). Isochronous lines are parsed on a best-effort basis: the
// per-frame descriptors are skipped.
type TextParser struct {
	prev  uint32
	epoch uint64
	seen  bool
}

// Parse parses one line. A blank line returns ok=false with no error.
func (tp *TextParser) Parse(line string) (p Packet, ok bool, err error) {
	f := strings.Fields(line)
	if len(f) == 0 {
		return Packet{}, false, nil
	}
	if len(f) < 6 {
		return Packet{}, false, fmt.Errorf("usbmon line has %d fields, want at least 6: %q", len(f), line)
	}
	urb, err := strconv.ParseUint(f[0], 16, 64)
	if err != nil {
		return Packet{}, false, fmt.Errorf("usbmon URB tag %q: %w", f[0], err)
	}
	ts, err := strconv.ParseUint(f[1], 10, 64)
	if err != nil {
		return Packet{}, false, fmt.Errorf("usbmon timestamp %q: %w", f[1], err)
	}
	p.URBID = urb
	p.TimestampNs = tp.unwrap(uint32(ts)) * 1000
	if len(f[2]) != 1 || (f[2][0] != Submit && f[2][0] != Complete && f[2][0] != Error) {
		return Packet{}, false, fmt.Errorf("usbmon event %q: want S, C or E", f[2])
	}
	p.Event = f[2][0]

	addr := strings.Split(f[3], ":")
	if len(addr) != 4 || len(addr[0]) != 2 {
		return Packet{}, false, fmt.Errorf("usbmon address %q: want <type><dir>:bus:device:endpoint", f[3])
	}
	switch addr[0][0] {
	case 'Z':
		p.Transfer = Isochronous
	case 'I':
		p.Transfer = Interrupt
	case 'C':
		p.Transfer = Control
	case 'B':
		p.Transfer = Bulk
	default:
		return Packet{}, false, fmt.Errorf("usbmon address %q: unknown transfer type %q", f[3], addr[0][:1])
	}
	switch addr[0][1] {
	case 'i':
		p.In = true
	case 'o':
	default:
		return Packet{}, false, fmt.Errorf("usbmon address %q: direction %q is not i or o", f[3], addr[0][1:])
	}
	for i, dst := range []*int{&p.Bus, &p.Device, &p.Endpoint} {
		n, err := strconv.Atoi(addr[i+1])
		if err != nil || n < 0 {
			return Packet{}, false, fmt.Errorf("usbmon address %q: bad number %q", f[3], addr[i+1])
		}
		*dst = n
	}

	rest := f[4:]
	switch {
	case rest[0] == "s":
		if len(rest) < 6 {
			return Packet{}, false, fmt.Errorf("usbmon setup packet truncated: %q", line)
		}
		var vals [5]uint64
		for i := range vals {
			if vals[i], err = strconv.ParseUint(rest[1+i], 16, 16); err != nil {
				return Packet{}, false, fmt.Errorf("usbmon setup field %q: %w", rest[1+i], err)
			}
		}
		p.HasSetup = true
		p.Setup = [8]byte{byte(vals[0]), byte(vals[1]), byte(vals[2]), byte(vals[2] >> 8), byte(vals[3]), byte(vals[3] >> 8), byte(vals[4]), byte(vals[4] >> 8)}
		rest = rest[6:]
	case len(rest[0]) == 1 && (rest[0][0] < '0' || rest[0][0] > '9') && rest[0] != "-":
		// "<flag> __ __ ____ ____ ____": the kernel could not copy the setup packet.
		if len(rest) < 6 {
			return Packet{}, false, fmt.Errorf("usbmon line truncated: %q", line)
		}
		rest = rest[6:]
	default:
		st := strings.Split(rest[0], ":")
		n, err := strconv.ParseInt(st[0], 10, 32)
		if err != nil {
			return Packet{}, false, fmt.Errorf("usbmon status %q: %w", rest[0], err)
		}
		p.Status = int32(n)
		rest = rest[1:]
	}
	// Isochronous lines carry "#ndesc" and per-frame "index:status:offset:length".
	for len(rest) > 0 && (strings.HasPrefix(rest[0], "#") || (p.Transfer == Isochronous && strings.Contains(rest[0], ":"))) {
		rest = rest[1:]
	}
	if len(rest) == 0 {
		return Packet{}, false, fmt.Errorf("usbmon line has no length field: %q", line)
	}
	n, err := strconv.ParseUint(rest[0], 10, 32)
	if err != nil {
		return Packet{}, false, fmt.Errorf("usbmon length %q: %w", rest[0], err)
	}
	p.Length = uint32(n)
	if len(rest) >= 2 && rest[1] == "=" {
		raw, err := hex.DecodeString(strings.Join(rest[2:], ""))
		if err != nil {
			return Packet{}, false, fmt.Errorf("usbmon data: %w", err)
		}
		p.Data = raw
	}
	return p, true, nil
}

// unwrap extends the 32-bit microsecond counter, tolerating slight reordering.
func (tp *TextParser) unwrap(ts uint32) int64 {
	if tp.seen {
		switch {
		case ts < tp.prev && tp.prev-ts > 1<<31:
			tp.epoch++
		case ts > tp.prev && ts-tp.prev > 1<<31 && tp.epoch > 0:
			// A line from just before the wrap, arriving after one from just after.
			return int64((tp.epoch-1)<<32 | uint64(ts))
		}
	}
	tp.seen, tp.prev = true, ts
	return int64(tp.epoch<<32 | uint64(ts))
}

// FormatText renders a packet as a usbmon text line (up to 32 data bytes), the
// inverse of Parse. It exists so tests and the synthetic source can produce
// the exact on-disk format.
func FormatText(p Packet) string {
	var sb strings.Builder
	typ := [...]byte{'Z', 'I', 'C', 'B'}[p.Transfer]
	dir := byte('o')
	if p.In {
		dir = 'i'
	}
	fmt.Fprintf(&sb, "%08x %d %c %c%c:%d:%03d:%d", p.URBID, (p.TimestampNs/1000)&0xffffffff, p.Event, typ, dir, p.Bus, p.Device, p.Endpoint)
	if p.HasSetup {
		fmt.Fprintf(&sb, " s %02x %02x %04x %04x %04x", p.Setup[0], p.Setup[1], uint16(p.Setup[2])|uint16(p.Setup[3])<<8,
			uint16(p.Setup[4])|uint16(p.Setup[5])<<8, uint16(p.Setup[6])|uint16(p.Setup[7])<<8)
	} else {
		fmt.Fprintf(&sb, " %d", p.Status)
	}
	fmt.Fprintf(&sb, " %d", p.Length)
	if len(p.Data) == 0 {
		if p.In {
			sb.WriteString(" <")
		} else {
			sb.WriteString(" >")
		}
		return sb.String()
	}
	d := p.Data
	if len(d) > 32 {
		d = d[:32]
	}
	sb.WriteString(" =")
	for i := 0; i < len(d); i += 4 {
		end := i + 4
		if end > len(d) {
			end = len(d)
		}
		fmt.Fprintf(&sb, " %x", d[i:end])
	}
	return sb.String()
}
