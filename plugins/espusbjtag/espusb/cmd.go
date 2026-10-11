package espusb

// Command nibbles. Two commands share a byte, high nibble first.
const (
	cmdRST   = 0x8 // | srst
	cmdFlush = 0xA
	cmdRep   = 0xC // + (r & 3)

	maxRun = 1024 // a command plus up to five REP nibbles
)

// clk is the CLK command: bit 2 capture TDO, bit 1 TMS, bit 0 TDI. One TCK
// pulse with TDI/TMS set first.
func clk(capture, tms, tdi bool) byte {
	var c byte
	if capture {
		c |= 4
	}
	if tms {
		c |= 2
	}
	if tdi {
		c |= 1
	}
	return c
}

// Encoder packs nibble commands into the bulk-OUT byte stream, run-length
// encoding repeated commands with REP nibbles. A REP nibble r repeats the
// previous command (r & 3) << (2n) more times, where n counts the REP nibbles
// since that command, so a run of N (1..1024) is the command followed by the
// base-4 digits of N-1, least significant first.
type Encoder struct {
	nibbles []byte
	prev    byte
	run     int
	has     bool
}

// Add appends one command.
func (e *Encoder) Add(c byte) {
	if c == cmdFlush {
		e.flushRun()
		e.nibbles = append(e.nibbles, c)
		return
	}
	if e.has && c == e.prev && e.run < maxRun {
		e.run++
		return
	}
	e.flushRun()
	e.prev, e.run, e.has = c, 1, true
}

func (e *Encoder) flushRun() {
	if !e.has {
		return
	}
	e.nibbles = append(e.nibbles, e.prev)
	for n := e.run - 1; n > 0; n >>= 2 {
		e.nibbles = append(e.nibbles, cmdRep+byte(n&3))
	}
	e.has, e.run = false, 0
}

// Bytes finishes the stream and returns the packed bytes. An odd trailing
// nibble is padded with FLUSH, which is harmless (a flush of an empty buffer
// is a no-op). The encoder is empty afterwards.
func (e *Encoder) Bytes() []byte {
	e.flushRun()
	if len(e.nibbles)%2 == 1 {
		e.nibbles = append(e.nibbles, cmdFlush)
	}
	out := make([]byte, len(e.nibbles)/2)
	for i := range out {
		out[i] = e.nibbles[2*i]<<4 | e.nibbles[2*i+1]
	}
	e.nibbles = e.nibbles[:0]
	return out
}

// Decode expands a byte stream back into commands (REP nibbles resolved). It
// is the reference the simulator and the tests use.
func Decode(b []byte) []byte {
	var out []byte
	var prev byte
	have := false
	shift := 0
	total := 0
	for _, by := range b {
		for _, n := range [2]byte{by >> 4, by & 0xF} {
			if n >= cmdRep {
				if !have {
					continue
				}
				extra := int(n&3) << shift
				shift += 2
				for i := 0; i < extra; i++ {
					out = append(out, prev)
				}
				total += extra
				continue
			}
			prev, have, shift = n, true, 0
			out = append(out, n)
		}
	}
	return out
}
