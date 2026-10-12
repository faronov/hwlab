package cortexm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
)

var ctx = context.Background()

func TestPrimaryErrorWithWrappedCleanup(t *testing.T) {
	cleanup := errors.New("cleanup transport failed")
	primary := fmt.Errorf("discover: %w", ErrAbsent)
	combined := fmt.Errorf("attach: %w", WithCleanupError(WithCleanupError(primary, cleanup), cleanup))
	got, separated := PrimaryError(combined)
	if !separated || !errors.Is(got, ErrAbsent) || errors.Is(got, cleanup) {
		t.Fatalf("primary %v, separated %v", got, separated)
	}
	if !errors.Is(combined, ErrAbsent) || !errors.Is(combined, cleanup) {
		t.Fatalf("combined error lost a cause: %v", combined)
	}
	got, separated = PrimaryError(primary)
	if separated || !errors.Is(got, ErrAbsent) {
		t.Fatalf("ordinary primary %v, separated %v", got, separated)
	}
}

func buses() map[string]func() (*Sim, Bus) {
	return map[string]func() (*Sim, Bus){
		"native":  func() (*Sim, Bus) { s := NewSim(); return s, s },
		"generic": func() (*Sim, Bus) { s := NewSim(); return s, SimNoNative{s} },
	}
}

func TestIdentity(t *testing.T) {
	id := DecodeCPUID(0x410FC241)
	if id.Name != "Cortex-M4" || id.ArchName != "ARMv7-M" || id.Revision != 1 || id.Variant != 0 || !id.IsV7M() {
		t.Fatalf("%+v", id)
	}
	if DecodeCPUID(0x410CC601).Name != "Cortex-M0+" || DecodeCPUID(0x411FD210).Name != "Cortex-M33" {
		t.Fatal("part table")
	}
	if DecodeCPUID(0x12345670).Name != "" {
		t.Fatal("non-Arm implementer must stay unnamed")
	}
}

func TestHaltResumeRegsStep(t *testing.T) {
	for name, mk := range buses() {
		t.Run(name, func(t *testing.T) {
			_, b := mk()
			c := New(b)
			if _, err := c.ReadAll(ctx); !errors.Is(err, ErrNotHalted) {
				t.Fatalf("regs while running: %v", err)
			}
			st, err := c.Halt(ctx)
			if err != nil || !st.Halted {
				t.Fatal(st, err)
			}
			r, err := c.ReadAll(ctx)
			if err != nil || r.SP != 0x20010000 {
				t.Fatalf("%+v %v", r, err)
			}
			steps := map[uint32]int{}
			for i := 0; i < 100; i++ {
				res, err := c.Step(ctx, true)
				if err != nil {
					t.Fatal(i, err)
				}
				d := res.PCAfter - res.PCBefore
				if d != 2 && d != 4 {
					t.Fatalf("step %d moved the PC by %d", i, d)
				}
				steps[d]++
			}
			if steps[2] == 0 || steps[4] == 0 {
				t.Fatalf("expected both 16- and 32-bit steps: %v", steps)
			}
			s, _ := c.Status(ctx)
			if s.Stepping || s.MaskInts || !s.Halted {
				t.Fatalf("step state not restored: %+v", s)
			}
			if err := c.WriteRegister(ctx, 3, 0xCAFEF00D); err != nil {
				t.Fatal(err)
			}
			if v, _ := c.ReadRegister(ctx, 3); v != 0xCAFEF00D {
				t.Fatal(v)
			}
			if st, err := c.Resume(ctx); err != nil || st.Halted {
				t.Fatal(st, err)
			}
			if _, err := c.Step(ctx, false); !errors.Is(err, ErrNotHalted) {
				t.Fatalf("step while running: %v", err)
			}
		})
	}
}

func TestMemoryAlignment(t *testing.T) {
	for name, mk := range buses() {
		t.Run(name, func(t *testing.T) {
			_, b := mk()
			c := New(b)
			base := uint32(0x20000100)
			orig, err := c.Read(ctx, base, 32)
			if err != nil {
				t.Fatal(err)
			}
			pat := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13}
			for off := uint32(0); off < 4; off++ {
				for n := 1; n <= len(pat); n++ {
					if err := c.WriteVerified(ctx, base+off, pat[:n]); err != nil {
						t.Fatal(off, n, err)
					}
					got, _ := c.Read(ctx, base, 32)
					if !bytes.Equal(got[:off], orig[:off]) || !bytes.Equal(got[int(off)+n:], orig[int(off)+n:]) {
						t.Fatalf("neighbours changed at off=%d n=%d", off, n)
					}
					if err := c.Write(ctx, base, orig); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := c.Read(ctx, 0xFFFFFFFE, 8); err == nil {
				t.Fatal("wrapping read must fail")
			}
			if _, err := c.Read(ctx, 0x60000000, 4); err == nil {
				t.Fatal("unmapped read must fail")
			}
		})
	}
}

func TestBreakpointHit(t *testing.T) {
	s := NewSim()
	c := New(s)
	if _, err := c.Halt(ctx); err != nil {
		t.Fatal(err)
	}
	f, err := c.DiscoverFPB(ctx)
	if err != nil || f.Info.NumCode != 6 || f.Info.NumLit != 2 || f.Info.Rev != 0 {
		t.Fatalf("%+v %v", f, err)
	}
	saved, _ := f.Save(ctx)
	if _, err := f.Set(ctx, 0x20000000); err == nil {
		t.Fatal("FPBv1 must refuse SRAM addresses")
	}
	if _, err := f.Set(ctx, 0x103); err == nil {
		t.Fatal("odd address")
	}
	// the stream from 0x100 is deterministic; pick an address 12 instructions on
	pc, _ := c.ReadRegister(ctx, RegPC)
	target := pc
	for i := 0; i < 12; i++ {
		target += s.instrLen(target)
	}
	bp, err := f.Set(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := f.Set(ctx, target); again.Slot != bp.Slot {
		t.Fatal("duplicate set must return the same slot")
	}
	if _, err := c.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	st, err := c.wait(ctx, "bp", func(s State) bool { return s.Halted })
	if err != nil || !st.Halted {
		t.Fatal(st, err)
	}
	cause, _ := c.HaltCause(ctx)
	got, _ := c.ReadRegister(ctx, RegPC)
	if !cause.Has("breakpoint") || got != target {
		t.Fatalf("cause %v pc 0x%x want 0x%x", cause, got, target)
	}
	if err := f.Clear(ctx, target); err != nil {
		t.Fatal(err)
	}
	if err := f.Restore(ctx, saved); err != nil {
		t.Fatal(err)
	}
	if l, _ := f.List(ctx); len(l) != 0 {
		t.Fatal(l)
	}
	for i := 0; i < 6; i++ {
		if _, err := f.Set(ctx, uint32(0x200+4*i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.Set(ctx, 0x300); err == nil {
		t.Fatal("seventh breakpoint must fail")
	}
}

func TestWatchpoints(t *testing.T) {
	c := New(NewSim())
	d, err := c.DiscoverDWT(ctx)
	if err != nil || d.NumComp != 4 {
		t.Fatalf("%+v %v", d, err)
	}
	w, err := d.Set(ctx, 0x20000010, 2, WatchWrite)
	if err != nil || w.Slot != 0 {
		t.Fatal(w, err)
	}
	if _, err := d.Set(ctx, 0x20000011, 2, WatchWrite); err == nil {
		t.Fatal("unaligned")
	}
	if _, err := d.Set(ctx, 0x20000020, 0, "bogus"); err == nil {
		t.Fatal("kind")
	}
	if l, _ := d.List(ctx); len(l) != 1 || l[0].Kind != WatchWrite || l[0].Mask != 2 {
		t.Fatal(l)
	}
	if err := d.Clear(ctx, 0x20000010); err != nil {
		t.Fatal(err)
	}
	if err := d.Clear(ctx, 0x20000010); err == nil {
		t.Fatal("double clear")
	}
}

func TestReset(t *testing.T) {
	s := NewSim()
	c := New(s)
	c.Timeout = 200_000_000
	if _, err := c.Halt(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := c.Step(ctx, true); err != nil {
			t.Fatal(err)
		}
	}
	res, err := c.Reset(ctx, ResetSystem, true)
	if err != nil || !res.Halted || !res.AtVector || res.PC != 0x100 {
		t.Fatalf("%+v %v", res, err)
	}
	if d, _ := s.Read32(ctx, AddrDEMCR); d&DEMCRVCCoreReset != 0 {
		t.Fatal("DEMCR vector catch must be restored")
	}
	res, err = c.Reset(ctx, ResetSystem, false)
	if err != nil || !res.ResetSeen {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := c.Reset(ctx, "bogus", false); err == nil {
		t.Fatal("kind")
	}
	// a reset the core never performs is an error, never a success
	s.Hooks = append(s.Hooks, func(a uint32, w bool, v uint32) (uint32, bool, error) {
		if w && a == AddrAIRCR {
			return 0, true, nil
		}
		return 0, false, nil
	})
	if _, err := c.Reset(ctx, ResetSystem, false); err == nil {
		t.Fatal("unproven reset reported success")
	}
}

func TestCortexM0PlusDiscoversWhatTheCoreHas(t *testing.T) {
	s := NewSimM0Plus()
	c := New(s)
	id, err := c.ReadIdentity(ctx)
	if err != nil || id.Name != "Cortex-M0+" || id.IsV7M() {
		t.Fatalf("%+v %v", id, err)
	}
	f, err := c.DiscoverFPB(ctx)
	if err != nil {
		t.Fatalf("an ARMv6-M BPU must be usable: %v", err)
	}
	if f.Info.NumCode != 4 || f.Info.NumLit != 0 || f.Info.Rev != 0 {
		t.Fatalf("%+v", f.Info)
	}
	if _, err := c.DiscoverDWT(ctx); !errors.Is(err, ErrAbsent) {
		t.Fatalf("a core without a DWT must report ErrAbsent, got %v", err)
	}
	if v, _ := s.Read32(ctx, AddrDEMCR); v != 0 {
		t.Fatalf("a failed DWT discovery must restore DEMCR, got 0x%x", v)
	}
	before := s.Resets
	if _, err := c.Reset(ctx, ResetCore, false); !errors.Is(err, ErrAbsent) {
		t.Fatalf("VECTRESET does not exist on ARMv6-M: %v", err)
	}
	if s.Resets != before {
		t.Fatal("the refused reset must not have touched the core")
	}
}

func TestCortexM0PlusBreakpointHits(t *testing.T) {
	s := NewSimM0Plus()
	c := New(s)
	if _, err := c.Halt(ctx); err != nil {
		t.Fatal(err)
	}
	f, err := c.DiscoverFPB(ctx)
	if err != nil {
		t.Fatal(err)
	}
	target, _ := c.ReadRegister(ctx, RegPC)
	for i := 0; i < 12; i++ {
		target += s.instrLen(target)
	}
	if _, err := f.Set(ctx, target); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	if st, err := c.wait(ctx, "bp", func(s State) bool { return s.Halted }); err != nil || !st.Halted {
		t.Fatal(st, err)
	}
	if got, _ := c.ReadRegister(ctx, RegPC); got != target {
		t.Fatalf("pc 0x%x want 0x%x", got, target)
	}
	if _, err := f.Set(ctx, 0x20000100); err == nil {
		t.Fatal("the BPU matches only the Code region")
	}
}

func TestFPBAbsentWhenNoCodeComparators(t *testing.T) {
	s := NewSim()
	s.fpCtrl = 0
	if _, err := New(s).DiscoverFPB(ctx); !errors.Is(err, ErrAbsent) {
		t.Fatal(err)
	}
}

// A part that gates its debug access with the reset (like the CC2340) must give
// ErrDebugLost, whether the AIRCR write itself fails or the debug unit just
// stops answering; neither is reported as a confirmed reset.
func TestResetReportsLostDebugAccess(t *testing.T) {
	t.Run("link down at the AIRCR write", func(t *testing.T) {
		s := NewSimM0Plus()
		c := New(s)
		c.Timeout = 100_000_000
		s.Hooks = append(s.Hooks, func(a uint32, w bool, v uint32) (uint32, bool, error) {
			if w && a == AddrAIRCR {
				return 0, true, errors.New("no acknowledge")
			}
			return 0, false, nil
		})
		res, err := c.Reset(ctx, ResetSystem, false)
		if !errors.Is(err, ErrDebugLost) || !res.DebugLost || res.ResetSeen {
			t.Fatalf("%+v %v", res, err)
		}
	})
	t.Run("debug unit stops answering", func(t *testing.T) {
		s := NewSimM0Plus()
		c := New(s)
		c.Timeout = 100_000_000
		gated := false
		s.Hooks = append(s.Hooks, func(a uint32, w bool, v uint32) (uint32, bool, error) {
			if w && a == AddrAIRCR {
				gated = true
				return 0, true, nil
			}
			if gated && !w && a == AddrDHCSR {
				return 0, true, nil
			}
			return 0, false, nil
		})
		for _, halt := range []bool{false, true} {
			res, err := c.Reset(ctx, ResetSystem, halt)
			if !errors.Is(err, ErrDebugLost) || !res.DebugLost || res.Halted {
				t.Fatalf("halt=%v: %+v %v", halt, res, err)
			}
		}
	})
}
