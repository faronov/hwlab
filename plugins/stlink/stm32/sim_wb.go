package stm32

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"

	"github.com/faronov/hwlab/internal/cortexm"
)

// SimWB is a SYNTHETIC STM32WB5x on top of cortexm.Sim: the CPU1 flash
// controller (KEYR, PER/PNB/STRT, double-word PG, BSY/CFGBSY), the secure
// boundary (FLASH_SFR, FLASH_SRRVR), PWR_CR4.C2BOOT, the IPCC registers and
// a scripted CPU2 that answers the mailbox the way AN5185/AN5289 describe
// it (ready events, FUS_GET_STATE, FW_UPGRADE, FW_DELETE, START_WS, the
// FUS's own system resets). It models only what the driver relies on.
// Passing against it is not evidence about real STM32WB silicon or about
// ST's FUS.
type SimWB struct {
	*cortexm.Sim
	mu sync.Mutex

	FlashKB int
	Flash   []byte // the whole array, base 0x08000000
	SFSA    uint32 // FLASH_SFR.SFSA (pages)
	SRRVR   uint32
	OPTR    uint32
	WRP1AR  uint32
	WRP1BR  uint32
	PCROPA  [2]uint32 // start, end
	PCROPB  [2]uint32

	// CPU2 / FUS model.
	FUSVersion, StackVersion uint32
	StackMem                 uint32
	// NewFUSVersion / NewStackVersion are what an upgrade with a FUS /
	// stack image installs; InstallSFSA the SFSA after a stack install,
	// DeleteSFSA after a delete.
	NewFUSVersion, NewStackVersion uint32
	InstallSFSA, DeleteSFSA        uint32
	// FirstStartUnknown makes the first FUS_GET_STATE report ERR_UNKNOWN.
	FirstStartUnknown bool
	// BusyTicks is how many polls an operation step lasts.
	BusyTicks int
	// IgnoreFUSRequest makes the wireless stack ignore the two
	// FUS_GET_STATE commands.
	IgnoreFUSRequest bool
	// PORResets makes the resets CPU2 raises reset the debug logic too, as
	// observed on hardware (the wireless stack rebooting into the FUS):
	// DEMCR clears, CPU1 runs and the debug link fails for LinkLossAccesses
	// accesses or until Reattach.
	PORResets        bool
	LinkLossAccesses int

	// Observations.
	Erases, Programs, CPU2Resets, Released, Reattaches int
	FlashWhileC2                                       int // flash operations started while C2BOOT was set
	Commands                                           []uint16
	LastStagedImage                                    uint32 // where FW_UPGRADE found the image

	sram2   *cortexm.Region
	locked  bool
	keySeq  int
	cr, sr  uint32
	dwLatch []uint32 // pending first word of a double-word: addr, value

	cr4, rccAHB3    uint32
	c1toc2, c2toc1  uint32
	mode            int // 0 off, 1 FUS, 2 wireless stack
	fusRequested    bool
	wsGetState      int
	state, errCode  byte
	op              int // 0 none, 1 FUS upgrade, 2 stack install, 3 delete
	opStep, opTicks int
	pendingCmd      int // ticks until the command is processed (-1 none)
	pendingReady    int
	pendingReset    int
	pendingFree     int
	unknownReported bool
	linkDown        int
	rccCSR          uint32
	evtSlot         uint32
}

const (
	simModeOff = iota
	simModeFUS
	simModeWS
)

// NewSimWB returns a 1 MiB STM32WB55 with FUS 1.1.1 and a BLE stack 1.10.0
// installed below SFSA 0xCB, as the reference board was found.
func NewSimWB() *SimWB {
	s := &SimWB{Sim: cortexm.NewSim(), FlashKB: 1024, Flash: make([]byte, 1024<<10), SFSA: 0xCB, SRRVR: 0x9E2B2C00, OPTR: 0x3DFFF1AA,
		WRP1AR: 0x000000FF, WRP1BR: 0x000000FF, PCROPA: [2]uint32{0x1FF, 0}, PCROPB: [2]uint32{0x1FF, 0},
		FUSVersion: 0x01010100, StackVersion: 0x010A0001, StackMem: 0x11160029,
		NewFUSVersion: 0x01020000, NewStackVersion: 0x01180000, InstallSFSA: 0xE0, DeleteSFSA: 0xF4,
		BusyTicks: 3, PORResets: true, LinkLossAccesses: 3, locked: true, sr: wbSROPTV, pendingCmd: -1, pendingReady: -1, pendingReset: -1, pendingFree: -1}
	for i := range s.Flash {
		s.Flash[i] = 0xFF
	}
	s.sram2 = &cortexm.Region{Base: wbSRAM2a, Data: make([]byte, 0x8000)}
	s.Regions = append(s.Regions, s.sram2)
	s.Hooks = append(s.Hooks, s.hook)
	return s
}

func (s *SimWB) secure() uint32 { return FlashBase + s.SFSA*wbPage }

func (s *SimWB) wrpPage(page uint32) bool {
	for _, ar := range []uint32{s.WRP1AR, s.WRP1BR} {
		if lo, hi := ar&0xFF, ar>>16&0xFF; lo <= hi && page >= lo && page <= hi {
			return true
		}
	}
	return false
}

// ---- SRAM2a as CPU2 sees it ----

func (s *SimWB) r32(a uint32) uint32 {
	if a < wbSRAM2a || a+4 > wbSRAM2a+uint32(len(s.sram2.Data)) {
		return 0
	}
	return binary.LittleEndian.Uint32(s.sram2.Data[a-wbSRAM2a:])
}

func (s *SimWB) w32(a, v uint32) {
	if a < wbSRAM2a || a+4 > wbSRAM2a+uint32(len(s.sram2.Data)) {
		return
	}
	binary.LittleEndian.PutUint32(s.sram2.Data[a-wbSRAM2a:], v)
}

func (s *SimWB) w8(a uint32, v byte) {
	if a >= wbSRAM2a && a < wbSRAM2a+uint32(len(s.sram2.Data)) {
		s.sram2.Data[a-wbSRAM2a] = v
	}
}

func (s *SimWB) refTable() (devinfo, sys, mm uint32) {
	return s.r32(wbSRAM2a), s.r32(wbSRAM2a + 12), s.r32(wbSRAM2a + 16)
}

func (s *SimWB) writeDevInfo() {
	d, _, _ := s.refTable()
	if d == 0 {
		return
	}
	for i := uint32(0); i < 0x60; i += 4 {
		s.w32(d+i, 0)
	}
	if s.mode == simModeFUS {
		s.w32(d, fusTableKey)
		s.w32(d+12, s.FUSVersion)
		s.w32(d+20, s.StackVersion)
		s.w32(d+24, s.StackMem)
		s.w32(d+40, 0x12345678)
		s.w32(d+48, 0x495)
		return
	}
	s.w32(d+4, s.FUSVersion)
	s.w32(d+16, s.StackVersion)
	s.w32(d+20, s.StackMem)
	s.w32(d+24, 0x01)
}

// pushEvent queues a vendor event on the system queue (LST_insert_tail)
// and raises channel 2 towards CPU1.
func (s *SimWB) pushEvent(sub uint16, payload ...byte) {
	_, sys, mm := s.refTable()
	head := s.r32(sys + 4)
	pool := s.r32(mm + 8)
	if head == 0 || pool == 0 {
		return
	}
	node := pool + s.evtSlot*0x40
	s.evtSlot = (s.evtSlot + 1) % 8
	s.w8(node+8, 0x12)
	s.w8(node+9, evtVendor)
	s.w8(node+10, byte(2+len(payload)))
	s.w8(node+11, byte(sub))
	s.w8(node+12, byte(sub>>8))
	for i, b := range payload {
		s.w8(node+13+uint32(i), b)
	}
	tail := s.r32(head + 4)
	s.w32(node, head)
	s.w32(node+4, tail)
	s.w32(head+4, node)
	s.w32(tail, node)
	s.c2toc1 |= ipccSysCh
}

func (s *SimWB) systemReset(fromCPU2 bool) {
	s.cr4, s.rccAHB3, s.c1toc2, s.c2toc1 = 0, 0, 0, 0
	s.mode, s.locked, s.cr = simModeOff, true, 0
	s.pendingCmd, s.pendingReady, s.pendingReset, s.pendingFree = -1, -1, -1, -1
	s.wsGetState = 0
	s.dwLatch = nil
	if !fromCPU2 {
		s.rccCSR |= rccSFTRSTF
		return
	}
	s.CPU2Resets++
	if !s.PORResets {
		s.rccCSR |= rccSFTRSTF
		s.ResetFromHook()
		return
	}
	s.rccCSR |= rccOBLRSTF
	s.PowerOnResetFromHook()
	s.linkDown = s.LinkLossAccesses
}

// Reattach models the debugger reconnecting (ENTER_SWD) after a reset that
// reset the debug port.
func (s *SimWB) Reattach() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.linkDown = 0
	s.Reattaches++
}

// errSimLinkLost is what the probe reports while the debug port is reset.
var errSimLinkLost = errors.New("simulated SWD failure: the debug port was reset (stlink status 0x18)")

func (s *SimWB) boot() {
	if s.rccAHB3&wbIPCCEN == 0 {
		return // CPU2 cannot signal without the IPCC clock
	}
	s.mode = simModeWS
	if s.fusRequested || s.StackVersion == 0 || s.op != 0 {
		s.mode = simModeFUS
	}
	s.writeDevInfo()
	s.pendingReady = 2
}

// tick advances CPU2 by one poll.
func (s *SimWB) tick() {
	if s.pendingReset >= 0 {
		if s.pendingReset == 0 {
			s.pendingReset = -1
			s.systemReset(true)
			return
		}
		s.pendingReset--
	}
	if s.pendingReady >= 0 {
		if s.pendingReady == 0 {
			s.pendingReady = -1
			code := byte(ReadyWirelessStack)
			if s.mode == simModeFUS {
				code = ReadyFUS
			}
			s.pushEvent(subEvtReady, code)
		} else {
			s.pendingReady--
		}
	}
	if s.pendingFree >= 0 {
		if s.pendingFree == 0 {
			s.pendingFree = -1
			_, _, mm := s.refTable()
			q := s.r32(mm + 16)
			for n := s.r32(q); n != q && n != 0; n = s.r32(n) {
				s.Released++
			}
			s.w32(q, q)
			s.w32(q+4, q)
			s.c1toc2 &^= ipccFreeCh
		} else {
			s.pendingFree--
		}
	}
	if s.pendingCmd >= 0 {
		if s.pendingCmd == 0 {
			s.pendingCmd = -1
			s.command()
			s.c1toc2 &^= ipccSysCh
		} else {
			s.pendingCmd--
		}
	}
	if s.mode == simModeFUS && s.op != 0 {
		s.opTicks++
		if s.opTicks >= s.BusyTicks {
			s.opTicks = 0
			s.advanceOp()
		}
	}
}

func (s *SimWB) advanceOp() {
	s.opStep++
	switch s.op {
	case 1, 2: // two FUS-initiated resets, then done
		if s.opStep <= 2 {
			s.pendingReset = 0
			return
		}
		if s.op == 1 {
			s.FUSVersion = s.NewFUSVersion
		} else {
			s.StackVersion, s.SFSA = s.NewStackVersion, s.InstallSFSA
		}
		s.op, s.state = 0, FUSStateIdle
		s.writeDevInfo()
	case 3:
		s.StackVersion, s.SFSA = 0, s.DeleteSFSA
		s.op, s.state = 0, FUSStateIdle
		s.writeDevInfo()
	}
}

func (s *SimWB) findImage() (uint32, bool) {
	for a := s.secure() - 4; a >= FlashBase; a -= 4 {
		if binary.LittleEndian.Uint32(s.Flash[a-FlashBase:]) == FUSImageMagic {
			return a, true
		}
		if a == FlashBase {
			break
		}
	}
	return 0, false
}

func (s *SimWB) command() {
	_, sys, _ := s.refTable()
	buf := s.r32(sys)
	op := binary.LittleEndian.Uint16(s.sram2.Data[buf+9-wbSRAM2a:])
	s.Commands = append(s.Commands, op)
	respond := func(p ...byte) {
		s.w8(buf, 0x13)
		s.w8(buf+1, evtCmdDone)
		s.w8(buf+2, byte(3+len(p)))
		s.w8(buf+3, 1)
		s.w8(buf+4, byte(op))
		s.w8(buf+5, byte(op>>8))
		for i, b := range p {
			s.w8(buf+6+uint32(i), b)
		}
	}
	if s.mode == simModeWS {
		if op == opFUSGetState {
			s.wsGetState++
			respond(FUSStateNotRunning, 0)
			if s.wsGetState == 2 && !s.IgnoreFUSRequest {
				s.fusRequested = true
				s.pendingReset = 2
			}
			return
		}
		respond(1)
		return
	}
	switch op {
	case opFUSGetState:
		if s.FirstStartUnknown && !s.unknownReported {
			s.unknownReported = true
			respond(FUSStateError, fusErrUnknown)
			return
		}
		st := s.state
		switch s.op {
		case 1:
			st = 0x20
		case 2:
			st = 0x10
		case 3:
			st = 0x30
		}
		respond(st, s.errCode)
	case opFUSFWUpgrade:
		a, ok := s.findImage()
		if !ok {
			respond(1)
			return
		}
		s.LastStagedImage = a
		isFUS := s.Flash[a-4-FlashBase] == 0xFF
		if !isFUS && s.StackVersion != 0 {
			respond(1)
			return
		}
		s.op, s.opStep, s.opTicks = 2, 0, 0
		if isFUS {
			s.op = 1
		}
		respond(0)
	case opFUSFWDelete:
		if s.StackVersion == 0 {
			respond(1)
			return
		}
		s.op, s.opStep, s.opTicks = 3, 0, 0
		respond(0)
	case opFUSStartWS:
		if s.StackVersion == 0 {
			respond(1)
			return
		}
		s.fusRequested = false
		respond(0)
		s.pendingReset = 1
	default:
		respond(1)
	}
}

func (s *SimWB) hook(addr uint32, write bool, v uint32) (uint32, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.linkDown > 0 {
		s.linkDown--
		return 0, true, errSimLinkLost
	}
	switch {
	case addr == wbRCCCSR:
		if write {
			if v&rccRMVF != 0 {
				s.rccCSR = 0
			}
			return 0, true, nil
		}
		return s.rccCSR, true, nil
	case addr == cortexm.AddrAIRCR && write:
		if v>>16 == 0x05FA && v&4 != 0 {
			s.systemReset(false)
		}
		return 0, false, nil // the core model performs the reset
	case addr == AddrDBGMCUIDCode && !write:
		return 0x20016495, true, nil
	case addr == AddrFlashSizeWB && !write:
		return uint32(s.FlashKB), true, nil
	case addr >= FlashBase && addr < FlashBase+uint32(len(s.Flash)):
		return s.flashAccess(addr, write, v)
	case addr == wbPWRCR4:
		if write {
			was := s.cr4
			s.cr4 = v
			if v&wbC2BOOT != 0 && was&wbC2BOOT == 0 {
				s.boot()
			}
			return 0, true, nil
		}
		s.tick()
		return s.cr4, true, nil
	case addr == wbRCCAHB3ENR:
		if write {
			s.rccAHB3 = v
			return 0, true, nil
		}
		return s.rccAHB3, true, nil
	case addr == ipccC1SCR && write:
		if s.rccAHB3&wbIPCCEN == 0 {
			return 0, true, nil
		}
		s.c2toc1 &^= v & 0x3F
		if v&(ipccSysCh<<16) != 0 && s.mode != simModeOff {
			s.c1toc2 |= ipccSysCh
			s.pendingCmd = 1
		}
		if v&(ipccFreeCh<<16) != 0 && s.mode != simModeOff {
			s.c1toc2 |= ipccFreeCh
			s.pendingFree = 1
		}
		return 0, true, nil
	case addr == ipccC1TOC2SR && !write:
		s.tick()
		return s.c1toc2, true, nil
	case addr == ipccC2TOC1SR && !write:
		s.tick()
		return s.c2toc1, true, nil
	case addr == ipccC1CR || addr == ipccC1MR:
		return 0, true, nil
	case addr >= wbFlashBase && addr < wbFlashBase+0x100:
		return s.flashReg(addr, write, v)
	}
	return 0, false, nil
}

func (s *SimWB) flashAccess(addr uint32, write bool, v uint32) (uint32, bool, error) {
	if addr >= s.secure() {
		return 0, true, fmt.Errorf("simulated bus fault: 0x%08x is in the CPU2 secure flash", addr)
	}
	off := addr - FlashBase
	if !write {
		return binary.LittleEndian.Uint32(s.Flash[off:]), true, nil
	}
	if s.locked || s.cr&wbCRPG == 0 {
		s.sr |= wbSRPGS
		return 0, true, nil
	}
	if s.cr4&wbC2BOOT != 0 {
		s.FlashWhileC2++
	}
	if s.wrpPage(off / wbPage) {
		s.sr |= wbSRWRP
		return 0, true, nil
	}
	if s.dwLatch == nil {
		if addr&7 != 0 {
			s.sr |= wbSRPGA
			return 0, true, nil
		}
		s.dwLatch = []uint32{addr, v}
		return 0, true, nil
	}
	first := s.dwLatch
	s.dwLatch = nil
	if addr != first[0]+4 {
		s.sr |= wbSRPGS
		return 0, true, nil
	}
	o := first[0] - FlashBase
	if binary.LittleEndian.Uint64(s.Flash[o:]) != ^uint64(0) {
		s.sr |= wbSRPROG
		return 0, true, nil
	}
	binary.LittleEndian.PutUint32(s.Flash[o:], first[1])
	binary.LittleEndian.PutUint32(s.Flash[o+4:], v)
	s.Programs++
	return 0, true, nil
}

func (s *SimWB) flashReg(addr uint32, write bool, v uint32) (uint32, bool, error) {
	switch addr {
	case wbKeyR:
		if write {
			switch {
			case v == key1:
				s.keySeq = 1
			case v == key2 && s.keySeq == 1:
				s.locked, s.keySeq = false, 0
			default:
				s.keySeq = 0
			}
		}
		return 0, true, nil
	case wbSR:
		if write {
			s.sr &^= v & wbSRW1C
			return 0, true, nil
		}
		return s.sr, true, nil
	case wbCR:
		if !write {
			r := s.cr
			if s.locked {
				r |= wbCRLock
			}
			return r, true, nil
		}
		if v&wbCRLock != 0 {
			s.locked, s.cr = true, 0
			return 0, true, nil
		}
		if s.locked {
			return 0, true, nil
		}
		prev := s.cr
		s.cr = v &^ wbCRSTRT
		if v&wbCRSTRT != 0 && prev&wbCRSTRT == 0 && v&wbCRPER != 0 {
			if s.cr4&wbC2BOOT != 0 {
				s.FlashWhileC2++
			}
			page := v >> wbCRPNB & 0xFF
			switch {
			case FlashBase+page*wbPage >= s.secure() || s.wrpPage(page):
				s.sr |= wbSRWRP
			default:
				for i := uint32(0); i < wbPage; i++ {
					s.Flash[page*wbPage+i] = 0xFF
				}
				s.Erases++
			}
		}
		return 0, true, nil
	case wbOPTR:
		return s.OPTR, true, nil
	case wbWRP1AR:
		return s.WRP1AR, true, nil
	case wbWRP1BR:
		return s.WRP1BR, true, nil
	case wbPCROP1ASR:
		return s.PCROPA[0], true, nil
	case wbPCROP1AER:
		return s.PCROPA[1], true, nil
	case wbPCROP1BSR:
		return s.PCROPB[0], true, nil
	case wbPCROP1BER:
		return s.PCROPB[1], true, nil
	case wbIPCCBR:
		return 0xFFFFC000, true, nil
	case wbSFR:
		return 0x1000 | s.SFSA, true, nil
	case wbSRRVR:
		return s.SRRVR, true, nil
	}
	if write {
		return 0, true, fmt.Errorf("simulated: write to unmodelled FLASH register 0x%08x", addr)
	}
	return 0, true, nil
}
