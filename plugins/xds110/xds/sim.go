package xds

import (
	"context"
	"encoding/binary"
	"sync"
	"time"

	"github.com/faronov/hwlab/internal/cortexm"
	"github.com/faronov/hwlab/internal/ti/cc23xx/ccsim"
)

// Sim is a SYNTHETIC XDS110 with a Cortex-M target on SWD and a two-TAP JTAG
// chain. It speaks the XDS110 command protocol (it is a [Transport]). It is the
// author's reading of the protocol, validated against the real probe only for
// the commands listed in docs/xds110.md; target-side behaviour (SWD queue
// results, error codes, JTAG chain content) has NOT been observed on hardware.
type Sim struct {
	mu sync.Mutex

	Firmware uint32
	Hardware uint16

	core *cortexm.Sim
	// cc, when set, models a CC23xx behind the DP: it owns the core and the
	// three access ports and the SACI boot state.
	cc *ccsim.Device

	connected  bool
	swd        bool
	cmapi      bool
	acquired   bool
	tck        uint32
	trst, srst bool
	cjtag      bool
	supplyMV   uint32
	supplyOn   bool

	// SWD DP/AP state.
	dpSelect uint32
	ctrlReq  uint32
	sticky   uint32
	csw, tar uint32
	post     uint32

	// JTAG model.
	tapState int
	ir       []uint32 // per TAP loaded instruction
	chain    []simTAP

	// Test hooks.
	waitNext int
	noTarget bool
	// ignoreSRST models an SRST line that is not wired to the target.
	ignoreSRST bool
	pinResets  int
	Commands   []byte
	failNext   Status
}

type simTAP struct {
	idcode uint32
	irLen  int
}

const (
	simRAMBase = 0x20000000
	simDPIDR   = 0x2BA01477
	simAPIDR   = 0x24770011

	apCSW  = 0x00
	apTAR  = 0x04
	apDRW  = 0x0c
	apBase = 0xf8
	apIDR  = 0xfc

	ctrlStickyErr  = 1 << 5
	ctrlCDBGPwrReq = 1 << 28
	ctrlCDBGPwrAck = 1 << 29
	ctrlCSYSPwrReq = 1 << 30
	ctrlCSYSPwrAck = 1 << 31
)

// SimIDCodes are the synthetic IDCODEs of the JTAG chain, nearest TDO first.
var SimIDCodes = []uint32{0x1B9AE02F, 0x0BA00477}

// NewSim returns a simulator with firmware 3.0.0.22 on hardware id 0x23.
func NewSim() *Sim {
	s := &Sim{Firmware: 0x03000022, Hardware: 0x23, core: cortexm.NewSim(), csw: 0x23000040, srst: true, trst: true}
	for _, id := range SimIDCodes {
		s.chain = append(s.chain, simTAP{idcode: id, irLen: 6})
	}
	s.ir = make([]uint32, len(s.chain))
	return s
}

// Core exposes the simulated Cortex-M model.
func (s *Sim) Core() *cortexm.Sim { return s.core }

// NewSimCC23xx returns a simulator whose target is a CC23xx (SACI boot state,
// CFG-AP and SEC-AP, flash and CCFG).
func NewSimCC23xx() *Sim {
	s := NewSim()
	s.cc = ccsim.New()
	s.core = s.cc.Core
	return s
}

// CC23xx returns the simulated CC23xx, or nil.
func (s *Sim) CC23xx() *ccsim.Device { return s.cc }

// IgnoreSRST models an SRST line that is not connected to the target.
func (s *Sim) IgnoreSRST(v bool) { s.mu.Lock(); s.ignoreSRST = v; s.mu.Unlock() }

// Resets returns how many target resets happened (pin and AIRCR).
func (s *Sim) Resets() int { s.mu.Lock(); defer s.mu.Unlock(); return s.pinResets + s.core.Resets }

// InjectWait makes the next n SWD queues answer WAIT.
func (s *Sim) InjectWait(n int) { s.mu.Lock(); s.waitNext = n; s.mu.Unlock() }

// SetNoTarget makes SWD answer with a device-id error, as an unwired line.
func (s *Sim) SetNoTarget(v bool) { s.mu.Lock(); s.noTarget = v; s.mu.Unlock() }

// FailNext makes the next command fail with st.
func (s *Sim) FailNext(st Status) { s.mu.Lock(); s.failNext = st; s.mu.Unlock() }

// Supply reports the simulated supply state.
func (s *Sim) Supply() (uint32, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.supplyMV, s.supplyOn
}

// Byte returns simulated memory; ok false when unmapped.
func (s *Sim) Byte(addr uint32) (byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.core.Load(addr, 1)
	return byte(v), err == nil
}

// Transport implementation.

// Describe names the simulator.
func (s *Sim) Describe() string { return "SYNTHETIC simulated XDS110" }

// Close does nothing.
func (s *Sim) Close() error { return nil }

func status(st Status, data ...byte) []byte {
	return append(binary.LittleEndian.AppendUint32(nil, uint32(int32(st))), data...)
}

// Exchange answers one command frame with its response payload.
func (s *Sim) Exchange(req []byte, _ time.Duration) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(req) < headerLen+1 || req[0] != frameStart || int(binary.LittleEndian.Uint16(req[1:])) != len(req)-headerLen {
		return nil, ErrShortReply
	}
	cmd, a := req[headerLen], req[headerLen+1:]
	s.Commands = append(s.Commands, cmd)
	if s.failNext != 0 {
		st := s.failNext
		s.failNext = 0
		return status(st), nil
	}
	switch cmd {
	case CmdConnect:
		s.connected = true
		return status(0), nil
	case CmdDisconnect:
		s.connected, s.swd, s.cmapi, s.acquired, s.cjtag = false, false, false, false, false
		return status(0), nil
	}
	if !s.connected {
		return status(StatusFail), nil
	}
	switch cmd {
	case CmdVersion:
		d := binary.LittleEndian.AppendUint32(nil, s.Firmware)
		return status(0, binary.LittleEndian.AppendUint16(d, s.Hardware)...), nil
	case CmdSetTCK:
		if len(a) < 4 {
			return status(StatusFail), nil
		}
		s.tck = binary.LittleEndian.Uint32(a)
	case CmdSetTRST:
		s.trst = len(a) > 0 && a[0] != 0
	case CmdSetSRST:
		level := len(a) > 0 && a[0] != 0
		// A falling edge on a wired SRST resets the simulated core.
		if s.srst && !level && !s.ignoreSRST {
			s.pinResets++
			_ = s.core.Write32(context.Background(), cortexm.AddrAIRCR, 0x05FA0004)
		}
		s.srst = level
	case CmdCycleTCK:
	case CmdCJTAGConnect:
		s.cjtag = true
		s.tapState = 1
	case CmdCJTAGDisc:
		s.cjtag = false
	case CmdSWDConnect:
		s.swd = true
	case CmdSWDDisconnect:
		s.swd, s.cmapi = false, false
	case CmdCMAPIConnect:
		if s.noTarget || !s.swd {
			return status(StatusSWDDevID), nil
		}
		s.cmapi = true
		return status(0, binary.LittleEndian.AppendUint32(nil, simDPIDR)...), nil
	case CmdCMAPIDisc:
		s.cmapi, s.acquired = false, false
	case CmdCMAPIAcquire:
		s.acquired = s.cmapi
	case CmdCMAPIRelease:
		s.acquired = false
	case CmdSetSupply:
		if s.Hardware != HWStandAlone || len(a) < 5 {
			return status(StatusFail), nil
		}
		s.supplyMV, s.supplyOn = binary.LittleEndian.Uint32(a), a[4] != 0
	case CmdGotoState:
		if len(a) < 4 {
			return status(StatusFail), nil
		}
		s.tapState = int(binary.LittleEndian.Uint32(a))
	case CmdJTAGScan:
		return s.jtagScan(a), nil
	case CmdDapRequest:
		return s.dapRequest(a), nil
	default:
		return status(StatusFail), nil
	}
	return status(0), nil
}

// JTAG model: the chain shifts IDCODEs (or BYPASS bits) after reset.

func (s *Sim) jtagScan(a []byte) []byte {
	if !s.cjtag || len(a) < 18 {
		return status(StatusFail)
	}
	nbits := int(binary.LittleEndian.Uint16(a))
	path, end := a[2], a[4]
	if end == byte(StExit1DR) || end == byte(StExit1IR) {
		return status(-122)
	}
	in := a[18:]
	if len(in) < (nbits+7)/8 {
		return status(StatusFail)
	}
	var stream []bool // bits shifted out of the chain, in order
	if path == byte(StShiftIR) {
		for i, t := range s.chain {
			for b := 0; b < t.irLen; b++ {
				stream = append(stream, b == 0) // capture pattern ...01
			}
			_ = i
		}
	} else {
		for i, t := range s.chain {
			if s.ir[i] == 0 || s.tapState == int(StReset) {
				for b := 0; b < 32; b++ {
					stream = append(stream, t.idcode>>b&1 == 1)
				}
			} else {
				stream = append(stream, false)
			}
		}
	}
	out := make([]byte, (nbits+7)/8)
	var loaded []bool
	for i := 0; i < nbits; i++ {
		inb := in[i/8]>>(i%8)&1 == 1
		loaded = append(loaded, inb)
		var o bool
		if i < len(stream) {
			o = stream[i]
		} else if j := i - len(stream); j < len(loaded) {
			o = loaded[j]
		}
		if o {
			out[i/8] |= 1 << (i % 8)
		}
	}
	if path == byte(StShiftIR) {
		pos := 0
		for i, t := range s.chain {
			var v uint32
			for b := 0; b < t.irLen && pos < len(loaded); b++ {
				if loaded[pos] {
					v |= 1 << b
				}
				pos++
			}
			s.ir[i] = v
		}
	}
	s.tapState = int(end)
	return status(0, out...)
}

// SWD model with posted AP reads.

func (s *Sim) dapRequest(a []byte) []byte {
	if !s.swd || !s.cmapi {
		return status(StatusFail)
	}
	if s.noTarget {
		return status(StatusSWDDevID)
	}
	if s.waitNext > 0 {
		s.waitNext--
		return status(StatusSWDWait)
	}
	var out []byte
	for i := 0; i < len(a) && a[i] != 0; {
		r := a[i]
		i++
		ap, read, reg := r&swdAP != 0, r&swdRead != 0, (r&0x18)>>1
		if s.sticky&ctrlStickyErr != 0 && (ap || (!read && reg == 8)) {
			return status(StatusSWDFault)
		}
		if read {
			out = binary.LittleEndian.AppendUint32(out, s.read(ap, reg))
			continue
		}
		if i+4 > len(a) {
			return status(StatusFail)
		}
		s.write(ap, reg, binary.LittleEndian.Uint32(a[i:]))
		i += 4
	}
	return status(0, out...)
}

func (s *Sim) read(ap bool, reg byte) uint32 {
	if !ap {
		switch reg {
		case 0:
			return simDPIDR
		case 4:
			v := s.ctrlReq | s.sticky
			if s.ctrlReq&ctrlCDBGPwrReq != 0 {
				v |= ctrlCDBGPwrAck
			}
			if s.ctrlReq&ctrlCSYSPwrReq != 0 {
				v |= ctrlCSYSPwrAck
			}
			return v
		case 0xc:
			return s.post
		}
		return 0
	}
	prev := s.post
	var v uint32
	if s.cc != nil {
		if cv, ok := s.cc.Read(uint8(s.dpSelect>>24), s.dpSelect&0xf0|uint32(reg)); ok {
			if s.cc.TakeFault() {
				s.sticky |= ctrlStickyErr
			}
			s.post = cv
			return prev
		}
	}
	if s.dpSelect>>24 == 0 {
		switch s.dpSelect&0xf0 | uint32(reg) {
		case apCSW:
			v = s.csw
		case apTAR:
			v = s.tar
		case apDRW:
			v = s.drwRead()
		case apBase:
			v = 0xE00FF003
		case apIDR:
			v = simAPIDR
		}
	}
	s.post = v
	return prev
}

func (s *Sim) write(ap bool, reg byte, v uint32) {
	if !ap {
		switch reg {
		case 0:
			if v&(1<<2) != 0 {
				s.sticky &^= ctrlStickyErr
			}
		case 4:
			s.ctrlReq = v & (ctrlCDBGPwrReq | ctrlCSYSPwrReq)
		case 8:
			s.dpSelect = v
		}
		return
	}
	if s.cc != nil && s.cc.Write(uint8(s.dpSelect>>24), s.dpSelect&0xf0|uint32(reg), v) {
		if s.cc.TakeFault() {
			s.sticky |= ctrlStickyErr
		}
		return
	}
	if s.dpSelect>>24 != 0 {
		return
	}
	switch s.dpSelect&0xf0 | uint32(reg) {
	case apCSW:
		s.csw = v
	case apTAR:
		s.tar = v
	case apDRW:
		s.drwWrite(v)
	}
}

func (s *Sim) cswSize() int { return 1 << (s.csw & 7) }

func (s *Sim) advance() {
	if s.csw&0x30 == 0x10 {
		s.tar = s.tar&^0x3FF | (s.tar+uint32(s.cswSize()))&0x3FF
	}
}

func (s *Sim) drwRead() uint32 {
	size, addr := s.cswSize(), s.tar
	if size > 4 || addr&uint32(size-1) != 0 {
		s.sticky |= ctrlStickyErr
		return 0
	}
	v, err := s.core.Load(addr, size)
	if err != nil {
		s.sticky |= ctrlStickyErr
	}
	s.advance()
	return v << (8 * (addr & 3))
}

func (s *Sim) drwWrite(v uint32) {
	size, addr := s.cswSize(), s.tar
	if size > 4 || addr&uint32(size-1) != 0 || addr < simRAMBase && addr < 0xE0000000 ||
		s.core.Store(addr, size, v>>(8*(addr&3))) != nil {
		s.sticky |= ctrlStickyErr
	}
	s.advance()
}
