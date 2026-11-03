package xtensa

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Scratch is the address register the debug sequences use as a pointer and
// for moving special registers through DDR. Its value is saved when the core
// halts and written back before the core runs again.
const Scratch = 3

// Errors.
var (
	ErrNotHalted     = errors.New("xtensa: core is not halted")
	ErrNoDebugModule = errors.New("xtensa: no debug module answered (OCDID reads 0 or all ones)")
	ErrTimeout       = errors.New("xtensa: timed out")
	ErrNoSlot        = errors.New("xtensa: no free hardware breakpoint slot")
)

// ExecError reports a DSR exec fault after instructions ran through DIR0EXEC.
type ExecError struct {
	Op  string
	DSR uint32
}

func (e *ExecError) Error() string {
	var why []string
	if e.DSR&DSRExecException != 0 {
		why = append(why, "exception")
	}
	if e.DSR&DSRExecOverrun != 0 {
		why = append(why, "overrun")
	}
	if e.DSR&DSRExecBusy != 0 {
		why = append(why, "busy")
	}
	return fmt.Sprintf("xtensa: %s: debug instruction %v (DSR 0x%08x)", e.Op, why, e.DSR)
}

// Core is one Xtensa core behind a DM.
type Core struct {
	DM *DM
	// DebugLevel is the core's debug interrupt level (XCHAL_DEBUGLEVEL):
	// PC and PS of the interrupted program are EPC/EPS[DebugLevel].
	DebugLevel int
	// Timeout bounds halt and step waits (default 1 s).
	Timeout time.Duration
	// Poll is the delay between DSR polls (default 1 ms).
	Poll time.Duration
	// ExcSRs are the special registers saved at halt and written back on
	// resume if a debug instruction raised an exception in between, since
	// a faulting load/store may record its cause and address in them.
	ExcSRs []uint8

	halted   bool // context saved for this halt
	a3       uint32
	saved    map[uint8]uint32
	excSeen  bool
	nIBreak  int // -1 until probed
	ibreak   [8]ibreakSlot
	lastHalt Halt
}

type ibreakSlot struct {
	used bool
	addr uint32
}

// NewCore returns a Core using the given debug level.
func NewCore(dm *DM, debugLevel int) *Core {
	return &Core{
		DM: dm, DebugLevel: debugLevel, Timeout: time.Second, Poll: time.Millisecond,
		ExcSRs: []uint8{SREXCCAUSE, SREXCVADDR, SREPC1, SRDEPC}, nIBreak: -1,
	}
}

// Info is what Examine found.
type Info struct {
	OCDID      uint32 `json:"ocdid"`
	PwrCtlPrev uint32 `json:"pwrctl_previous"`
	PwrStat    uint32 `json:"pwrstat"`
	PwrStatNow uint32 `json:"pwrstat_after_clear"`
	DSR        uint32 `json:"dsr"`
	DCR        uint32 `json:"dcr"`
}

// Examine powers up the debug domain (PWRCTL wake-ups, then JTAGDEBUGUSE),
// clears the "was reset" flags, enables OCD and reads OCDID, DSR and DCR. It
// does not halt the core.
func (c *Core) Examine(ctx context.Context) (Info, error) {
	var q Queue
	prev := q.PwrCtl(PwrWake)
	q.PwrCtl(PwrOn)
	st := q.PwrStat(StatDebugWasReset | StatCoreWasReset)
	st2 := q.PwrStat(0)
	q.Write(RegDCRSET, DCREnableOCD)
	id := q.Read(RegOCDID)
	dsr := q.Read(RegDSR)
	dcr := q.Read(RegDCRSET)
	r, err := c.DM.Run(ctx, &q)
	if err != nil {
		return Info{}, err
	}
	in := Info{OCDID: r[id], PwrCtlPrev: r[prev], PwrStat: r[st], PwrStatNow: r[st2], DSR: r[dsr], DCR: r[dcr]}
	if in.OCDID == 0 || in.OCDID == 0xFFFFFFFF {
		return in, fmt.Errorf("%w (OCDID 0x%08x, PWRSTAT 0x%02x)", ErrNoDebugModule, in.OCDID, in.PwrStat)
	}
	if in.DSR&DSRStopped == 0 {
		c.halted = false
	}
	return in, nil
}

// Status is a snapshot of the core's debug state.
type Status struct {
	DSR     uint32 `json:"dsr"`
	PwrStat uint32 `json:"pwrstat"`
	Halted  bool   `json:"halted"`
	// CoreWasReset is PWRSTAT.CoreWasReset (sticky until Examine clears it).
	CoreWasReset bool `json:"core_was_reset"`
}

// Status reads DSR and PWRSTAT (without clearing anything). JTAGDEBUGUSE is
// re-asserted first, as a debugger polling the core must.
func (c *Core) Status(ctx context.Context) (Status, error) {
	var q Queue
	q.PwrCtl(PwrWake)
	q.PwrCtl(PwrOn)
	ps := q.PwrStat(0)
	d := q.Read(RegDSR)
	r, err := c.DM.Run(ctx, &q)
	if err != nil {
		return Status{}, err
	}
	s := Status{DSR: r[d], PwrStat: r[ps], Halted: r[d]&DSRStopped != 0, CoreWasReset: r[ps]&StatCoreWasReset != 0}
	if !s.Halted {
		c.halted = false
	}
	return s, nil
}

// Halt is the state of a halted core.
type Halt struct {
	PC         uint32   `json:"pc"`
	PS         uint32   `json:"ps"`
	DebugCause uint32   `json:"debugcause"`
	Causes     []string `json:"causes,omitempty"`
	DSR        uint32   `json:"dsr"`
	// Already is true when the core was halted before the request.
	Already bool `json:"already_halted,omitempty"`
}

// Halted reports whether the core is believed halted with its context saved.
func (c *Core) Halted() bool { return c.halted }

// LastHalt is the state recorded when the core last halted; ok is false when
// no context is saved (the core is running or not attached).
func (c *Core) LastHalt() (h Halt, ok bool) { return c.lastHalt, c.halted }

// Halt stops the core with a debug interrupt and saves the scratch context.
func (c *Core) Halt(ctx context.Context) (Halt, error) {
	st, err := c.Status(ctx)
	if err != nil {
		return Halt{}, err
	}
	if st.Halted {
		h, err := c.onHalt(ctx)
		h.Already = true
		return h, err
	}
	if err := c.DM.WriteReg(ctx, RegDCRSET, DCREnableOCD|DCRDebugInterrupt); err != nil {
		return Halt{}, err
	}
	if _, err := c.waitStopped(ctx, c.timeout()); err != nil {
		return Halt{}, err
	}
	return c.onHalt(ctx)
}

func (c *Core) timeout() time.Duration {
	if c.Timeout <= 0 {
		return time.Second
	}
	return c.Timeout
}

func (c *Core) waitStopped(ctx context.Context, timeout time.Duration) (uint32, error) {
	deadline := time.Now().Add(timeout)
	for {
		v, err := c.DM.ReadReg(ctx, RegDSR)
		if err != nil {
			return 0, err
		}
		if v&DSRStopped != 0 {
			return v, nil
		}
		if time.Now().After(deadline) {
			return v, fmt.Errorf("%w waiting for the core to stop (DSR 0x%08x)", ErrTimeout, v)
		}
		if c.Poll > 0 {
			select {
			case <-ctx.Done():
				return v, ctx.Err()
			case <-time.After(c.Poll):
			}
		} else if err := ctx.Err(); err != nil {
			return v, err
		}
	}
}

// WaitHalt waits up to timeout for the core to stop (breakpoint, step or an
// external halt) and saves its context.
func (c *Core) WaitHalt(ctx context.Context, timeout time.Duration) (Halt, error) {
	if c.halted {
		h := c.lastHalt
		h.Already = true
		return h, nil
	}
	if _, err := c.waitStopped(ctx, timeout); err != nil {
		return Halt{}, err
	}
	return c.onHalt(ctx)
}

// onHalt runs once per halt: it acknowledges the halt in DSR/DCR, saves a3
// and ExcSRs, and reads PC, PS and DEBUGCAUSE.
func (c *Core) onHalt(ctx context.Context) (Halt, error) {
	if c.halted {
		return c.lastHalt, nil
	}
	var q Queue
	q.Write(RegDCRCLR, DCRDebugInterrupt)
	q.Write(RegDSR, dsrHaltClear|dsrExecClear)
	q.Exec(WSR(SRDDR, Scratch))
	a3 := q.Read(RegDDR)
	exc := make([]int, len(c.ExcSRs))
	for i, sr := range c.ExcSRs {
		exc[i] = c.queueReadSR(&q, sr)
	}
	pc := c.queueReadSR(&q, EPC(c.DebugLevel))
	ps := c.queueReadSR(&q, EPS(c.DebugLevel))
	cause := c.queueReadSR(&q, SRDEBUGCAUSE)
	dsr := q.Read(RegDSR)
	q.Write(RegDSR, dsrExecClear)
	r, err := c.DM.Run(ctx, &q)
	if err != nil {
		return Halt{}, err
	}
	if r[dsr]&dsrExecErrors != 0 {
		return Halt{}, &ExecError{Op: "save context", DSR: r[dsr]}
	}
	c.a3 = r[a3]
	c.saved = make(map[uint8]uint32, len(c.ExcSRs))
	for i, sr := range c.ExcSRs {
		c.saved[sr] = r[exc[i]]
	}
	c.excSeen = false
	c.halted = true
	c.lastHalt = Halt{PC: r[pc], PS: r[ps], DebugCause: r[cause], Causes: CauseNames(r[cause]), DSR: r[dsr]}
	return c.lastHalt, nil
}

// queueReadSR queues "RSR a3, sr; WSR a3, DDR; read DDR".
func (c *Core) queueReadSR(q *Queue, sr uint8) int {
	q.Exec(RSR(sr, Scratch))
	q.Exec(WSR(SRDDR, Scratch))
	return q.Read(RegDDR)
}

// queueWriteSR queues "DDR = v; RSR a3, DDR; WSR a3, sr".
func (c *Core) queueWriteSR(q *Queue, sr uint8, v uint32) {
	q.Write(RegDDR, v)
	q.Exec(RSR(SRDDR, Scratch))
	q.Exec(WSR(sr, Scratch))
}

// queueRestore queues writing back what onHalt saved (ExcSRs only when an
// exec exception may have changed them; a3 always).
func (c *Core) queueRestore(q *Queue) {
	if c.excSeen {
		for _, sr := range c.ExcSRs {
			c.queueWriteSR(q, sr, c.saved[sr])
		}
	}
	q.Write(RegDDR, c.a3)
	q.Exec(RSR(SRDDR, Scratch))
}

// check runs q followed by a DSR read and, on an exec fault, clears the
// sticky bits and returns an ExecError. It returns q's results.
func (c *Core) check(ctx context.Context, q *Queue, op string) ([]uint32, error) {
	dsr := q.Read(RegDSR)
	r, err := c.DM.Run(ctx, q)
	if err != nil {
		return nil, err
	}
	if r[dsr]&dsrExecErrors != 0 {
		c.excSeen = true
		if err := c.DM.WriteReg(ctx, RegDSR, dsrExecClear); err != nil {
			return nil, err
		}
		return r, &ExecError{Op: op, DSR: r[dsr]}
	}
	return r, nil
}

func (c *Core) needHalted() error {
	if !c.halted {
		return ErrNotHalted
	}
	return nil
}

// Resume restores the saved context and leaves debug mode (RFDO). If the
// core sits on one of this Core's instruction breakpoints it first steps
// over it with that breakpoint disabled.
func (c *Core) Resume(ctx context.Context) error {
	if err := c.needHalted(); err != nil {
		return err
	}
	if slot := c.slotAt(c.lastHalt.PC); slot >= 0 {
		if _, err := c.Step(ctx); err != nil {
			return fmt.Errorf("xtensa: step off breakpoint: %w", err)
		}
	}
	return c.rfdo(ctx, "resume")
}

func (c *Core) rfdo(ctx context.Context, op string) error {
	var q Queue
	c.queueRestore(&q)
	q.Exec(InsnRFDO)
	if _, err := c.check(ctx, &q, op); err != nil {
		return err
	}
	c.halted = false
	return nil
}

// Release writes back what this Core changed while the core was halted (a3,
// and the exception registers if a debug instruction faulted) and forgets
// the saved context without resuming: the core stays halted and a later Halt
// or WaitHalt, through this or another Core, attaches to it again. A debugger
// that lets go of a halted core must call it, or the program would continue
// with a clobbered a3.
func (c *Core) Release(ctx context.Context) error {
	if !c.halted {
		return nil
	}
	var q Queue
	c.queueRestore(&q)
	if _, err := c.check(ctx, &q, "release"); err != nil {
		return err
	}
	c.halted = false
	return nil
}

// Reset pulses PWRCTL.CoreReset. With halt the debug interrupt is armed
// first so the core stops at its reset vector. Breakpoints this Core set
// are forgotten (the reset clears IBREAKENABLE).
func (c *Core) Reset(ctx context.Context, halt bool) error {
	var q Queue
	q.PwrCtl(PwrWake | PwrCoreReset)
	q.PwrCtl(PwrOn | PwrCoreReset)
	if _, err := c.DM.Run(ctx, &q); err != nil {
		return err
	}
	time.Sleep(10 * time.Millisecond)
	q = Queue{}
	if halt {
		q.Write(RegDCRSET, DCREnableOCD|DCRDebugInterrupt)
	}
	q.PwrCtl(PwrWake)
	q.PwrCtl(PwrOn)
	if _, err := c.DM.Run(ctx, &q); err != nil {
		return err
	}
	c.halted = false
	c.ibreak = [8]ibreakSlot{}
	return nil
}
