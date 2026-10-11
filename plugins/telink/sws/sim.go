package sws

import (
	"encoding/binary"
	"io"
	"sync"
	"time"
)

// Sim is a SYNTHETIC SWire programmer with a TLSR8258-like target behind it.
// It is written from the same reading of the reference tool as the client, so
// it proves the client is self-consistent and exercises its error handling; it
// does not prove the protocol is right on hardware.
type Sim struct {
	mu sync.Mutex

	rx []byte // answers waiting to be read

	// Fault injection, counted in requests.
	DropNext     int  // ignore this many requests (the client sees a timeout)
	BadCRCNext   int  // corrupt the CRC of this many answers
	NoTarget     bool // activation and SWire reads fail with the SWire timeout error
	Asleep       bool // the target answers nothing over SWire (deep sleep)
	BusyPolls    int  // report flash busy for this many status reads after a write/erase
	StuckProtect bool // status-register writes have no effect (hardware write protect)

	// Observations for tests.
	Requests   int
	FlashOps   []FlashOp
	ResetLevel bool
	Activated  bool

	fw       uint16
	swdiv    byte
	addrlen  byte
	swbuf    [6]byte
	flash    []byte
	sr       byte
	wel      bool
	busy     int
	mem      map[uint32]byte
	ctl      byte
	pc       uint32
	regs     [32]uint32
	bkpt     uint32
	bkptSet  bool
	atBkpt   bool
	stepped  int
	rebooted int
}

// FlashOp records a state-changing flash command the simulated chip received.
type FlashOp struct {
	Kind string // "erase", "write", "status-write"
	Addr uint32
	Len  int
}

// SimFlashUID is the SYNTHETIC unique id of the simulated flash.
var SimFlashUID = [FlashUIDBytes]byte{0x53, 0x59, 0x4e, 0x54, 0x48, 0x45, 0x54, 0x49}

// SimFlashSize is the simulated flash (W25Q40-class, JEDEC EF 40 13).
const SimFlashSize = 0x80000

// NewSim returns a simulator with a blank-but-for-factory-data flash: the
// EUI-64 and config sectors hold recognisable non-FF bytes so tests can prove
// they stay untouched.
func NewSim() *Sim {
	s := &Sim{
		fw:      8,
		swdiv:   5,
		addrlen: 3,
		swbuf:   DefaultSWBuf,
		flash:   make([]byte, SimFlashSize),
		mem:     map[uint32]byte{},
		ctl:     cpuGo,
		pc:      0x1000,
	}
	for i := range s.flash {
		s.flash[i] = 0xff
	}
	copy(s.flash[0x76000:], []byte{0xde, 0xad, 0xbe, 0xef, 0x00, 0x11, 0x22, 0x33})
	copy(s.flash[0x77000:], []byte("factory-cfg+adc-cal"))
	copy(s.flash[0:], []byte{0x0b, 0x80, 0x00, 0x00, 0x04, 0x20, 0x01, 0x02})
	for i := range s.regs {
		s.regs[i] = 0x1000 * uint32(i)
	}
	s.regs[13] = 0x84fff0
	return s
}

// Flash exposes the simulated flash contents to tests.
func (s *Sim) Flash() []byte { s.mu.Lock(); defer s.mu.Unlock(); return s.flash }

// ProtectFlash sets block-protection status bits.
func (s *Sim) ProtectFlash() { s.mu.Lock(); s.sr = 0x1c; s.mu.Unlock() }

// Stats reports debug-visible state for tests.
func (s *Sim) Stats() (ctl byte, pc uint32, steps, reboots int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ctl, s.pc, s.stepped, s.rebooted
}

// Write implements Link: it receives one request frame.
func (s *Sim) Write(frame []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Requests++
	if s.DropNext > 0 {
		s.DropNext--
		return len(frame), nil
	}
	if !crcOK(frame) {
		s.rx = append(s.rx, s.answer(frame[0], ErrCRC, 0, nil)...)
		return len(frame), nil
	}
	s.rx = append(s.rx, s.handle(frame[:len(frame)-2])...)
	return len(frame), nil
}

// Read implements Link.
func (s *Sim) Read(b []byte) (int, error) {
	s.mu.Lock()
	if len(s.rx) == 0 {
		s.mu.Unlock()
		time.Sleep(time.Millisecond)
		return 0, nil
	}
	n := copy(b, s.rx)
	s.rx = s.rx[n:]
	s.mu.Unlock()
	return n, nil
}

// SetReadTimeout implements Link.
func (s *Sim) SetReadTimeout(time.Duration) error { return nil }

// ResetInput implements Link.
func (s *Sim) ResetInput() error { s.mu.Lock(); s.rx = nil; s.mu.Unlock(); return nil }

var _ Link = (*Sim)(nil)
var _ io.ReadWriter = (*Sim)(nil)

// answer builds [cmd, err, count lo, count hi, data..., crc].
func (s *Sim) answer(cmd, errCode byte, count uint16, data []byte) []byte {
	out := []byte{cmd, errCode, byte(count), byte(count >> 8)}
	out = append(out, data...)
	out = withCRC(out)
	if s.BadCRCNext > 0 {
		s.BadCRCNext--
		out[len(out)-1] ^= 0xff
	}
	return out
}

func (s *Sim) ok(cmd byte, count uint16, data []byte) []byte { return s.answer(cmd, 0, count, data) }

func (s *Sim) fail(cmd, code byte) []byte { return s.answer(cmd, code, 0, nil) }

func addrOf(r []byte) uint32 { return uint32(r[1]) | uint32(r[2])<<8 | uint32(r[3])<<16 }

func (s *Sim) targetUp() bool { return s.Activated && !s.NoTarget && !s.Asleep }

func (s *Sim) handle(r []byte) []byte {
	cmd := r[0]
	switch cmd {
	case cmdFuncs:
		if len(r) < 4 {
			return s.fail(cmd, ErrLen)
		}
		switch r[1] {
		case fnVersion:
			d := []byte{byte(s.fw), byte(s.fw >> 8), 0x62, 0x55, 1, s.swdiv, s.addrlen}
			d = append(d, s.swbuf[:]...)
			return s.ok(cmd, uint16(len(d)), d)
		case 2:
			if len(r) != 12 {
				return s.fail(cmd, ErrLen)
			}
			s.swdiv, s.addrlen = r[4], r[5]
			copy(s.swbuf[:], r[6:12])
			d := append([]byte{s.swdiv, s.addrlen}, s.swbuf[:]...)
			return s.ok(cmd, uint16(len(d)), d)
		case fnReset:
			s.ResetLevel = r[2] != 0
			if !s.ResetLevel {
				s.Activated = false
			}
			return s.ok(cmd, 1, []byte{r[2]})
		case fnActivate:
			if s.NoTarget {
				return s.fail(cmd, ErrRead)
			}
			s.Activated = true
			s.ctl = cpuGo
			var d [4]byte
			binary.LittleEndian.PutUint32(d[:], s.pc)
			return s.ok(cmd, 4, d[:])
		}
		return s.fail(cmd, ErrFunc)
	case cmdSWRead:
		if len(r) != 6 {
			return s.fail(cmd, ErrLen)
		}
		n := int(binary.LittleEndian.Uint16(r[4:6]))
		if !s.targetUp() {
			return s.fail(cmd, ErrRead)
		}
		a := addrOf(r)
		d := make([]byte, n)
		for i := range d {
			d[i] = s.readByte(a + uint32(i))
		}
		if a <= 0x6bc && a+uint32(n) > 0x6bc && !s.atBkpt && s.ctl == cpuGo {
			s.pc += 2
		}
		return s.ok(cmd, uint16(n), d)
	case cmdSWWrite:
		if len(r) < 5 {
			return s.fail(cmd, ErrLen)
		}
		if !s.targetUp() {
			return s.fail(cmd, ErrRead)
		}
		a := addrOf(r)
		for i, v := range r[4:] {
			s.writeByte(a+uint32(i), v)
		}
		return s.ok(cmd, uint16(len(r)-4), nil)
	case cmdFlashJEDEC:
		return s.ok(cmd, 3, []byte{0xef, 0x40, 0x13})
	case cmdFlashStat:
		sr := s.sr
		if s.busy > 0 {
			s.busy--
			sr |= 1
		}
		return s.ok(cmd, 1, []byte{sr})
	case cmdFlashRead:
		if len(r) != 6 {
			return s.fail(cmd, ErrLen)
		}
		a := addrOf(r)
		n := int(binary.LittleEndian.Uint16(r[4:6]))
		if int(a)+n > len(s.flash) {
			return s.fail(cmd, ErrLen)
		}
		return s.ok(cmd, uint16(n), append([]byte(nil), s.flash[a:int(a)+n]...))
	case cmdFlashWrite:
		if len(r) < 5 || len(r)-4 > FlashPage {
			return s.fail(cmd, ErrLen)
		}
		a := addrOf(r)
		data := r[4:]
		s.FlashOps = append(s.FlashOps, FlashOp{"write", a, len(data)})
		if s.sr&0x1c == 0 {
			page := a &^ (FlashPage - 1)
			for i, v := range data {
				p := page + (a-page+uint32(i))%FlashPage
				s.flash[p] &= v
			}
		}
		s.busy = s.BusyPolls
		return s.ok(cmd, uint16(len(data)), nil)
	case cmdSectErase:
		a := addrOf(r) &^ (FlashSector - 1)
		s.FlashOps = append(s.FlashOps, FlashOp{"erase", a, FlashSector})
		if s.sr&0x1c == 0 && int(a)+FlashSector <= len(s.flash) {
			for i := 0; i < FlashSector; i++ {
				s.flash[int(a)+i] = 0xff
			}
		}
		s.busy = s.BusyPolls
		return s.ok(cmd, 0, nil)
	case cmdFlashCRC:
		a := addrOf(r)
		n := int(binary.LittleEndian.Uint16(r[4:6]))
		if int(a)+n > len(s.flash) {
			return s.fail(cmd, ErrLen)
		}
		return s.ok(cmd, CRC16(s.flash[a:int(a)+n]), nil)
	case cmdFlashWrRd:
		if len(r) < 7 {
			return s.fail(cmd, ErrLen)
		}
		rd := int(binary.LittleEndian.Uint16(r[4:6]))
		d := make([]byte, rd)
		switch r[6] {
		case 0x06:
			s.wel = true
		case 0x01:
			if s.wel && len(r) > 7 && !s.StuckProtect {
				s.sr = r[7]
				s.FlashOps = append(s.FlashOps, FlashOp{"status-write", 0, 1})
			}
			s.wel = false
		case 0x05:
			for i := range d {
				d[i] = s.sr
			}
		case 0x4B:
			copy(d[min(4, rd):], SimFlashUID[:])
		}
		return s.ok(cmd, uint16(rd), d)
	}
	return s.fail(cmd, ErrFunc)
}

func (s *Sim) readByte(a uint32) byte {
	switch {
	case a >= 0x7d && a < 0x80:
		return []byte{0x03, 0x62, 0x55}[a-0x7d]
	case a >= RegPC && a < RegPC+4:
		return byte(s.pc >> (8 * (a - RegPC)))
	case a >= RegCPURegs && a < RegCPURegs+128:
		i := (a - RegCPURegs) / 4
		v := s.regs[i]
		if i == 15 {
			v = s.pc
		}
		return byte(v >> (8 * ((a - RegCPURegs) % 4)))
	case a == RegCPUCtl:
		return s.ctl
	}
	return s.mem[a]
}

func (s *Sim) writeByte(a uint32, v byte) {
	s.mem[a] = v
	switch {
	case a == RegCPUCtl:
		s.ctl = v
		if v == cpuGoBkpt {
			if s.mem[RegBkpt+7] == 0x01 {
				s.bkpt = uint32(s.mem[RegBkpt+4]) | uint32(s.mem[RegBkpt+5])<<8 | uint32(s.mem[RegBkpt+6])<<16
				s.bkptSet, s.atBkpt = true, true
				s.pc = s.bkpt
			}
		} else {
			s.atBkpt = false
		}
	case a == RegStep && v == stepAction:
		if s.ctl == cpuStop || s.ctl == cpuStall {
			s.pc += 2
			s.stepped++
		}
	case a == RegReboot && v == 0x20:
		s.rebooted++
		s.Activated = false
		s.pc, s.ctl, s.atBkpt = 0, cpuGo, false
	}
}
