package cortexm

import (
	"context"
	"errors"
	"testing"
)

func TestFPBForeignStateAndRollback(t *testing.T) {
	ctx := context.Background()
	b := &faultBus{Sim: NewSim()}
	c := New(b)
	f, err := c.DiscoverFPB(ctx)
	if err != nil {
		t.Fatal(err)
	}
	foreign, _ := f.encode(0x200)
	if err := b.Sim.Write32(ctx, AddrFPComp, foreign); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Set(ctx, 0x200); !errors.Is(err, ErrOwnership) {
		t.Fatalf("adopted foreign breakpoint: %v", err)
	}
	saved, err := f.Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	bp, err := f.Set(ctx, 0x300)
	if err != nil {
		t.Fatal(err)
	}
	// Another debugger changes its own comparator after our snapshot.
	replacement, _ := f.encode(0x400)
	if err := b.Sim.Write32(ctx, AddrFPComp, replacement); err != nil {
		t.Fatal(err)
	}
	if err := f.Restore(ctx, saved); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.Read32(ctx, AddrFPComp); got != replacement {
		t.Fatal("restored stale FPB snapshot over foreign comparator")
	}
	if got, _ := b.Read32(ctx, f.compAddr(bp.Slot)); got&1 != 0 {
		t.Fatal("owned FPB comparator leaked")
	}
	armed := false
	b.read = func(ctx context.Context, addr uint32) (uint32, bool, error) {
		if addr == AddrFPComp+4 && armed {
			b.read = nil
			return 0, true, errors.New("read failed after arm")
		}
		return 0, false, nil
	}
	b.write = func(ctx context.Context, addr, v uint32) (bool, error) {
		if addr == AddrFPComp+4 && v&1 != 0 {
			armed = true
		}
		return false, nil
	}
	if _, err := f.Set(ctx, 0x300); err == nil {
		t.Fatal("failed read-back returned success")
	}
	if got, _ := b.Sim.Read32(ctx, AddrFPComp+4); got&1 != 0 {
		t.Fatal("failed FPB install remained armed")
	}
}

func TestFPBChangedOwnedSlotNotCleared(t *testing.T) {
	ctx := context.Background()
	s := NewSim()
	c := New(s)
	f, _ := c.DiscoverFPB(ctx)
	bp, err := f.Set(ctx, 0x300)
	if err != nil {
		t.Fatal(err)
	}
	foreign, _ := f.encode(0x400)
	if err := s.Write32(ctx, f.compAddr(bp.Slot), foreign); err != nil {
		t.Fatal(err)
	}
	if err := f.ClearSlot(ctx, bp.Slot); !errors.Is(err, ErrOwnership) {
		t.Fatalf("changed slot not rejected: %v", err)
	}
	if got, _ := s.Read32(ctx, f.compAddr(bp.Slot)); got != foreign {
		t.Fatal("foreign breakpoint cleared")
	}
}
