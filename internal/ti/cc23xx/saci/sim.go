package saci

import (
	"hash/crc32"
)

// Sim is a SACI device model for tests and probe replay: the mailbox
// protocol, the documented commands and a flash image with 2 KiB sectors.
// It follows the TRM; it is not a copy of any firmware.
type Sim struct {
	Flash   []byte // MAIN flash, SectorSize multiple
	CCFG    []byte // the CCFG sector
	UserRec []byte

	// Permissions.
	AllowErase, AllowProgram, AllowVerify bool
	DebugAuth                             byte // 0x5A open, 0xA5 password needed

	// Session state.
	CCFGValid        bool
	SessionTainted   bool // chip erase or CCFG program ran: exits are refused
	Exited           string
	Resets           int
	ProtectedSectors map[int]bool

	cmdStart bool
	words    []uint32
	rx       []uint32
	txFull   bool
	Log      []CmdID
}

// NewSim returns an erased device with flash of the given size in bytes.
func NewSim(flashBytes int) *Sim {
	s := &Sim{Flash: make([]byte, flashBytes), CCFG: make([]byte, SectorSize), UserRec: make([]byte, 128),
		AllowErase: true, AllowProgram: true, AllowVerify: true, DebugAuth: 0x5A, CCFGValid: true,
		ProtectedSectors: map[int]bool{}}
	for i := range s.Flash {
		s.Flash[i] = 0xFF
	}
	for i := range s.CCFG {
		s.CCFG[i] = 0xFF
	}
	return s
}

// ReadReg implements Mailbox.
func (s *Sim) ReadReg(reg byte) (uint32, error) {
	switch reg {
	case RegTXCTL:
		return 0, nil
	case RegRXCTL:
		if len(s.rx) > 0 {
			return rxFull, nil
		}
		return 0, nil
	case RegRXD:
		if len(s.rx) == 0 {
			return 0, nil
		}
		v := s.rx[0]
		s.rx = s.rx[1:]
		return v, nil
	}
	return 0, nil
}

// WriteReg implements Mailbox.
func (s *Sim) WriteReg(reg byte, v uint32) error {
	switch reg {
	case RegTXCTL:
		if v&txCmdStart != 0 {
			s.cmdStart = true
			s.words = s.words[:0]
		}
	case RegTXD:
		s.words = append(s.words, v)
		s.step()
	}
	return nil
}

// WriteWords implements Mailbox.
func (s *Sim) WriteWords(reg byte, data []uint32) error {
	for _, w := range data {
		if err := s.WriteReg(reg, w); err != nil {
			return err
		}
	}
	return nil
}

func (s *Sim) need(w0 uint32) int {
	switch CmdID(w0) {
	case CmdFlashEraseChip:
		return 2
	case CmdFlashVerifyCCFG:
		return 5
	case CmdFlashProgCCFG:
		return 2 + CCFGSectorWords
	case CmdFlashProgUserRec:
		return 2 + UserRecordWords
	case CmdFlashProgMain:
		return 3 + (int(w0>>16)+3)/4
	case CmdFlashVerifyMain:
		return 4
	case CmdDebugSubmitAuth:
		return 1 + int(w0>>16)
	}
	return 1
}

func (s *Sim) respond(w0 uint32, res Result, data ...uint32) {
	s.rx = append(s.rx, w0&0xFFFF|uint32(res)<<16|uint32(len(data))<<24)
	s.rx = append(s.rx, data...)
}

func (s *Sim) step() {
	if !s.cmdStart || len(s.words) < s.need(s.words[0]) {
		return
	}
	w := s.words
	s.cmdStart = false
	s.exec(w)
}

func (s *Sim) exec(w []uint32) {
	w0 := w[0]
	id := CmdID(w0)
	s.Log = append(s.Log, id)
	ok := Result(ResSuccess)
	switch id {
	case CmdNop:
		s.respond(w0, ok)
	case CmdGetDieID:
		s.respond(w0, ok, 0xD1E1D001, 0xD1E1D002, 0xD1E1D003, 0xD1E1D004)
	case CmdGetCCFGUserRec:
		if !s.CCFGValid {
			s.respond(w0, ResNotAllowed)
			return
		}
		s.respond(w0, ok, Pack(s.UserRec, 0xFF)...)
	case CmdDebugReqPwdID:
		if s.DebugAuth == 0xA5 {
			s.respond(w0, ok, 0x11111111, 0x22222222)
		} else {
			s.respond(w0, ok)
		}
	case CmdDebugSubmitAuth:
		s.respond(w0, ResInvalidPwd)
	case CmdExitSACIHalt, CmdExitSACIRun, CmdExitSACIShutdown:
		if s.SessionTainted || !s.CCFGValid {
			s.respond(w0, ResNotAllowed)
			return
		}
		s.Exited = id.String()
		s.respond(w0, ok)
	case CmdResetDevice:
		s.Resets++
		s.SessionTainted = false
		if w0>>16&1 == 1 {
			s.respond(w0, ok)
		}
	case CmdFlashEraseChip:
		if !s.AllowErase {
			s.respond(w0, ResNotAllowed)
			return
		}
		if w[1] != FlashKey {
			s.respond(w0, ResInvalidKey)
			return
		}
		retain := uint16(w0 >> 16)
		for i := 0; i < len(s.Flash)/SectorSize; i++ {
			if i < 16 && retain&(1<<i) != 0 {
				continue
			}
			fill(s.Flash[i*SectorSize:(i+1)*SectorSize], 0xFF)
		}
		fill(s.CCFG, 0xFF)
		s.CCFGValid, s.SessionTainted = false, true
		s.respond(w0, ok)
	case CmdFlashProgMain:
		s.progMain(w)
	case CmdFlashProgCCFG:
		if !s.AllowProgram {
			s.respond(w0, ResNotAllowed)
			return
		}
		if w[1] != FlashKey {
			s.respond(w0, ResInvalidKey)
			return
		}
		for _, b := range s.CCFG {
			if b != 0xFF {
				s.respond(w0, ResFlashFSMError)
				return
			}
		}
		copy(s.CCFG, unpack(w[2:2+CCFGSectorWords]))
		s.CCFGValid, s.SessionTainted = true, true
		s.respond(w0, ok)
	case CmdFlashVerifyMain:
		s.verifyMain(w)
	case CmdFlashVerifyCCFG:
		if w0>>31 == 1 {
			for _, b := range s.CCFG {
				if b != 0xFF {
					s.respond(w0, ResBlankCheckFailed)
					return
				}
			}
		}
		if w0>>31 == 0 {
			c := s.CCFG
			sums := [4]uint32{crc32.ChecksumIEEE(c[0:0xC]), crc32.ChecksumIEEE(c[0x10:0x74C]), crc32.ChecksumIEEE(c[0x750:0x7CC]), crc32.ChecksumIEEE(c[0x7D0:0x7FC])}
			for i, v := range sums {
				if i == 2 && w0>>16&2 != 0 {
					continue
				}
				if w[1+i] != v {
					s.respond(w0, ResCRC32Mismatch)
					return
				}
			}
		}
		s.respond(w0, ok)
	default:
		s.respond(w0, ResInvalidCmdID)
	}
}

func fill(b []byte, v byte) {
	for i := range b {
		b[i] = v
	}
}

func unpack(w []uint32) []byte {
	b := make([]byte, 4*len(w))
	for i, v := range w {
		b[4*i], b[4*i+1], b[4*i+2], b[4*i+3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
	}
	return b
}

func (s *Sim) progMain(w []uint32) {
	w0 := w[0]
	n := int(w0 >> 16)
	addr := int(w[2])
	switch {
	case !s.AllowProgram:
		s.respond(w0, ResNotAllowed)
	case w[1] != FlashKey:
		s.respond(w0, ResInvalidKey)
	case n == 0:
		s.respond(w0, ResSuccess)
	case addr < 0 || addr+n > len(s.Flash):
		s.respond(w0, ResInvalidAddress)
	case addr/SectorSize != (addr+n-1)/SectorSize:
		s.respond(w0, ResInvalidSize)
	case s.ProtectedSectors[addr/SectorSize]:
		s.respond(w0, ResFlashFSMError)
	default:
		data := unpack(w[3:])[:n]
		for i, b := range data {
			// Flash can only clear bits; programming over data fails.
			if s.Flash[addr+i] != 0xFF && s.Flash[addr+i] != b {
				s.respond(w0, ResFlashFSMError)
				return
			}
		}
		copy(s.Flash[addr:], data)
		s.respond(w0, ResSuccess)
	}
}

func (s *Sim) verifyMain(w []uint32) {
	w0, addr, n, want := w[0], int(w[1]), int(w[2]), w[3]
	if !s.AllowVerify {
		s.respond(w0, ResNotAllowed)
		return
	}
	if addr < 0 || addr%SectorSize != 0 || addr+n > len(s.Flash) || (n%SectorSize != 0 && n%SectorSize != SectorSize-4) {
		s.respond(w0, ResInvalidSize)
		return
	}
	region := s.Flash[addr : addr+n]
	if w0>>31 == 1 {
		for _, b := range region {
			if b != 0xFF {
				s.respond(w0, ResBlankCheckFailed)
				return
			}
		}
		s.respond(w0, ResSuccess)
		return
	}
	if crc32.ChecksumIEEE(region) != want {
		s.respond(w0, ResCRC32Mismatch)
		return
	}
	s.respond(w0, ResSuccess)
}
