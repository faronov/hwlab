package riscv

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Trigger module (debug spec chapter 5). Types of tdata1.
const (
	TrigNone     = 0
	TrigLegacy   = 1
	TrigMControl = 2
	TrigICount   = 3
	TrigITrigger = 4
	TrigETrigger = 5
	TrigMCtl6    = 6
	TrigDisabled = 15
)

// Errors.
var (
	ErrNoTrigger = errors.New("riscv: no free trigger that can do an execute breakpoint")
	ErrTrigger   = errors.New("riscv: trigger programming failed")
)

// Trigger is one trigger slot as found on the hart.
type Trigger struct {
	Index   int    `json:"index"`
	TData1  uint32 `json:"tdata1"`
	TData2  uint32 `json:"tdata2"`
	TInfo   uint32 `json:"tinfo"`
	HasInfo bool   `json:"has_tinfo"`
	Type    int    `json:"type"`
	DMode   bool   `json:"dmode"`
	// Executes is set when an enabled execute match is configured; Free means
	// no mode is enabled so the slot can be borrowed.
	InUse bool `json:"in_use"`
	// Types lists the trigger types tinfo says the slot supports.
	Types []int `json:"supported_types,omitempty"`
}

func trigType(t1 uint32) int { return int(t1 >> 28) }

// A slot is in use when a mcontrol-style trigger has any of m/s/u set, or a
// non-mcontrol type is not "none/disabled".
func trigInUse(t1 uint32) bool {
	switch trigType(t1) {
	case TrigMControl, TrigMCtl6:
		return t1&(1<<6|1<<4|1<<3) != 0
	case TrigNone, TrigDisabled:
		return false
	}
	return t1&0x0FFFFFFF != 0
}

// maxTriggers bounds enumeration so a target whose tselect ignores writes
// cannot loop forever.
const maxTriggers = 32

// Triggers enumerates the trigger slots of the halted hart. The hart's tselect
// is restored. Nothing is written except tselect (which is restored).
func (m *DM) Triggers(ctx context.Context) ([]Trigger, error) {
	if err := m.requireHalted(ctx); err != nil {
		return nil, err
	}
	orig, err := m.ReadReg(ctx, CSRTSelect)
	if err != nil {
		return nil, fmt.Errorf("%w: tselect not readable (no trigger module?): %w", ErrTrigger, err)
	}
	defer func() { _ = m.WriteReg(ctx, CSRTSelect, orig) }()
	var out []Trigger
	for i := 0; i < maxTriggers; i++ {
		if err := m.WriteReg(ctx, CSRTSelect, uint32(i)); err != nil {
			break
		}
		back, err := m.ReadReg(ctx, CSRTSelect)
		if err != nil || back != uint32(i) {
			break
		}
		t1, err := m.ReadReg(ctx, CSRTData1)
		if err != nil {
			break
		}
		t := Trigger{Index: i, TData1: t1, Type: trigType(t1), DMode: t1>>27&1 == 1, InUse: trigInUse(t1)}
		if t2, err := m.ReadReg(ctx, CSRTData2); err == nil {
			t.TData2 = t2
		}
		if ti, err := m.ReadReg(ctx, CSRTInfo); err == nil {
			t.TInfo, t.HasInfo = ti, true
			for ty := 0; ty < 16; ty++ {
				if ti>>ty&1 == 1 && ty > 0 {
					t.Types = append(t.Types, ty)
				}
			}
		}
		out = append(out, t)
	}
	return out, nil
}

// TriggerState is a saved slot that can be restored.
type TriggerState struct {
	Index  int    `json:"index"`
	TData1 uint32 `json:"tdata1"`
	TData2 uint32 `json:"tdata2"`
}

// SaveTriggers captures tdata1/tdata2 of every slot.
func (m *DM) SaveTriggers(ctx context.Context) ([]TriggerState, error) {
	ts, err := m.Triggers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]TriggerState, len(ts))
	for i, t := range ts {
		out[i] = TriggerState{Index: t.Index, TData1: t.TData1, TData2: t.TData2}
	}
	return out, nil
}

// RestoreTriggers writes saved slots back. For each slot tdata1 is disabled
// first so that rewriting tdata2 cannot fire a half-configured trigger.
func (m *DM) RestoreTriggers(ctx context.Context, saved []TriggerState) error {
	if err := m.requireHalted(ctx); err != nil {
		return err
	}
	orig, _ := m.ReadReg(ctx, CSRTSelect)
	defer func() { _ = m.WriteReg(ctx, CSRTSelect, orig) }()
	for _, s := range saved {
		if err := m.WriteReg(ctx, CSRTSelect, uint32(s.Index)); err != nil {
			return err
		}
		if err := m.WriteReg(ctx, CSRTData1, 0); err != nil {
			return err
		}
		if err := m.WriteReg(ctx, CSRTData2, s.TData2); err != nil {
			return err
		}
		if err := m.WriteReg(ctx, CSRTData1, s.TData1); err != nil {
			return err
		}
	}
	return nil
}

// ExecTrigger encodes an mcontrol/mcontrol6 execute-address match that enters
// debug mode (action 1) in M, S and U mode. The two types share these bit
// positions.
func ExecTrigger(typ int) uint32 {
	return uint32(typ)<<28 | 1<<27 | 1<<12 | 0<<7 | 1<<6 | 1<<4 | 1<<3 | 1<<2
}

// Breakpoint is an installed hardware execution breakpoint.
type Breakpoint struct {
	Trigger int    `json:"trigger"`
	Addr    uint32 `json:"addr"`
	Type    int    `json:"type"`
	Saved   TriggerState
}

// SetBreakpoint installs an execute breakpoint at addr on the first free slot
// that accepts it and reads back as programmed. The slot's previous content is
// kept in the result so ClearBreakpoint restores it.
func (m *DM) SetBreakpoint(ctx context.Context, addr uint32) (Breakpoint, error) {
	ts, err := m.Triggers(ctx)
	if err != nil {
		return Breakpoint{}, err
	}
	orig, _ := m.ReadReg(ctx, CSRTSelect)
	defer func() { _ = m.WriteReg(ctx, CSRTSelect, orig) }()
	var errs []string
	for _, t := range ts {
		if t.InUse || t.DMode {
			continue
		}
		for _, typ := range []int{TrigMControl, TrigMCtl6} {
			if t.HasInfo && t.TInfo != 0 && t.TInfo>>typ&1 == 0 {
				continue
			}
			bp, err := m.tryTrigger(ctx, t, typ, addr)
			if err == nil {
				return bp, nil
			}
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return Breakpoint{}, fmt.Errorf("%w: %s", ErrNoTrigger, strings.Join(errs, "; "))
	}
	return Breakpoint{}, ErrNoTrigger
}

func (m *DM) tryTrigger(ctx context.Context, t Trigger, typ int, addr uint32) (Breakpoint, error) {
	saved := TriggerState{Index: t.Index, TData1: t.TData1, TData2: t.TData2}
	if err := m.WriteReg(ctx, CSRTSelect, uint32(t.Index)); err != nil {
		return Breakpoint{}, err
	}
	want := ExecTrigger(typ)
	fail := func(why string) (Breakpoint, error) {
		_ = m.RestoreTriggers(ctx, []TriggerState{saved})
		return Breakpoint{}, fmt.Errorf("%w: slot %d type %d: %s", ErrTrigger, t.Index, typ, why)
	}
	if err := m.WriteReg(ctx, CSRTData1, 0); err != nil {
		return fail(err.Error())
	}
	if err := m.WriteReg(ctx, CSRTData2, addr); err != nil {
		return fail(err.Error())
	}
	if err := m.WriteReg(ctx, CSRTData1, want); err != nil {
		return fail(err.Error())
	}
	got, err := m.ReadReg(ctx, CSRTData1)
	if err != nil {
		return fail(err.Error())
	}
	// Compare the fields that matter; hit and implementation fields may differ.
	// S and U enables are not compared: a core without those modes reads them as 0.
	mask := uint32(0xF<<28 | 1<<27 | 0xF<<12 | 0xF<<7 | 1<<6 | 1<<2)
	if got&mask != want&mask {
		return fail(fmt.Sprintf("tdata1 read back %#x, wanted %#x", got, want))
	}
	if a, err := m.ReadReg(ctx, CSRTData2); err != nil || a != addr {
		return fail(fmt.Sprintf("tdata2 read back %#x", a))
	}
	return Breakpoint{Trigger: t.Index, Addr: addr, Type: typ, Saved: saved}, nil
}

// ClearBreakpoint restores the slot the breakpoint used to its saved content.
func (m *DM) ClearBreakpoint(ctx context.Context, bp Breakpoint) error {
	return m.RestoreTriggers(ctx, []TriggerState{bp.Saved})
}
