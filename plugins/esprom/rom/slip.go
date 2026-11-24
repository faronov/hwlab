package rom

// SLIP framing (RFC 1055) as used by the ESP serial protocol: every packet
// starts and ends with 0xC0; 0xC0 and 0xDB inside are sent as 0xDB 0xDC and
// 0xDB 0xDD.
const (
	slipEnd    = 0xC0
	slipEsc    = 0xDB
	slipEscEnd = 0xDC
	slipEscEsc = 0xDD
)

// SlipEncode returns p as one SLIP frame.
func SlipEncode(p []byte) []byte {
	out := make([]byte, 0, len(p)+len(p)/32+2)
	out = append(out, slipEnd)
	for _, b := range p {
		switch b {
		case slipEnd:
			out = append(out, slipEsc, slipEscEnd)
		case slipEsc:
			out = append(out, slipEsc, slipEscEsc)
		default:
			out = append(out, b)
		}
	}
	return append(out, slipEnd)
}

// junkKeep bounds the bytes kept from outside frames (the ROM boot message).
const junkKeep = 512

// SlipDecoder turns a byte stream into frames. Bytes outside frames (the
// ROM's boot message, an application's console output, line noise) are
// counted and the last junkKeep of them kept for diagnostics. A frame with
// an invalid escape is dropped whole.
type SlipDecoder struct {
	in, esc, bad bool
	frame        []byte
	// Junk counts bytes received outside any frame.
	Junk int
	// Dropped counts frames discarded for an invalid escape sequence.
	Dropped int
	tail    []byte
}

// Feed consumes b and returns the frames it completed.
func (d *SlipDecoder) Feed(b []byte) [][]byte {
	var out [][]byte
	for _, c := range b {
		if c == slipEnd {
			if d.in && len(d.frame) > 0 {
				if d.bad {
					d.Dropped++
				} else {
					out = append(out, d.frame)
				}
			}
			d.in, d.esc, d.bad, d.frame = true, false, false, nil
			continue
		}
		if !d.in {
			d.Junk++
			d.tail = append(d.tail, c)
			if len(d.tail) > junkKeep {
				d.tail = d.tail[len(d.tail)-junkKeep:]
			}
			continue
		}
		if d.esc {
			d.esc = false
			switch c {
			case slipEscEnd:
				c = slipEnd
			case slipEscEsc:
				c = slipEsc
			default:
				d.bad = true
			}
			d.frame = append(d.frame, c)
			continue
		}
		if c == slipEsc {
			d.esc = true
			continue
		}
		d.frame = append(d.frame, c)
	}
	return out
}

// Reset forgets a partial frame (after the input was flushed).
func (d *SlipDecoder) Reset() { d.in, d.esc, d.bad, d.frame = false, false, false, nil }

// JunkText returns the kept bytes received outside frames and clears them.
func (d *SlipDecoder) JunkText() []byte {
	t := d.tail
	d.tail = nil
	return t
}
