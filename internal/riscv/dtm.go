// Package riscv implements the RISC-V External Debug Support protocol (debug
// spec 0.13 / 1.0) above a generic JTAG driver: the JTAG Debug Transport
// Module (dtmcs/dmi), the Debug Module (halt, resume, abstract register
// access, System Bus Access reads) and nothing else.
//
// It is independent of any adapter and of any particular chip. Any
// jtag.Port (FTDI/MPSSE, Espressif USB-JTAG, ...) can carry it.
//
// Provenance: written from the public RISC-V Debug Specification (CC-BY-4.0).
package riscv

import (
	"context"
	"errors"
	"fmt"

	"github.com/faronov/hwlab/internal/jtag"
)

// JTAG instructions of the RISC-V DTM (debug spec, "JTAG Debug Transport
// Module"). The IDCODE instruction is 0x01 for every compliant TAP.
const (
	irIDCode = 0x01
	irDTMCS  = 0x10
	irDMI    = 0x11
)

// DMI operations and results.
const (
	dmiNop   = 0
	dmiRead  = 1
	dmiWrite = 2

	dmiOK     = 0
	dmiFailed = 2
	dmiBusy   = 3
)

// Errors.
var (
	// ErrBusy is a DMI transaction that stayed busy after all retries.
	ErrBusy = errors.New("riscv: debug module stayed busy")
	// ErrDMIFailed is a DMI operation the debug interconnect rejected.
	ErrDMIFailed = errors.New("riscv: DMI operation failed")
	// ErrNoDTM means dtmcs did not look like a RISC-V DTM.
	ErrNoDTM = errors.New("riscv: no RISC-V debug transport module at this IR length")
)

// DTMCS is the decoded dtmcs register.
type DTMCS struct {
	Raw     uint32 `json:"raw"`
	Version int    `json:"version"` // 1 = spec 0.13 and 1.0 (0 = 0.11, unsupported)
	ABits   int    `json:"abits"`   // DMI address width
	DMIStat int    `json:"dmistat"`
	Idle    int    `json:"idle"` // suggested Run-Test/Idle cycles per DMI access
}

func decodeDTMCS(v uint32) DTMCS {
	return DTMCS{Raw: v, Version: int(v & 0xF), ABits: int(v >> 4 & 0x3F), DMIStat: int(v >> 10 & 3), Idle: int(v >> 12 & 7)}
}

// DTM is the JTAG Debug Transport Module of a single-TAP chain. It is not safe
// for concurrent use.
type DTM struct {
	drv    *jtag.Driver
	irLen  int
	ir     uint32
	irOK   bool
	cs     DTMCS
	idle   int
	maxTry int
}

// NewDTM resets the TAP and reads dtmcs. irLen is the IR length of the only TAP
// in the chain. The TAP reset leaves the DM untouched.
func NewDTM(ctx context.Context, drv *jtag.Driver, irLen int) (*DTM, error) {
	if irLen < 5 || irLen > 32 {
		return nil, fmt.Errorf("riscv: IR length %d is not valid for a RISC-V DTM (5..32)", irLen)
	}
	d := &DTM{drv: drv, irLen: irLen, maxTry: 8}
	if err := d.Reset(ctx); err != nil {
		return nil, err
	}
	cs, err := d.ReadDTMCS(ctx)
	if err != nil {
		return nil, err
	}
	if cs.ABits < 7 || cs.ABits > 32 || cs.Version != 1 {
		return nil, fmt.Errorf("%w (dtmcs=%#08x)", ErrNoDTM, cs.Raw)
	}
	d.cs = cs
	d.idle = max(cs.Idle, 1)
	return d, nil
}

// CS returns the dtmcs read at open.
func (d *DTM) CS() DTMCS { return d.cs }

// Idle is the Run-Test/Idle cycle count currently used per DMI access.
func (d *DTM) Idle() int { return d.idle }

// Reset forces Test-Logic-Reset, which also reselects IDCODE.
func (d *DTM) Reset(ctx context.Context) error {
	d.irOK = false
	return d.drv.Reset(ctx)
}

func (d *DTM) selectIR(ctx context.Context, ir uint32) error {
	if d.irOK && d.ir == ir {
		return nil
	}
	d.irOK = false
	b := []byte{byte(ir), byte(ir >> 8), byte(ir >> 16), byte(ir >> 24)}
	if _, err := d.drv.ShiftIR(ctx, b, d.irLen, false); err != nil {
		return err
	}
	d.ir, d.irOK = ir, true
	return nil
}

// ReadDTMCS reads and decodes dtmcs.
func (d *DTM) ReadDTMCS(ctx context.Context) (DTMCS, error) {
	if err := d.selectIR(ctx, irDTMCS); err != nil {
		return DTMCS{}, err
	}
	out, err := d.drv.ShiftDR(ctx, make([]byte, 4), 32, true)
	if err != nil {
		return DTMCS{}, err
	}
	return decodeDTMCS(uint32(jtag.Field(out, 0, 32))), nil
}

// ClearSticky issues dmireset, clearing a sticky busy/failed status.
func (d *DTM) ClearSticky(ctx context.Context) error {
	if err := d.selectIR(ctx, irDTMCS); err != nil {
		return err
	}
	_, err := d.drv.ShiftDR(ctx, []byte{0, 0, 1, 0}, 32, false) // dmireset = bit 16
	return err
}

// IDCode reads the TAP IDCODE through the IDCODE instruction.
func (d *DTM) IDCode(ctx context.Context) (uint32, error) {
	if err := d.selectIR(ctx, irIDCode); err != nil {
		return 0, err
	}
	out, err := d.drv.ShiftDR(ctx, make([]byte, 4), 32, true)
	if err != nil {
		return 0, err
	}
	return uint32(jtag.Field(out, 0, 32)), nil
}

func (d *DTM) scanDMI(ctx context.Context, op uint32, addr, data uint32) (rop uint32, rdata uint32, err error) {
	n := d.cs.ABits + 34
	buf := make([]byte, (n+7)/8)
	put := func(off, width int, v uint64) {
		for i := 0; i < width; i++ {
			jtag.SetBit(buf, off+i, v>>i&1 == 1)
		}
	}
	put(0, 2, uint64(op))
	put(2, 32, uint64(data))
	put(34, d.cs.ABits, uint64(addr))
	out, err := d.drv.ShiftDR(ctx, buf, n, true)
	if err != nil {
		return 0, 0, err
	}
	return uint32(jtag.Field(out, 0, 2)), uint32(jtag.Field(out, 2, 32)), nil
}

// access performs one DMI operation and returns the read data. A busy
// response clears the sticky status, adds idle cycles and retries.
func (d *DTM) access(ctx context.Context, op uint32, addr, data uint32) (uint32, error) {
	if err := d.selectIR(ctx, irDMI); err != nil {
		return 0, err
	}
	for try := 0; try < d.maxTry; try++ {
		if _, _, err := d.scanDMI(ctx, op, addr, data); err != nil {
			return 0, err
		}
		if err := d.drv.RunTest(ctx, d.idle); err != nil {
			return 0, err
		}
		rop, rdata, err := d.scanDMI(ctx, dmiNop, 0, 0)
		if err != nil {
			return 0, err
		}
		switch rop {
		case dmiOK:
			return rdata, nil
		case dmiBusy:
			if err := d.ClearSticky(ctx); err != nil {
				return 0, err
			}
			if err := d.selectIR(ctx, irDMI); err != nil {
				return 0, err
			}
			d.idle = min(d.idle+2, 64)
		case dmiFailed:
			_ = d.ClearSticky(ctx)
			return 0, fmt.Errorf("%w (op %d, address %#x)", ErrDMIFailed, op, addr)
		default:
			return 0, fmt.Errorf("riscv: reserved DMI status %d (op %d, address %#x)", rop, op, addr)
		}
	}
	return 0, fmt.Errorf("%w (address %#x, idle %d)", ErrBusy, addr, d.idle)
}

// Read reads one debug module register.
func (d *DTM) Read(ctx context.Context, addr uint32) (uint32, error) {
	return d.access(ctx, dmiRead, addr, 0)
}

// Write writes one debug module register.
func (d *DTM) Write(ctx context.Context, addr, data uint32) error {
	_, err := d.access(ctx, dmiWrite, addr, data)
	return err
}

// DMIOp is one debug module register access of a pipelined sequence.
type DMIOp struct {
	Write bool
	Addr  uint32
	Data  uint32
}

// Pipeline performs the accesses back to back, one DMI scan each, and returns
// the read data (0 for writes). The result of access i is collected by scan
// i+1, so the whole sequence costs a single batch instead of several scans per
// register.
//
// If any access reports busy the sticky status is cleared, the idle cycle
// count is raised and the WHOLE sequence is run again. Callers must therefore
// pass sequences that are safe to repeat from the start (they set up their own
// state first). A failed access ends the sequence with ErrDMIFailed.
func (d *DTM) Pipeline(ctx context.Context, ops []DMIOp) ([]uint32, error) {
	if len(ops) == 0 {
		return nil, nil
	}
	if err := d.selectIR(ctx, irDMI); err != nil {
		return nil, err
	}
	n := d.cs.ABits + 34
	req := func(op uint32, addr, data uint32) []byte {
		buf := make([]byte, (n+7)/8)
		for i := 0; i < 2; i++ {
			jtag.SetBit(buf, i, op>>i&1 == 1)
		}
		for i := 0; i < 32; i++ {
			jtag.SetBit(buf, 2+i, data>>i&1 == 1)
		}
		for i := 0; i < d.cs.ABits; i++ {
			jtag.SetBit(buf, 34+i, addr>>i&1 == 1)
		}
		return buf
	}
	for try := 0; try < d.maxTry; try++ {
		b := d.drv.NewBatch()
		idx := make([]int, len(ops))
		for i, op := range ops {
			code := uint32(dmiRead)
			if op.Write {
				code = dmiWrite
			}
			// The result of the previous access arrives with this scan.
			cap := 0
			if i > 0 {
				cap = 2
				if !ops[i-1].Write {
					cap = 34
				}
			}
			r := b.ShiftDR(req(code, op.Addr, op.Data), n, cap)
			if i > 0 {
				idx[i-1] = r
			}
			b.RunTest(d.idle)
		}
		last := len(ops) - 1
		cap := 2
		if !ops[last].Write {
			cap = 34
		}
		idx[last] = b.ShiftDR(req(dmiNop, 0, 0), n, cap)
		res, err := b.Run(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]uint32, len(ops))
		busy := false
		for i := range ops {
			r := res[idx[i]]
			switch st := uint32(jtag.Field(r, 0, 2)); st {
			case dmiOK:
				if !ops[i].Write {
					out[i] = uint32(jtag.Field(r, 2, 32))
				}
			case dmiBusy:
				busy = true
			case dmiFailed:
				_ = d.ClearSticky(ctx)
				return nil, fmt.Errorf("%w (pipelined op %d of %d, address %#x)", ErrDMIFailed, i, len(ops), ops[i].Addr)
			default:
				return nil, fmt.Errorf("riscv: reserved DMI status %d (pipelined op %d)", st, i)
			}
			if busy {
				break
			}
		}
		if !busy {
			return out, nil
		}
		if err := d.ClearSticky(ctx); err != nil {
			return nil, err
		}
		if err := d.selectIR(ctx, irDMI); err != nil {
			return nil, err
		}
		d.idle = min(d.idle+2, 64)
	}
	return nil, fmt.Errorf("%w (pipelined, idle %d)", ErrBusy, d.idle)
}
