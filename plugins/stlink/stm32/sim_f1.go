package stm32

import (
	"encoding/binary"
	"fmt"
	"sync"

	"github.com/faronov/hwlab/internal/cortexm"
)

// thumbExec interprets the small Thumb-1 subset the hwlab flash loader uses
// (MOVS/CMP/ADDS/SUBS immediate, LDR/STR/LDRH/STRH immediate offset, LSRS
// immediate, TST, B, B<cond>, BKPT) so the simulated F1 runs the real loader
// bytes instead of a hook that merely imitates them. It only runs code in
// SRAM; anything else falls back to the simulator's placeholder.
func thumbExec(m cortexm.Machine) bool {
	pc := m.Reg(15)
	if pc>>29 != 1 { // 0x2000_0000..0x3FFF_FFFF
		return false
	}
	w, err := m.Load(pc, 4)
	if err != nil {
		return false
	}
	op := uint16(w)
	xpsr := m.Reg(16)
	setNZ := func(v uint32) {
		xpsr &^= 3 << 30
		if v&(1<<31) != 0 {
			xpsr |= 1 << 31
		}
		if v == 0 {
			xpsr |= 1 << 30
		}
	}
	setC := func(c bool) {
		xpsr &^= 1 << 29
		if c {
			xpsr |= 1 << 29
		}
	}
	setV := func(v bool) {
		xpsr &^= 1 << 28
		if v {
			xpsr |= 1 << 28
		}
	}
	rd8 := int(op >> 8 & 7)
	imm8 := uint32(op & 0xFF)
	next := pc + 2
	switch {
	case op>>11 == 0x04: // MOVS Rd, #imm8
		m.SetReg(rd8, imm8)
		setNZ(imm8)
	case op>>11 == 0x05: // CMP Rn, #imm8
		a := m.Reg(rd8)
		r := a - imm8
		setNZ(r)
		setC(a >= imm8)
		setV((a^imm8)&(a^r)&(1<<31) != 0)
	case op>>11 == 0x06: // ADDS Rd, #imm8
		a := m.Reg(rd8)
		r := a + imm8
		m.SetReg(rd8, r)
		setNZ(r)
		setC(r < a)
		setV(^(a^imm8)&(a^r)&(1<<31) != 0)
	case op>>11 == 0x07: // SUBS Rd, #imm8
		a := m.Reg(rd8)
		r := a - imm8
		m.SetReg(rd8, r)
		setNZ(r)
		setC(a >= imm8)
		setV((a^imm8)&(a^r)&(1<<31) != 0)
	case op>>11 == 0x01: // LSRS Rd, Rm, #imm5
		sh := uint32(op >> 6 & 0x1F)
		if sh == 0 {
			sh = 32
		}
		v := m.Reg(int(op >> 3 & 7))
		var r uint32
		var c bool
		if sh < 32 {
			r, c = v>>sh, v>>(sh-1)&1 != 0
		} else {
			c = v>>31 != 0
		}
		m.SetReg(int(op&7), r)
		setNZ(r)
		setC(c)
	case op&0xFFC0 == 0x4200: // TST Rn, Rm
		setNZ(m.Reg(int(op&7)) & m.Reg(int(op>>3&7)))
	case op>>11 == 0x0C || op>>11 == 0x0D || op>>11 == 0x10 || op>>11 == 0x11: // STR LDR STRH LDRH
		rn, rt := int(op>>3&7), int(op&7)
		imm5 := uint32(op >> 6 & 0x1F)
		size, load := 4, op>>11 == 0x0D || op>>11 == 0x11
		if op>>11 >= 0x10 {
			size = 2
		}
		addr := m.Reg(rn) + imm5*uint32(size)
		if load {
			v, err := m.Load(addr, size)
			if err != nil {
				return false
			}
			m.SetReg(rt, v)
		} else if err := m.Store(addr, size, m.Reg(rt)); err != nil {
			return false
		}
	case op>>12 == 0xD && op>>8&0xF < 0xE: // B<cond>
		n, z, c, v := xpsr>>31&1 != 0, xpsr>>30&1 != 0, xpsr>>29&1 != 0, xpsr>>28&1 != 0
		var take bool
		switch op >> 8 & 0xF {
		case 0:
			take = z
		case 1:
			take = !z
		case 2:
			take = c
		case 3:
			take = !c
		case 4:
			take = n
		case 5:
			take = !n
		case 6:
			take = v
		case 7:
			take = !v
		case 8:
			take = c && !z
		case 9:
			take = !c || z
		case 10:
			take = n == v
		case 11:
			take = n != v
		case 12:
			take = !z && n == v
		case 13:
			take = z || n != v
		}
		if take {
			next = pc + 4 + uint32(int32(int8(op&0xFF)))*2
		}
	case op>>11 == 0x1C: // B
		off := int32(op&0x7FF) << 21 >> 20
		next = pc + 4 + uint32(off)
	case op>>8 == 0xBE: // BKPT
		m.Bkpt()
		return true
	default:
		return false
	}
	m.SetReg(16, xpsr)
	m.SetReg(15, next)
	return true
}

// SimF1 is a SYNTHETIC STM32F1 flash controller and identification block on
// top of cortexm.Sim. It models the documented controller behaviour the
// driver relies on (key sequence, PG/PER/STRT, halfword programming that only
// accepts erased halfwords, write protection, BSY) and runs the real loader
// instructions through thumbExec. Passing against it is not evidence about
// real STM32 silicon.
type SimF1 struct {
	*cortexm.Sim
	mu sync.Mutex

	DevID   uint32
	FlashKB int
	// SizeReg is what 0x1FFFF7E0 reads (low 16 bits are the size in KB).
	SizeReg uint32
	Flash   []byte
	// WRPR is FLASH_WRPR (a clear bit write-protects 4 KiB). RDP is the
	// option-byte RDP byte (0xA5 = level 0).
	WRPR uint32
	RDP  byte
	// HiddenWRPR write-protects pages without FLASH_WRPR showing it, so the
	// loader's own error path can be exercised (a clear bit protects).
	HiddenWRPR uint32
	// BusyReads makes SR.BSY read set that many times after STRT or a program.
	BusyReads int
	// CorruptProgram flips a bit of the next programmed halfword.
	CorruptProgram bool
	Erases         int
	Programs       int // halfword programs
	// WordWriteFaults counts 32-bit stores into the flash with PG set.
	WordWriteFaults int

	locked bool
	keySeq int
	cr, sr uint32
	ar     uint32
	busy   int
}

// NewSimF1 returns an F1 medium-density simulator with kb KiB of erased flash
// and a 20 KiB SRAM.
func NewSimF1(kb int) *SimF1 {
	s := &SimF1{Sim: cortexm.NewSim(), DevID: 0x20036410, FlashKB: kb, SizeReg: uint32(kb), Flash: make([]byte, kb<<10), WRPR: 0xFFFFFFFF, HiddenWRPR: 0xFFFFFFFF, RDP: 0xA5, locked: true}
	for i := range s.Flash {
		s.Flash[i] = 0xFF
	}
	s.Regions[1].Data = s.Regions[1].Data[:20<<10]
	s.Hooks = append(s.Hooks, s.hook)
	s.SubWord = append(s.SubWord, s.subWord)
	s.Exec = thumbExec
	return s
}

func (s *SimF1) page(addr uint32) uint32 { return (addr - FlashBase) &^ 1023 }

func (s *SimF1) inFlash(addr uint32) bool {
	return addr >= FlashBase && addr < FlashBase+uint32(len(s.Flash))
}

func (s *SimF1) protected(addr uint32) bool {
	g := (addr - FlashBase) / 4096
	if g > 31 {
		g = 31
	}
	return (s.WRPR&s.HiddenWRPR)>>g&1 == 0
}

func (s *SimF1) subWord(addr uint32, size int, v uint32) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.inFlash(addr) {
		return false, nil
	}
	if size != 2 {
		return true, fmt.Errorf("simulated bus fault: %d-bit store to flash", size*8)
	}
	if s.locked || s.cr&f1CRPG == 0 {
		return true, nil // ignored, as on hardware
	}
	if s.protected(addr) {
		s.sr |= f1SRWRPErr
		return true, nil
	}
	off := addr - FlashBase
	old := uint32(binary.LittleEndian.Uint16(s.Flash[off:]))
	if old != 0xFFFF && v&0xFFFF != 0 {
		s.sr |= f1SRPGErr
		return true, nil
	}
	nv := old & v & 0xFFFF
	if s.CorruptProgram {
		nv ^= 1
		s.CorruptProgram = false
	}
	binary.LittleEndian.PutUint16(s.Flash[off:], uint16(nv))
	s.Programs++
	s.busy = s.BusyReads
	return true, nil
}

func (s *SimF1) hook(addr uint32, write bool, v uint32) (uint32, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case addr == AddrDBGMCUIDCode && !write:
		return s.DevID, true, nil
	case addr == AddrFlashSizeF1&^3 && !write:
		return s.SizeReg, true, nil
	case addr >= 0x1FFFF800 && addr < 0x1FFFF810 && !write:
		var ob [16]byte
		ob[0], ob[1] = s.RDP, ^s.RDP
		ob[2], ob[3] = 0xFF, 0xFF
		for i := 0; i < 4; i++ {
			ob[8+2*i], ob[9+2*i] = byte(s.WRPR>>(8*i)), ^byte(s.WRPR>>(8*i))
		}
		return binary.LittleEndian.Uint32(ob[addr-0x1FFFF800:]), true, nil
	case s.inFlash(addr):
		off := addr - FlashBase
		if !write {
			return binary.LittleEndian.Uint32(s.Flash[off:]), true, nil
		}
		s.WordWriteFaults++
		return 0, true, fmt.Errorf("simulated bus fault: 32-bit store to flash")
	case addr == f1KeyR && write:
		switch {
		case v == key1:
			s.keySeq = 1
		case v == key2 && s.keySeq == 1:
			s.locked, s.keySeq = false, 0
		default:
			s.keySeq = 0
		}
		return 0, true, nil
	case addr == f1SR:
		if write {
			s.sr &^= v & f1SRW1C
			return 0, true, nil
		}
		r := s.sr
		if s.busy > 0 {
			s.busy--
			r |= f1SRBSY
		}
		return r, true, nil
	case addr == f1AR:
		if write && !s.locked {
			s.ar = v
		}
		return s.ar, true, nil
	case addr == f1CR:
		if !write {
			r := s.cr
			if s.locked {
				r |= f1CRLock
			}
			return r, true, nil
		}
		if v&f1CRLock != 0 {
			s.locked, s.cr = true, 0
			return 0, true, nil
		}
		if s.locked {
			return 0, true, nil
		}
		prev := s.cr
		s.cr = v &^ f1CRSTRT
		if v&f1CRSTRT != 0 && prev&f1CRSTRT == 0 && v&f1CRPER != 0 {
			switch {
			case !s.inFlash(s.ar):
				s.sr |= f1SRPGErr
			case s.protected(s.ar):
				s.sr |= f1SRWRPErr
			default:
				base := s.page(s.ar)
				for i := uint32(0); i < 1024; i++ {
					s.Flash[base+i] = 0xFF
				}
				s.Erases++
				s.busy = s.BusyReads
			}
		}
		return 0, true, nil
	case addr == f1OBR && !write:
		r := uint32(0x03FFFFFC)
		if s.RDP != 0xA5 {
			r |= 2
		}
		return r, true, nil
	case addr == f1WRPR && !write:
		return s.WRPR, true, nil
	}
	return 0, false, nil
}
