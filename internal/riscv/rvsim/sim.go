// Package rvsim is a SYNTHETIC RISC-V target: a bit-accurate IEEE 1149.1 TAP
// with the RISC-V debug transport module and a debug module (spec 0.13
// register layout) behind it. It exists to test and replay the transport and
// debug layers without hardware. Nothing it answers is evidence about real
// silicon; real captures are stored separately and labelled REAL.
//
// The model is written independently of package riscv so the two cross-check
// each other.
package rvsim

import (
	"context"
	"errors"
	"fmt"

	"github.com/faronov/hwlab/internal/jtag"
)

// Region is a block of simulated memory.
type Region struct {
	Base uint32
	Data []byte
}

// Device is a memory-mapped register block (for example a flash controller).
type Device struct {
	Base, Size uint32
	Read       func(off uint32) uint32
	Write      func(off, v uint32)
}

// Config describes the simulated target.
type Config struct {
	IDCode uint32 // default 0x0000dc25
	IRLen  int    // default 5
	Harts  int    // default 1
	Memory []Region
	// NoSBA removes System Bus Access (sbasize reads 0).
	NoSBA bool
	// BusyOps makes the first N DMI operations report busy (sticky until
	// dmireset), to exercise the retry path.
	BusyOps int
	// Active starts the module with dmactive already set.
	Active bool
	// CSRs overrides/adds CSR values (misa, mvendorid, ...).
	CSRs map[uint16]uint32
	// Devices are MMIO blocks reachable through System Bus Access.
	Devices []Device
	// Only32 removes 8/16-bit System Bus accesses.
	Only32 bool
	// Triggers is the number of trigger slots (default 2, -1 for none).
	Triggers int
	// Unavailable marks harts that report "unavailable".
	Unavailable map[int]bool
	// HartCSRs overrides CSRs of one hart.
	HartCSRs map[int]map[uint16]uint32
	// ResetDropsDM makes ndmreset clear the whole debug module state.
	ResetDropsDM bool
}

const (
	irIDCode = 0x01
	irDTMCS  = 0x10
	irDMI    = 0x11
	abits    = 7
)

// Target is the simulated chip. It is not safe for concurrent use.
type Target struct {
	cfg   Config
	state jtag.State
	ir    uint32
	irSh  uint64
	drSh  uint64
	drLen int

	// DTM
	sticky  int // 0 ok, 3 busy
	lastOp  uint32
	lastDat uint32
	lastAdr uint32
	busyRem int

	// DM
	active    bool
	hartsel   uint32
	harts     []*hart
	data0     uint32
	cmderr    uint32
	sbcs      uint32 // flags the host wrote
	sbaddr    uint32
	sbdata    uint32
	sberror   uint32
	sbBusyErr bool

	// Counters tests can look at.
	SBAReads  int
	SBAWrites int
	Halts     int
	Resumes   int
	Steps     int
	Resets    int
	DMIOps    int
	BusyCount int
}

type trigSlot struct{ t1, t2 uint32 }

type hart struct {
	halted, resumeAck bool
	haveReset         bool
	resetHaltReq      bool
	gpr               [32]uint32
	csr               map[uint16]uint32
	tsel              uint32
	trig              []trigSlot
}

// New returns a Target in Test-Logic-Reset.
func New(cfg Config) *Target {
	if cfg.IDCode == 0 {
		cfg.IDCode = 0x0000dc25
	}
	if cfg.IRLen == 0 {
		cfg.IRLen = 5
	}
	if cfg.Harts == 0 {
		cfg.Harts = 1
	}
	if cfg.Triggers == 0 {
		cfg.Triggers = 2
	}
	t := &Target{cfg: cfg, state: jtag.TestLogicReset, ir: irIDCode, busyRem: cfg.BusyOps, active: cfg.Active}
	for i := 0; i < cfg.Harts; i++ {
		h := &hart{csr: map[uint16]uint32{0x301: 0x40903105, 0xF11: 0x612, 0xF12: 0x80000002, 0xF13: 2, 0xF14: uint32(i), 0x7B1: 0x42000000, 0x7B0: 0xC3, 0x300: 0x1800}}
		for k, v := range cfg.CSRs {
			h.csr[k] = v
		}
		for k, v := range cfg.HartCSRs[i] {
			h.csr[k] = v
		}
		for j := range h.gpr {
			h.gpr[j] = 0x1000 + uint32(j) + uint32(i)<<16
		}
		h.gpr[0] = 0
		if cfg.Triggers > 0 {
			h.trig = make([]trigSlot, cfg.Triggers)
		}
		t.harts = append(t.harts, h)
	}
	return t
}

// Halted reports whether hart h is halted.
func (t *Target) Halted(h int) bool { return t.harts[h].halted }

// Active reports dmactive.
func (t *Target) Active() bool { return t.active }

// SetGPR sets a general purpose register of hart 0.
func (t *Target) SetGPR(n int, v uint32) { t.harts[0].gpr[n] = v }

// SetCSR sets a CSR of hart h.
func (t *Target) SetCSR(h int, reg uint16, v uint32) { t.harts[h].csr[reg] = v }

// CSR reads a CSR of hart h.
func (t *Target) CSR(h int, reg uint16) uint32 { return t.harts[h].csr[reg] }

// Trigger returns a trigger slot of hart h.
func (t *Target) Trigger(h, i int) (tdata1, tdata2 uint32) {
	s := t.harts[h].trig[i]
	return s.t1, s.t2
}

// Memory returns a copy of the bytes at addr, or nil if unmapped.
func (t *Target) Memory(addr uint32, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		v, ok := t.readSize(addr+uint32(i), 1)
		if !ok {
			return nil
		}
		out[i] = byte(v)
	}
	return out
}

// TDO is the level on TDO now (valid while shifting).
func (t *Target) tdo() bool {
	switch t.state {
	case jtag.ShiftDR:
		return t.drSh&1 == 1
	case jtag.ShiftIR:
		return t.irSh&1 == 1
	}
	return true
}

// Clock applies one rising TCK edge and returns the TDO level sampled at it.
func (t *Target) Clock(tms, tdi bool) bool {
	tdo := t.tdo()
	switch t.state {
	case jtag.ShiftDR:
		t.drSh >>= 1
		if tdi {
			t.drSh |= 1 << (t.drLen - 1)
		}
	case jtag.ShiftIR:
		t.irSh >>= 1
		if tdi {
			t.irSh |= 1 << (t.cfg.IRLen - 1)
		}
	}
	t.state = jtag.Next(t.state, tms)
	switch t.state {
	case jtag.TestLogicReset:
		t.ir = irIDCode
	case jtag.CaptureIR:
		t.irSh = 0x01
	case jtag.UpdateIR:
		t.ir = uint32(t.irSh)
	case jtag.CaptureDR:
		t.captureDR()
	case jtag.UpdateDR:
		t.updateDR()
	}
	return tdo
}

func (t *Target) dtmcs() uint64 {
	return 1 | abits<<4 | uint64(t.sticky)<<10 | 1<<12
}

func (t *Target) captureDR() {
	switch t.ir {
	case irIDCode:
		t.drSh, t.drLen = uint64(t.cfg.IDCode), 32
	case irDTMCS:
		t.drSh, t.drLen = t.dtmcs(), 32
	case irDMI:
		op := t.lastOp
		if t.sticky == 3 {
			op = 3
		}
		t.drSh, t.drLen = uint64(op)|uint64(t.lastDat)<<2|uint64(t.lastAdr)<<34, abits+34
	default:
		t.drSh, t.drLen = 0, 1 // BYPASS
	}
}

func (t *Target) updateDR() {
	switch t.ir {
	case irDTMCS:
		if t.drSh>>16&1 == 1 {
			t.sticky, t.lastOp = 0, 0
		}
	case irDMI:
		op := uint32(t.drSh & 3)
		data := uint32(t.drSh >> 2)
		addr := uint32(t.drSh>>34) & (1<<abits - 1)
		if op == 0 {
			return
		}
		t.DMIOps++
		if t.sticky == 3 {
			return
		}
		if t.busyRem > 0 {
			t.busyRem--
			t.sticky, t.lastOp, t.BusyCount = 3, 3, t.BusyCount+1
			return
		}
		t.lastAdr, t.lastOp = addr, 0
		if op == 1 {
			t.lastDat = t.dmRead(addr)
		} else {
			t.lastDat = 0
			t.dmWrite(addr, data)
		}
	}
}

func (t *Target) cur() (*hart, int, bool) {
	h := int(t.hartsel)
	if h < len(t.harts) {
		return t.harts[h], h, true
	}
	return nil, h, false
}

func (t *Target) dmRead(a uint32) uint32 {
	switch a {
	case 0x04:
		return t.data0
	case 0x10:
		if t.active {
			return 1 | t.hartsel&0x3FF<<16
		}
		return 0
	case 0x11:
		v := uint32(2) | 1<<7 | 1<<5
		h, idx, ok := t.cur()
		switch {
		case !t.active:
		case !ok:
			v |= 3 << 14
		case t.cfg.Unavailable[idx]:
			v |= 3 << 12
		case h.halted:
			v |= 3 << 8
		default:
			v |= 3 << 10
		}
		if ok && h.resumeAck {
			v |= 3 << 16
		}
		if ok && h.haveReset {
			v |= 3 << 18
		}
		return v
	case 0x12:
		return 0
	case 0x16:
		return 16<<24 | t.cmderr<<8 | 2
	case 0x38:
		v := uint32(1)<<29 | t.sbcs&(1<<20|7<<17|1<<16|1<<15) | t.sberror<<12
		if t.sbBusyErr {
			v |= 1 << 22
		}
		if !t.cfg.NoSBA {
			v |= 32<<5 | 1<<2
			if !t.cfg.Only32 {
				v |= 1<<1 | 1
			}
		}
		return v
	case 0x39:
		return t.sbaddr
	case 0x3C:
		v := t.sbdata
		if t.sbcs&(1<<15) != 0 {
			t.sbRead()
		}
		return v
	}
	return 0
}

func (t *Target) dmWrite(a, v uint32) {
	switch a {
	case 0x04:
		t.data0 = v
	case 0x10:
		t.dmControl(v)
	case 0x16:
		t.cmderr &^= v >> 8 & 7
	case 0x17:
		t.command(v)
	case 0x38:
		t.sbcs = v & (1<<20 | 7<<17 | 1<<16 | 1<<15)
		t.sberror &^= v >> 12 & 7
		if v>>22&1 == 1 {
			t.sbBusyErr = false
		}
	case 0x39:
		t.sbaddr = v
		if t.sbcs&(1<<20) != 0 {
			t.sbRead()
		}
	case 0x3C:
		t.sbdata = v
		t.sbWrite(v)
	}
}

func (t *Target) dmControl(v uint32) {
	was := t.active
	t.active = v&1 == 1
	if !t.active {
		if was {
			for _, h := range t.harts {
				h.resumeAck = false
			}
			t.cmderr, t.sbcs = 0, 0
		}
		return
	}
	t.hartsel = (v >> 16 & 0x3FF) | (v>>6&0x3FF)<<10
	h, idx, ok := t.cur()
	if !ok {
		return
	}
	if v>>3&1 == 1 {
		h.resetHaltReq = true
	}
	if v>>2&1 == 1 {
		h.resetHaltReq = false
	}
	if v>>28&1 == 1 {
		h.haveReset = false
	}
	if v>>1&1 == 1 {
		t.Resets++
		for i, x := range t.harts {
			t.resetHart(i, x, v>>31&1 == 1)
		}
		if t.cfg.ResetDropsDM {
			t.active = false
		}
		return
	}
	if v>>29&1 == 1 {
		t.Resets++
		t.resetHart(idx, h, v>>31&1 == 1)
		return
	}
	if v>>31&1 == 1 && !h.halted && !t.cfg.Unavailable[idx] {
		h.halted = true
		t.Halts++
		t.setCause(h, 3)
	}
	if v>>30&1 == 1 {
		t.Resumes++
		h.resumeAck = true
		if h.halted {
			h.halted = false
			t.resume(h)
		}
	} else if v>>31&1 == 1 {
		h.resumeAck = false
	}
}

func (t *Target) resetHart(idx int, h *hart, halt bool) {
	h.gpr = [32]uint32{}
	h.csr[0x7B1] = 0x40000080
	h.haveReset = true
	h.resumeAck = false
	for i := range h.trig {
		h.trig[i] = trigSlot{}
	}
	if halt || h.resetHaltReq {
		h.halted = true
		t.setCause(h, 5)
		return
	}
	h.halted = false
}

func (t *Target) setCause(h *hart, c uint32) {
	h.csr[0x7B0] = h.csr[0x7B0]&^(7<<6) | c<<6
}

// instLen reads the instruction at pc and returns its length, or 0 when pc is
// not readable.
func (t *Target) instLen(pc uint32) (int, uint32) {
	lo, ok := t.readSize(pc, 2)
	if !ok {
		return 0, 0
	}
	if lo&3 != 3 {
		return 2, lo
	}
	hi, ok := t.readSize(pc+2, 2)
	if !ok {
		return 0, 0
	}
	return 4, hi<<16 | lo
}

// resume models what the hart does after a resume request: a single step when
// dcsr.step is set, otherwise straight-line execution that stops at an
// execute trigger within the next 4 KiB.
func (t *Target) resume(h *hart) {
	pc := h.csr[0x7B1]
	if h.csr[0x7B0]&(1<<2) != 0 {
		t.Steps++
		n, inst := t.instLen(pc)
		switch {
		case n == 0:
		case inst == 0x6f || inst == 0xa001: // jal x0,0 / c.j 0: a self-loop
		default:
			pc += uint32(n)
		}
		h.csr[0x7B1] = pc
		t.setCause(h, 4)
		h.halted = true
		return
	}
	for off := 0; off < 4096; {
		for _, s := range h.trig {
			if s.t1>>28 == 2 || s.t1>>28 == 6 {
				if s.t1&(1<<2) != 0 && s.t1&(1<<6) != 0 && s.t2 == pc {
					h.csr[0x7B1] = pc
					t.setCause(h, 2)
					h.halted = true
					return
				}
			}
		}
		n, _ := t.instLen(pc)
		if n == 0 {
			break
		}
		pc += uint32(n)
		off += n
	}
}

func (t *Target) command(v uint32) {
	if t.cmderr != 0 {
		return
	}
	h, idx, ok := t.cur()
	if !ok || !h.halted || t.cfg.Unavailable[idx] {
		t.cmderr = 4
		return
	}
	if v>>24 != 0 || v>>20&7 != 2 {
		t.cmderr = 2
		return
	}
	if v>>17&1 == 0 {
		return
	}
	reg := uint16(v)
	write := v>>16&1 == 1
	switch {
	case reg >= 0x1000 && reg < 0x1020:
		n := reg - 0x1000
		if write {
			if n != 0 {
				h.gpr[n] = t.data0
			}
		} else {
			t.data0 = h.gpr[n]
		}
	case reg < 0x1000:
		t.csrAccess(h, reg, write)
	default:
		t.cmderr = 2
	}
}

func (t *Target) csrAccess(h *hart, reg uint16, write bool) {
	switch reg {
	case 0x7A0, 0x7A1, 0x7A2, 0x7A4:
		if h.trig == nil {
			t.cmderr = 3
			return
		}
		if reg == 0x7A4 {
			if write {
				return
			}
			t.data0 = 1<<2 | 1<<6
			return
		}
		if write {
			switch reg {
			case 0x7A0:
				if int(t.data0) < len(h.trig) {
					h.tsel = t.data0
				}
			case 0x7A1:
				ty := t.data0 >> 28
				if ty == 2 || ty == 6 || t.data0 == 0 {
					h.trig[h.tsel].t1 = t.data0
				}
			case 0x7A2:
				h.trig[h.tsel].t2 = t.data0
			}
			return
		}
		switch reg {
		case 0x7A0:
			t.data0 = h.tsel
		case 0x7A1:
			t.data0 = h.trig[h.tsel].t1
		case 0x7A2:
			t.data0 = h.trig[h.tsel].t2
		}
		return
	}
	val, known := h.csr[reg]
	if !known {
		t.cmderr = 3
		return
	}
	if write {
		h.csr[reg] = t.data0
	} else {
		t.data0 = val
	}
}

// readSize reads size bytes (1, 2, 4) at addr from memory or a device.
func (t *Target) readSize(addr uint32, size int) (uint32, bool) {
	for _, d := range t.cfg.Devices {
		if addr >= d.Base && addr-d.Base < d.Size {
			if size != 4 || addr&3 != 0 {
				return 0, false
			}
			return d.Read(addr - d.Base), true
		}
	}
	for _, r := range t.cfg.Memory {
		if addr >= r.Base && uint64(addr)+uint64(size) <= uint64(r.Base)+uint64(len(r.Data)) {
			o := addr - r.Base
			var v uint32
			for i := 0; i < size; i++ {
				v |= uint32(r.Data[int(o)+i]) << (8 * i)
			}
			return v, true
		}
	}
	return 0, false
}

func (t *Target) writeSize(addr uint32, size int, v uint32) bool {
	for _, d := range t.cfg.Devices {
		if addr >= d.Base && addr-d.Base < d.Size {
			if size != 4 || addr&3 != 0 {
				return false
			}
			d.Write(addr-d.Base, v)
			return true
		}
	}
	for _, r := range t.cfg.Memory {
		if addr >= r.Base && uint64(addr)+uint64(size) <= uint64(r.Base)+uint64(len(r.Data)) {
			o := int(addr - r.Base)
			for i := 0; i < size; i++ {
				r.Data[o+i] = byte(v >> (8 * i))
			}
			return true
		}
	}
	return false
}

func (t *Target) sbWidth() (int, bool) {
	switch t.sbcs >> 17 & 7 {
	case 0:
		if t.cfg.Only32 {
			return 0, false
		}
		return 1, true
	case 1:
		if t.cfg.Only32 {
			return 0, false
		}
		return 2, true
	case 2:
		return 4, true
	}
	return 0, false
}

func (t *Target) sbRead() {
	if t.sberror != 0 {
		return
	}
	t.SBAReads++
	w, ok := t.sbWidth()
	if !ok {
		t.sberror = 4
		return
	}
	if t.sbaddr&uint32(w-1) != 0 {
		t.sberror = 3
		return
	}
	v, ok := t.readSize(t.sbaddr, w)
	if !ok {
		t.sberror = 2
		return
	}
	t.sbdata = v
	if t.sbcs&(1<<16) != 0 {
		t.sbaddr += uint32(w)
	}
}

func (t *Target) sbWrite(v uint32) {
	if t.sberror != 0 {
		return
	}
	t.SBAWrites++
	w, ok := t.sbWidth()
	if !ok {
		t.sberror = 4
		return
	}
	if t.sbaddr&uint32(w-1) != 0 {
		t.sberror = 3
		return
	}
	if !t.writeSize(t.sbaddr, w, v) {
		t.sberror = 2
		return
	}
	if t.sbcs&(1<<16) != 0 {
		t.sbaddr += uint32(w)
	}
}

// Port adapts the Target to jtag.Port, clocking it directly.
type Port struct{ T *Target }

// TMS implements jtag.Port.
func (p Port) TMS(_ context.Context, tms []bool, tdi bool) error {
	for _, b := range tms {
		p.T.Clock(b, tdi)
	}
	return nil
}

// Shift implements jtag.Port.
func (p Port) Shift(_ context.Context, tdi []byte, nbits int, exit, capture bool) ([]byte, error) {
	if nbits < 1 {
		return nil, errors.New("rvsim: zero-bit shift")
	}
	if tdi != nil && len(tdi) < (nbits+7)/8 {
		return nil, fmt.Errorf("rvsim: short data")
	}
	var out []byte
	if capture {
		out = make([]byte, (nbits+7)/8)
	}
	for i := 0; i < nbits; i++ {
		in := tdi != nil && tdi[i/8]>>(i%8)&1 == 1
		o := p.T.Clock(exit && i == nbits-1, in)
		if capture && o {
			out[i/8] |= 1 << (i % 8)
		}
	}
	return out, nil
}

// BatchPort is a Port that also implements jtag.Batcher, so tests exercise
// the pipelined path. Calls counts Batch invocations.
type BatchPort struct {
	Port
	Calls int
}

// Batch implements jtag.Batcher.
func (p *BatchPort) Batch(_ context.Context, ops []jtag.Op) ([][]byte, error) {
	p.Calls++
	var res [][]byte
	for _, op := range ops {
		if op.NBits == 0 {
			for _, b := range op.TMS {
				p.T.Clock(b, op.TDI)
			}
			continue
		}
		var out []byte
		if op.Capture > 0 {
			out = make([]byte, (op.Capture+7)/8)
		}
		for i := 0; i < op.NBits; i++ {
			in := op.Data != nil && op.Data[i/8]>>(i%8)&1 == 1
			o := p.T.Clock(op.Exit && i == op.NBits-1, in)
			if i < op.Capture && o {
				out[i/8] |= 1 << (i % 8)
			}
		}
		if op.Capture > 0 {
			res = append(res, out)
		}
	}
	return res, nil
}
