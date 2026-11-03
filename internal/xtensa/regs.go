package xtensa

import (
	"context"
	"fmt"
	"strings"
)

// RegValue is one register read.
type RegValue struct {
	Name  string `json:"name"`
	Value uint32 `json:"value"`
	// Error is set when the core refused the access (for example a special
	// register this configuration does not implement).
	Error string `json:"error,omitempty"`
}

// ReadRegisters reads registers by name (see parseReg) in one batch. a3 is
// the saved value; pc and ps are those of the interrupted program.
func (c *Core) ReadRegisters(ctx context.Context, names []string) ([]RegValue, error) {
	if err := c.needHalted(); err != nil {
		return nil, err
	}
	type item struct {
		kind   regKind
		idx    int
		dsrIdx int
	}
	items := make([]item, len(names))
	var q Queue
	for i, n := range names {
		k, num, err := parseReg(n)
		if err != nil {
			return nil, err
		}
		it := item{kind: k, idx: -1, dsrIdx: -1}
		switch k {
		case kindAR:
			if num != Scratch {
				q.Exec(WSR(SRDDR, num))
				it.idx = q.Read(RegDDR)
			}
		case kindPC:
			it.idx = c.queueReadSR(&q, EPC(c.DebugLevel))
		case kindPS:
			it.idx = c.queueReadSR(&q, EPS(c.DebugLevel))
		case kindSR:
			it.idx = c.queueReadSR(&q, num)
			it.dsrIdx = q.Read(RegDSR)
			q.Write(RegDSR, dsrExecClear)
		}
		items[i] = it
	}
	r, err := c.check(ctx, &q, "read registers")
	if err != nil {
		return nil, err
	}
	out := make([]RegValue, len(names))
	for i, it := range items {
		out[i].Name = strings.ToLower(names[i])
		switch {
		case it.kind == kindAR && it.idx < 0:
			out[i].Value = c.a3
		case it.dsrIdx >= 0 && r[it.dsrIdx]&dsrExecErrors != 0:
			c.excSeen = true
			out[i].Error = fmt.Sprintf("not readable on this core (DSR 0x%08x)", r[it.dsrIdx])
		default:
			out[i].Value = r[it.idx]
		}
	}
	return out, nil
}

// ReadRegister reads one register.
func (c *Core) ReadRegister(ctx context.Context, name string) (uint32, error) {
	r, err := c.ReadRegisters(ctx, []string{name})
	if err != nil {
		return 0, err
	}
	if r[0].Error != "" {
		return 0, fmt.Errorf("xtensa: %s: %s", name, r[0].Error)
	}
	return r[0].Value, nil
}

// WriteRegister writes one register. a3 updates the saved value (written
// when the core resumes); pc and ps write EPC/EPS[DebugLevel].
func (c *Core) WriteRegister(ctx context.Context, name string, v uint32) error {
	if err := c.needHalted(); err != nil {
		return err
	}
	k, num, err := parseReg(name)
	if err != nil {
		return err
	}
	var q Queue
	switch k {
	case kindAR:
		if num == Scratch {
			c.a3 = v
			return nil
		}
		q.Write(RegDDR, v)
		q.Exec(RSR(SRDDR, num))
	case kindPC:
		c.queueWriteSR(&q, EPC(c.DebugLevel), v)
	case kindPS:
		c.queueWriteSR(&q, EPS(c.DebugLevel), v)
	case kindSR:
		if num == SRDDR {
			return fmt.Errorf("xtensa: DDR is the debugger's data register and cannot be written as a register")
		}
		c.queueWriteSR(&q, num, v)
		q.Exec(InsnISYNC)
	}
	if _, err := c.check(ctx, &q, "write "+name); err != nil {
		return err
	}
	switch k {
	case kindPC:
		c.lastHalt.PC = v
	case kindPS:
		c.lastHalt.PS = v
	case kindSR:
		if _, ok := c.saved[num]; ok {
			c.saved[num] = v
		}
	}
	return nil
}

// CoreRegisters is the default register set for a register dump.
var CoreRegisters = []string{
	"pc", "ps", "a0", "a1", "a2", "a3", "a4", "a5", "a6", "a7", "a8", "a9", "a10", "a11", "a12", "a13", "a14", "a15",
	"sar", "windowbase", "windowstart", "lbeg", "lend", "lcount",
	"exccause", "excvaddr", "epc1", "debugcause", "intenable", "interrupt", "vecbase", "prid", "ccount",
	"icount", "icountlevel", "ibreakenable", "cpenable",
}
