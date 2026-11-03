// Package xtensa drives the Xtensa On-Chip Debug (OCD) module over JTAG: the
// Xtensa TAP's NARSEL register access (NAR address + NDR data scans), the
// PWRCTL/PWRSTAT power registers, and on top of that halt/resume, register
// and memory access by executing instructions through DIR0EXEC and moving
// data through DDR, single-step (ICOUNT) and hardware instruction
// breakpoints (IBREAKA/IBREAKENABLE).
//
// It has no vendor knowledge. TAP instruction codes, NAR register numbers and
// bit positions are documented interface facts taken from the Xtensa Debug
// Guide as reflected in OpenOCD's src/target/xtensa/xtensa_debug_module.h
// (read for reference only; nothing is copied) and instruction encodings from
// the Xtensa Instruction Set Architecture Reference Manual. See
// docs/adr/0038-xtensa-esp32s3.md.
package xtensa

import (
	"context"
	"errors"
	"fmt"

	"github.com/faronov/hwlab/internal/jtag"
)

// IRLen is the Xtensa TAP instruction register length.
const IRLen = 5

// TAP instructions.
const (
	InsPWRCTL  = 0x08
	InsPWRSTAT = 0x09
	InsNARSEL  = 0x1C
	InsIDCODE  = 0x1E
	InsBYPASS  = 0x1F
)

// Scan lengths of the TAP data registers.
const (
	narBits = 8
	ndrBits = 32
	pwrBits = 8
)

// Reg is a debug-module register number as addressed through NAR.
type Reg uint8

// Debug-module registers (NAR addresses).
const (
	RegOCDID    Reg = 0x40
	RegDCRCLR   Reg = 0x42
	RegDCRSET   Reg = 0x43
	RegDSR      Reg = 0x44
	RegDDR      Reg = 0x45
	RegDDREXEC  Reg = 0x46
	RegDIR0EXEC Reg = 0x47
	RegDIR0     Reg = 0x48
)

// PWRCTL bits (JTAG view).
const (
	PwrCoreWakeup   = 1 << 0
	PwrMemWakeup    = 1 << 1
	PwrDebugWakeup  = 1 << 2
	PwrCoreReset    = 1 << 4
	PwrDebugReset   = 1 << 6
	PwrJTAGDebugUse = 1 << 7

	// PwrWake requests all power domains; PwrOn adds JTAGDEBUGUSE, the
	// steady value while a debugger is attached. JTAGDEBUGUSE takes effect
	// on a 0->1 transition, so it is always written as PwrWake then PwrOn.
	PwrWake = PwrDebugWakeup | PwrMemWakeup | PwrCoreWakeup
	PwrOn   = PwrJTAGDebugUse | PwrWake
)

// PWRSTAT bits.
const (
	StatCoreDomainOn    = 1 << 0
	StatMemDomainOn     = 1 << 1
	StatDebugDomainOn   = 1 << 2
	StatCoreStillNeeded = 1 << 3
	StatCoreWasReset    = 1 << 4
	StatDebugWasReset   = 1 << 6
)

// DCR (debug control register) bits, written through DCRSET/DCRCLR.
const (
	DCREnableOCD      = 1 << 0
	DCRDebugInterrupt = 1 << 1
	DCRInterruptAll   = 1 << 2
	DCRBreakInEn      = 1 << 16
	DCRBreakOutEn     = 1 << 17
	DCRDebugSWActive  = 1 << 20
	DCRRunStallInEn   = 1 << 21
	DCRDebugModeOutEn = 1 << 22
)

// DSR (debug status register) bits. The sticky bits are cleared by writing 1.
const (
	DSRExecDone       = 1 << 0
	DSRExecException  = 1 << 1
	DSRExecBusy       = 1 << 2
	DSRExecOverrun    = 1 << 3
	DSRStopped        = 1 << 4
	DSRCoreWroteDDR   = 1 << 10
	DSRCoreReadDDR    = 1 << 11
	DSRHostWroteDDR   = 1 << 14
	DSRHostReadDDR    = 1 << 15
	DSRDebugPendBreak = 1 << 16
	DSRDebugPendHost  = 1 << 17
	DSRDebugPendTrax  = 1 << 18
	DSRDebugIntBreak  = 1 << 20
	DSRDebugIntHost   = 1 << 21
	DSRDebugIntTrax   = 1 << 22
	DSRRunStallToggle = 1 << 23
	DSRRunStallSample = 1 << 24
	DSRDbgModPowerOn  = 1 << 31

	dsrExecErrors = DSRExecException | DSRExecOverrun | DSRExecBusy
	dsrExecClear  = DSRExecDone | DSRExecException | DSRExecOverrun
	dsrHaltClear  = DSRDebugPendBreak | DSRDebugIntBreak | DSRDebugPendHost | DSRDebugIntHost | DSRDebugPendTrax | DSRDebugIntTrax
)

// ErrChain is returned for a chain position that cannot address an Xtensa TAP.
var ErrChain = errors.New("xtensa: bad TAP chain position")

// DM is the debug module behind one Xtensa TAP in a JTAG chain. Index 0 of
// the IR lengths is the TAP nearest TDO (jtag.Chain order); every other TAP
// is held in BYPASS (all-ones instruction, one DR bit).
type DM struct {
	d     *jtag.Driver
	ntaps int
	index int
	irPre int // IR bits of the TAPs between this one and TDO
	irAll int
	// MaxOps bounds how many register accesses go into one JTAG batch.
	MaxOps int
}

// NewDM addresses the Xtensa TAP at chain position index. irLens gives the IR
// length of every TAP in the chain, nearest TDO first; the selected one must
// be IRLen bits.
func NewDM(d *jtag.Driver, irLens []int, index int) (*DM, error) {
	if index < 0 || index >= len(irLens) {
		return nil, fmt.Errorf("%w: index %d of %d TAP(s)", ErrChain, index, len(irLens))
	}
	if irLens[index] != IRLen {
		return nil, fmt.Errorf("%w: TAP %d has a %d-bit IR, Xtensa uses %d", ErrChain, index, irLens[index], IRLen)
	}
	m := &DM{d: d, ntaps: len(irLens), index: index, MaxOps: 256}
	for i, l := range irLens {
		if l < 1 {
			return nil, fmt.Errorf("%w: TAP %d IR length %d", ErrChain, i, l)
		}
		if i < index {
			m.irPre += l
		}
		m.irAll += l
	}
	return m, nil
}

// Index is the chain position this DM addresses.
func (m *DM) Index() int { return m.index }

// Driver is the JTAG driver underneath.
func (m *DM) Driver() *jtag.Driver { return m.d }

// irBits is the whole-chain IR pattern selecting ins on this TAP and BYPASS
// on the others.
func (m *DM) irBits(ins uint8) []byte {
	b := jtag.Ones(m.irAll)
	for i := 0; i < IRLen; i++ {
		jtag.SetBit(b, m.irPre+i, ins>>i&1 == 1)
	}
	return b
}

// drBits places an n-bit value for this TAP into a whole-chain DR scan. The
// TAPs nearer TDO each contribute one BYPASS bit before it.
func (m *DM) drBits(v uint32, n int) ([]byte, int) {
	total := n + m.ntaps - 1
	b := make([]byte, (total+7)/8)
	for i := 0; i < n; i++ {
		jtag.SetBit(b, m.index+i, v>>i&1 == 1)
	}
	return b, total
}

type opKind uint8

const (
	opRead opKind = iota
	opWrite
	opPwrCtl
	opPwrStat
)

type qop struct {
	kind opKind
	reg  Reg
	val  uint32
}

// Queue collects debug-module accesses to run as one pipelined JTAG
// transfer. Read-type entries return the index of their value in Run's
// result.
type Queue struct {
	ops   []qop
	nread int
}

func (q *Queue) add(o qop, read bool) int {
	q.ops = append(q.ops, o)
	if !read {
		return -1
	}
	q.nread++
	return q.nread - 1
}

// Read queues a read of a debug register.
func (q *Queue) Read(r Reg) int { return q.add(qop{kind: opRead, reg: r}, true) }

// Write queues a write of a debug register.
func (q *Queue) Write(r Reg, v uint32) { q.add(qop{kind: opWrite, reg: r, val: v}, false) }

// Exec queues one instruction for execution (a DIR0EXEC write).
func (q *Queue) Exec(ins uint32) { q.Write(RegDIR0EXEC, ins) }

// PwrCtl queues a PWRCTL write; the result is the previous value.
func (q *Queue) PwrCtl(v uint8) int { return q.add(qop{kind: opPwrCtl, val: uint32(v)}, true) }

// PwrStat queues a PWRSTAT access writing clear (1s clear the "was reset"
// bits); the result is the value before the write.
func (q *Queue) PwrStat(clear uint8) int {
	return q.add(qop{kind: opPwrStat, val: uint32(clear)}, true)
}

// Len is the number of queued accesses.
func (q *Queue) Len() int { return len(q.ops) }

// Run executes the queue in order and returns the read values. Long queues
// are split into several JTAG batches; the order of accesses is kept.
func (m *DM) Run(ctx context.Context, q *Queue) ([]uint32, error) {
	out := make([]uint32, 0, q.nread)
	limit := m.MaxOps
	if limit <= 0 {
		limit = 256
	}
	for start := 0; start < len(q.ops); start += limit {
		r, err := m.run(ctx, q.ops[start:min(start+limit, len(q.ops))])
		if err != nil {
			return nil, err
		}
		out = append(out, r...)
	}
	return out, nil
}

func (m *DM) run(ctx context.Context, ops []qop) ([]uint32, error) {
	b := m.d.NewBatch()
	ir := -1
	setIR := func(ins uint8) {
		if ir != int(ins) {
			b.ShiftIR(m.irBits(ins), m.irAll, 0)
			ir = int(ins)
		}
	}
	type capt struct{ idx, bits int }
	var caps []capt
	for _, o := range ops {
		switch o.kind {
		case opRead, opWrite:
			setIR(InsNARSEL)
			nar := uint32(o.reg) << 1
			if o.kind == opWrite {
				nar |= 1
			}
			d, n := m.drBits(nar, narBits)
			b.ShiftDR(d, n, 0)
			d, n = m.drBits(o.val, ndrBits)
			if o.kind == opRead {
				caps = append(caps, capt{b.ShiftDR(d, n, m.index+ndrBits), ndrBits})
			} else {
				b.ShiftDR(d, n, 0)
			}
		case opPwrCtl, opPwrStat:
			ins := uint8(InsPWRCTL)
			if o.kind == opPwrStat {
				ins = InsPWRSTAT
			}
			setIR(ins)
			d, n := m.drBits(o.val, pwrBits)
			caps = append(caps, capt{b.ShiftDR(d, n, m.index+pwrBits), pwrBits})
		}
	}
	res, err := b.Run(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]uint32, len(caps))
	for i, c := range caps {
		if c.idx < 0 || c.idx >= len(res) {
			return nil, fmt.Errorf("xtensa: missing JTAG capture %d", i)
		}
		out[i] = uint32(jtag.Field(res[c.idx], m.index, c.bits))
	}
	return out, nil
}

// ReadReg reads one debug register.
func (m *DM) ReadReg(ctx context.Context, r Reg) (uint32, error) {
	var q Queue
	q.Read(r)
	v, err := m.Run(ctx, &q)
	if err != nil {
		return 0, err
	}
	return v[0], nil
}

// WriteReg writes one debug register.
func (m *DM) WriteReg(ctx context.Context, r Reg, v uint32) error {
	var q Queue
	q.Write(r, v)
	_, err := m.Run(ctx, &q)
	return err
}
