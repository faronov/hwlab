package native

import (
	"fmt"
	"sync"
	"syscall"
	"time"
)

// NewSimulator returns a Transport backed by a CC2530 debug-interface
// simulator: a halted, freshly reset target with deterministic flash. Intended
// for tests of this package and of code that embeds it; it implements only the
// adapter framing and 8051 instructions the debugger issues. It never touches
// hardware.
func NewSimulator() Transport { return newSim() }

// sim records all USB traffic so tests can assert on exactly what was sent.
type sim struct {
	mu sync.Mutex

	targetID uint16
	revision uint16

	halted    bool
	haltCause bool // HALT_STATUS
	oscStable bool
	locked    bool
	config    uint8
	pc        uint16
	bank      uint8 // value reported by GET_BM (FMAP & 7)

	a      uint8
	sfr    [256]uint8 // direct addresses 0x80..0xFF
	iram   [128]uint8
	xdata  [0x2000]uint8
	flash  []byte // physical CODE, 0x40000 bytes
	regs   [8]uint8
	dptr0  uint16
	pswBit uint8

	// Behavior switches.
	failBulkWriteAt int // 1-based bulk write index to fail, 0 = never
	shortReplyAt    int // 1-based bulk read index to shorten, 0 = never
	corruptSFR      bool

	// Flash controller, DMA and CRC model.
	fctl, faddr   uint16
	fctlBusyReads int // FCTL reads that still report BUSY
	fctlStuck     bool
	crc           uint16
	crcLo         uint8
	erasePolls    int // READ_STATUS polls that still report chip-erase busy
	chipID, chver uint8
	info0, info1  uint8

	// Flash-programming behaviour switches.
	flashBusyReads    int  // BUSY reads after each erase/write (default 2)
	flashBusyForever  bool // FCTL never leaves BUSY once a command was issued
	eraseBusyPolls    int  // chip-erase polls reporting busy (default 3)
	eraseNever        bool // chip erase never finishes
	eraseKeepsLock    bool // chip erase leaves the debug lock set
	eraseDisconnect   bool // USB device vanishes right after the chip-erase command
	corruptFlashWrite bool // flip a bit in every programmed word
	badCRC            bool // CRC hardware returns a wrong value
	flashAbort        bool // every erase/write sets ABORT
	writeAbort        bool // every flash write (not erase) sets ABORT
	chipEraseSent     int
	writeCount        int
	eraseCount        int
	failErr           error // error returned by the failing bulk write

	// Traffic log.
	ctrlWrites []ctrlRecord
	ctrlReads  int
	bulkWrites [][]byte
	bulkReads  int
	closed     bool
	slowWrite  time.Duration
	pending    [][]byte
}

type ctrlRecord struct {
	reqType, req uint8
	value, index uint16
	data         []byte
}

func newSim() *sim {
	s := &sim{targetID: targetCC2530, revision: 0x0102, oscStable: true}
	s.flash = make([]byte, 0x40000)
	for i := range s.flash {
		s.flash[i] = byte(i*7 + i>>8)
	}
	s.chipID, s.chver, s.info0, s.info1 = 0xA5, 0x04, 0x44, 0x07 // CC2530F256, no USB, 8 KB RAM
	s.flashBusyReads, s.eraseBusyPolls = 2, 3
	for i := 0x3F800; i < 0x40000; i++ {
		s.flash[i] = 0xFF
	}
	copy(s.flash[0x3FFE8:], []byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88}) // secondary IEEE
	s.resetTarget()
	return s
}

func (s *sim) resetTarget() {
	s.halted, s.haltCause = true, false
	s.config = resetConfig
	s.pc = 0
	s.a = 0x5A
	s.sfr = [256]uint8{}
	s.sfr[0x81-0x80] = 0x07 // SP
	s.sfr[0xD0-0x80] = 0x00 // PSW
	s.sfr[0x9F-0x80] = 0x01 // FMAP
	s.sfr[0xC7-0x80] = 0x01 // MEMCTR
	s.regs = [8]uint8{1, 2, 3, 4, 5, 6, 7, 8}
	s.dptr0 = 0x1234
	s.bank = 1
	s.fctl, s.faddr, s.fctlBusyReads, s.erasePolls = 0, 0, 0, 0
}

func (s *sim) status() uint8 {
	var v uint8
	if s.oscStable {
		v |= 0x02
	}
	if s.locked {
		v |= 0x04
	}
	if s.erasePolls > 0 || s.eraseNever && s.chipEraseSent > 0 {
		v |= 0x80
	}
	if s.halted {
		v |= 0x20
		if s.haltCause {
			v |= 0x08
		}
	}
	return v
}

func (s *sim) Revision() (uint16, error) { return s.revision, nil }

func (s *sim) Close() error { s.closed = true; return nil }

func (s *sim) ControlRead(rt, req uint8, value, index uint16, length int, _ time.Duration) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ctrlReads++
	if rt != 0xC0 || req != 0xC0 {
		return nil, fmt.Errorf("unexpected control read %02x/%02x", rt, req)
	}
	return []byte{byte(s.targetID), byte(s.targetID >> 8), 0x05, 0x00, 0x09, 0x00, 0, 0}[:length], nil
}

func (s *sim) ControlWrite(rt, req uint8, value, index uint16, data []byte, _ time.Duration) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ctrlWrites = append(s.ctrlWrites, ctrlRecord{rt, req, value, index, append([]byte(nil), data...)})
	if rt == 0x40 && req == 0xC9 && value == 0 && index == 1 && len(data) == 0 {
		s.resetTarget()
	}
	return len(data), nil
}

func (s *sim) BulkWrite(ep uint8, data []byte, _ time.Duration) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bulkWrites = append(s.bulkWrites, append([]byte(nil), data...))
	if s.slowWrite > 0 {
		time.Sleep(s.slowWrite)
	}
	if s.failBulkWriteAt == len(s.bulkWrites) {
		if s.failErr != nil {
			return 0, s.failErr
		}
		return 0, fmt.Errorf("simulated USB failure")
	}
	if ep != EndpointOut {
		return 0, fmt.Errorf("bulk write on 0x%02x", ep)
	}
	reply, err := s.command(data)
	if err != nil {
		return 0, err
	}
	if reply != nil {
		s.pending = append(s.pending, reply)
	}
	return len(data), nil
}

func (s *sim) BulkRead(ep uint8, length int, _ time.Duration) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bulkReads++
	if ep != EndpointIn || len(s.pending) == 0 {
		return nil, fmt.Errorf("simulated read timeout")
	}
	r := s.pending[0]
	s.pending = s.pending[1:]
	if len(r) != length {
		return nil, fmt.Errorf("simulator reply is %d bytes, host asked for %d", len(r), length)
	}
	if s.shortReplyAt == s.bulkReads {
		return r[:len(r)-1], nil
	}
	return r, nil
}

func (s *sim) command(p []byte) ([]byte, error) {
	switch {
	case len(p) >= 4 && p[0] == burstPrefix:
		return nil, s.burst(p)
	case len(p) == 2 && p[0] == 0x1C && p[1] == cmdChipErase:
		s.chipEraseSent++
		s.erasePolls = s.eraseBusyPolls
		if s.eraseDisconnect {
			return nil, syscall.ENODEV
		}
		return nil, nil
	case len(p) == 2 && p[0] == 0x3F && p[1] == cmdGetChipID:
		return []byte{s.chipID, s.chver}, nil
	case len(p) == 2 && p[0] == 0x1F:
		return s.single(p[1])
	case len(p) == 2 && p[0] == 0x3F && p[1] == cmdGetPC:
		return []byte{byte(s.pc >> 8), byte(s.pc)}, nil
	case len(p) == 5 && p[0] == 0xAF && p[1] == cmdSetHWBreak:
		return []byte{s.status()}, nil
	case len(p) == 3 && p[0] == 0x4C && p[1] == cmdWriteConfig:
		s.config = p[2]
		return nil, nil
	}
	if r, ok := s.pipeline(p); ok {
		return r, nil
	}
	return nil, fmt.Errorf("simulator: unknown packet % x", p)
}

func (s *sim) single(cmd byte) ([]byte, error) {
	switch cmd {
	case cmdReadStatus:
		v := s.status()
		if s.erasePolls > 0 {
			s.erasePolls--
			if s.erasePolls == 0 && !s.eraseNever {
				s.finishChipErase()
			}
		}
		return []byte{v}, nil
	case cmdReadConfig:
		return []byte{s.config}, nil
	case cmdGetBank:
		return []byte{s.sfr[0x9F-0x80] & 7}, nil
	case cmdHalt:
		s.halted = true
		return []byte{s.status()}, nil
	case cmdResume:
		s.halted, s.haltCause = false, false
		s.pc = 0x0100
		return []byte{s.status()}, nil
	case cmdStepInstr:
		s.pc++
		return []byte{s.a}, nil
	}
	return nil, fmt.Errorf("simulator: unknown command 0x%02x", cmd)
}

func (s *sim) dirRead(d uint8) uint8 {
	switch {
	case d == 0xE0:
		return s.a
	case d == 0x82:
		return uint8(s.dptr0)
	case d == 0x83:
		return uint8(s.dptr0 >> 8)
	case d == 0xBC:
		return uint8(s.crc)
	case d == 0xBD:
		return uint8(s.crc >> 8)
	case d >= 0x80:
		return s.sfr[d-0x80]
	}
	return s.iram[d]
}

func (s *sim) dirWrite(d, v uint8) {
	switch {
	case d == 0xE0:
		s.a = v
	case d == 0x82:
		s.dptr0 = s.dptr0&0xFF00 | uint16(v)
	case d == 0x83:
		s.dptr0 = s.dptr0&0x00FF | uint16(v)<<8
	case d == 0xBC:
		s.crc = uint16(s.crcLo)<<8 | uint16(v)
		s.crcLo = v
	case d == 0xD6:
		s.sfr[0xD6-0x80] |= v & 0x1F
	case d == 0xD7:
		s.sfr[0xD7-0x80] = 0
		if v&1 != 0 && s.sfr[0xD6-0x80]&1 != 0 {
			s.runCRC()
		}
	case d >= 0x80:
		s.sfr[d-0x80] = v
		if d == 0xD0 {
			s.pswBit = v
		}
	default:
		s.iram[d] = v
	}
}

func (s *sim) codeByte(addr uint16) uint8 {
	if addr < 0x8000 {
		return s.flash[addr]
	}
	bank := int(s.sfr[0x9F-0x80] & 7)
	return s.flash[bank*0x8000+int(addr&0x7FFF)]
}

func (s *sim) exec(ins []byte) uint8 {
	switch op := ins[0]; {
	case op == 0x00:
	case op == 0xE5:
		s.a = s.dirRead(ins[1])
	case op >= 0xE8 && op <= 0xEF:
		s.a = s.regs[op-0xE8]
	case op == 0x74:
		s.a = ins[1]
	case op == 0x75:
		s.dirWrite(ins[1], ins[2])
	case op == 0x90:
		s.dptr0 = uint16(ins[1])<<8 | uint16(ins[2])
	case op == 0xE0:
		s.a = s.xread(s.dptr0)
	case op == 0xF0:
		s.xwrite(s.dptr0, s.a)
	case op == 0xA3:
		s.dptr0++
	case op == 0xE4:
		s.a = 0
	case op == 0x93:
		s.a = s.codeByte(s.dptr0 + uint16(s.a))
	default:
		panic(fmt.Sprintf("simulator: unsupported instruction % x", ins))
	}
	return s.a
}

func (s *sim) lockBits() []byte { return s.flash[0x3FFF0:0x40000] }

func (s *sim) pageLocked(page int) bool {
	if page >= 127 {
		return false // the lock-bit page itself is writable in debug mode
	}
	return s.lockBits()[page/8]>>(page%8)&1 == 0
}

func (s *sim) xread(a uint16) uint8 {
	switch {
	case a < 0x2000:
		return s.xdata[a]
	case a == 0x6249:
		return s.chver
	case a == 0x624A:
		return s.chipID
	case a == 0x6276:
		return s.info0
	case a == 0x6277:
		return s.info1
	case a == 0x6270:
		v := s.fctl
		if s.fctlStuck || s.fctlBusyReads > 0 {
			if s.fctlBusyReads > 0 {
				s.fctlBusyReads--
			}
			v |= 0x80
		}
		return uint8(v)
	case a == 0x6271:
		return uint8(s.faddr)
	case a == 0x6272:
		return uint8(s.faddr >> 8)
	case a >= 0x7080 && a < 0x7100:
		return s.dirRead(uint8(a - 0x7000))
	case a >= 0x7800 && a < 0x8000:
		return s.infoByte(int(a) - 0x7800)
	case a >= 0x8000:
		return s.flash[int(s.sfr[0xC7-0x80]&7)*0x8000+int(a&0x7FFF)]
	}
	return 0
}

func (s *sim) infoByte(off int) uint8 {
	if off >= 0x0C && off < 0x14 {
		return []byte{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF, 0x01, 0x02}[off-0x0C]
	}
	return uint8(off*3 + 1)
}

func (s *sim) xwrite(a uint16, v uint8) {
	switch {
	case a < 0x2000:
		s.xdata[a] = v
		if s.corruptSFR {
			s.xdata[a] ^= 1
		}
	case a == 0x6271:
		s.faddr = s.faddr&0xFF00 | uint16(v)
	case a == 0x6272:
		s.faddr = s.faddr&0x00FF | uint16(v)<<8
	case a == 0x6270:
		s.flashControl(v)
	case a >= 0x7080 && a < 0x7100:
		s.dirWrite(uint8(a-0x7000), v)
	}
}

func (s *sim) flashControl(v uint8) {
	s.fctl = 0
	s.fctlBusyReads = s.flashBusyReads
	s.fctlStuck = s.flashBusyForever
	byteAddr := int(s.faddr) * 4
	page := byteAddr / 2048
	switch {
	case v&1 != 0:
		s.eraseCount++
		if s.flashAbort || s.pageLocked(page) {
			s.fctl = 0x20
			return
		}
		for i := page * 2048; i < page*2048+2048; i++ {
			s.flash[i] = 0xFF
		}
	case v&2 != 0:
		s.writeCount++
		desc := s.descriptor(1, 2)
		if s.sfr[0xD6-0x80]&4 == 0 || desc.trig != 18 || s.flashAbort || s.writeAbort || s.pageLocked(page) {
			s.fctl = 0x20
			return
		}
		for i := 0; i < desc.length; i++ {
			b := s.xread(desc.src + uint16(i))
			if s.corruptFlashWrite {
				b ^= 0x01
			}
			if byteAddr+i < len(s.flash) {
				s.flash[byteAddr+i] &= b
			}
		}
		s.sfr[0xD6-0x80] &^= 4
	}
}

type simDesc struct {
	src, dest uint16
	length    int
	trig      byte
}

func (s *sim) descriptor(base uint8, ch int) simDesc {
	var ptr uint16
	switch base {
	case 1:
		ptr = uint16(s.sfr[0xD3-0x80])<<8 | uint16(s.sfr[0xD2-0x80]) + uint16(ch-1)*8
	default:
		ptr = uint16(s.sfr[0xD5-0x80])<<8 | uint16(s.sfr[0xD4-0x80])
	}
	r := func(o int) uint8 { return s.xdata[(int(ptr)+o)&0x1FFF] }
	return simDesc{
		src:    uint16(r(0))<<8 | uint16(r(1)),
		dest:   uint16(r(2))<<8 | uint16(r(3)),
		length: int(r(4)&0x1F)<<8 | int(r(5)),
		trig:   r(6) & 0x1F,
	}
}

func (s *sim) burst(p []byte) error {
	n := int(p[1]&7)<<8 | int(p[2])
	if p[1]&0xF8 != 0x80 || len(p) != 3+n {
		return fmt.Errorf("simulator: malformed burst write % x", p[:3])
	}
	desc := s.descriptor(1, 1)
	if s.sfr[0xD6-0x80]&2 == 0 || desc.trig != 31 {
		return nil // not armed: the debug interface drops the data
	}
	for i := 0; i < n && i < desc.length; i++ {
		s.xwrite(desc.dest+uint16(i), p[3+i])
	}
	if n >= desc.length {
		s.sfr[0xD6-0x80] &^= 2
	}
	return nil
}

func (s *sim) runCRC() {
	d := s.descriptor(0, 0)
	crc := s.crc
	for i := 0; i < d.length; i++ {
		crc ^= uint16(s.xread(d.src+uint16(i))) << 8
		for b := 0; b < 8; b++ {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x8005
			} else {
				crc <<= 1
			}
		}
	}
	if s.badCRC {
		crc ^= 0x0100
	}
	s.crc = crc
	s.sfr[0xD6-0x80] &^= 1
}

func (s *sim) finishChipErase() {
	for i := range s.flash {
		s.flash[i] = 0xFF
	}
	if !s.eraseKeepsLock {
		s.locked = false
	}
}

// pipeline parses a DEBUG_INSTR packet holding several instructions. An
// instruction whose high nibble is 3n+1 stores its accumulator reply; 3n+2
// does not; a low nibble of 0xF flushes the stored replies to the host.
func (s *sim) pipeline(p []byte) ([]byte, bool) {
	var stored []byte
	var flushed bool
	for i := 0; i < len(p); {
		if len(p)-i < 3 || p[i+1]&0xFC != cmdDebugInstr {
			return nil, false
		}
		n := int(p[i+1] & 3)
		hi, lo := p[i]>>4, p[i]&0x0F
		if n < 1 || n > 3 || i+2+n > len(p) || (hi != byte(3*n+1) && hi != byte(3*n+2)) || (lo != 0xE && lo != 0xF) {
			return nil, false
		}
		a := s.exec(p[i+2 : i+2+n])
		if hi == byte(3*n+1) {
			stored = append(stored, a)
		}
		flushed = flushed || lo == 0xF
		i += 2 + n
	}
	if !flushed {
		return nil, true
	}
	return stored, true
}
