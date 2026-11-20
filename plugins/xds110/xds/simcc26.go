package xds

import (
	"encoding/binary"
	"encoding/hex"

	"github.com/faronov/hwlab/internal/cortexm"
)

// SYNTHETIC CC13x2/CC26x2 model for the cJTAG/ICEPick path: a two-pin
// target that only answers after the probe's activation run, an ICEPick-C
// that links the CPU DAP (port 0) or the chip erase test TAP (port 21) into
// the chain on Run-Test/Idle, a JTAG-DP in front of the simulated Cortex-M4,
// and the CC2652P memory map (FCFG1, FLASH_SIZE, VIMS, ROM tables) with the
// ROM flash API executed on the halted core.

// Synthetic CC2652P identity.
const (
	SimCC26ICEPickID = 0x3BB4102F
	SimCC26DAPID     = 0x4BA00477
	SimCC26UserID    = 0x3202F000
	SimCC26Sectors   = 44
	SimCC26Flash     = SimCC26Sectors * 8192
	simCC26Erase     = 0x10002997
	simCC26Program   = 0x10002885
	simCC26FlashTbl  = 0x100002C4
)

// SimCC26CCFG is the synthetic CCFG: bootloader with backdoor on DIO15 active
// low, debug and every TAP enabled, image valid, no write protection.
const SimCC26CCFG = "00008001100080fffdff58003affbbffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffc50ffec5ffffffffc5ffffffc5c5c5ffc5c5c5ff00000000ffffffffffffffffffffffffffffffff"

// SimCC26IEEE is the synthetic factory IEEE address in FCFG1.
var SimCC26IEEE = [8]byte{0x00, 0x12, 0x4B, 0x00, 0x11, 0x22, 0x33, 0x44}

type cc26Sim struct {
	// format is the two-pin state: 0 power-up, FormatOScan1/2 active,
	// FormatJTAG switched to four pins (TDO not wired: reads zeros),
	// fmtLost a mangled activation (reads ones).
	format  uint32
	locked  bool
	irPick  uint32
	irDAP   uint32
	irTest  uint32
	connect bool
	rcap    uint32
	regs    map[uint32]uint32
	// selected TAPs the ICEPick links on the next Run-Test/Idle; linked
	// is the TAP in the chain (0 none, 1 DAP, 2 test TAP).
	pending, linked int
	dapRes          uint32
	flash           *cortexm.Region
	chipErases      int
	romCalls        []string
}

const fmtLost = 0xFFFF

// NewSimCC26x2 returns a simulator whose target is a CC2652P reached over
// two-pin cJTAG through an ICEPick.
func NewSimCC26x2() *Sim {
	s := NewSim()
	cs := &cc26Sim{regs: map[uint32]uint32{}}
	s.cc26 = cs
	core := s.core
	flash := make([]byte, SimCC26Flash)
	for i := range flash {
		flash[i] = 0xFF
	}
	copy(flash, core.Regions[0].Data[:0x400])
	cfg, _ := hex.DecodeString(SimCC26CCFG)
	copy(flash[SimCC26Flash-len(cfg):], cfg)
	cs.flash = &cortexm.Region{Base: 0, Data: flash, ROM: true}
	fcfg := make([]byte, 0x400)
	binary.LittleEndian.PutUint32(fcfg[0x318:], SimCC26ICEPickID)
	binary.LittleEndian.PutUint32(fcfg[0x294:], SimCC26UserID)
	for i := 0; i < 8; i++ {
		fcfg[0x2F0+i] = SimCC26IEEE[7-i]
	}
	flashRegs := make([]byte, 0x30)
	binary.LittleEndian.PutUint32(flashRegs[0x2C:], SimCC26Sectors)
	vims := make([]byte, 8)
	binary.LittleEndian.PutUint32(vims[4:], 1) // cache mode
	rom := make([]byte, 0x40000)
	binary.LittleEndian.PutUint32(rom[0x180:], 0xCB41)
	binary.LittleEndian.PutUint32(rom[0x180+4*10:], simCC26FlashTbl)
	binary.LittleEndian.PutUint32(rom[simCC26FlashTbl-0x10000000+4*5:], simCC26Erase)
	binary.LittleEndian.PutUint32(rom[simCC26FlashTbl-0x10000000+4*6:], simCC26Program)
	binary.LittleEndian.PutUint32(rom[0x48:], 0x10005085)
	core.Regions = []*cortexm.Region{
		cs.flash,
		{Base: 0x20000000, Data: make([]byte, 80<<10)},
		{Base: 0x50001000, Data: fcfg, ROM: true},
		{Base: 0x40030000, Data: flashRegs},
		{Base: 0x40034000, Data: vims},
		{Base: 0x10000000, Data: rom, ROM: true},
	}
	core.Exec = s.cc26Exec
	core.Hooks = append(core.Hooks, s.cc26AIRCR)
	return s
}

// CC26Flash returns a copy of the simulated CC26x2 flash.
func (s *Sim) CC26Flash() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.cc26.flash.Data...)
}

// SetCC26Flash replaces bytes of the simulated flash (test setup).
func (s *Sim) SetCC26Flash(addr uint32, b []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	copy(s.cc26.flash.Data[addr:], b)
}

// SetDAPLocked models a CCFG that keeps the CPU DAP closed: the ICEPick
// never links it.
func (s *Sim) SetDAPLocked(v bool) { s.mu.Lock(); s.cc26.locked = v; s.mu.Unlock() }

// CC26Format is the scan format the simulated target is in.
func (s *Sim) CC26Format() uint32 { s.mu.Lock(); defer s.mu.Unlock(); return s.cc26.format }

// ChipErases counts ICEPick chip erases.
func (s *Sim) ChipErases() int { s.mu.Lock(); defer s.mu.Unlock(); return s.cc26.chipErases }

// ROMCalls lists the ROM flash API calls the core executed.
func (s *Sim) ROMCalls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.cc26.romCalls...)
}

// dropFormat models a reset of the target: the two-pin format and the
// ICEPick routing are lost.
func (cs *cc26Sim) dropFormat() {
	cs.format, cs.linked, cs.pending, cs.connect = 0, 0, 0, false
	cs.irPick, cs.irDAP, cs.irTest = 0x04, 0x0E, 0
}

// cc26AIRCR resets the whole device on SYSRESETREQ, debug logic included.
func (s *Sim) cc26AIRCR(addr uint32, write bool, v uint32) (uint32, bool, error) {
	if !write || addr != cortexm.AddrAIRCR || v>>16 != 0x05FA || v&(1<<2) == 0 {
		return 0, false, nil
	}
	s.core.PowerOnResetFromHook()
	s.cc26.dropFormat()
	return 0, true, nil
}

// cc26Exec runs the ROM flash API and the BKPT stub.
func (s *Sim) cc26Exec(m cortexm.Machine) bool {
	cs := s.cc26
	pc := m.Reg(15)
	switch pc {
	case simCC26Erase &^ 1:
		addr := m.Reg(0)
		st := uint32(0)
		sec := int(addr / 8192)
		prot := cs.flash.Data[SimCC26Flash-88+0x48+sec/8]&(1<<(sec%8)) == 0
		switch {
		case addr%8192 != 0 || addr >= SimCC26Flash:
			st = 4
		case prot:
			st = 4
		default:
			for i := addr; i < addr+8192; i++ {
				cs.flash.Data[i] = 0xFF
			}
		}
		cs.romCalls = append(cs.romCalls, "erase")
		m.SetReg(0, st)
		m.SetReg(15, m.Reg(14)&^1)
		return true
	case simCC26Program &^ 1:
		buf, addr, n := m.Reg(0), m.Reg(1), m.Reg(2)
		st := uint32(0)
		if uint64(addr)+uint64(n) > SimCC26Flash {
			st = 4
		} else {
			for i := uint32(0); i < n; i++ {
				b, err := m.Load(buf+i, 1)
				if err != nil {
					st = 4
					break
				}
				cs.flash.Data[addr+i] &= byte(b)
			}
		}
		cs.romCalls = append(cs.romCalls, "program")
		m.SetReg(0, st)
		m.SetReg(15, m.Reg(14)&^1)
		return true
	}
	if hw, err := m.Load(pc, 2); err == nil && hw == 0xBE00 {
		m.Bkpt()
		return true
	}
	return false
}

// cc26CJTAGConnect runs the activation for a format.
func (s *Sim) cc26CJTAGConnect(format uint32) {
	cs := s.cc26
	switch {
	case format == FormatJTAG:
		cs.format = FormatJTAG
	case cs.format == 0:
		cs.dropFormat()
		cs.format = format
	default:
		// The activation expects the power-up state; from an active format
		// it mangles the TAP.7 controller, as observed on a CC2652P.
		cs.format = fmtLost
	}
}

// cc26CycleTCK models the probe's TCK run with TMS held: a long one returns
// a two-pin target to its power-up state.
func (s *Sim) cc26CycleTCK(n uint32) {
	if !s.cjtag && n >= 1000 {
		s.cc26.format = 0
	}
}

// chain lists the TAPs from TDO: the linked TAP, then the ICEPick.
func (cs *cc26Sim) chainIR() []int {
	if cs.linked != 0 {
		return []int{4, 6}
	}
	return []int{6}
}

func (cs *cc26Sim) drLen(tap int) int {
	if tap == 0 && cs.linked != 0 {
		if cs.linked == 1 {
			switch cs.irDAP {
			case 0x8, 0xA, 0xB:
				return 35
			case 0xE:
				return 32
			}
			return 1
		}
		if cs.irTest == 0x1 {
			return 8
		}
		return 1
	}
	switch cs.irPick {
	case 0x04, 0x05, 0x02:
		return 32
	case 0x07:
		return 8
	}
	return 1
}

func getBits(b []byte, off, n int) uint64 {
	var v uint64
	for i := 0; i < n; i++ {
		if j := off + i; j/8 < len(b) && b[j/8]>>(j%8)&1 == 1 {
			v |= 1 << i
		}
	}
	return v
}

func putBits(b []byte, off, n int, v uint64) {
	for i := 0; i < n; i++ {
		if v>>i&1 == 1 {
			b[(off+i)/8] |= 1 << ((off + i) % 8)
		}
	}
}

// cc26Scan shifts one IR or DR scan through the modelled chain.
func (s *Sim) cc26Scan(ir bool, in []byte, nbits int, end uint32) []byte {
	cs := s.cc26
	out := make([]byte, (nbits+7)/8)
	if !s.cjtag {
		return out
	}
	switch cs.format {
	case FormatJTAG, 0:
		return out // TDO is not wired / the target ignores the probe
	case FormatOScan1, FormatOScan2:
	default:
		for i := range out {
			out[i] = 0xFF
		}
		return out
	}
	taps := []int{0, 1}
	if cs.linked == 0 {
		taps = []int{1} // ICEPick only
	}
	lens := make([]int, len(taps))
	caps := make([]uint64, len(taps))
	for i, t := range taps {
		if ir {
			lens[i] = cs.chainIR()[i]
			caps[i] = 1
		} else {
			lens[i] = cs.drLen(t)
			caps[i] = s.cc26Capture(t, lens[i])
		}
	}
	total := 0
	for _, l := range lens {
		total += l
	}
	off := 0
	for i := range taps {
		putBits(out, off, min(lens[i], max(nbits-off, 0)), caps[i])
		off += lens[i]
	}
	for i := total; i < nbits; i++ {
		if getBits(in, i-total, 1) == 1 {
			out[i/8] |= 1 << (i % 8)
		}
	}
	if nbits >= total {
		off = nbits - total
		for i, t := range taps {
			v := getBits(in, off, lens[i])
			off += lens[i]
			if ir {
				s.cc26SetIR(t, uint32(v))
			} else {
				s.cc26Update(t, lens[i], v)
			}
		}
	}
	if end == StIdle {
		s.cc26Idle()
	}
	return out
}

func (s *Sim) cc26SetIR(tap int, v uint32) {
	cs := s.cc26
	if tap == 1 {
		cs.irPick = v
		return
	}
	if cs.linked == 1 {
		cs.irDAP = v
	} else {
		cs.irTest = v
	}
}

func (s *Sim) cc26Capture(tap, n int) uint64 {
	cs := s.cc26
	if tap == 1 {
		switch cs.irPick {
		case 0x04:
			return SimCC26ICEPickID
		case 0x05:
			return 0x41611CC0
		case 0x02:
			return uint64(cs.rcap)
		}
		return 0
	}
	if cs.linked == 1 {
		switch cs.irDAP {
		case 0xE:
			return SimCC26DAPID
		case 0x8, 0xA, 0xB:
			ack := uint64(2)
			if s.waitNext > 0 {
				ack = 1
			}
			return ack | uint64(cs.dapRes)<<3
		}
	}
	return 0
}

func (s *Sim) cc26Update(tap, n int, v uint64) {
	cs := s.cc26
	if tap == 1 {
		switch cs.irPick {
		case 0x07:
			cs.connect = v&0xFF == 0x89
		case 0x02:
			block, reg := uint32(v>>28)&7, uint32(v>>24)&0xF
			key := block<<4 | reg
			if v>>31 == 1 {
				if cs.connect {
					cs.regs[key] = uint32(v) & 0xFFFFFF
					if uint32(v)&0x100 != 0 {
						switch {
						case block == 2 && reg == 0 && !cs.locked:
							cs.pending = 1
						case block == 1 && reg == 5:
							cs.pending = 2
						}
					}
				}
				cs.rcap = uint32(v) &^ (1 << 31)
			} else {
				cs.rcap = block<<28 | reg<<24 | cs.regs[key]
			}
		}
		return
	}
	if cs.linked == 2 {
		if cs.irTest == 0x1 && v&0xFF == 0x22 {
			s.cc26ChipErase()
		}
		return
	}
	if n != 35 {
		return
	}
	if s.waitNext > 0 {
		s.waitNext--
		return
	}
	rnw, reg, data := v&1 == 1, byte(v>>1&3)<<2, uint32(v>>3)
	switch cs.irDAP {
	case 0x8:
		return
	case 0xA:
		if rnw {
			switch reg {
			case 0x0, 0xC:
				cs.dapRes = 0
			default:
				cs.dapRes = s.read(false, reg)
			}
			return
		}
		if reg == 0x4 && data&ctrlStickyErr != 0 {
			s.sticky &^= ctrlStickyErr
		}
		s.write(false, reg, data)
	case 0xB:
		if s.sticky&ctrlStickyErr != 0 {
			return
		}
		if rnw {
			s.read(true, reg)
			cs.dapRes = s.post
			return
		}
		s.write(true, reg, data)
	}
}

// cc26Idle applies a pending routing change, as the ICEPick does on
// Run-Test/Idle.
func (s *Sim) cc26Idle() {
	cs := s.cc26
	if cs.pending != 0 {
		cs.linked, cs.pending = cs.pending, 0
		cs.irPick = 0x3F
	}
}

// cc26ChipErase erases everything and leaves TI's default security block,
// then resets the device (the two-pin format is lost).
func (s *Sim) cc26ChipErase() {
	cs := s.cc26
	for i := range cs.flash.Data {
		cs.flash.Data[i] = 0xFF
	}
	sec := []byte{0xFF, 0xFF, 0xFF, 0xC5, 0xFF, 0xFF, 0xFF, 0xFF, 0xC5, 0xFF, 0xFF, 0xFF, 0xC5, 0xC5, 0xC5, 0xFF, 0xC5, 0xC5, 0xC5, 0xFF}
	copy(cs.flash.Data[SimCC26Flash-88+0x30:], sec)
	cs.chipErases++
	cs.locked = false
	s.core.PowerOnResetFromHook()
	cs.dropFormat()
}

// cc26ScanRequest answers OCD_SCAN_REQUEST.
func (s *Sim) cc26ScanRequest(a []byte) []byte {
	var out []byte
	for i := 0; i < len(a) && a[i] != 0; {
		switch a[i] {
		case 1, 2:
			if i+4 > len(a) {
				return status(StatusFail)
			}
			end, bits := a[i+1], int(binary.LittleEndian.Uint16(a[i+2:]))
			n := (bits + 7) / 8
			if i+4+n > len(a) {
				return status(StatusFail)
			}
			out = append(out, s.cc26Scan(a[i] == 1, a[i+4:i+4+n], bits, uint32(end))...)
			i += 4 + n
		case 3:
			if i+6 > len(a) {
				return status(StatusFail)
			}
			s.cc26Idle() // a run-test passes Run-Test/Idle
			i += 6
		default:
			return status(StatusFail)
		}
	}
	return status(0, out...)
}
