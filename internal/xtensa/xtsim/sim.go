// Package xtsim is a SYNTHETIC Xtensa On-Chip-Debug target for tests: a
// bit-level JTAG TAP with the NARSEL/PWRCTL/PWRSTAT data registers, a debug
// module (OCDID, DCR, DSR, DDR, DIR0, DIR0EXEC, DDREXEC) and a toy core that
// executes the handful of instructions a debugger issues through DIR0EXEC
// (RSR/WSR/XSR, LDDR32.P, SDDR32.P, RFDO, ISYNC/RSYNC/DSYNC). While running,
// the core walks a fixed list of PCs, honouring IBREAK and ICOUNT. Nothing it
// reports is evidence about real hardware.
package xtsim

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"

	"github.com/faronov/hwlab/internal/jtag"
	"github.com/faronov/hwlab/internal/xtensa"
)

// Region is a block of memory.
type Region struct {
	Base     uint32
	Data     []byte
	ReadOnly bool
}

// Device is a memory-mapped peripheral: word accesses at Base..Base+Size go
// to Read/Write with the offset from Base (an SPI controller, for instance).
type Device struct {
	Base, Size uint32
	Read       func(off uint32) uint32
	Write      func(off, v uint32)
}

// Config describes one simulated core and its TAP.
type Config struct {
	IDCode     uint32
	OCDID      uint32
	PRID       uint32
	DebugLevel int // default 6
	NIBreak    int // implemented IBREAKA registers (default 2)
	// Held keeps the core in reset with its debug module dark (OCDID and
	// DSR read 0), like a second core the boot ROM has not released.
	Held bool
	// Loop is the PC sequence the running core repeats (default a single
	// address 0x40000400). ResetVector is where a core reset restarts it
	// (default Loop[0]).
	Loop        []uint32
	ResetVector uint32
	Regions     []Region
	Devices     []Device
	// SRs seeds special registers (merged over the defaults).
	SRs map[uint8]uint32
}

// Target is one Xtensa TAP + debug module + core. All methods are safe for
// concurrent use.
type Target struct {
	mu  sync.Mutex
	cfg Config

	// TAP
	state jtag.State
	ir    uint8
	irSh  uint32
	drSh  uint64
	drLen int
	ndr   bool // next NARSEL DR scan is NDR
	nar   uint8

	// debug module
	pwrctl  uint8
	pwrstat uint8
	dcr     uint32
	dsr     uint32
	ddr     uint32
	dir0    uint32

	// core
	stopped bool
	inReset bool
	pc, ps  uint32
	loopIdx int
	ar      [16]uint32
	sr      map[uint8]uint32
	regions []Region

	// Execs counts instructions executed through the debug module.
	Execs int
	// Faults counts debug instructions that raised an exception.
	Faults int
}

// New returns a running Target.
func New(cfg Config) *Target {
	if cfg.DebugLevel == 0 {
		cfg.DebugLevel = 6
	}
	if cfg.NIBreak == 0 {
		cfg.NIBreak = 2
	}
	if len(cfg.Loop) == 0 {
		cfg.Loop = []uint32{0x40000400}
	}
	if cfg.ResetVector == 0 {
		cfg.ResetVector = cfg.Loop[0]
	}
	t := &Target{cfg: cfg, state: jtag.TestLogicReset, ir: xtensa.InsIDCODE, dcr: xtensa.DCRRunStallInEn | xtensa.DCRDebugModeOutEn}
	t.sr = map[uint8]uint32{}
	for _, n := range implemented(cfg) {
		t.sr[n] = 0
	}
	t.sr[xtensa.SRPRID] = cfg.PRID
	t.sr[xtensa.SRVECBASE] = 0x40000000
	t.sr[xtensa.SRWINDOWSTART] = 1
	for k, v := range cfg.SRs {
		t.sr[k] = v
	}
	for _, r := range cfg.Regions {
		t.regions = append(t.regions, Region{Base: r.Base, Data: append([]byte(nil), r.Data...), ReadOnly: r.ReadOnly})
	}
	t.pc = cfg.Loop[0]
	t.inReset = cfg.Held
	t.pwrstat = xtensa.StatCoreDomainOn | xtensa.StatMemDomainOn | xtensa.StatDebugDomainOn | xtensa.StatCoreStillNeeded | xtensa.StatCoreWasReset | xtensa.StatDebugWasReset
	return t
}

func implemented(cfg Config) []uint8 {
	s := []uint8{
		xtensa.SRLBEG, xtensa.SRLEND, xtensa.SRLCOUNT, xtensa.SRSAR, xtensa.SRWINDOWBASE, xtensa.SRWINDOWSTART,
		xtensa.SRIBREAKENABLE, xtensa.SRATOMCTL, xtensa.SRCONFIGID0, xtensa.SRCONFIGID1, xtensa.SRDEPC,
		xtensa.SRCPENABLE, xtensa.SRINTERRUPT, xtensa.SRINTCLEAR, xtensa.SRINTENABLE, xtensa.SRPS, xtensa.SRVECBASE,
		xtensa.SREXCCAUSE, xtensa.SRDEBUGCAUSE, xtensa.SRCCOUNT, xtensa.SRPRID, xtensa.SRICOUNT, xtensa.SRICOUNTLEVEL,
		xtensa.SREXCVADDR, xtensa.SRCCOMPARE0, xtensa.SRMISC0,
		xtensa.SRDBREAKA0, xtensa.SRDBREAKA0 + 1, xtensa.SRDBREAKC0, xtensa.SRDBREAKC0 + 1,
	}
	for i := 0; i < cfg.NIBreak; i++ {
		s = append(s, uint8(xtensa.SRIBREAKA0+i))
	}
	for l := 1; l <= 7; l++ {
		s = append(s, xtensa.EPC(l), uint8(xtensa.SREXCSAVE1+l-1))
		if l >= 2 {
			s = append(s, xtensa.EPS(l))
		}
	}
	return s
}

// Clock advances the TAP by one TCK and returns TDO (sampled before the
// edge, as on a real TAP). A running core executes one instruction per TCK.
func (t *Target) Clock(tms, tdi bool) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
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
			t.irSh |= 1 << (xtensa.IRLen - 1)
		}
	}
	t.state = jtag.Next(t.state, tms)
	switch t.state {
	case jtag.TestLogicReset:
		t.ir = xtensa.InsIDCODE
	case jtag.CaptureIR:
		t.irSh = 0x01
	case jtag.UpdateIR:
		t.ir = uint8(t.irSh)
		t.ndr = false
	case jtag.CaptureDR:
		t.captureDR()
	case jtag.UpdateDR:
		t.updateDR()
	}
	t.tick()
	return tdo
}

func (t *Target) tdo() bool {
	switch t.state {
	case jtag.ShiftDR:
		return t.drSh&1 == 1
	case jtag.ShiftIR:
		return t.irSh&1 == 1
	}
	return false
}

func (t *Target) dark() bool { return t.cfg.Held || t.pwrctl&xtensa.PwrDebugWakeup == 0 }

func (t *Target) captureDR() {
	switch t.ir {
	case xtensa.InsIDCODE:
		t.drSh, t.drLen = uint64(t.cfg.IDCode), 32
	case xtensa.InsPWRCTL:
		t.drSh, t.drLen = uint64(t.pwrctl), 8
	case xtensa.InsPWRSTAT:
		t.drSh, t.drLen = uint64(t.pwrstat), 8
	case xtensa.InsNARSEL:
		if !t.ndr {
			t.drSh, t.drLen = 0, 8
		} else {
			t.drSh, t.drLen = uint64(t.readNAR(xtensa.Reg(t.nar>>1))), 32
		}
	default:
		t.drSh, t.drLen = 0, 1
	}
}

func (t *Target) updateDR() {
	v := uint32(t.drSh)
	switch t.ir {
	case xtensa.InsPWRCTL:
		t.setPwrCtl(uint8(v))
	case xtensa.InsPWRSTAT:
		t.pwrstat &^= uint8(v) & (xtensa.StatCoreWasReset | xtensa.StatDebugWasReset)
	case xtensa.InsNARSEL:
		if !t.ndr {
			t.nar = uint8(v)
		} else if !t.dark() {
			r := xtensa.Reg(t.nar >> 1)
			if t.nar&1 == 1 {
				t.writeNAR(r, v)
			} else if r == xtensa.RegDDREXEC {
				t.exec(t.dir0)
			}
		}
		t.ndr = !t.ndr
	}
}

func (t *Target) setPwrCtl(v uint8) {
	was := t.pwrctl
	t.pwrctl = v
	if t.cfg.Held {
		return
	}
	if v&xtensa.PwrCoreReset != 0 && was&xtensa.PwrCoreReset == 0 {
		t.inReset, t.stopped = true, false
	}
	if v&xtensa.PwrCoreReset == 0 && t.inReset {
		t.inReset = false
		t.pc, t.loopIdx = t.cfg.ResetVector, 0
		for i, p := range t.cfg.Loop {
			if p == t.pc {
				t.loopIdx = i
			}
		}
		t.ps = 0x1F
		t.sr[xtensa.SRIBREAKENABLE] = 0
		t.sr[xtensa.SRICOUNTLEVEL] = 0
		t.pwrstat |= xtensa.StatCoreWasReset
		if t.dcr&xtensa.DCRDebugInterrupt != 0 {
			t.enterDebug(xtensa.CauseDebugInt)
		}
	}
}

func (t *Target) readNAR(r xtensa.Reg) uint32 {
	if t.dark() {
		return 0
	}
	switch r {
	case xtensa.RegOCDID:
		return t.cfg.OCDID
	case xtensa.RegDCRSET, xtensa.RegDCRCLR:
		return t.dcr
	case xtensa.RegDSR:
		d := t.dsr | xtensa.DSRDbgModPowerOn
		if t.stopped {
			d |= xtensa.DSRStopped
		}
		return d
	case xtensa.RegDDR, xtensa.RegDDREXEC:
		return t.ddr
	case xtensa.RegDIR0, xtensa.RegDIR0EXEC:
		return t.dir0
	}
	return 0
}

func (t *Target) writeNAR(r xtensa.Reg, v uint32) {
	switch r {
	case xtensa.RegDCRSET:
		t.dcr |= v
		if v&xtensa.DCRDebugInterrupt != 0 && t.dcr&xtensa.DCREnableOCD != 0 && !t.inReset {
			if !t.stopped {
				t.enterDebug(xtensa.CauseDebugInt)
			}
			t.dcr &^= xtensa.DCRDebugInterrupt
		}
	case xtensa.RegDCRCLR:
		t.dcr &^= v
	case xtensa.RegDSR:
		t.dsr &^= v
	case xtensa.RegDDR:
		t.ddr = v
	case xtensa.RegDDREXEC:
		t.ddr = v
		t.exec(t.dir0)
	case xtensa.RegDIR0:
		t.dir0 = v
	case xtensa.RegDIR0EXEC:
		t.dir0 = v
		t.exec(v)
	}
}

func (t *Target) enterDebug(cause uint32) {
	dl := t.cfg.DebugLevel
	t.sr[xtensa.EPC(dl)] = t.pc
	t.sr[xtensa.EPS(dl)] = t.ps
	t.sr[xtensa.SRDEBUGCAUSE] = cause
	t.ps = t.ps&^0xF | uint32(dl) | 0x10
	t.stopped = true
	if cause == xtensa.CauseDebugInt {
		t.dsr |= xtensa.DSRDebugIntHost
	} else {
		t.dsr |= xtensa.DSRDebugIntBreak
	}
}

// tick runs one instruction of the program when the core is free.
func (t *Target) tick() {
	if t.stopped || t.inReset {
		return
	}
	dl := uint32(t.cfg.DebugLevel)
	lvl := t.ps & 0xF
	if lvl < dl {
		ibe := t.sr[xtensa.SRIBREAKENABLE]
		for i := 0; i < t.cfg.NIBreak; i++ {
			if ibe>>i&1 == 1 && t.sr[uint8(xtensa.SRIBREAKA0+i)] == t.pc {
				t.enterDebug(xtensa.CauseIBreak)
				return
			}
		}
	}
	counting := t.sr[xtensa.SRICOUNTLEVEL] > lvl
	if counting && t.sr[xtensa.SRICOUNT] == 0xFFFFFFFF {
		t.sr[xtensa.SRICOUNT] = 0
		t.enterDebug(xtensa.CauseICount)
		return
	}
	if counting {
		t.sr[xtensa.SRICOUNT]++
	}
	t.sr[xtensa.SRCCOUNT]++
	t.loopIdx = (t.loopIdx + 1) % len(t.cfg.Loop)
	t.pc = t.cfg.Loop[t.loopIdx]
}

// exec runs one debug instruction.
func (t *Target) exec(ins uint32) {
	t.Execs++
	if !t.stopped {
		t.dsr |= xtensa.DSRExecOverrun
		return
	}
	// A prior unacknowledged fault blocks further execution, as the
	// sticky ExecException does on hardware.
	if t.dsr&xtensa.DSRExecException != 0 {
		return
	}
	s := uint8(ins>>8) & 15
	tt := uint8(ins>>4) & 15
	sr := uint8(ins >> 8)
	switch {
	case ins == xtensa.InsnISYNC || ins == xtensa.InsnRSYNC || ins == xtensa.InsnDSYNC:
	case ins == xtensa.InsnRFDO:
		dl := t.cfg.DebugLevel
		t.pc = t.sr[xtensa.EPC(dl)]
		t.ps = t.sr[xtensa.EPS(dl)]
		for i, p := range t.cfg.Loop {
			if p == t.pc {
				t.loopIdx = i
			}
		}
		t.stopped = false
	case ins&0xFF000F == 0x030000: // RSR
		v, ok := t.readSR(sr)
		if !ok {
			t.fault(0, 0)
			return
		}
		t.ar[tt] = v
	case ins&0xFF000F == 0x130000: // WSR
		if !t.writeSR(sr, t.ar[tt]) {
			t.fault(0, 0)
			return
		}
	case ins&0xFF000F == 0x610000: // XSR
		v, ok := t.readSR(sr)
		if !ok || !t.writeSR(sr, t.ar[tt]) {
			t.fault(0, 0)
			return
		}
		t.ar[tt] = v
	case ins&0x00F00F == 0x00C002: // ADDI
		t.ar[tt] = t.ar[s] + uint32(int32(int8(ins>>16)))
	case ins&0xFFF0FF == 0x0070E0: // LDDR32.P
		v, ok := t.load(t.ar[s])
		if !ok {
			t.fault(28, t.ar[s])
			return
		}
		t.ddr = v
		t.ar[s] += 4
	case ins&0xFFF0FF == 0x0070F0: // SDDR32.P
		if !t.store(t.ar[s], t.ddr) {
			t.fault(29, t.ar[s])
			return
		}
		t.ar[s] += 4
	default:
		t.fault(0, 0)
		return
	}
	t.dsr |= xtensa.DSRExecDone
}

func (t *Target) fault(cause, vaddr uint32) {
	t.Faults++
	t.dsr |= xtensa.DSRExecDone | xtensa.DSRExecException
	t.sr[xtensa.SREXCCAUSE] = cause
	if cause != 0 {
		t.sr[xtensa.SREXCVADDR] = vaddr
	}
	// The real ESP32-S3 also loaded DEPC with VECBASE+0x280 on a faulting
	// debug load; mimic it so restore paths are exercised.
	t.sr[xtensa.SRDEPC] = t.sr[xtensa.SRVECBASE] + 0x280
}

func (t *Target) readSR(n uint8) (uint32, bool) {
	switch n {
	case xtensa.SRDDR:
		return t.ddr, true
	case xtensa.SRPS:
		return t.ps, true
	}
	v, ok := t.sr[n]
	return v, ok
}

func (t *Target) writeSR(n uint8, v uint32) bool {
	switch n {
	case xtensa.SRDDR:
		t.ddr = v
		return true
	case xtensa.SRPS:
		t.ps = v
		return true
	case xtensa.SRPRID, xtensa.SRCONFIGID0, xtensa.SRCONFIGID1:
		return false
	}
	if _, ok := t.sr[n]; !ok {
		return false
	}
	t.sr[n] = v
	return true
}

func (t *Target) region(addr uint32) (*Region, int) {
	for i := range t.regions {
		r := &t.regions[i]
		if addr >= r.Base && uint64(addr)+4 <= uint64(r.Base)+uint64(len(r.Data)) {
			return r, int(addr - r.Base)
		}
	}
	return nil, 0
}

func (t *Target) device(addr uint32) *Device {
	for i := range t.cfg.Devices {
		d := &t.cfg.Devices[i]
		if addr >= d.Base && addr-d.Base < d.Size {
			return d
		}
	}
	return nil
}

func (t *Target) load(addr uint32) (uint32, bool) {
	if addr%4 != 0 {
		return 0, false
	}
	if d := t.device(addr); d != nil {
		return d.Read(addr - d.Base), true
	}
	r, off := t.region(addr)
	if r == nil {
		return 0, false
	}
	return binary.LittleEndian.Uint32(r.Data[off:]), true
}

func (t *Target) store(addr, v uint32) bool {
	if addr%4 != 0 {
		return false
	}
	if d := t.device(addr); d != nil {
		d.Write(addr-d.Base, v)
		return true
	}
	r, off := t.region(addr)
	if r == nil || r.ReadOnly {
		return false
	}
	binary.LittleEndian.PutUint32(r.Data[off:], v)
	return true
}

// Mem returns n bytes at addr (outside any region reads as zero).
func (t *Target) Mem(addr uint32, n int) []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]byte, n)
	for i := range out {
		a := addr + uint32(i)
		if r, off := t.region(a &^ 3); r != nil {
			out[i] = r.Data[off+int(a&3)]
		}
	}
	return out
}

// SetMem writes bytes directly (no protection checks).
func (t *Target) SetMem(addr uint32, b []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i, v := range b {
		a := addr + uint32(i)
		if r, off := t.region(a &^ 3); r != nil {
			r.Data[off+int(a&3)] = v
		}
	}
}

// Stopped reports whether the core is in debug mode.
func (t *Target) Stopped() bool { t.mu.Lock(); defer t.mu.Unlock(); return t.stopped }

// PC is the running PC (or, when stopped, where it stopped).
func (t *Target) PC() uint32 {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped {
		return t.sr[xtensa.EPC(t.cfg.DebugLevel)]
	}
	return t.pc
}

// AR returns address register i of the current window.
func (t *Target) AR(i int) uint32 { t.mu.Lock(); defer t.mu.Unlock(); return t.ar[i&15] }

// SetAR sets address register i.
func (t *Target) SetAR(i int, v uint32) { t.mu.Lock(); defer t.mu.Unlock(); t.ar[i&15] = v }

// SR returns special register n and whether it is implemented.
func (t *Target) SR(n uint8) (uint32, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.readSR(n)
}

// SetSR sets special register n (implementing it if needed).
func (t *Target) SetSR(n uint8, v uint32) { t.mu.Lock(); defer t.mu.Unlock(); t.sr[n] = v }

// PS is the running PS.
func (t *Target) PS() uint32 { t.mu.Lock(); defer t.mu.Unlock(); return t.ps }

// SetPS sets the running PS.
func (t *Target) SetPS(v uint32) { t.mu.Lock(); defer t.mu.Unlock(); t.ps = v }

// DSR is the raw sticky DSR (without the computed Stopped/PowerOn bits).
func (t *Target) DSR() uint32 { t.mu.Lock(); defer t.mu.Unlock(); return t.dsr }

// InReset reports whether the core is held in reset.
func (t *Target) InReset() bool { t.mu.Lock(); defer t.mu.Unlock(); return t.inReset }

// Chain is several TAPs in series; TAPs[0] is nearest TDO.
type Chain struct {
	TAPs []*Target
}

// Clock clocks every TAP: TDI enters the last TAP, each TAP's TDO feeds the
// one nearer TDO, and TAPs[0] drives TDO.
func (c *Chain) Clock(tms, tdi bool) bool {
	in := tdi
	var out bool
	for i := len(c.TAPs) - 1; i >= 0; i-- {
		out = c.TAPs[i].Clock(tms, in)
		in = out
	}
	return out
}

// Clocker is anything with a TCK edge (Target, Chain).
type Clocker interface {
	Clock(tms, tdi bool) bool
}

// Port adapts a Clocker to jtag.Port.
type Port struct{ C Clocker }

// TMS implements jtag.Port.
func (p Port) TMS(_ context.Context, tms []bool, tdi bool) error {
	for _, b := range tms {
		p.C.Clock(b, tdi)
	}
	return nil
}

// Shift implements jtag.Port.
func (p Port) Shift(_ context.Context, tdi []byte, nbits int, exit, capture bool) ([]byte, error) {
	if nbits < 1 {
		return nil, errors.New("xtsim: zero-bit shift")
	}
	if tdi != nil && len(tdi) < (nbits+7)/8 {
		return nil, fmt.Errorf("xtsim: short data")
	}
	var out []byte
	if capture {
		out = make([]byte, (nbits+7)/8)
	}
	for i := 0; i < nbits; i++ {
		in := tdi != nil && tdi[i/8]>>(i%8)&1 == 1
		if p.C.Clock(exit && i == nbits-1, in) && capture {
			out[i/8] |= 1 << (i % 8)
		}
	}
	return out, nil
}

// BatchPort is a Port that also implements jtag.Batcher. Calls counts Batch
// invocations.
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
				p.C.Clock(b, op.TDI)
			}
			continue
		}
		var out []byte
		if op.Capture > 0 {
			out = make([]byte, (op.Capture+7)/8)
		}
		for i := 0; i < op.NBits; i++ {
			in := op.Data != nil && op.Data[i/8]>>(i%8)&1 == 1
			if p.C.Clock(op.Exit && i == op.NBits-1, in) && i < op.Capture {
				out[i/8] |= 1 << (i % 8)
			}
		}
		if op.Capture > 0 {
			res = append(res, out)
		}
	}
	return res, nil
}
