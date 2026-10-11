package riscv

import (
	"context"
	"fmt"
	"time"
)

// HaltInfo describes why and where a hart is halted.
type HaltInfo struct {
	Cause    int    `json:"cause"`
	CauseStr string `json:"cause_name"`
	PC       uint32 `json:"pc"`
	DCSR     uint32 `json:"dcsr"`
}

// Halted reads the debug entry cause and pc of the halted hart.
func (m *DM) HaltedInfo(ctx context.Context) (HaltInfo, error) {
	vals, err := m.ReadRegs(ctx, []RegNum{CSRDCSR, CSRDPC})
	if err != nil {
		return HaltInfo{}, err
	}
	c := int(vals[0] & dcsrCauseMsk >> dcsrCauseSh)
	return HaltInfo{Cause: c, CauseStr: CauseName(c), PC: vals[1], DCSR: vals[0]}, nil
}

// ResumeNoWait asks the hart to resume, waits only for the acknowledge and
// returns without requiring it to stay running (it may already have stopped at
// a breakpoint).
func (m *DM) ResumeNoWait(ctx context.Context) error {
	if err := m.Activate(ctx); err != nil {
		return err
	}
	if err := m.resumeStart(ctx); err != nil {
		return err
	}
	_, err := m.poll(ctx, "resume acknowledge", func(s Status) bool { return s.ResumeAck })
	if cerr := m.clearRequests(ctx); err == nil {
		err = cerr
	}
	return err
}

// WaitHalt waits up to timeout for the selected hart to be halted and returns
// the entry cause and pc.
func (m *DM) WaitHalt(ctx context.Context, timeout time.Duration) (HaltInfo, error) {
	saved := m.Timeout
	m.Timeout = timeout
	_, err := m.poll(ctx, "halt", func(s Status) bool { return s.State == HartHalted })
	m.Timeout = saved
	if err != nil {
		return HaltInfo{}, fmt.Errorf("%w: %v", ErrHaltTimeout, err)
	}
	return m.HaltedInfo(ctx)
}

// ResetKind selects what is reset.
type ResetKind string

// Reset kinds.
const (
	// ResetHart resets only the selected hart(s) (dmcontrol.hartreset).
	ResetHart ResetKind = "hart"
	// ResetSystem resets everything the debug module can reach except the debug
	// module itself (dmcontrol.ndmreset). On some chips this also drops the
	// debug transport.
	ResetSystem ResetKind = "system"
)

// Reset errors.
var ErrResetUnsupported = fmt.Errorf("riscv: reset not supported by this debug module")

// Reset performs the requested reset. With haltAfter the hart is asked to halt
// as soon as it comes out of reset (resethaltreq where supported, otherwise
// haltreq held through the reset). It does not wait for the hart to come back;
// call WaitHalt/Status afterwards. The reset request is always deasserted
// again, unless the transport is lost, in which case the error is returned and
// the caller must rebuild the whole stack.
func (m *DM) Reset(ctx context.Context, kind ResetKind, haltAfter bool) error {
	if err := m.Activate(ctx); err != nil {
		return err
	}
	st, err := m.Status(ctx)
	if err != nil {
		return err
	}
	var bit uint32
	switch kind {
	case ResetSystem:
		bit = 1 << 1
	case ResetHart:
		bit = 1 << 29
	default:
		return fmt.Errorf("riscv: unknown reset kind %q", kind)
	}
	var halt uint32
	if haltAfter {
		if st.HasResetHaltReq {
			// setresethaltreq (bit 3): halt the hart when it leaves reset.
			if err := m.dtm.Write(ctx, regDMControl, m.control(1<<3)); err != nil {
				return err
			}
		} else {
			halt = dmcHaltReq
		}
	}
	if err := m.dtm.Write(ctx, regDMControl, m.control(bit|halt)); err != nil {
		return err
	}
	// Deassert the reset (keeping haltreq/resethaltreq) so the hart can run.
	if err := m.dtm.Write(ctx, regDMControl, m.control(halt)); err != nil {
		return err
	}
	return nil
}

// FinishReset acknowledges havereset and drops haltreq/resethaltreq.
func (m *DM) FinishReset(ctx context.Context) error {
	// ackhavereset = bit 28, clrresethaltreq = bit 2.
	if err := m.dtm.Write(ctx, regDMControl, m.control(1<<28|1<<2)); err != nil {
		return err
	}
	return m.dtm.Write(ctx, regDMControl, m.control(0))
}

// InvalidateCache forgets cached capabilities. Call it after anything that can
// change the debug module (a system reset).
func (m *DM) InvalidateCache() { m.sbaOK = false }
