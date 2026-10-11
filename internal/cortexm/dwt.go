package cortexm

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Data Watchpoint and Trace unit (Arm DDI 0403; optional in ARMv6-M, DDI 0419).
// Whether a core has one, and how many comparators, is read from DWT_CTRL.
const (
	AddrDWTCtrl = 0xE0001000
	AddrDWTComp = 0xE0001020 // COMPn at +16n, MASKn +4, FUNCTIONn +8
)

// WatchKind is the access type a watchpoint matches.
type WatchKind string

// Watchpoint kinds.
const (
	WatchRead   WatchKind = "read"
	WatchWrite  WatchKind = "write"
	WatchAccess WatchKind = "access"
)

func (k WatchKind) function() (uint32, error) {
	switch k {
	case WatchRead:
		return 5, nil
	case WatchWrite:
		return 6, nil
	case WatchAccess:
		return 7, nil
	}
	return 0, fmt.Errorf("unknown watchpoint kind %q (read, write, access)", k)
}

// Watchpoint is one active DWT comparator.
type Watchpoint struct {
	Slot int       `json:"slot"`
	Addr uint32    `json:"addr"`
	Mask int       `json:"mask_bits"`
	Kind WatchKind `json:"kind"`
}

// DWT is the watchpoint unit of one core.
type DWT struct {
	c           *Core
	NumComp     int
	Ctrl        uint32
	traceRaised bool
	closed      bool
	owned       map[int]*ownedWatch
}

type ownedWatch struct {
	wp         Watchpoint
	saved      [3]uint32
	confirmed  bool
	installing bool
}

var ErrOwnership = errors.New("debug comparator ownership changed")
var ErrAlreadySet = errors.New("watchpoint already installed with different settings")

// DiscoverDWT reuses the ownership ledger, not cached hardware contents.
// Close restores only the TRCENA bit and disabled slots changed by this unit.
func (c *Core) DiscoverDWT(ctx context.Context) (unit *DWT, err error) {
	if c.dwt != nil && !c.dwt.closed {
		if err := c.dwt.enable(ctx); err != nil {
			return nil, err
		}
		return c.dwt, nil
	}
	demcr, err := c.Bus.Read32(ctx, AddrDEMCR)
	if err != nil {
		return nil, err
	}
	v, err := c.Bus.Read32(ctx, AddrDWTCtrl)
	if err != nil {
		return nil, err
	}
	raised := false
	defer func() {
		if err != nil && raised {
			cctx, cancel := cleanupContext(ctx)
			defer cancel()
			if e := c.Bus.Write32(cctx, AddrDEMCR, demcr); e != nil {
				err = WithCleanupError(err, fmt.Errorf("restore DEMCR after discovery: %w", e))
			}
		}
	}()
	if v>>28 == 0 && demcr&DEMCRTraceEn == 0 {
		// Some cores hide the DWT registers until TRCENA is set.
		raised = true
		if err := c.Bus.Write32(ctx, AddrDEMCR, demcr|DEMCRTraceEn); err != nil {
			return nil, err
		}
		if v, err = c.Bus.Read32(ctx, AddrDWTCtrl); err != nil {
			return nil, err
		}
	}
	if v>>28 == 0 {
		return nil, fmt.Errorf("%w: DWT_CTRL reads 0x%08x, there are no watchpoint comparators", ErrAbsent, v)
	}
	if demcr&DEMCRTraceEn == 0 && !raised {
		raised = true
		if err := c.Bus.Write32(ctx, AddrDEMCR, demcr|DEMCRTraceEn); err != nil {
			return nil, err
		}
	}
	c.dwt = &DWT{c: c, Ctrl: v, NumComp: int(v >> 28), traceRaised: raised, owned: map[int]*ownedWatch{}}
	return c.dwt, nil
}

func (d *DWT) base(slot int) uint32 { return AddrDWTComp + 16*uint32(slot) }

func cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
}

func (d *DWT) enable(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d.closed {
		return fmt.Errorf("%w: DWT handle was closed; rediscover it", ErrOwnership)
	}
	v, err := d.c.Bus.Read32(ctx, AddrDEMCR)
	if err != nil {
		return err
	}
	if v&DEMCRTraceEn == 0 {
		d.traceRaised = true
		return d.c.Bus.Write32(ctx, AddrDEMCR, v|DEMCRTraceEn)
	}
	return nil
}

// Owned reports only verified watchpoints, never pending failed installs.
func (d *DWT) Owned() map[uint32]Watchpoint {
	out := map[uint32]Watchpoint{}
	if d != nil {
		for _, w := range d.owned {
			if w.confirmed {
				out[w.wp.Addr] = w.wp
			}
		}
	}
	return out
}

// List returns the comparators whose FUNCTION is a data-address match.
func (d *DWT) List(ctx context.Context) ([]Watchpoint, error) {
	if err := d.enable(ctx); err != nil {
		return nil, err
	}
	var out []Watchpoint
	for s := 0; s < d.NumComp; s++ {
		fn, err := d.c.Bus.Read32(ctx, d.base(s)+8)
		if err != nil {
			return nil, err
		}
		if fn&0xF == 0 {
			if w := d.owned[s]; w != nil {
				w.confirmed = false
			}
		}
		var kind WatchKind
		switch fn & 0xF {
		case 5:
			kind = WatchRead
		case 6:
			kind = WatchWrite
		case 7:
			kind = WatchAccess
		default:
			if w := d.owned[s]; w != nil && !w.installing && fn&0xF != 0 {
				delete(d.owned, s)
			}
			continue
		}
		comp, err := d.c.Bus.Read32(ctx, d.base(s))
		if err != nil {
			return nil, err
		}
		mask, err := d.c.Bus.Read32(ctx, d.base(s)+4)
		if err != nil {
			return nil, err
		}
		wp := Watchpoint{Slot: s, Addr: comp, Mask: int(mask & 0x1F), Kind: kind}
		if w := d.owned[s]; w != nil && !w.installing && w.wp != wp {
			// A reset or another debugger replaced this slot. Do not restore
			// an old snapshot over the new owner's state.
			delete(d.owned, s)
		}
		out = append(out, wp)
	}
	return out, nil
}

// Set programs a free comparator to watch 2^maskBits bytes at addr
// (addr must be aligned to that size).
func (d *DWT) Set(ctx context.Context, addr uint32, maskBits int, kind WatchKind) (result Watchpoint, err error) {
	fn, err := kind.function()
	if err != nil {
		return Watchpoint{}, err
	}
	if maskBits < 0 || maskBits > 15 {
		return Watchpoint{}, fmt.Errorf("mask of %d bits is out of range 0..15", maskBits)
	}
	if addr&(1<<uint(maskBits)-1) != 0 {
		return Watchpoint{}, fmt.Errorf("address 0x%08x is not aligned to the %d-byte watch size", addr, 1<<uint(maskBits))
	}
	cur, err := d.List(ctx)
	if err != nil {
		return Watchpoint{}, err
	}
	reconciled := false
	for slot, w := range d.owned {
		if !w.confirmed {
			if err := d.clearOwned(ctx, slot); err != nil {
				return Watchpoint{}, fmt.Errorf("reconcile DWT slot %d before set: %w", slot, err)
			}
			reconciled = true
		}
	}
	if reconciled {
		cur, err = d.List(ctx)
		if err != nil {
			return Watchpoint{}, err
		}
	}
	for _, w := range d.owned {
		if w.confirmed && w.wp.Addr == addr && (w.wp.Mask != maskBits || w.wp.Kind != kind) {
			return Watchpoint{}, fmt.Errorf("%w: 0x%08x", ErrAlreadySet, addr)
		}
	}
	used := map[int]bool{}
	for _, w := range cur {
		if w.Addr == addr && w.Mask == maskBits && w.Kind == kind {
			if mine := d.owned[w.Slot]; mine != nil && mine.confirmed {
				return w, nil
			}
			return Watchpoint{}, fmt.Errorf("%w: watchpoint at 0x%08x was not installed by this DWT", ErrOwnership, addr)
		}
		used[w.Slot] = true
	}
	// A comparator is free when FUNCTION is 0 (disabled).
	slot := -1
	for s := 0; s < d.NumComp; s++ {
		if used[s] || d.owned[s] != nil {
			continue
		}
		f, err := d.c.Bus.Read32(ctx, d.base(s)+8)
		if err != nil {
			return Watchpoint{}, err
		}
		if f&0xF == 0 {
			slot = s
			break
		}
	}
	if slot < 0 {
		return Watchpoint{}, fmt.Errorf("no free DWT comparator (%d present)", d.NumComp)
	}
	b := d.base(slot)
	w := &ownedWatch{wp: Watchpoint{Slot: slot, Addr: addr, Mask: maskBits, Kind: kind}, installing: true}
	for i, off := range []uint32{0, 4, 8} {
		w.saved[i], err = d.c.Bus.Read32(ctx, b+off)
		if err != nil {
			return Watchpoint{}, err
		}
	}
	d.owned[slot] = w
	defer func() {
		if err != nil {
			cctx, cancel := cleanupContext(ctx)
			defer cancel()
			if e := d.restoreSlot(cctx, slot); e != nil {
				err = WithCleanupError(err, fmt.Errorf("rollback DWT slot %d: %w", slot, e))
			}
		}
	}()
	for _, w := range []struct{ a, v uint32 }{{b + 8, 0}, {b, addr}, {b + 4, uint32(maskBits)}, {b + 8, fn}} {
		if err := ctx.Err(); err != nil {
			return Watchpoint{}, err
		}
		if err := d.c.Bus.Write32(ctx, w.a, w.v); err != nil {
			return Watchpoint{}, err
		}
	}
	got, err := d.c.Bus.Read32(ctx, b+8)
	if err != nil {
		return Watchpoint{}, err
	}
	if got&0xF != fn {
		return Watchpoint{}, fmt.Errorf("%w: DWT_FUNCTION%d wrote %d, read %d (the comparator does not support this kind)", ErrVerify, slot, fn, got&0xF)
	}
	for _, field := range []struct{ addr, want, mask uint32 }{
		{b, addr, 0xFFFFFFFF}, {b + 4, uint32(maskBits), 0x1F},
	} {
		got, e := d.c.Bus.Read32(ctx, field.addr)
		if e != nil {
			return Watchpoint{}, e
		}
		if got&field.mask != field.want {
			return Watchpoint{}, fmt.Errorf("%w: DWT register 0x%08x wrote 0x%x, read 0x%x", ErrVerify, field.addr, field.want, got)
		}
	}
	if err := ctx.Err(); err != nil {
		return Watchpoint{}, err
	}
	w.confirmed = true
	w.installing = false
	return w.wp, nil
}

// Clear disables only an owned watchpoint, then mutates its ownership ledger.
func (d *DWT) Clear(ctx context.Context, addr uint32) error {
	if err := d.enable(ctx); err != nil {
		return err
	}
	for slot, w := range d.owned {
		if w.wp.Addr == addr {
			return d.clearOwned(ctx, slot)
		}
	}
	return fmt.Errorf("no watchpoint at 0x%08x", addr)
}

func (d *DWT) clearOwned(ctx context.Context, slot int) error {
	w := d.owned[slot]
	b := d.base(slot)
	fn, err := d.c.Bus.Read32(ctx, b+8)
	if err != nil {
		return err
	}
	if fn&0xF != 0 {
		want, _ := w.wp.Kind.function()
		comp, err := d.c.Bus.Read32(ctx, b)
		if err != nil {
			return err
		}
		mask, err := d.c.Bus.Read32(ctx, b+4)
		if err != nil {
			return err
		}
		matches := comp == w.wp.Addr && int(mask&0x1F) == w.wp.Mask && fn&0xF == want
		if w.installing {
			// A failed install can have stopped between register writes or
			// latched a different unsupported FUNCTION. Keep rollback retryable.
			matches = (comp == w.wp.Addr || comp == w.saved[0]) &&
				(int(mask&0x1F) == w.wp.Mask || mask == w.saved[1])
		}
		if !matches {
			delete(d.owned, slot)
			return fmt.Errorf("%w: DWT slot %d is no longer ours; left untouched", ErrOwnership, slot)
		}
	}
	return d.restoreSlot(ctx, slot)
}

func (d *DWT) restoreSlot(ctx context.Context, slot int) error {
	w := d.owned[slot]
	if err := d.ClearSlot(ctx, slot); err != nil {
		return err
	}
	// Only a slot originally disabled was allocated. Never re-arm a snapshot.
	for i, off := range []uint32{0, 4, 8} {
		if err := d.c.Bus.Write32(ctx, d.base(slot)+off, w.saved[i]); err != nil {
			return err
		}
		got, err := d.c.Bus.Read32(ctx, d.base(slot)+off)
		if err != nil {
			return err
		}
		mask := uint32(0xFFFFFFFF)
		if off == 8 {
			mask = 0xF // MATCHED is read-only.
		}
		if got&mask != w.saved[i]&mask {
			return fmt.Errorf("%w: restoring DWT slot %d register +%d", ErrVerify, slot, off)
		}
	}
	delete(d.owned, slot)
	return nil
}

// Close is retryable after failure; it retains unresolved slot ownership.
func (d *DWT) Close(ctx context.Context) error {
	if d == nil || d.closed {
		return nil
	}
	if err := d.enable(ctx); err != nil {
		return err
	}
	var slots []int
	for slot := range d.owned {
		slots = append(slots, slot)
	}
	sort.Ints(slots)
	var errs []error
	for _, slot := range slots {
		if err := d.clearOwned(ctx, slot); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) != 0 {
		return errors.Join(errs...)
	}
	if d.traceRaised {
		// Do not turn trace off underneath a foreign comparator.
		foreign := false
		for s := 0; s < d.NumComp; s++ {
			fn, err := d.c.Bus.Read32(ctx, d.base(s)+8)
			if err != nil {
				return err
			}
			foreign = foreign || fn&0xF != 0
		}
		if !foreign {
			v, err := d.c.Bus.Read32(ctx, AddrDEMCR)
			if err != nil {
				return err
			}
			if err := d.c.Bus.Write32(ctx, AddrDEMCR, v&^DEMCRTraceEn); err != nil {
				return err
			}
			got, err := d.c.Bus.Read32(ctx, AddrDEMCR)
			if err != nil {
				return err
			}
			if got&DEMCRTraceEn != 0 {
				return fmt.Errorf("%w: DEMCR.TRCENA is still enabled", ErrVerify)
			}
		}
	}
	d.closed = true
	d.c.dwt = nil
	return nil
}

// CleanupWatchpoints uses a fresh bounded context even after cancellation.
func (c *Core) CleanupWatchpoints(ctx context.Context) error {
	if c == nil || c.dwt == nil {
		return nil
	}
	cctx, cancel := cleanupContext(ctx)
	defer cancel()
	return c.dwt.Close(cctx)
}

// ClearSlot disables a comparator and verifies it.
func (d *DWT) ClearSlot(ctx context.Context, slot int) error {
	if slot < 0 || slot >= d.NumComp {
		return fmt.Errorf("slot %d is outside 0..%d", slot, d.NumComp-1)
	}
	if err := d.c.Bus.Write32(ctx, d.base(slot)+8, 0); err != nil {
		return err
	}
	got, err := d.c.Bus.Read32(ctx, d.base(slot)+8)
	if err != nil {
		return err
	}
	if got&0xF != 0 {
		return fmt.Errorf("%w: DWT_FUNCTION%d is still 0x%x", ErrVerify, slot, got)
	}
	return nil
}
