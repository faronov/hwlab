// Package sim is a SYNTHETIC J-Link probe for tests. It is a usb.Transport:
// it answers the probe command set on the wire and decodes SWD bit sequences
// cycle by cycle against a modelled SW-DP, an AHB MEM-AP and a Cortex-M
// (internal/cortexm.Sim).
//
// It is the author's reading of the SWD specification and of what was
// observed on one real probe. It is NOT evidence that a probe behaves this
// way: nothing here has run on hardware, and replays of it must never be
// labelled REAL_HW_VERIFIED.
package sim

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync"

	"github.com/faronov/hwlab/internal/cortexm"
	"github.com/faronov/hwlab/plugins/jlinknative/jlerr"
	"github.com/faronov/hwlab/plugins/jlinknative/protocol"
)

const (
	dpIDR  = 0x2BA01477
	apIDR  = 0x24770011
	ramLo  = 0x20000000
	csw    = 0x00
	tar    = 0x04
	drw    = 0x0C
	cfg    = 0xF4
	base   = 0xF8
	idr    = 0xFC
	ctrlOR = 1 << 1
	ctrlCM = 1 << 4
	ctrlER = 1 << 5
)

// Sim is the probe plus target.
type Sim struct {
	mu   sync.Mutex
	core *cortexm.Sim

	// Serial is reported by Describe.
	Serial string

	pending []byte // reply bytes the host has not read yet
	wedged  bool

	iface    uint32
	speedKHz uint16
	vtarget  uint16
	resetLow bool

	// JTAG scan chain model (see jtag.go).
	tap tapModel

	// Wire state.
	switched bool
	window   uint16
	ones     int

	// DP/AP state.
	sel, ctrlReq, sticky uint32
	cswv, tarv           uint32
	rdbuff               uint32

	// Test hooks.
	noTarget   bool
	waitNext   int
	faultNext  int
	flipParity int
	cmds       []byte
	seqs       int
	linkResets int
	resetPulse int
}

// New returns a simulator with a Cortex-M4 target at 3.3 V.
func New() *Sim { return NewWith(cortexm.NewSim()) }

// NewWith returns a simulator on a given core model.
func NewWith(c *cortexm.Sim) *Sim {
	return &Sim{core: c, Serial: "SIM00000001", vtarget: 3300, cswv: 0x23000040}
}

// Core exposes the core model for tests.
func (s *Sim) Core() *cortexm.Sim { return s.core }

// SetNoTarget makes the data line float: no ACK for any request.
func (s *Sim) SetNoTarget(v bool) { s.mu.Lock(); s.noTarget = v; s.mu.Unlock() }

// SetVTarget sets the voltage the probe senses, in mV.
func (s *Sim) SetVTarget(mv uint16) { s.mu.Lock(); s.vtarget = mv; s.mu.Unlock() }

// InjectWait makes the next n requests answer WAIT.
func (s *Sim) InjectWait(n int) { s.mu.Lock(); s.waitNext = n; s.mu.Unlock() }

// InjectFault makes the next n AP requests answer FAULT.
func (s *Sim) InjectFault(n int) { s.mu.Lock(); s.faultNext = n; s.mu.Unlock() }

// CorruptParity flips the parity bit of the next n read data phases.
func (s *Sim) CorruptParity(n int) { s.mu.Lock(); s.flipParity = n; s.mu.Unlock() }

// Sequences counts bit-sequence commands, LineResets the line resets seen.
func (s *Sim) Sequences() int   { s.mu.Lock(); defer s.mu.Unlock(); return s.seqs }
func (s *Sim) LineResets() int  { s.mu.Lock(); defer s.mu.Unlock(); return s.linkResets }
func (s *Sim) SpeedKHz() uint16 { s.mu.Lock(); defer s.mu.Unlock(); return s.speedKHz }

// Commands returns the command ids received, in order.
func (s *Sim) Commands() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.cmds...)
}

// Describe implements usb.Transport.
func (s *Sim) Describe() string { return "SYNTHETIC simulated J-Link " + s.Serial }

// Close implements usb.Transport.
func (s *Sim) Close() error { return nil }

// Exchange implements usb.Transport with the real probe's framing rules: a
// reply may be read in several transfers, and sending a command while reply
// bytes are unread wedges the probe, as observed on hardware.
func (s *Sim) Exchange(_ context.Context, cmd []byte, n int) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.wedged {
		return nil, jlerr.New(jlerr.USBTimeout, "sim", fmt.Errorf("simulated probe wedged"))
	}
	if len(cmd) > 0 {
		if len(s.pending) != 0 {
			s.wedged = true
			return nil, jlerr.New(jlerr.USBTimeout, "sim", fmt.Errorf("command 0x%02x sent with %d reply bytes unread", cmd[0], len(s.pending)))
		}
		s.cmds = append(s.cmds, cmd[0])
		rep, err := s.handle(cmd)
		if err != nil {
			return nil, err
		}
		s.pending = rep
	}
	if len(s.pending) < n {
		return nil, jlerr.New(jlerr.USBTimeout, "sim", fmt.Errorf("reply of %d bytes requested, %d available", n, len(s.pending)))
	}
	out := append([]byte(nil), s.pending[:n]...)
	s.pending = s.pending[n:]
	return out, nil
}

func (s *Sim) handle(cmd []byte) ([]byte, error) {
	need := func(n int) error {
		if len(cmd) < n {
			s.wedged = true
			return jlerr.New(jlerr.UnsupportedProbeCommand, "sim", fmt.Errorf("command 0x%02x truncated", cmd[0]))
		}
		return nil
	}
	u32 := func(v uint32) []byte { return binary.LittleEndian.AppendUint32(nil, v) }
	switch cmd[0] {
	case protocol.CmdVersion:
		txt := []byte("SIM J-Link V9 (synthetic)\x00Copyright 2003-2012 SEGGER: www.segger.com\x00")
		return append(binary.LittleEndian.AppendUint16(nil, uint16(len(txt))), txt...), nil
	case protocol.CmdGetCaps:
		return u32(0xB9FF7BBF), nil
	case protocol.CmdGetExtCaps:
		b := make([]byte, 32)
		copy(b, u32(0xB9FF7BBF))
		return b, nil
	case protocol.CmdGetHWVer:
		return u32(97000), nil
	case protocol.CmdGetHWStatus:
		b := make([]byte, 8)
		binary.LittleEndian.PutUint16(b, s.vtarget)
		copy(b[2:], []byte{0, 1, 0, 1, 1, 1})
		return b, nil
	case protocol.CmdGetSpeeds:
		b := make([]byte, 6)
		binary.LittleEndian.PutUint32(b, 12000000)
		binary.LittleEndian.PutUint16(b[4:], 1)
		return b, nil
	case protocol.CmdSelectIF:
		if err := need(2); err != nil {
			return nil, err
		}
		if cmd[1] == 0xFF {
			return u32(0x7B), nil
		}
		prev := s.iface
		s.iface = uint32(cmd[1])
		return u32(prev), nil
	case protocol.CmdSetSpeed:
		if err := need(3); err != nil {
			return nil, err
		}
		s.speedKHz = binary.LittleEndian.Uint16(cmd[1:])
		return nil, nil
	case protocol.CmdResetLow:
		s.resetLow = true
		return nil, nil
	case protocol.CmdResetHigh:
		s.resetLow = false
		return nil, nil
	case protocol.CmdSequence:
		return s.sequence(cmd)
	}
	s.wedged = true
	return nil, jlerr.New(jlerr.UnsupportedProbeCommand, "sim", fmt.Errorf("unknown command 0x%02x", cmd[0]))
}

func getBit(v []byte, i int) byte { return (v[i/8] >> (i % 8)) & 1 }

func par(v uint32) byte {
	v ^= v >> 16
	v ^= v >> 8
	v ^= v >> 4
	v ^= v >> 2
	v ^= v >> 1
	return byte(v & 1)
}

// sequence decodes an SWD bit vector. tout[c] is the level the target drives
// in wire cycle c (when the probe releases the line); reply bit i is tout[i+1].
func (s *Sim) sequence(cmd []byte) ([]byte, error) {
	if len(cmd) >= 4 && s.iface == uint32(protocol.IfJTAG) {
		return s.jtagSequence(cmd)
	}
	if len(cmd) < 4 || s.iface != uint32(protocol.IfSWD) {
		s.wedged = true
		return nil, jlerr.New(jlerr.UnsupportedProbeCommand, "sim", fmt.Errorf("sequence in interface %d", s.iface))
	}
	n := int(binary.LittleEndian.Uint16(cmd[2:]))
	nb := (n + 7) / 8
	if len(cmd) != 4+2*nb {
		s.wedged = true
		return nil, jlerr.New(jlerr.ProtocolError, "sim", fmt.Errorf("sequence length %d for %d bits", len(cmd), n))
	}
	s.seqs++
	dir, out := cmd[4:4+nb], cmd[4+nb:]
	drv := func(c int) bool { return c < n && getBit(dir, c) == 1 }
	val := func(c int) byte { return getBit(out, c) }
	tout := make([]byte, n+2)
	if s.noTarget {
		for i := range tout {
			tout[i] = 1
		}
	}
	for c := 0; c < n; {
		if !drv(c) {
			c++
			continue
		}
		// Line reset and JTAG-to-SWD select tracking on driven bits.
		if val(c) == 1 {
			s.ones++
		} else {
			if s.ones >= 50 {
				s.linkResets++
				s.sel, s.ctrlReq, s.sticky = 0, 0, 0
			}
			s.ones = 0
		}
		s.window = s.window>>1 | uint16(val(c))<<15
		if s.window == 0xE79E {
			s.switched = true
		}
		if val(c) != 1 || !s.switched || s.noTarget {
			c++
			continue
		}
		// Possible request: 8 driven bits start,APnDP,RnW,A2,A3,parity,stop,park.
		ok := c+8 <= n
		var r [8]byte
		for i := 0; ok && i < 8; i++ {
			ok = drv(c + i)
			if ok {
				r[i] = val(c + i)
			}
		}
		if !ok || r[6] != 0 || r[7] != 1 || r[5] != (r[1]^r[2]^r[3]^r[4]) {
			c++
			continue
		}
		ap, rd := r[1] == 1, r[2] == 1
		reg := r[3]<<2 | r[4]<<3
		ack := s.gate(ap, rd, reg)
		for i := 0; i < 3; i++ { // ACK occupies cycles c+9..c+11
			tout[c+9+i] = byte(ack>>i) & 1
		}
		consumed := 8
		if ack == 0b001 {
			if rd {
				v := s.read(ap, reg)
				p := par(v)
				if s.flipParity > 0 {
					s.flipParity--
					p ^= 1
				}
				for i := 0; i < 32; i++ {
					tout[c+12+i] = byte(v>>i) & 1
				}
				tout[c+44] = p
				consumed = 45
			} else {
				// Data follows after the turnaround, at cycles c+13..c+45.
				if c+46 > n {
					c += 8
					continue
				}
				var v uint32
				for i := 0; i < 32; i++ {
					v |= uint32(val(c+13+i)) << i
				}
				if par(v) == val(c+45) {
					s.write(ap, reg, v)
				}
				consumed = 46
			}
		}
		c += consumed
		s.ones, s.window = 0, 0
	}
	reply := make([]byte, nb+1)
	for i := 0; i < n; i++ {
		reply[i/8] |= tout[i+1] << (i % 8)
	}
	return reply, nil
}

func (s *Sim) gate(ap, rd bool, reg byte) byte {
	if s.waitNext > 0 {
		s.waitNext--
		return 0b010
	}
	if ap && s.faultNext > 0 {
		s.faultNext--
		s.sticky |= ctrlER
	}
	if s.sticky&ctrlER != 0 && (ap || (!rd && reg == 0x8)) {
		return 0b100
	}
	return 0b001
}

func (s *Sim) read(ap bool, reg byte) uint32 {
	if !ap {
		switch reg {
		case 0x0:
			return dpIDR
		case 0x4:
			v := s.ctrlReq | s.sticky
			if s.ctrlReq&(1<<28) != 0 {
				v |= 1 << 29
			}
			if s.ctrlReq&(1<<30) != 0 {
				v |= 1 << 31
			}
			return v
		case 0xC:
			return s.rdbuff
		}
		return 0
	}
	// Posted: return the previous result and start the new read.
	prev := s.rdbuff
	s.rdbuff = s.apRead(s.sel&0xF0 | uint32(reg))
	return prev
}

func (s *Sim) write(ap bool, reg byte, v uint32) {
	if !ap {
		switch reg {
		case 0x0:
			if v&(1<<2) != 0 {
				s.sticky &^= ctrlER
			}
			if v&(1<<1) != 0 {
				s.sticky &^= ctrlCM
			}
			if v&(1<<4) != 0 {
				s.sticky &^= ctrlOR
			}
		case 0x4:
			s.ctrlReq = v & (1<<28 | 1<<30)
		case 0x8:
			s.sel = v
		}
		return
	}
	if s.sel>>24 != 0 {
		return
	}
	switch s.sel&0xF0 | uint32(reg) {
	case csw:
		s.cswv = v
	case tar:
		s.tarv = v
	case drw:
		s.drwWrite(v)
	}
}

func (s *Sim) apRead(a uint32) uint32 {
	if s.sel>>24 != 0 {
		return 0
	}
	switch a {
	case csw:
		return s.cswv
	case tar:
		return s.tarv
	case drw:
		return s.drwRead()
	case base:
		return 0xE00FF003
	case idr:
		return apIDR
	}
	return 0
}

func (s *Sim) size() int { return 1 << (s.cswv & 7) }

func (s *Sim) advance() {
	if s.cswv&0x30 == 0x10 {
		s.tarv = s.tarv&^0x3FF | (s.tarv+uint32(s.size()))&0x3FF
	}
}

func (s *Sim) drwRead() uint32 {
	size, addr := s.size(), s.tarv
	if size > 4 || addr&uint32(size-1) != 0 || (addr >= 0xE000E000 && addr < 0xE000F000 && size != 4) {
		s.sticky |= ctrlER
		return 0
	}
	v, err := s.core.Load(addr, size)
	if err != nil {
		s.sticky |= ctrlER
		return 0
	}
	s.advance()
	return v << (8 * (addr & 3))
}

func (s *Sim) drwWrite(v uint32) {
	size, addr := s.size(), s.tarv
	if size > 4 || addr&uint32(size-1) != 0 || (addr >= 0xE000E000 && addr < 0xE000F000 && size != 4) || addr < ramLo && addr < 0xE0000000 {
		s.sticky |= ctrlER
		return
	}
	if err := s.core.Store(addr, size, v>>(8*(addr&3))); err != nil {
		s.sticky |= ctrlER
	}
	s.advance()
}
