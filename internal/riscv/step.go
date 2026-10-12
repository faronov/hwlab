package riscv

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// dcsr fields (debug spec 4.9.1).
const (
	dcsrStep     = 1 << 2
	dcsrCauseSh  = 6
	dcsrCauseMsk = 7 << dcsrCauseSh
)

// Debug entry causes (dcsr.cause).
const (
	CauseEBreak    = 1
	CauseTrigger   = 2
	CauseHaltReq   = 3
	CauseStep      = 4
	CauseResetHalt = 5
	CauseGroup     = 6
)

// CauseName names a dcsr.cause value.
func CauseName(c int) string {
	switch c {
	case 0:
		return "none"
	case CauseEBreak:
		return "ebreak"
	case CauseTrigger:
		return "trigger"
	case CauseHaltReq:
		return "haltreq"
	case CauseStep:
		return "step"
	case CauseResetHalt:
		return "resethaltreq"
	case CauseGroup:
		return "group"
	}
	return fmt.Sprintf("reserved(%d)", c)
}

// ErrStep is a single-step that did not behave like a single-step.
var ErrStep = errors.New("riscv: single-step failed")

// StepResult is one executed instruction.
type StepResult struct {
	PCBefore  uint32 `json:"pc_before"`
	PCAfter   uint32 `json:"pc_after"`
	Cause     int    `json:"cause"`
	CauseStr  string `json:"cause_name"`
	DCSRAfter uint32 `json:"dcsr_after"`
}

// waitResumedAndHalted waits for a resume to be acknowledged and the hart to
// halt again (a step, a trigger or a breakpoint).
func (m *DM) waitResumedHalted(ctx context.Context, timeout time.Duration) (Status, error) {
	saved := m.Timeout
	m.Timeout = timeout
	defer func() { m.Timeout = saved }()
	return m.poll(ctx, "halt after resume", func(s Status) bool { return s.ResumeAck && s.State == HartHalted })
}

// resumeStart sends a resume request without waiting and drops resumereq once
// the request was acknowledged.
func (m *DM) resumeStart(ctx context.Context) error {
	return m.dtm.Write(ctx, regDMControl, m.control(dmcResumeReq))
}

func (m *DM) clearRequests(ctx context.Context) error {
	return m.dtm.Write(ctx, regDMControl, m.control(0))
}

// Step executes exactly one instruction of the halted hart with dcsr.step and
// verifies that the hart re-entered debug mode with cause "step". The step bit
// is restored to its original value afterwards.
func (m *DM) Step(ctx context.Context) (StepResult, error) {
	rs, err := m.StepN(ctx, 1)
	if err != nil {
		return StepResult{}, err
	}
	return rs[0], nil
}

// StepN executes n single steps, setting dcsr.step once and restoring it at the
// end, also on error. Every step is checked: the hart must halt again with
// cause "step".
func (m *DM) StepN(ctx context.Context, n int) (res []StepResult, err error) {
	if n < 1 || n > 1<<20 {
		return nil, fmt.Errorf("riscv: step count %d out of range", n)
	}
	if err := m.requireHalted(ctx); err != nil {
		return nil, err
	}
	orig, err := m.ReadReg(ctx, CSRDCSR)
	if err != nil {
		return nil, err
	}
	if err := m.WriteReg(ctx, CSRDCSR, orig|dcsrStep); err != nil {
		return nil, err
	}
	defer func() {
		if rerr := m.WriteReg(ctx, CSRDCSR, orig); rerr != nil && err == nil {
			err = fmt.Errorf("restoring dcsr: %w", rerr)
		}
	}()
	pc, err := m.ReadReg(ctx, CSRDPC)
	if err != nil {
		return nil, err
	}
	for i := 0; i < n; i++ {
		if err := m.resumeStart(ctx); err != nil {
			return res, err
		}
		st, werr := m.waitResumedHalted(ctx, 2*time.Second)
		if cerr := m.clearRequests(ctx); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return res, fmt.Errorf("%w: step %d: %w (dmstatus=%#08x)", ErrStep, i, werr, st.Raw)
		}
		vals, err := m.ReadRegs(ctx, []RegNum{CSRDCSR, CSRDPC})
		if err != nil {
			return res, err
		}
		cause := int(vals[0] & dcsrCauseMsk >> dcsrCauseSh)
		r := StepResult{PCBefore: pc, PCAfter: vals[1], Cause: cause, CauseStr: CauseName(cause), DCSRAfter: vals[0]}
		res = append(res, r)
		if cause != CauseStep {
			return res, fmt.Errorf("%w: step %d halted with cause %s (dcsr=%#x), expected step", ErrStep, i, CauseName(cause), vals[0])
		}
		pc = vals[1]
	}
	return res, nil
}

// InstLen returns the length in bytes of the instruction whose first 16 bits are
// lo (2 for a compressed instruction, 4 for a standard 32-bit one).
func InstLen(lo uint16) int {
	if lo&3 != 3 {
		return 2
	}
	return 4
}

// IsControlTransfer reports whether the instruction (first 16 bits lo, and the
// upper half hi when it is 32 bits wide) can change the PC non-sequentially, or
// halts/waits (so the next PC is not simply pc+len).
func IsControlTransfer(lo, hi uint16) bool {
	if InstLen(lo) == 2 {
		q := lo & 3
		f3 := lo >> 13
		switch q {
		case 1:
			return f3 == 1 || f3 == 5 || f3 == 6 || f3 == 7 // c.jal, c.j, c.beqz, c.bnez
		case 2:
			if f3 == 4 {
				// c.jr/c.mv/c.ebreak/c.jalr/c.add: bit 12 and rs2.
				rs2 := lo >> 2 & 0x1F
				if rs2 == 0 { // c.jr, c.jalr, c.ebreak
					return true
				}
			}
		}
		return false
	}
	op := lo & 0x7F
	switch op {
	case 0x63, 0x67, 0x6F: // branch, jalr, jal
		return true
	case 0x73: // ecall, ebreak, mret, sret, wfi have funct3 0; CSR accesses do not
		return (uint32(hi)<<16|uint32(lo))>>12&7 == 0
	}
	return false
}
