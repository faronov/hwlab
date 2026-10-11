package stm32

import (
	"encoding/binary"
	"sync"

	"github.com/faronov/hwlab/internal/cortexm"
)

// SimF4 is a SYNTHETIC STM32F4 flash controller and identification block on
// top of cortexm.Sim. It models the documented behaviour the driver relies
// on (key sequence, SER/SNB/STRT, PG with PSIZE, write-protect, AND-only
// programming, BSY) and nothing else. Passing against it is not evidence
// about real STM32 silicon.
type SimF4 struct {
	*cortexm.Sim
	mu sync.Mutex

	DevID   uint32 // DBGMCU_IDCODE
	FlashKB int
	Flash   []byte // the flash array, base 0x08000000
	// NWRP is OPTCR[27:16] (a clear bit write-protects the sector).
	NWRP uint32
	// RDP is OPTCR[15:8] (0xAA = level 0).
	RDP byte
	// BusyReads makes SR.BSY read set that many times after STRT or a
	// program write.
	BusyReads int
	// CorruptProgram flips one bit of the next programmed word, so a verify
	// must fail.
	CorruptProgram bool
	// Erases and Programs count operations.
	Erases, Programs int

	locked bool
	keySeq int
	cr, sr uint32
	busy   int
}

// NewSimF4 returns a controller with kb KiB of erased flash.
func NewSimF4(kb int) *SimF4 {
	s := &SimF4{Sim: cortexm.NewSim(), DevID: 0x20006413, FlashKB: kb, Flash: make([]byte, kb<<10), NWRP: 0xFFF, RDP: 0xAA, locked: true}
	for i := range s.Flash {
		s.Flash[i] = 0xFF
	}
	s.Hooks = append(s.Hooks, s.hook)
	return s
}

func (s *SimF4) sectorAt(addr uint32) (Sector, bool) {
	p := &Profile{Sectors: f4Layout(s.FlashKB)}
	return p.SectorAt(addr)
}

func (s *SimF4) hook(addr uint32, write bool, v uint32) (uint32, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case addr == AddrDBGMCUIDCode && !write:
		return s.DevID, true, nil
	case addr == AddrFlashSizeF4&^3 && !write:
		return uint32(s.FlashKB) << 16, true, nil
	case addr >= FlashBase && addr < FlashBase+uint32(len(s.Flash)):
		off := addr - FlashBase
		if !write {
			return binary.LittleEndian.Uint32(s.Flash[off:]), true, nil
		}
		if s.locked || s.cr&crPG == 0 {
			s.sr |= 1 << 7 // PGSERR
			return 0, true, nil
		}
		if sec, ok := s.sectorAt(addr); ok && s.NWRP>>uint(sec.Index)&1 == 0 {
			s.sr |= 1 << 4 // WRPERR
			return 0, true, nil
		}
		old := binary.LittleEndian.Uint32(s.Flash[off:])
		nv := old & v
		if s.CorruptProgram {
			nv ^= 1
			s.CorruptProgram = false
		}
		binary.LittleEndian.PutUint32(s.Flash[off:], nv)
		s.Programs++
		s.busy = s.BusyReads
		return 0, true, nil
	case addr == f4KeyR && write:
		switch {
		case v == key1:
			s.keySeq = 1
		case v == key2 && s.keySeq == 1:
			s.locked, s.keySeq = false, 0
		default:
			s.keySeq = 0
		}
		return 0, true, nil
	case addr == f4SR:
		if write {
			s.sr &^= v & srW1C
			return 0, true, nil
		}
		r := s.sr
		if s.busy > 0 {
			s.busy--
			r |= srBSY
		}
		return r, true, nil
	case addr == f4CR:
		if !write {
			r := s.cr
			if s.locked {
				r |= crLock
			}
			return r, true, nil
		}
		if v&crLock != 0 {
			s.locked, s.cr = true, 0
			return 0, true, nil
		}
		if s.locked {
			return 0, true, nil
		}
		prev := s.cr
		s.cr = v &^ crSTRT
		if v&crSTRT != 0 && prev&crSTRT == 0 && v&crSER != 0 {
			idx := int(v >> crSNB & 0xF)
			if idx < len(f4Layout(s.FlashKB)) {
				sec := f4Layout(s.FlashKB)[idx]
				if s.NWRP>>uint(idx)&1 == 0 {
					s.sr |= 1 << 4
				} else {
					for i := uint32(0); i < sec.Size; i++ {
						s.Flash[sec.Addr-FlashBase+i] = 0xFF
					}
					s.Erases++
					s.busy = s.BusyReads
				}
			} else {
				s.sr |= 1 << 1 // OPERR
			}
		}
		return 0, true, nil
	case addr == f4OptCR && !write:
		return 0x0F<<24 | s.NWRP<<16 | uint32(s.RDP)<<8 | 0xED, true, nil
	}
	return 0, false, nil
}
