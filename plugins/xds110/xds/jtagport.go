package xds

import (
	"context"
	"errors"
	"fmt"

	"github.com/faronov/hwlab/internal/jtag"
)

// JTAGPort adapts the probe's state-based JTAG engine to jtag.Port so the
// shared jtag.Driver (reset, chain scan, IR/DR scans) runs on it.
//
// The probe cannot clock raw TMS patterns: it moves between stable TAP states
// (GOTO_STATE) and performs whole IR/DR scans. The port therefore tracks the
// TAP state the Driver believes (virtual) and the state the probe is really in,
// and turns the Driver's TMS paths into the equivalent probe commands. Limits:
//   - Shift(exit=false) is unsupported (a scan always leaves Shift);
//   - scans finish in Run-Test/Idle, so a Driver scan that asks for a Pause end
//     state is refused instead of being approximated;
//   - TDI is not driven during TMS-only clocking.
type JTAGPort struct {
	P       *Probe
	real    jtag.State
	virt    jtag.State
	pending bool
}

// NewJTAGPort returns a port whose TAP state is unknown until the Driver
// resets it.
func NewJTAGPort(p *Probe) *JTAGPort {
	return &JTAGPort{P: p, real: jtag.StateUnknown, virt: jtag.StateUnknown}
}

var _ jtag.Port = (*JTAGPort)(nil)

func xdsState(s jtag.State) (uint32, bool) {
	switch s {
	case jtag.TestLogicReset:
		return StReset, true
	case jtag.RunTestIdle:
		return StIdle, true
	case jtag.PauseDR:
		return StPauseDR, true
	case jtag.PauseIR:
		return StPauseIR, true
	}
	return 0, false
}

// TMS implements jtag.Port.
func (j *JTAGPort) TMS(ctx context.Context, tms []bool, _ bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(tms) == 0 {
		return nil
	}
	start := j.virt
	startPending := j.pending
	allZero := true
	ones := 0
	for _, b := range tms {
		if b {
			allZero = false
			ones++
		} else {
			ones = 0
		}
		if j.virt == jtag.StateUnknown {
			if ones >= 5 {
				j.virt = jtag.TestLogicReset
			}
			continue
		}
		j.virt = jtag.Next(j.virt, b)
	}
	if j.virt == jtag.StateUnknown {
		return errors.New("xds110: TAP state is unknown; reset it with five TMS-high clocks first")
	}
	switch j.virt {
	case jtag.TestLogicReset, jtag.RunTestIdle, jtag.PauseDR, jtag.PauseIR:
	default:
		j.pending = true // mid-sequence state the probe reaches on its own
		return nil
	}
	if j.pending && j.virt == j.real {
		j.pending = false // the previous scan already passed through Update
		return nil
	}
	if j.pending || j.virt != j.real {
		if j.pending && j.real == jtag.RunTestIdle && (j.virt == jtag.PauseDR || j.virt == jtag.PauseIR) {
			j.virt, j.pending = jtag.StateUnknown, false
			return errors.New("xds110: scans end in Run-Test/Idle; a Pause end state is not supported")
		}
		id, _ := xdsState(j.virt)
		if err := j.P.GotoState(id); err != nil {
			j.virt, j.real, j.pending = jtag.StateUnknown, jtag.StateUnknown, false
			return err
		}
		j.real, j.pending = j.virt, false
		return nil
	}
	if start == j.virt && allZero && !startPending {
		j.pending = false
		return j.P.CycleTCK(uint32(len(tms)))
	}
	return nil
}

// Shift implements jtag.Port. The Driver has already moved to Shift-IR or
// Shift-DR; the probe performs the route from its real state itself.
func (j *JTAGPort) Shift(ctx context.Context, tdi []byte, nbits int, exit, capture bool) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !exit {
		return nil, errors.New("xds110: a JTAG scan cannot stay in Shift (chunked scans are not supported)")
	}
	var ir bool
	switch j.virt {
	case jtag.ShiftDR:
	case jtag.ShiftIR:
		ir = true
	default:
		return nil, fmt.Errorf("xds110: Shift requested in TAP state %v", j.virt)
	}
	if j.real == jtag.StateUnknown {
		return nil, errors.New("xds110: TAP state is unknown")
	}
	out, err := j.P.Scan(ir, tdi, nbits, StIdle)
	if err != nil {
		j.virt, j.real, j.pending = jtag.StateUnknown, jtag.StateUnknown, false
		return nil, err
	}
	j.real, j.pending = jtag.RunTestIdle, true
	if ir {
		j.virt = jtag.Exit1IR
	} else {
		j.virt = jtag.Exit1DR
	}
	if !capture {
		return nil, nil
	}
	return out, nil
}
