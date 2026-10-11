package cortexm

import (
	"context"
	"errors"
	"fmt"
)

// Flash Patch and Breakpoint unit (Arm DDI 0403) / Breakpoint Unit (ARMv6-M,
// Arm DDI 0419). Both expose FP_CTRL and FP_COMPn at the same addresses; what a
// given core really has is read from FP_CTRL, never taken from its name.
const (
	AddrFPCtrl = 0xE0002000
	AddrFPComp = 0xE0002008 // FP_COMPn at +4n
)

// FPBInfo describes the FPB found on the target.
type FPBInfo struct {
	Ctrl    uint32
	Rev     int // FP_CTRL.REV: 0 = REPLACE-field comparators (FPBv1 / ARMv6-M BPU), 1 = FPBv2
	NumCode int // code comparators = hardware breakpoint slots
	NumLit  int // literal comparators, never used for breakpoints
	Enabled bool
	// CompWritable is the set of FP_COMPn bits that held a written 1 when
	// ProbeComparatorBits ran on a disabled comparator; 0 until probed.
	CompWritable uint32
}

// Breakpoint is one active hardware breakpoint.
type Breakpoint struct {
	Slot int    `json:"slot"`
	Addr uint32 `json:"addr"`
	Comp uint32 `json:"comp"`
}

// FPB is the breakpoint unit of one core.
type FPB struct {
	c       *Core
	Info    FPBInfo
	owned   map[int]Breakpoint
	saved   map[int]uint32
	managed bool
}

// DiscoverFPB reads FP_CTRL and returns the unit. The number of slots is what
// the hardware reports. A core without a breakpoint unit (FP_CTRL reads 0 or
// reports no code comparators) gets an error wrapping [ErrAbsent].
func (c *Core) DiscoverFPB(ctx context.Context) (*FPB, error) {
	if c.fpb != nil {
		return c.fpb, nil
	}
	v, err := c.Bus.Read32(ctx, AddrFPCtrl)
	if err != nil {
		return nil, err
	}
	info := decodeFPCtrl(v)
	switch {
	case v == 0:
		return nil, fmt.Errorf("%w: FP_CTRL reads 0, there is no breakpoint unit", ErrAbsent)
	case info.Rev > 1:
		return nil, fmt.Errorf("unsupported FPB revision %d (FP_CTRL 0x%08x)", info.Rev, v)
	case info.NumCode == 0:
		return nil, fmt.Errorf("%w: FP_CTRL 0x%08x reports no code comparators", ErrAbsent, v)
	}
	c.fpb = &FPB{c: c, Info: info, owned: map[int]Breakpoint{}, saved: map[int]uint32{}}
	return c.fpb, nil
}

func decodeFPCtrl(v uint32) FPBInfo {
	return FPBInfo{Ctrl: v, Rev: int(v >> 28), NumCode: int(v>>4&0xF | v>>12&0x7<<4), NumLit: int(v >> 8 & 0xF), Enabled: v&1 != 0}
}

// ProbeComparatorBits finds which FP_COMPn bits the hardware implements by
// writing all ones (with ENABLE clear, so nothing can match) to the first
// disabled comparator and reading it back; the previous value is restored.
// It is what tells the address-match fields apart without relying on the
// architecture name.
func (f *FPB) ProbeComparatorBits(ctx context.Context) (uint32, error) {
	for s := 0; s < f.Info.NumCode; s++ {
		old, err := f.c.Bus.Read32(ctx, f.compAddr(s))
		if err != nil {
			return 0, err
		}
		if old&1 != 0 {
			continue
		}
		if err := f.c.Bus.Write32(ctx, f.compAddr(s), 0xFFFFFFFE); err != nil {
			return 0, err
		}
		got, rerr := f.c.Bus.Read32(ctx, f.compAddr(s))
		if err := f.c.Bus.Write32(ctx, f.compAddr(s), old); err != nil {
			return 0, err
		}
		if rerr != nil {
			return 0, rerr
		}
		if back, err := f.c.Bus.Read32(ctx, f.compAddr(s)); err != nil {
			return 0, err
		} else if back != old {
			return 0, fmt.Errorf("%w: FP_COMP%d restored 0x%08x, read 0x%08x", ErrVerify, s, old, back)
		}
		f.Info.CompWritable = got &^ 1
		return f.Info.CompWritable, nil
	}
	return 0, fmt.Errorf("every comparator is enabled; none can be probed without disturbing a breakpoint")
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func (f *FPB) compAddr(slot int) uint32 { return AddrFPComp + 4*uint32(slot) }

// encode builds a FP_COMPn value for a Thumb breakpoint at addr.
func (f *FPB) encode(addr uint32) (uint32, error) {
	if addr&1 != 0 {
		return 0, fmt.Errorf("breakpoint address 0x%08x is odd; give the instruction address without the Thumb bit", addr)
	}
	if f.Info.Rev == 0 {
		if addr >= 0x20000000 {
			return 0, fmt.Errorf("FPBv1 can only break in the Code region (< 0x20000000), not at 0x%08x", addr)
		}
		repl := uint32(1) // lower halfword
		if addr&2 != 0 {
			repl = 2 // upper halfword
		}
		return repl<<30 | (addr & 0x1FFFFFFC) | 1, nil
	}
	return addr | 1, nil
}

func (f *FPB) decode(comp uint32) (uint32, bool) {
	if comp&1 == 0 {
		return 0, false
	}
	if f.Info.Rev == 0 {
		addr := comp & 0x1FFFFFFC
		if comp>>30 == 2 {
			addr |= 2
		}
		return addr, true
	}
	return comp &^ 1, true
}

// List returns every enabled code comparator, including those not set by us.
func (f *FPB) List(ctx context.Context) ([]Breakpoint, error) {
	var out []Breakpoint
	for s := 0; s < f.Info.NumCode; s++ {
		v, err := f.c.Bus.Read32(ctx, f.compAddr(s))
		if err != nil {
			return nil, err
		}
		if a, ok := f.decode(v); ok {
			out = append(out, Breakpoint{Slot: s, Addr: a, Comp: v})
		}
	}
	return out, nil
}

// Set installs a breakpoint in a free slot, reads the comparator back and
// returns the slot. Setting an address that already has a breakpoint returns
// that slot.
func (f *FPB) Set(ctx context.Context, addr uint32) (result Breakpoint, err error) {
	comp, err := f.encode(addr)
	if err != nil {
		return Breakpoint{}, err
	}
	if f.Info.NumCode == 0 {
		return Breakpoint{}, fmt.Errorf("this FPB has no code comparators")
	}
	cur, err := f.List(ctx)
	if err != nil {
		return Breakpoint{}, err
	}
	used := map[int]bool{}
	for _, b := range cur {
		if b.Addr == addr {
			if f.owned[b.Slot] == b {
				return b, nil
			}
			return Breakpoint{}, fmt.Errorf("%w: breakpoint at 0x%08x was not installed by this FPB", ErrOwnership, addr)
		}
		used[b.Slot] = true
	}
	slot := -1
	for s := 0; s < f.Info.NumCode; s++ {
		if !used[s] && f.owned[s].Comp == 0 {
			slot = s
			break
		}
	}
	if slot < 0 {
		return Breakpoint{}, fmt.Errorf("all %d hardware breakpoint slots are in use", f.Info.NumCode)
	}
	old, err := f.c.Bus.Read32(ctx, f.compAddr(slot))
	if err != nil {
		return Breakpoint{}, err
	}
	ctrl, err := f.c.Bus.Read32(ctx, AddrFPCtrl)
	if err != nil {
		return Breakpoint{}, err
	}
	if !f.managed {
		f.Info.Ctrl = ctrl
	}
	f.managed = true
	f.saved[slot] = old
	f.owned[slot] = Breakpoint{Slot: slot, Addr: addr, Comp: comp}
	defer func() {
		if err != nil {
			cctx, cancel := cleanupContext(ctx)
			defer cancel()
			e := f.restoreSlot(cctx, slot)
			if e == nil && len(f.owned) == 0 {
				e = f.restoreEnable(cctx, ctrl)
				if e == nil {
					f.managed = false
				}
			}
			if e != nil {
				err = WithCleanupError(err, fmt.Errorf("rollback FPB: %w", e))
			}
		}
	}()
	if err := f.c.Bus.Write32(ctx, AddrFPCtrl, 3); err != nil { // KEY | ENABLE
		return Breakpoint{}, err
	}
	if err := f.c.Bus.Write32(ctx, f.compAddr(slot), comp); err != nil {
		return Breakpoint{}, err
	}
	got, err := f.c.Bus.Read32(ctx, f.compAddr(slot))
	if err != nil {
		return Breakpoint{}, err
	}
	if got != comp {
		return Breakpoint{}, fmt.Errorf("%w: FP_COMP%d wrote 0x%08x, read 0x%08x", ErrVerify, slot, comp, got)
	}
	f.Info.Enabled = true
	return Breakpoint{Slot: slot, Addr: addr, Comp: comp}, nil
}

// CleanupDebug disables only triggers owned by this core's debug session.
// The primary operation's cancellation must not prevent cleanup.
func (c *Core) CleanupDebug(ctx context.Context) error {
	if c == nil {
		return nil
	}
	err := c.CleanupWatchpoints(ctx)
	if c.fpb != nil && c.fpb.managed {
		cctx, cancel := cleanupContext(ctx)
		defer cancel()
		err = errors.Join(err, c.fpb.Restore(cctx, FPBState{Ctrl: c.fpb.Info.Ctrl}))
	}
	return err
}

// Clear removes the breakpoint at addr and verifies the comparator is off.
func (f *FPB) Clear(ctx context.Context, addr uint32) error {
	cur, err := f.List(ctx)
	if err != nil {
		return err
	}
	for _, b := range cur {
		if b.Addr == addr {
			return f.ClearSlot(ctx, b.Slot)
		}
	}
	return fmt.Errorf("no hardware breakpoint at 0x%08x", addr)
}

// ClearSlot disables one comparator.
func (f *FPB) ClearSlot(ctx context.Context, slot int) error {
	if slot < 0 || slot >= f.Info.NumCode {
		return fmt.Errorf("slot %d is outside 0..%d", slot, f.Info.NumCode-1)
	}
	if bp, ok := f.owned[slot]; ok {
		got, err := f.c.Bus.Read32(ctx, f.compAddr(slot))
		if err != nil {
			return err
		}
		if got&1 != 0 && got != bp.Comp {
			delete(f.owned, slot)
			delete(f.saved, slot)
			return fmt.Errorf("%w: FPB slot %d changed; left untouched", ErrOwnership, slot)
		}
		return f.restoreSlot(ctx, slot)
	}
	if f.managed {
		return fmt.Errorf("%w: FPB slot %d is not owned", ErrOwnership, slot)
	}
	return f.disableSlot(ctx, slot)
}

func (f *FPB) disableSlot(ctx context.Context, slot int) error {
	if err := f.c.Bus.Write32(ctx, f.compAddr(slot), 0); err != nil {
		return err
	}
	got, err := f.c.Bus.Read32(ctx, f.compAddr(slot))
	if err != nil {
		return err
	}
	if got&1 != 0 {
		return fmt.Errorf("%w: FP_COMP%d is still enabled (0x%08x)", ErrVerify, slot, got)
	}
	return nil
}

func (f *FPB) restoreSlot(ctx context.Context, slot int) error {
	if err := f.disableSlot(ctx, slot); err != nil {
		return err
	}
	if err := f.c.Bus.Write32(ctx, f.compAddr(slot), f.saved[slot]); err != nil {
		return err
	}
	got, err := f.c.Bus.Read32(ctx, f.compAddr(slot))
	if err != nil {
		return err
	}
	if got != f.saved[slot] {
		return fmt.Errorf("%w: restoring FPB slot %d", ErrVerify, slot)
	}
	delete(f.owned, slot)
	delete(f.saved, slot)
	return nil
}

func (f *FPB) restoreEnable(ctx context.Context, ctrl uint32) error {
	live, err := f.List(ctx)
	if err != nil {
		return err
	}
	if ctrl&1 == 0 && len(live) > 0 {
		return nil // Do not disable a newly installed foreign breakpoint.
	}
	if err := f.c.Bus.Write32(ctx, AddrFPCtrl, 2|ctrl&1); err != nil {
		return err
	}
	got, err := f.c.Bus.Read32(ctx, AddrFPCtrl)
	if err != nil {
		return err
	}
	if got&1 != ctrl&1 {
		return fmt.Errorf("%w: restoring FP_CTRL.ENABLE", ErrVerify)
	}
	f.Info.Enabled = got&1 != 0
	return nil
}

// FPBState is a saved FP_CTRL and comparator set.
type FPBState struct {
	Ctrl  uint32
	Comps []uint32
}

// Save captures the registers so they can be restored byte-exact.
func (f *FPB) Save(ctx context.Context) (FPBState, error) {
	s := FPBState{}
	var err error
	if s.Ctrl, err = f.c.Bus.Read32(ctx, AddrFPCtrl); err != nil {
		return s, err
	}
	for i := 0; i < f.Info.NumCode; i++ {
		v, err := f.c.Bus.Read32(ctx, f.compAddr(i))
		if err != nil {
			return s, err
		}
		s.Comps = append(s.Comps, v)
	}
	return s, nil
}

// Restore writes a saved state back and verifies it.
func (f *FPB) Restore(ctx context.Context, s FPBState) error {
	if f.managed {
		var errs []error
		for slot := range f.owned {
			if err := f.ClearSlot(ctx, slot); err != nil {
				errs = append(errs, err)
			}
		}
		if len(errs) != 0 {
			return errors.Join(errs...)
		}
		if err := f.restoreEnable(ctx, s.Ctrl); err != nil {
			return err
		}
		f.managed = false
		return nil
	}
	if err := f.c.Bus.Write32(ctx, AddrFPCtrl, 2|s.Ctrl&1); err != nil {
		return err
	}
	for i, v := range s.Comps {
		if err := f.c.Bus.Write32(ctx, f.compAddr(i), v); err != nil {
			return err
		}
		got, err := f.c.Bus.Read32(ctx, f.compAddr(i))
		if err != nil {
			return err
		}
		if got != v {
			return fmt.Errorf("%w: FP_COMP%d restored 0x%08x, read 0x%08x", ErrVerify, i, v, got)
		}
	}
	got, err := f.c.Bus.Read32(ctx, AddrFPCtrl)
	if err != nil {
		return err
	}
	if got&1 != s.Ctrl&1 {
		return fmt.Errorf("%w: FP_CTRL.ENABLE is %d, want %d", ErrVerify, got&1, s.Ctrl&1)
	}
	return nil
}
