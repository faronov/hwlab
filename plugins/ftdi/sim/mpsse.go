package sim

// process runs every complete MPSSE command in d.pend.
func (d *Device) process() {
	for len(d.pend) > 0 {
		n := d.step()
		if n == 0 {
			return
		}
		d.pend = d.pend[n:]
	}
}

func (d *Device) bad(op byte) int {
	d.fifo = append(d.fifo, 0xFA, op)
	return 1
}

// sipeedShiftOK is the data-shifting subset the Sipeed firmware implements:
// write and write+read byte/bit commands and the TMS commands.
func sipeedShiftOK(op byte) bool {
	switch op {
	case 0x11, 0x15, 0x19, 0x1D, 0x31, 0x35, 0x39, 0x3D,
		0x13, 0x17, 0x1B, 0x1F, 0x33, 0x37, 0x3B, 0x3F,
		0x4B, 0x4F, 0x6B, 0x6F:
		return true
	}
	return false
}

// step decodes one command at the front of d.pend and returns how many bytes
// it consumed, or 0 if the command is still incomplete.
func (d *Device) step() int {
	p := d.pend
	op := p[0]
	need := func(n int) bool { return len(p) >= n }
	switch op {
	case 0x80, 0x82:
		if !need(3) {
			return 0
		}
		if !d.cfg.Sipeed {
			if op == 0x80 {
				d.gpioLow = [2]byte{p[1], p[2]}
				d.tmsPin = p[1]&0x08 != 0
			} else {
				d.gpioHigh = [2]byte{p[1], p[2]}
			}
		}
		return 3
	case 0x81, 0x83:
		switch {
		case d.cfg.Sipeed:
			d.fifo = append(d.fifo, op-0x80) // constant, not a pin read
		case op == 0x81:
			d.fifo = append(d.fifo, d.gpioLow[0])
		default:
			d.fifo = append(d.fifo, d.gpioHigh[0])
		}
		return 1
	case 0x84, 0x85:
		if !d.cfg.Sipeed {
			d.loopback = op == 0x84
		}
		return 1
	case 0x86:
		if !need(3) {
			return 0
		}
		if !d.cfg.Sipeed {
			d.divisor = uint16(p[1]) | uint16(p[2])<<8
		}
		return 3
	case 0x87, 0x88, 0x89:
		return 1
	case 0x8A, 0x8B, 0x8C, 0x8D, 0x96, 0x97:
		if !d.cfg.HighSpeed && !d.cfg.Sipeed {
			return d.bad(op)
		}
		if d.cfg.HighSpeed {
			switch op {
			case 0x8B:
				d.div5 = true
			case 0x8A:
				d.div5 = false
			}
		}
		return 1
	case 0x8E:
		if !d.cfg.HighSpeed {
			return d.bad(op)
		}
		if !need(2) {
			return 0
		}
		return 2
	case 0x8F:
		if !d.cfg.HighSpeed {
			return d.bad(op)
		}
		if !need(3) {
			return 0
		}
		return 3
	}
	if op >= 0x80 || op < 0x10 {
		return d.bad(op)
	}
	return d.shift(op)
}

// shift executes a data-shifting command (0x10..0x7F).
func (d *Device) shift(op byte) int {
	p := d.pend
	if d.cfg.Sipeed && !sipeedShiftOK(op) {
		return d.bad(op)
	}
	isTMS := op&0x40 != 0
	bitMode := op&0x02 != 0
	write := op&0x10 != 0
	read := op&0x20 != 0
	readNeg := op&0x04 != 0
	lsb := op&0x08 != 0
	if isTMS {
		if !bitMode {
			return d.bad(op)
		}
		if len(p) < 3 {
			return 0
		}
		n := int(p[1]&7) + 1
		data := p[2]
		var acc byte
		for i := 0; i < n; i++ {
			d.tmsPin = data>>i&1 == 1
			s := d.tick(data&0x80 != 0, readNeg)
			if read {
				acc = acc>>1 | b2b(s)<<7
			}
		}
		if read {
			d.fifo = append(d.fifo, acc)
		}
		return 3
	}
	if !write && !read {
		return d.bad(op)
	}
	hdr, count := 3, 0
	if len(p) < hdr && !bitMode {
		return 0
	}
	if bitMode {
		hdr = 2
		if len(p) < hdr {
			return 0
		}
		count = int(p[1]&7) + 1
	} else {
		count = (int(p[1]) | int(p[2])<<8) + 1
	}
	total := hdr
	if write {
		if bitMode {
			total++
		} else {
			total += count
		}
	}
	if len(p) < total {
		return 0
	}
	var data []byte
	if write {
		data = p[hdr:total]
	}
	if bitMode {
		var acc byte
		for i := 0; i < count; i++ {
			s := d.tick(writeBit(data, i, lsb), readNeg)
			if read {
				acc = shiftIn(acc, s, lsb)
			}
		}
		if read {
			d.fifo = append(d.fifo, acc)
		}
		return total
	}
	for by := 0; by < count; by++ {
		var acc byte
		for i := 0; i < 8; i++ {
			var tdi bool
			if write {
				tdi = writeBit(data[by:], i, lsb)
			}
			s := d.tick(tdi, readNeg)
			if read {
				acc = shiftIn(acc, s, lsb)
			}
		}
		if read {
			d.fifo = append(d.fifo, acc)
		}
	}
	return total
}

func writeBit(data []byte, i int, lsb bool) bool {
	if len(data) == 0 {
		return false
	}
	if lsb {
		return data[0]>>i&1 == 1
	}
	return data[0]>>(7-i)&1 == 1
}

func shiftIn(acc byte, s, lsb bool) byte {
	if lsb {
		return acc>>1 | b2b(s)<<7
	}
	return acc<<1 | b2b(s)
}

func b2b(b bool) byte {
	if b {
		return 1
	}
	return 0
}

// tick applies one clock with the current TMS pin and returns the TDO sample:
// before the rising edge by default, after it when readNeg is set. With
// loopback on, the sample is the TDI bit itself.
func (d *Device) tick(tdi, readNeg bool) bool {
	s := d.tap.tdo()
	if d.loopback {
		s = tdi
	}
	d.tap.clock(d.tmsPin, tdi)
	if readNeg && !d.loopback {
		s = d.tap.tdo()
	}
	return s
}
