package xtensa

import (
	"context"
	"errors"
	"fmt"
)

// Step executes one instruction with ICOUNT: ICOUNTLEVEL = DebugLevel and
// ICOUNT = -2 make the core take an ICOUNT debug exception after exactly one
// counted instruction. ICOUNT/ICOUNTLEVEL and the instruction breakpoint at
// the current PC (if this Core set one) are restored afterwards.
func (c *Core) Step(ctx context.Context) (Halt, error) {
	if err := c.needHalted(); err != nil {
		return Halt{}, err
	}
	if lvl := int(c.lastHalt.PS & 0xF); lvl >= c.DebugLevel {
		return Halt{}, fmt.Errorf("xtensa: cannot step: PS.INTLEVEL %d is not below the debug level %d", lvl, c.DebugLevel)
	}
	var q Queue
	ic := c.queueReadSR(&q, SRICOUNT)
	icl := c.queueReadSR(&q, SRICOUNTLEVEL)
	ibe := c.queueReadSR(&q, SRIBREAKENABLE)
	r, err := c.check(ctx, &q, "step: save ICOUNT")
	if err != nil {
		return Halt{}, err
	}
	saveIC, saveICL, saveIBE := r[ic], r[icl], r[ibe]
	ibeStep := saveIBE
	if slot := c.slotAt(c.lastHalt.PC); slot >= 0 {
		ibeStep &^= 1 << slot
	}

	q = Queue{}
	if ibeStep != saveIBE {
		c.queueWriteSR(&q, SRIBREAKENABLE, ibeStep)
	}
	c.queueWriteSR(&q, SRICOUNTLEVEL, uint32(c.DebugLevel))
	c.queueWriteSR(&q, SRICOUNT, 0xFFFFFFFE)
	q.Exec(InsnISYNC)
	if _, err := c.check(ctx, &q, "step: arm ICOUNT"); err != nil {
		return Halt{}, err
	}
	if err := c.rfdo(ctx, "step"); err != nil {
		return Halt{}, err
	}
	_, werr := c.waitStopped(ctx, c.timeout())
	if werr != nil {
		// Still running: stop it so ICOUNT can be restored, then report.
		if _, herr := c.Halt(ctx); herr != nil {
			return Halt{}, errors.Join(werr, herr)
		}
	}
	h, err := c.onHalt(ctx)
	if err != nil {
		return Halt{}, err
	}
	q = Queue{}
	c.queueWriteSR(&q, SRICOUNT, saveIC)
	c.queueWriteSR(&q, SRICOUNTLEVEL, saveICL)
	if ibeStep != saveIBE {
		c.queueWriteSR(&q, SRIBREAKENABLE, saveIBE)
	}
	q.Exec(InsnISYNC)
	if _, err := c.check(ctx, &q, "step: restore ICOUNT"); err != nil {
		return h, err
	}
	if werr != nil {
		return h, fmt.Errorf("xtensa: step did not complete: %w", werr)
	}
	return h, nil
}

// IBreak is an instruction breakpoint slot.
type IBreak struct {
	Slot int    `json:"slot"`
	Addr uint32 `json:"address"`
}

// IBreakSlots returns how many IBREAKA registers the core implements,
// probing IBREAKA0.. (up to 8) for an exec exception the first time.
func (c *Core) IBreakSlots(ctx context.Context) (int, error) {
	if c.nIBreak >= 0 {
		return c.nIBreak, nil
	}
	if err := c.needHalted(); err != nil {
		return 0, err
	}
	var q Queue
	idx := make([]int, len(c.ibreak))
	for i := range c.ibreak {
		q.Exec(RSR(uint8(SRIBREAKA0+i), Scratch))
		idx[i] = q.Read(RegDSR)
		q.Write(RegDSR, dsrExecClear)
	}
	r, err := c.DM.Run(ctx, &q)
	if err != nil {
		return 0, err
	}
	n := 0
	for i := range c.ibreak {
		if r[idx[i]]&dsrExecErrors != 0 {
			c.excSeen = true
			break
		}
		n++
	}
	c.nIBreak = n
	return n, nil
}

// SetIBreak arms an instruction breakpoint at addr in a slot that is neither
// used by this Core nor enabled in IBREAKENABLE by someone else.
func (c *Core) SetIBreak(ctx context.Context, addr uint32) (IBreak, error) {
	n, err := c.IBreakSlots(ctx)
	if err != nil {
		return IBreak{}, err
	}
	for i := 0; i < n; i++ {
		if c.ibreak[i].used && c.ibreak[i].addr == addr {
			return IBreak{Slot: i, Addr: addr}, nil
		}
	}
	ibe, err := c.ReadRegister(ctx, "ibreakenable")
	if err != nil {
		return IBreak{}, err
	}
	for i := 0; i < n; i++ {
		if c.ibreak[i].used || ibe&(1<<i) != 0 {
			continue
		}
		var q Queue
		c.queueWriteSR(&q, uint8(SRIBREAKA0+i), addr)
		c.queueWriteSR(&q, SRIBREAKENABLE, ibe|1<<i)
		q.Exec(InsnISYNC)
		if _, err := c.check(ctx, &q, "set breakpoint"); err != nil {
			return IBreak{}, err
		}
		c.ibreak[i] = ibreakSlot{used: true, addr: addr}
		return IBreak{Slot: i, Addr: addr}, nil
	}
	return IBreak{}, fmt.Errorf("%w (%d slot(s), IBREAKENABLE 0x%x)", ErrNoSlot, n, ibe)
}

// ClearIBreak disarms this Core's breakpoint at addr.
func (c *Core) ClearIBreak(ctx context.Context, addr uint32) error {
	slot := c.slotAt(addr)
	if slot < 0 {
		return fmt.Errorf("xtensa: no breakpoint at %#x", addr)
	}
	if err := c.needHalted(); err != nil {
		return err
	}
	ibe, err := c.ReadRegister(ctx, "ibreakenable")
	if err != nil {
		return err
	}
	var q Queue
	c.queueWriteSR(&q, SRIBREAKENABLE, ibe&^(1<<slot))
	c.queueWriteSR(&q, uint8(SRIBREAKA0+slot), 0)
	q.Exec(InsnISYNC)
	if _, err := c.check(ctx, &q, "clear breakpoint"); err != nil {
		return err
	}
	c.ibreak[slot] = ibreakSlot{}
	return nil
}

// IBreaks lists this Core's armed breakpoints.
func (c *Core) IBreaks() []IBreak {
	var r []IBreak
	for i, s := range c.ibreak {
		if s.used {
			r = append(r, IBreak{Slot: i, Addr: s.addr})
		}
	}
	return r
}

func (c *Core) slotAt(addr uint32) int {
	for i, s := range c.ibreak {
		if s.used && s.addr == addr {
			return i
		}
	}
	return -1
}
