package dap

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync"
	"time"

	"github.com/faronov/hwlab/internal/cortexm"
	"github.com/faronov/hwlab/internal/ti/cc23xx/ccsim"
)

// Sim is a SYNTHETIC CMSIS-DAP probe with a Cortex-M target behind it. It
// speaks the DAP command protocol on the wire (it is a [Transport]) and models
// a SW-DP, an AHB MEM-AP, 64 KiB of RAM at 0x20000000, 64 KiB of read-only
// flash at 0 and the Cortex-M debug registers.
//
// It is the author's reading of the specifications. It is NOT evidence that a
// real probe behaves the same way; nothing here has run on real hardware.
type Sim struct {
	mu sync.Mutex

	// Packet is the DAP packet size the probe reports (default 64).
	Packet int

	// core is the behavioural Cortex-M model behind the MEM-AP: SRAM, flash,
	// SCS, FPB and DWT.
	core      *cortexm.Sim
	cc        *ccsim.Device // optional CC23xx personality, see NewSimCC23xx
	pinResets int

	// DAP state.
	connected bool
	switched  bool // JTAG-to-SWD sequence seen
	dpSelect  uint32
	ctrlReq   uint32
	sticky    uint32
	csw, tar  uint32
	ctrlPins  byte
	resetLow  bool

	// Test hooks.
	waitNext     int
	noTarget     bool
	dropOnPinRst bool
	ignorePinRst bool
	packets      int
}

const (
	simRAMBase = 0x20000000
	simDPIDR   = 0x2BA01477
	simAPIDR   = 0x24770011

	// DP register addresses and CTRL/STAT bits the simulator models.
	apCSW          = 0x00
	apTAR          = 0x04
	apDRW          = 0x0C
	apCFG          = 0xF4
	apBase         = 0xF8
	apIDR          = 0xFC
	dpDPIDR        = 0x0
	dpCtrl         = 0x4
	dpSelect       = 0x8
	ctrlStickyOrun = 1 << 1
	ctrlStickyCmp  = 1 << 4
	ctrlStickyErr  = 1 << 5
	ctrlCDBGPwrReq = 1 << 28
	ctrlCDBGPwrAck = 1 << 29
	ctrlCSYSPwrReq = 1 << 30
	ctrlCSYSPwrAck = 1 << 31
)

// NewSim returns a simulator on a fresh [cortexm.Sim] (flash at 0, SRAM at
// 0x20000000, FPB and DWT).
func NewSim() *Sim {
	s := &Sim{Packet: 64, core: cortexm.NewSim()}
	s.ctrlPins = PinnRESET | PinnTRST
	s.csw = 0x23000040
	return s
}

// NewSimM0Plus returns a simulator whose core is shaped like the CC2340's
// Cortex-M0+: four breakpoint comparators and no DWT.
func NewSimM0Plus() *Sim {
	s := NewSim()
	s.core = cortexm.NewSimM0Plus()
	return s
}

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

// Core returns the simulated Cortex-M model, for tests that need to inspect
// or program it.
func (s *Sim) Core() *cortexm.Sim { return s.core }

// SimProbe returns a ready [Probe] on a fresh simulator.
func SimProbe() (*Probe, *Sim, error) {
	s := NewSim()
	p, err := NewProbe(s)
	return p, s, err
}

// Test hooks.

// InjectWait makes the next n transfers answer WAIT.
func (s *Sim) InjectWait(n int) { s.mu.Lock(); s.waitNext = n; s.mu.Unlock() }

// SetNoTarget makes every transfer answer "no ACK", as with an unwired line.
func (s *Sim) SetNoTarget(v bool) { s.mu.Lock(); s.noTarget = v; s.mu.Unlock() }

// DropDPOnPinReset makes the nRESET pin take the debug port down, as many
// real parts do.
// IgnorePinReset models an nRESET line that is not wired to the target: the
// probe pulses its pin but the core never resets.
func (s *Sim) IgnorePinReset(v bool) { s.mu.Lock(); s.ignorePinRst = v; s.mu.Unlock() }

func (s *Sim) DropDPOnPinReset(v bool) { s.mu.Lock(); s.dropOnPinRst = v; s.mu.Unlock() }

// Packets returns how many command packets were exchanged.
func (s *Sim) Packets() int { s.mu.Lock(); defer s.mu.Unlock(); return s.packets }

// Resets returns how many target resets happened.
func (s *Sim) Resets() int { s.mu.Lock(); defer s.mu.Unlock(); return s.pinResets + s.core.Resets }

// Byte returns a byte of simulated memory; ok is false when unmapped.
func (s *Sim) Byte(addr uint32) (byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readByte(addr)
}

// Transport implementation.

func (s *Sim) PacketSize() int  { return s.Packet }
func (s *Sim) Describe() string { return "SYNTHETIC simulated CMSIS-DAP probe" }
func (s *Sim) Close() error     { return nil }

// Exchange answers one DAP command.
func (s *Sim) Exchange(req []byte, _ time.Duration) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.packets++
	if len(req) == 0 {
		return nil, fmt.Errorf("empty DAP request")
	}
	if len(req) > s.Packet {
		return nil, fmt.Errorf("request of %d bytes exceeds the %d-byte packet size", len(req), s.Packet)
	}
	switch req[0] {
	case cmdInfo:
		return s.info(req), nil
	case cmdHostStatus, cmdTransferCfg, cmdSWJClock, cmdSWDConfigure, cmdDelay:
		return []byte{req[0], 0}, nil
	case cmdConnect:
		if len(req) < 2 {
			return []byte{cmdConnect, 0}, nil
		}
		switch req[1] {
		case PortDefault, PortSWD:
			s.connected = true
			return []byte{cmdConnect, PortSWD}, nil
		}
		return []byte{cmdConnect, 0}, nil
	case cmdDisconnect:
		s.connected, s.switched = false, false
		return []byte{cmdDisconnect, 0}, nil
	case cmdWriteABORT:
		if len(req) >= 6 {
			s.abort(binary.LittleEndian.Uint32(req[2:]))
		}
		return []byte{cmdWriteABORT, 0}, nil
	case cmdResetTarget:
		return []byte{cmdResetTarget, 0, 0}, nil
	case cmdSWJPins:
		return s.pins(req), nil
	case cmdSWJSequence:
		return s.sequence(req), nil
	case cmdTransfer:
		return s.transfer(req), nil
	case cmdTransferBlk:
		return s.block(req), nil
	}
	return []byte{respInvalid}, nil
}

func (s *Sim) info(req []byte) []byte {
	if len(req) < 2 {
		return []byte{cmdInfo, 0}
	}
	str := func(v string) []byte { return append([]byte{cmdInfo, byte(len(v) + 1)}, append([]byte(v), 0)...) }
	switch req[1] {
	case InfoVendor:
		return str("hwlab")
	case InfoProduct:
		return str("Simulated CMSIS-DAP (synthetic)")
	case InfoSerial:
		return str("SIM0001")
	case InfoProtocolVer:
		return str("2.1.0")
	case InfoCaps:
		return []byte{cmdInfo, 1, CapSWD}
	case InfoPacketCount:
		return []byte{cmdInfo, 1, 1}
	case InfoPacketSize:
		return []byte{cmdInfo, 2, byte(s.Packet), byte(s.Packet >> 8)}
	}
	return []byte{cmdInfo, 0}
}

func (s *Sim) sequence(req []byte) []byte {
	if len(req) < 2 {
		return []byte{cmdSWJSequence, 0xFF}
	}
	bits := int(req[1])
	if bits == 0 {
		bits = 256
	}
	data := req[2:]
	if len(data) != (bits+7)/8 {
		return []byte{cmdSWJSequence, 0xFF}
	}
	if bits == 16 && data[0] == 0x9E && data[1] == 0xE7 {
		s.switched = true
	}
	return []byte{cmdSWJSequence, 0}
}

func (s *Sim) pins(req []byte) []byte {
	if len(req) < 7 {
		return []byte{cmdSWJPins, 0}
	}
	out, sel := req[1], req[2]
	if sel&PinnRESET != 0 {
		if out&PinnRESET == 0 {
			s.resetLow = true
			s.ctrlPins &^= PinnRESET
		} else {
			s.ctrlPins |= PinnRESET
			if s.resetLow {
				s.resetLow = false
				if s.ignorePinRst {
					return []byte{cmdSWJPins, s.ctrlPins}
				}
				s.coreReset()
				if s.dropOnPinRst {
					s.switched, s.ctrlReq, s.dpSelect = false, 0, 0
				}
			}
		}
	}
	return []byte{cmdSWJPins, s.ctrlPins}
}

func (s *Sim) abort(v uint32) {
	if v&(1<<2) != 0 {
		s.sticky &^= ctrlStickyErr
	}
	if v&(1<<1) != 0 {
		s.sticky &^= ctrlStickyCmp
	}
	if v&(1<<4) != 0 {
		s.sticky &^= ctrlStickyOrun
	}
}

// Request handling shared by DAP_Transfer and DAP_TransferBlock.

// gate decides the ACK of the next request.
func (s *Sim) gate(r byte) byte {
	apndp, rnw, a := r&1 != 0, r&2 != 0, r&0x0C
	if s.noTarget || !s.connected || !s.switched {
		return AckNone
	}
	if s.waitNext > 0 {
		s.waitNext--
		return AckWait
	}
	if s.sticky&ctrlStickyErr != 0 && (apndp || (!rnw && a == dpSelect)) {
		return AckFault
	}
	return AckOK
}

func (s *Sim) transfer(req []byte) []byte {
	if len(req) < 3 {
		return []byte{cmdTransfer, 0, AckNone}
	}
	count := int(req[2])
	body := req[3:]
	resp := []byte{cmdTransfer, 0, AckOK}
	for i := 0; i < count; i++ {
		if len(body) < 1 {
			return append(resp[:2], AckFault)
		}
		r := body[0]
		body = body[1:]
		ack := s.gate(r)
		if ack != AckOK {
			resp[2] = ack
			return resp
		}
		if r&2 != 0 {
			resp = binary.LittleEndian.AppendUint32(resp, s.read(r))
		} else {
			if len(body) < 4 {
				resp[2] = AckFault
				return resp
			}
			s.write(r, binary.LittleEndian.Uint32(body))
			body = body[4:]
		}
		resp[1]++
	}
	return resp
}

func (s *Sim) block(req []byte) []byte {
	if len(req) < 5 {
		return []byte{cmdTransferBlk, 0, 0, AckFault}
	}
	count := int(binary.LittleEndian.Uint16(req[2:]))
	r := req[4]
	body := req[5:]
	resp := []byte{cmdTransferBlk, 0, 0, AckOK}
	done := 0
	for i := 0; i < count; i++ {
		ack := s.gate(r)
		if ack != AckOK {
			resp[3] = ack
			break
		}
		if r&2 != 0 {
			resp = binary.LittleEndian.AppendUint32(resp, s.read(r))
		} else {
			if len(body) < 4 {
				resp[3] = AckFault
				break
			}
			s.write(r, binary.LittleEndian.Uint32(body))
			body = body[4:]
		}
		done++
	}
	binary.LittleEndian.PutUint16(resp[1:], uint16(done))
	return resp
}

// DP and AP registers.

func (s *Sim) read(r byte) uint32 {
	a := r & 0x0C
	if r&1 == 0 {
		switch a {
		case dpDPIDR:
			return simDPIDR
		case dpCtrl:
			v := s.ctrlReq | s.sticky
			if s.ctrlReq&ctrlCDBGPwrReq != 0 {
				v |= ctrlCDBGPwrAck
			}
			if s.ctrlReq&ctrlCSYSPwrReq != 0 {
				v |= ctrlCSYSPwrAck
			}
			return v
		}
		return 0
	}
	if s.cc != nil {
		if cv, ok := s.cc.Read(uint8(s.dpSelect>>24), s.dpSelect&0xF0|uint32(a)); ok {
			if s.cc.TakeFault() {
				s.sticky |= ctrlStickyErr
			}
			return cv
		}
	}
	if s.dpSelect>>24 != 0 {
		return 0 // no such AP
	}
	switch s.dpSelect&0xF0 | uint32(a) {
	case apCSW:
		return s.csw
	case apTAR:
		return s.tar
	case apDRW:
		return s.drwRead()
	case apCFG:
		return 0
	case apBase:
		return 0xE00FF003
	case apIDR:
		return simAPIDR
	}
	return 0
}

func (s *Sim) write(r byte, v uint32) {
	a := r & 0x0C
	if r&1 == 0 {
		switch a {
		case dpDPIDR: // ABORT
			s.abort(v)
		case dpCtrl:
			s.ctrlReq = v & (ctrlCDBGPwrReq | ctrlCSYSPwrReq)
		case dpSelect:
			s.dpSelect = v
		}
		return
	}
	if s.cc != nil && s.cc.Write(uint8(s.dpSelect>>24), s.dpSelect&0xF0|uint32(a), v) {
		if s.cc.TakeFault() {
			s.sticky |= ctrlStickyErr
		}
		return
	}
	if s.dpSelect>>24 != 0 {
		return
	}
	switch s.dpSelect&0xF0 | uint32(a) {
	case apCSW:
		s.csw = v
	case apTAR:
		s.tar = v
	case apDRW:
		s.drwWrite(v)
	}
}

func (s *Sim) cswSize() int { return 1 << (s.csw & 7) }

// advance applies the single auto-increment, which wraps inside 1 KiB.
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
	v, ok := s.memRead(addr, size)
	if !ok {
		s.sticky |= ctrlStickyErr
	}
	s.advance()
	return v
}

func (s *Sim) drwWrite(v uint32) {
	size, addr := s.cswSize(), s.tar
	if size > 4 || addr&uint32(size-1) != 0 || !s.memWrite(addr, size, v) {
		s.sticky |= ctrlStickyErr
	}
	s.advance()
}

// Memory.

func (s *Sim) readByte(addr uint32) (byte, bool) {
	v, err := s.core.Load(addr, 1)
	if err != nil {
		return 0, false
	}
	return byte(v), true
}

// memRead returns the item in its byte lane of the 32-bit DRW.
func (s *Sim) memRead(addr uint32, size int) (uint32, bool) {
	if addr >= 0xE000E000 && addr < 0xE000F000 && size != 4 {
		return 0, false
	}
	v, err := s.core.Load(addr, size)
	if err != nil {
		return 0, false
	}
	return v << (8 * (addr & 3)), true
}

func (s *Sim) memWrite(addr uint32, size int, v uint32) bool {
	if addr >= 0xE000E000 && addr < 0xE000F000 && size != 4 {
		return false
	}
	if addr < 0x20000000 {
		return false // flash is read-only here
	}
	return s.core.Store(addr, size, v>>(8*(addr&3))) == nil
}

// coreReset is a system reset through the simulated AIRCR.
func (s *Sim) coreReset() {
	s.pinResets++
	_ = s.core.Write32(context.Background(), cortexm.AddrAIRCR, 0x05FA0004)
}
