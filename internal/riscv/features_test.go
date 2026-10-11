package riscv_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/faronov/hwlab/internal/jtag"
	"github.com/faronov/hwlab/internal/riscv"
	"github.com/faronov/hwlab/internal/riscv/rvsim"
)

const (
	sramBase = 0x40800000
	codeBase = 0x42000000
)

// code: c.nop (2), addi x1,x1,1 (4), c.nop (2), c.nop (2), addi (4), j . (4)
func code() []byte {
	return []byte{
		0x01, 0x00,
		0x93, 0x80, 0x10, 0x00,
		0x01, 0x00,
		0x01, 0x00,
		0x93, 0x80, 0x10, 0x00,
		0x6f, 0x00, 0x00, 0x00,
	}
}

func fullCfg() rvsim.Config {
	return rvsim.Config{Harts: 2, Memory: []rvsim.Region{
		{Base: sramBase, Data: make([]byte, 256)},
		{Base: codeBase, Data: code()},
	}}
}

func TestWriteMemWidths(t *testing.T) {
	dm, tgt := setup(t, fullCfg())
	orig, err := dm.ReadMem(ctx, sramBase+0x40, 32)
	if err != nil {
		t.Fatal(err)
	}
	pat := make([]byte, 32)
	for i := range pat {
		pat[i] = byte(0xA0 + i)
	}
	// Aligned 32-bit run, then every unaligned head/tail combination.
	for _, c := range []struct{ off, n int }{{0, 32}, {1, 5}, {2, 2}, {3, 7}, {5, 1}, {6, 9}, {0, 1}, {31, 1}} {
		if err := dm.WriteMem(ctx, sramBase+0x40+uint32(c.off), pat[:c.n]); err != nil {
			t.Fatalf("%+v: %v", c, err)
		}
		got, _ := dm.ReadMem(ctx, sramBase+0x40+uint32(c.off), c.n)
		if !bytes.Equal(got, pat[:c.n]) {
			t.Fatalf("%+v: % x != % x", c, got, pat[:c.n])
		}
	}
	// Neighbours untouched by a 1-byte write.
	if err := dm.WriteMem(ctx, sramBase+0x80, []byte{0xFF, 0xFF, 0xFF, 0xFF}); err != nil {
		t.Fatal(err)
	}
	if err := dm.WriteMem(ctx, sramBase+0x81, []byte{0x11}); err != nil {
		t.Fatal(err)
	}
	if got := tgt.Memory(sramBase+0x80, 4); !bytes.Equal(got, []byte{0xFF, 0x11, 0xFF, 0xFF}) {
		t.Fatalf("% x", got)
	}
	if tgt.SBAWrites == 0 {
		t.Fatal("no writes")
	}
	if err := dm.WriteMem(ctx, sramBase+0x40, orig); err != nil {
		t.Fatal(err)
	}
	if got, _ := dm.ReadMem(ctx, sramBase+0x40, 32); !bytes.Equal(got, orig) {
		t.Fatal("restore failed")
	}
}

func TestWriteMemRMWWhenOnly32(t *testing.T) {
	cfg := fullCfg()
	cfg.Only32 = true
	dm, tgt := setup(t, cfg)
	if err := dm.WriteMem(ctx, sramBase, []byte{1, 2, 3, 4, 5, 6, 7, 8}); err != nil {
		t.Fatal(err)
	}
	if err := dm.WriteMem(ctx, sramBase+1, []byte{0xAA, 0xBB}); err != nil {
		t.Fatal(err)
	}
	if got := tgt.Memory(sramBase, 8); !bytes.Equal(got, []byte{1, 0xAA, 0xBB, 4, 5, 6, 7, 8}) {
		t.Fatalf("% x", got)
	}
	if err := dm.WriteMemWidth(ctx, sramBase, 1, []byte{9}); !errors.Is(err, riscv.ErrWidth) {
		t.Fatalf("8-bit access on a 32-bit-only bus: %v", err)
	}
}

func TestWriteMemErrors(t *testing.T) {
	dm, tgt := setup(t, fullCfg())
	if err := dm.WriteMemWidth(ctx, sramBase+1, 4, make([]byte, 4)); !errors.Is(err, riscv.ErrAlignment) {
		t.Fatalf("misaligned: %v", err)
	}
	if err := dm.WriteMemWidth(ctx, sramBase+2, 2, make([]byte, 3)); !errors.Is(err, riscv.ErrAlignment) {
		t.Fatalf("odd length: %v", err)
	}
	if tgt.SBAWrites != 0 {
		t.Fatal("rejected accesses must not reach the bus")
	}
	// Unmapped: bad address, and the sticky error is cleared afterwards.
	if err := dm.WriteMem(ctx, 0x50000000, []byte{1, 2, 3, 4}); !errors.Is(err, riscv.ErrBusAccess) {
		t.Fatalf("unmapped: %v", err)
	}
	if err := dm.WriteMem(ctx, sramBase, []byte{1, 2, 3, 4}); err != nil {
		t.Fatalf("after bus error: %v", err)
	}
	if err := dm.WriteMem(ctx, 0xFFFFFFFE, []byte{1, 2, 3, 4}); !errors.Is(err, riscv.ErrRangeAccess) {
		t.Fatalf("wrap: %v", err)
	}
	dm, _ = setup(t, rvsim.Config{NoSBA: true})
	if err := dm.WriteMem(ctx, sramBase, []byte{1}); !errors.Is(err, riscv.ErrNoSBA) {
		t.Fatalf("no SBA: %v", err)
	}
}

func TestWriteMemLarge(t *testing.T) {
	cfg := rvsim.Config{Memory: []rvsim.Region{{Base: sramBase, Data: make([]byte, 64<<10)}}}
	dm, _ := setup(t, cfg)
	data := make([]byte, 64<<10-3)
	for i := range data {
		data[i] = byte(i * 7)
	}
	if err := dm.WriteMem(ctx, sramBase+3, data); err != nil {
		t.Fatal(err)
	}
	got, err := dm.ReadMem(ctx, sramBase+3, len(data))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("mismatch %v", err)
	}
}

func TestStep(t *testing.T) {
	dm, tgt := setup(t, fullCfg())
	if _, err := dm.Step(ctx); !errors.Is(err, riscv.ErrNotHalted) {
		t.Fatalf("running: %v", err)
	}
	if _, err := dm.Halt(ctx); err != nil {
		t.Fatal(err)
	}
	dcsr0, _ := dm.ReadReg(ctx, riscv.CSRDCSR)
	// Lengths of the five instructions before the self-loop: 2, 4, 2, 2, 4.
	wantLen := []uint32{2, 4, 2, 2, 4, 0}
	var pc = uint32(codeBase)
	for i, l := range wantLen {
		r, err := dm.Step(ctx)
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if r.Cause != riscv.CauseStep || r.PCBefore != pc || r.PCAfter != pc+l {
			t.Fatalf("step %d: %+v want delta %d", i, r, l)
		}
		pc = r.PCAfter
	}
	if tgt.Steps != len(wantLen) {
		t.Fatal(tgt.Steps)
	}
	dcsr1, _ := dm.ReadReg(ctx, riscv.CSRDCSR)
	if dcsr1&(1<<2) != 0 || dcsr0&(1<<2) != 0 {
		t.Fatalf("step bit not restored: %#x -> %#x", dcsr0, dcsr1)
	}
	if !tgt.Halted(0) {
		t.Fatal("hart must stay halted after a step")
	}
	// Many steps in one call end with the step bit cleared.
	if _, err := dm.StepN(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if v, _ := dm.ReadReg(ctx, riscv.CSRDCSR); v&(1<<2) != 0 {
		t.Fatal("step bit left set")
	}
	if _, err := dm.StepN(ctx, 0); err == nil {
		t.Fatal("zero steps accepted")
	}
}

func TestInstLen(t *testing.T) {
	if riscv.InstLen(0x0001) != 2 || riscv.InstLen(0x8093) != 4 {
		t.Fatal("length")
	}
	if riscv.IsControlTransfer(0x0001, 0) || riscv.IsControlTransfer(0x8093, 0x0010) {
		t.Fatal("nop/addi are sequential")
	}
	if !riscv.IsControlTransfer(0x006f, 0) || !riscv.IsControlTransfer(0xa001, 0) {
		t.Fatal("jal and c.j are control transfers")
	}
}

func TestTriggersEnumerateAndBreakpoint(t *testing.T) {
	cfg := fullCfg()
	cfg.Triggers = 3
	dm, tgt := setup(t, cfg)
	if _, err := dm.Halt(ctx); err != nil {
		t.Fatal(err)
	}
	ts, err := dm.Triggers(ctx)
	if err != nil || len(ts) != 3 {
		t.Fatalf("%d %v", len(ts), err)
	}
	if !ts[0].HasInfo || ts[0].TInfo != 0x44 {
		t.Fatalf("%+v", ts[0])
	}
	saved, err := dm.SaveTriggers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	bp, err := dm.SetBreakpoint(ctx, codeBase+8)
	if err != nil {
		t.Fatal(err)
	}
	t1, t2 := tgt.Trigger(0, bp.Trigger)
	if t1>>28 != 2 || t2 != codeBase+8 || t1&(1<<2) == 0 {
		t.Fatalf("%#x %#x", t1, t2)
	}
	if err := dm.ResumeNoWait(ctx); err != nil {
		t.Fatal(err)
	}
	hi, err := dm.WaitHalt(ctx, 500e6)
	if err != nil {
		t.Fatal(err)
	}
	if hi.Cause != riscv.CauseTrigger || hi.PC != codeBase+8 {
		t.Fatalf("%+v", hi)
	}
	if err := dm.ClearBreakpoint(ctx, bp); err != nil {
		t.Fatal(err)
	}
	after, _ := dm.SaveTriggers(ctx)
	if len(after) != len(saved) {
		t.Fatal("slot count changed")
	}
	for i := range saved {
		if after[i] != saved[i] {
			t.Fatalf("slot %d not restored: %+v != %+v", i, after[i], saved[i])
		}
	}
}

func TestBreakpointSlotExhaustion(t *testing.T) {
	cfg := fullCfg()
	cfg.Triggers = 2
	dm, _ := setup(t, cfg)
	dm.Halt(ctx)
	var bps []riscv.Breakpoint
	for i := 0; i < 2; i++ {
		bp, err := dm.SetBreakpoint(ctx, codeBase+uint32(2*i))
		if err != nil {
			t.Fatal(err)
		}
		bps = append(bps, bp)
	}
	if bps[0].Trigger == bps[1].Trigger {
		t.Fatal("same slot used twice")
	}
	if _, err := dm.SetBreakpoint(ctx, codeBase+4); !errors.Is(err, riscv.ErrNoTrigger) {
		t.Fatalf("%v", err)
	}
	for _, b := range bps {
		if err := dm.ClearBreakpoint(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNoTriggerModule(t *testing.T) {
	cfg := fullCfg()
	cfg.Triggers = -1
	dm, _ := setup(t, cfg)
	dm.Halt(ctx)
	if _, err := dm.Triggers(ctx); !errors.Is(err, riscv.ErrTrigger) {
		t.Fatalf("%v", err)
	}
}

func TestHartsReportAndSelection(t *testing.T) {
	cfg := fullCfg()
	cfg.Harts = 3
	cfg.Unavailable = map[int]bool{2: true}
	cfg.HartCSRs = map[int]map[uint16]uint32{1: {0x301: 0x40001105}}
	dm, tgt := setup(t, cfg)
	if _, err := dm.Halt(ctx); err != nil { // hart 0
		t.Fatal(err)
	}
	reps, err := dm.Harts(ctx, 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(reps) != 3 || reps[0].State != riscv.HartHalted || reps[0].XLEN != 32 || !reps[0].GPRAccess || reps[0].MHartID != 0 ||
		reps[1].State != riscv.HartRunning || reps[1].Halted || reps[2].State != riscv.HartUnavailable {
		t.Fatalf("%+v", reps)
	}
	if dm.Hart() != 0 {
		t.Fatal("selection not restored")
	}
	// Switching harts never disturbs the others.
	for i := 0; i < 20; i++ {
		h := uint32(i % 2)
		if err := dm.Select(ctx, h); err != nil {
			t.Fatal(err)
		}
		st, err := dm.Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		want := riscv.HartRunning
		if h == 0 {
			want = riscv.HartHalted
		}
		if st.State != want {
			t.Fatalf("hart %d: %s", h, st.State)
		}
	}
	if tgt.Halted(1) || tgt.Halts != 1 {
		t.Fatal("hart 1 disturbed")
	}
	if err := dm.Select(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := dm.Halt(ctx); err != nil {
		t.Fatal(err)
	}
	v, _ := dm.ReadReg(ctx, riscv.CSRMHartID)
	g, _ := dm.ReadReg(ctx, riscv.GPR(5))
	if v != 1 || g != 0x10000+0x1005 {
		t.Fatalf("hart 1 view: mhartid %d x5 %#x", v, g)
	}
	n, err := dm.HartCount(ctx, 8)
	if err != nil || n != 3 {
		t.Fatalf("%d %v", n, err)
	}
	// Unavailable hart: halt must not report success.
	if err := dm.Select(ctx, 2); err != nil {
		t.Fatal(err)
	}
	dm.Timeout = 50e6
	if _, err := dm.Halt(ctx); err == nil {
		t.Fatal("halt of an unavailable hart succeeded")
	}
}

func TestResetKinds(t *testing.T) {
	dm, tgt := setup(t, fullCfg())
	if _, err := dm.Halt(ctx); err != nil {
		t.Fatal(err)
	}
	if err := dm.Reset(ctx, riscv.ResetSystem, true); err != nil {
		t.Fatal(err)
	}
	if tgt.Resets != 1 {
		t.Fatal(tgt.Resets)
	}
	hi, err := dm.WaitHalt(ctx, 500e6)
	if err != nil || hi.Cause != riscv.CauseResetHalt {
		t.Fatalf("%+v %v", hi, err)
	}
	st, _ := dm.Status(ctx)
	if !st.HaveReset {
		t.Fatal("havereset not reported")
	}
	if err := dm.FinishReset(ctx); err != nil {
		t.Fatal(err)
	}
	if st, _ = dm.Status(ctx); st.HaveReset {
		t.Fatal("havereset not acknowledged")
	}
	if err := dm.Reset(ctx, riscv.ResetHart, false); err != nil {
		t.Fatal(err)
	}
	if err := dm.Reset(ctx, "bogus", false); err == nil {
		t.Fatal("bogus kind accepted")
	}
}

func TestPipelineMatchesSerial(t *testing.T) {
	dm, _ := setup(t, fullCfg())
	dm.Halt(ctx)
	regs := []riscv.RegNum{riscv.GPR(1), riscv.GPR(2), riscv.CSRMISA, riscv.CSRDPC, riscv.CSRDCSR}
	got, err := dm.ReadRegs(ctx, regs)
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range regs {
		v, err := dm.ReadReg(ctx, r)
		if err != nil || v != got[i] {
			t.Fatalf("%#x: %#x vs %#x %v", uint16(r), v, got[i], err)
		}
	}
	if err := dm.WriteReg(ctx, riscv.GPR(7), 0x1234); err != nil {
		t.Fatal(err)
	}
	if v, _ := dm.ReadReg(ctx, riscv.GPR(7)); v != 0x1234 {
		t.Fatal(v)
	}
	if err := dm.WriteReg(ctx, 0x0BAD, 1); !errors.Is(err, riscv.ErrAbstract) {
		t.Fatalf("%v", err)
	}
}

func TestPipelineBusyRetry(t *testing.T) {
	dm, tgt := setup(t, rvsim.Config{BusyOps: 2, Memory: fullCfg().Memory})
	got, err := dm.ReadMem(ctx, codeBase, 8)
	if err != nil || !bytes.Equal(got, code()[:8]) {
		t.Fatalf("%v % x", err, got)
	}
	if tgt.BusyCount != 2 {
		t.Fatal(tgt.BusyCount)
	}
}

func TestBatchedPortAndRoundTrips(t *testing.T) {
	tgt := rvsim.New(fullCfg())
	bp := &rvsim.BatchPort{Port: rvsim.Port{T: tgt}}
	dtm, err := riscv.NewDTM(ctx, jtag.NewDriver(bp), 5)
	if err != nil {
		t.Fatal(err)
	}
	dm := riscv.NewDM(dtm)
	data := make([]byte, 512)
	for i := range data {
		data[i] = byte(i)
	}
	if err := dm.WriteMem(ctx, sramBase, data[:256]); err != nil {
		t.Fatal(err)
	}
	before := bp.Calls
	got, err := dm.ReadMem(ctx, sramBase, 256)
	if err != nil || !bytes.Equal(got, data[:256]) {
		t.Fatalf("%v", err)
	}
	// 64 words are fetched in a single pipelined batch.
	if n := bp.Calls - before; n != 1 {
		t.Fatalf("%d batches for a 256 B read", n)
	}
	dm.Halt(ctx)
	before = bp.Calls
	if _, err := dm.ReadRegs(ctx, []riscv.RegNum{riscv.GPR(1), riscv.GPR(2), riscv.GPR(3)}); err != nil {
		t.Fatal(err)
	}
	if n := bp.Calls - before; n > 2 {
		t.Fatalf("%d batches for 3 registers", n)
	}
}

func TestBusBatch(t *testing.T) {
	var wrote []uint32
	cfg := fullCfg()
	cfg.Devices = []rvsim.Device{{Base: 0x60003000, Size: 0x100,
		Read:  func(off uint32) uint32 { return 0xC0DE0000 + off },
		Write: func(off, v uint32) { wrote = append(wrote, off, v) }}}
	dm, _ := setup(t, cfg)
	res, err := dm.BusBatch(ctx, []riscv.BusOp{
		{Kind: riscv.BusWrite, Addr: 0x60003018, Vals: []uint32{1, 2, 3}},
		{Kind: riscv.BusRead, Addr: 0x60003000, Words: 1},
		{Kind: riscv.BusRead, Addr: 0x60003058, Words: 4},
		{Kind: riscv.BusRead, Addr: 0x6000302C, Words: 1},
		{Kind: riscv.BusWrite, Addr: 0x60003000, Vals: []uint32{0x40000}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res[0] != nil || res[1][0] != 0xC0DE0000 || len(res[2]) != 4 || res[2][3] != 0xC0DE0058+12 || res[3][0] != 0xC0DE002C {
		t.Fatalf("%x", res)
	}
	want := []uint32{0x18, 1, 0x1C, 2, 0x20, 3, 0, 0x40000}
	if len(wrote) != len(want) {
		t.Fatalf("%x", wrote)
	}
	for i := range want {
		if wrote[i] != want[i] {
			t.Fatalf("%x", wrote)
		}
	}
	if _, err := dm.BusBatch(ctx, []riscv.BusOp{{Kind: riscv.BusRead, Addr: 0x60003001, Words: 1}}); !errors.Is(err, riscv.ErrAlignment) {
		t.Fatal(err)
	}
	if _, err := dm.BusBatch(ctx, []riscv.BusOp{{Kind: riscv.BusRead, Addr: 0x70000000, Words: 1}}); !errors.Is(err, riscv.ErrBusAccess) {
		t.Fatal(err)
	}
	if _, err := dm.BusBatch(ctx, []riscv.BusOp{{Kind: riscv.BusRead, Addr: 0x60003000, Words: 0}}); err == nil {
		t.Fatal("empty read accepted")
	}
}
