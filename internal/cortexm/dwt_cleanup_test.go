package cortexm

import (
	"context"
	"errors"
	"testing"
)

type faultBus struct {
	*Sim
	read  func(context.Context, uint32) (uint32, bool, error)
	write func(context.Context, uint32, uint32) (bool, error)
}

func (b *faultBus) Read32(ctx context.Context, addr uint32) (uint32, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if b.read != nil {
		if v, handled, err := b.read(ctx, addr); handled {
			return v, err
		}
	}
	return b.Sim.Read32(ctx, addr)
}

func (b *faultBus) Write32(ctx context.Context, addr, v uint32) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if b.write != nil {
		if handled, err := b.write(ctx, addr, v); handled {
			return err
		}
	}
	return b.Sim.Write32(ctx, addr, v)
}

func TestDWTSetRollback(t *testing.T) {
	for _, failure := range []string{"write", "verify-read", "verify-mismatch", "mask-mismatch", "cancel", "rollback-failure"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			b := &faultBus{Sim: NewSim()}
			c := New(b)
			d, err := c.DiscoverDWT(ctx)
			if err != nil {
				t.Fatal(err)
			}
			primary := errors.New("PROTOCOL_ERROR: injected failure")
			cleanup := errors.New("DEVICE_GONE: rollback failed")
			armed := false
			b.write = func(ctx context.Context, a, v uint32) (bool, error) {
				if a == AddrDWTComp+8 && v == 6 {
					armed = true
					if err := b.Sim.Write32(ctx, a, v); err != nil {
						t.Fatal(err)
					}
					if failure == "write" || failure == "rollback-failure" {
						return true, primary
					}
					if failure == "cancel" {
						cancel()
					}
					return true, nil
				}
				if armed && failure == "rollback-failure" && a == AddrDWTComp+8 && v == 0 {
					return true, cleanup
				}
				return false, nil
			}
			b.read = func(ctx context.Context, a uint32) (uint32, bool, error) {
				if armed && a == AddrDWTComp+8 {
					switch failure {
					case "verify-read":
						b.read = nil
						return 0, true, primary
					case "verify-mismatch":
						b.read = nil
						return 7, true, nil
					}
				}
				if armed && failure == "mask-mismatch" && a == AddrDWTComp+4 {
					b.read = nil
					return 0, true, nil
				}
				return 0, false, nil
			}
			if _, err := d.Set(ctx, 0x20000010, 2, WatchWrite); err == nil {
				t.Fatal("failed install reported success")
			} else if failure == "rollback-failure" && (!errors.Is(err, primary) || !errors.Is(err, cleanup)) {
				t.Fatalf("primary/cleanup error lost: %v", err)
			}
			if len(d.Owned()) != 0 {
				t.Fatal("cache claimed failed watchpoint")
			}
			fn, _ := b.Sim.Read32(context.Background(), AddrDWTComp+8)
			if failure == "rollback-failure" {
				if fn&0xF != 6 || len(d.owned) != 1 {
					t.Fatal("unresolved comparator not represented conservatively")
				}
			} else if fn&0xF != 0 {
				t.Fatalf("FUNCTION left armed: 0x%x", fn)
			}
			b.read, b.write = nil, nil
			if err := c.CleanupWatchpoints(ctx); err != nil {
				t.Fatal(err)
			}
			fn, _ = b.Sim.Read32(context.Background(), AddrDWTComp+8)
			if fn&0xF != 0 || len(d.owned) != 0 {
				t.Fatal("cleanup failed to disable pending comparator")
			}
		})
	}
}

func TestDWTClearFailureRetainsOwnership(t *testing.T) {
	ctx := context.Background()
	b := &faultBus{Sim: NewSim()}
	c := New(b)
	d, _ := c.DiscoverDWT(ctx)
	wp, err := d.Set(ctx, 0x20000010, 2, WatchWrite)
	if err != nil {
		t.Fatal(err)
	}
	b.write = func(_ context.Context, a, v uint32) (bool, error) {
		return a == d.base(wp.Slot)+8 && v == 0, nil // dropped write
	}
	if err := d.Clear(ctx, wp.Addr); !errors.Is(err, ErrVerify) {
		t.Fatalf("clear failed verification: %v", err)
	}
	if len(d.Owned()) != 1 {
		t.Fatal("ownership removed before hardware was disabled")
	}
	b.write = nil
	if err := d.Clear(ctx, wp.Addr); err != nil {
		t.Fatal(err)
	}
	if len(d.Owned()) != 0 {
		t.Fatal("ownership survived successful clear")
	}
}

func TestDWTResetAndForeignOwnership(t *testing.T) {
	ctx := context.Background()
	s := NewSim()
	c := New(s)
	// A foreign watchpoint and unrelated DEMCR bit must never be cleared.
	for a, v := range map[uint32]uint32{
		AddrDEMCR:   DEMCRTraceEn | DEMCRVCCoreReset,
		AddrDWTComp: 0x20000020, AddrDWTComp + 4: 2, AddrDWTComp + 8: 6,
	} {
		if err := s.Write32(ctx, a, v); err != nil {
			t.Fatal(err)
		}
	}
	d, _ := c.DiscoverDWT(ctx)
	if _, err := d.Set(ctx, 0x20000020, 2, WatchWrite); !errors.Is(err, ErrOwnership) {
		t.Fatalf("adopted foreign comparator: %v", err)
	}
	for i := 0; i < 20; i++ {
		wp, err := d.Set(ctx, 0x20000010, 2, WatchWrite)
		if err != nil || wp.Slot != 1 {
			t.Fatalf("reuse %d: %v, %v", i, wp, err)
		}
		if err := d.Clear(ctx, wp.Addr); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.Set(ctx, 0x20000010, 2, WatchWrite); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Reset(ctx, ResetSystem, true); err != nil {
		t.Fatal(err)
	}
	if len(d.Owned()) != 0 || !d.closed {
		t.Fatal("reset retained stale DWT cache")
	}
	d2, err := c.DiscoverDWT(ctx)
	if err != nil || d2 == d {
		t.Fatalf("reset reused closed unit: %v", err)
	}
	fn, _ := s.Read32(ctx, AddrDWTComp+8)
	if fn&0xF != 6 {
		t.Fatal("reset cleanup cleared foreign comparator")
	}
}

func TestDWTChangedComparatorLeftUntouched(t *testing.T) {
	ctx := context.Background()
	s := NewSim()
	c := New(s)
	d, _ := c.DiscoverDWT(ctx)
	wp, _ := d.Set(ctx, 0x20000010, 2, WatchWrite)
	if err := s.Write32(ctx, d.base(wp.Slot), 0x20000080); err != nil {
		t.Fatal(err)
	}
	if err := d.Clear(ctx, wp.Addr); !errors.Is(err, ErrOwnership) {
		t.Fatalf("changed slot not rejected: %v", err)
	}
	fn, _ := s.Read32(ctx, d.base(wp.Slot)+8)
	if fn&0xF != 6 || len(d.Owned()) != 0 {
		t.Fatal("foreign state modified or claimed")
	}
}

func TestDWTReconcileBeforeComparatorReuse(t *testing.T) {
	for _, scenario := range []string{"external-reset", "failed-rollback"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			b := &faultBus{Sim: NewSim()}
			c := New(b)
			d, err := c.DiscoverDWT(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "failed-rollback" {
				armed := false
				b.write = func(ctx context.Context, addr, v uint32) (bool, error) {
					if addr == AddrDWTComp+8 && v == 6 {
						armed = true
						return true, b.Sim.Write32(ctx, addr, 4)
					}
					if armed && addr == AddrDWTComp+8 && v == 0 {
						return true, errors.New("DEVICE_GONE: failed rollback")
					}
					return false, nil
				}
				if _, err := d.Set(ctx, 0x20000010, 2, WatchWrite); err == nil {
					t.Fatal("unsupported FUNCTION returned success")
				}
				b.write = nil
				if _, err := d.List(ctx); err != nil {
					t.Fatal(err)
				}
				if len(d.owned) != 1 {
					t.Fatal("listing discarded unresolved install ownership")
				}
			} else {
				if _, err := d.Set(ctx, 0x20000010, 2, WatchWrite); err != nil {
					t.Fatal(err)
				}
				if err := b.Sim.Write32(ctx, AddrDWTComp+8, 0); err != nil {
					t.Fatal(err)
				}
			}
			wp, err := d.Set(ctx, 0x20000010, 2, WatchWrite)
			if err != nil || wp.Slot != 0 || len(d.owned) != 1 {
				t.Fatalf("reuse retained stale ownership: %+v, %v, owned=%d", wp, err, len(d.owned))
			}
			if err := d.Clear(ctx, wp.Addr); err != nil {
				t.Fatal(err)
			}
			if fn, err := b.Read32(ctx, AddrDWTComp+8); err != nil || fn&0xF != 0 || len(d.owned) != 0 {
				t.Fatalf("clear leaked a duplicate comparator: fn=%d, err=%v, owned=%d", fn, err, len(d.owned))
			}
		})
	}
}
