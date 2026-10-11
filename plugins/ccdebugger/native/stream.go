package native

import "github.com/faronov/hwlab/plugins/ccdebugger/native/target"

// stream builds a pipelined DEBUG_INSTR packet: several 8051 instructions in
// one USB write. The adapter framing was inferred from the single-instruction
// forms already validated on hardware: the first byte of each instruction
// carries 3n+1 (accumulator stored) or 3n+2 (not stored) in its high nibble
// and 0xF (flush stored bytes to the host) or 0xE (keep buffering) in the low
// nibble; the second byte is DEBUG_INSTR|n. This is an observed adapter
// behaviour, validated against the single-instruction path on a real device
// before use (see docs/notes/phase15-ccdebugger-results.md).
type stream struct {
	b       []byte
	replies int
}

func (s *stream) ins(store, flush bool, ins ...byte) {
	n := len(ins)
	hi := byte(3*n + 2)
	if store {
		hi = byte(3*n + 1)
		s.replies++
	}
	lo := byte(0x0E)
	if flush {
		lo = 0x0F
	}
	s.b = append(s.b, hi<<4|lo, cmdDebugInstr|byte(n))
	s.b = append(s.b, ins...)
}

func (d *Debugger) pipeline(s *stream, dl *deadline) ([]byte, error) {
	if s.replies == 0 {
		return nil, d.writePacket(s.b, dl)
	}
	return d.exchange(s.b, s.replies, dl)
}

// setDPTR loads DPTR0 (DPS must be 0).
func (d *Debugger) setDPTR(addr uint16, dl *deadline) error {
	_, err := d.instruction([]byte{0x90, byte(addr >> 8), byte(addr)}, dl)
	return err
}

// xput writes data to consecutive XDATA bytes one verified-by-reply instruction
// at a time. Used for small control writes only.
func (d *Debugger) xput(addr uint16, data []byte, dl *deadline) error {
	if err := d.setDPTR(addr, dl); err != nil {
		return err
	}
	for i, v := range data {
		if _, err := d.instruction([]byte{0x74, v}, dl); err != nil {
			return err
		}
		if _, err := d.instruction([]byte{0xF0}, dl); err != nil {
			return err
		}
		if i+1 < len(data) {
			if _, err := d.instruction([]byte{0xA3}, dl); err != nil {
				return err
			}
		}
	}
	return nil
}

func (d *Debugger) xput1(addr uint16, v byte, dl *deadline) error {
	return d.xput(addr, []byte{v}, dl)
}

// xget reads n bytes of XDATA (or CODE when code is set) with pipelined
// instructions, 64 bytes per USB round trip.
func (d *Debugger) xget(addr uint16, n int, code bool, dl *deadline) ([]byte, error) {
	out := make([]byte, 0, n)
	for n > 0 {
		c := min(n, streamChunk)
		var s stream
		s.ins(false, false, 0x90, byte(addr>>8), byte(addr))
		for i := 0; i < c; i++ {
			if code {
				s.ins(false, false, 0xE4)
				s.ins(true, i == c-1, 0x93)
			} else {
				s.ins(true, i == c-1, 0xE0)
			}
			if i < c-1 {
				s.ins(false, false, 0xA3)
			}
		}
		b, err := d.pipeline(&s, dl)
		if err != nil {
			return nil, err
		}
		out = append(out, b...)
		addr += uint16(c)
		n -= c
	}
	return out, nil
}

func (d *Debugger) sfrPut(sfr, v byte, dl *deadline) error {
	_, err := d.instruction([]byte{0x75, sfr, v}, dl)
	return err
}

func (d *Debugger) sfrGet(sfr byte, dl *deadline) (byte, error) {
	return d.instruction([]byte{0xE5, sfr}, dl)
}

// burstWrite sends 1..2048 bytes through the debug interface's burst write,
// which lands in DBGDATA and triggers the DBG_BW DMA trigger once per byte.
func (d *Debugger) burstWrite(data []byte, dl *deadline) error {
	n := len(data)
	if n < 1 || n > 2048 {
		return invalidf("burst write must carry 1..2048 bytes")
	}
	pkt := make([]byte, 0, 3+n)
	pkt = append(pkt, burstPrefix, 0x80|byte(n>>8&7), byte(n))
	return d.writePacket(append(pkt, data...), dl)
}

// inBank maps physical flash bank `bank` for CODE reads (bank 0 needs no
// mapping), runs fn with the CODE window offset to use, then restores FMAP and
// verifies MEMCTR and FMAP are as they were.
func (d *Debugger) inBank(bank int, dl *deadline, fn func(window uint16) error) error {
	if bank == 0 {
		return fn(0)
	}
	memctr, err := d.sfrGet(sfrMEMCTR, dl)
	if err != nil {
		return err
	}
	if memctr&0x08 != 0 {
		return transportf("XMAP overlays banked CODE; refusing physical flash access")
	}
	fmap, err := d.sfrGet(sfrFMAP, dl)
	if err != nil {
		return err
	}
	sel := fmap&0xF8 | byte(bank)
	if err := d.sfrPut(sfrFMAP, sel, dl); err != nil {
		return err
	}
	if v, err := d.sfrGet(sfrFMAP, dl); err != nil {
		return err
	} else if v != sel {
		return transportf("physical CODE bank selection failed; do not resume")
	}
	if err := fn(0x8000); err != nil {
		return err
	}
	if v, err := d.sfrGet(sfrMEMCTR, dl); err != nil {
		return err
	} else if v != memctr {
		return transportf("MEMCTR changed during physical flash access; do not resume")
	}
	if err := d.sfrPut(sfrFMAP, fmap, dl); err != nil {
		return err
	}
	if v, err := d.sfrGet(sfrFMAP, dl); err != nil {
		return err
	} else if v != fmap {
		return transportf("physical CODE bank restoration failed; do not resume")
	}
	return nil
}

// flashRead reads n bytes of physical flash inside one bank.
func (d *Debugger) flashRead(addr, n int, dl *deadline) ([]byte, error) {
	bank, off := target.Bank(addr)
	if off+n > target.BankSize {
		return nil, invalidf("physical flash read crosses a bank boundary")
	}
	var out []byte
	err := d.inBank(bank, dl, func(window uint16) (e error) {
		out, e = d.xget(window+uint16(off), n, true, dl)
		return e
	})
	return out, err
}
