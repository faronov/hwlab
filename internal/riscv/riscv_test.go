package riscv_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/faronov/hwlab/internal/jtag"
	"github.com/faronov/hwlab/internal/riscv"
	"github.com/faronov/hwlab/internal/riscv/rvsim"
)

var ctx = context.Background()

func mem() []rvsim.Region {
	d := make([]byte, 64)
	for i := range d {
		d[i] = byte(0x10 + i)
	}
	return []rvsim.Region{{Base: 0x40800000, Data: d}}
}

func setup(t *testing.T, cfg rvsim.Config) (*riscv.DM, *rvsim.Target) {
	t.Helper()
	tgt := rvsim.New(cfg)
	dtm, err := riscv.NewDTM(ctx, jtag.NewDriver(rvsim.Port{T: tgt}), 5)
	if err != nil {
		t.Fatal(err)
	}
	dm := riscv.NewDM(dtm)
	dm.Timeout = 200 * time.Millisecond
	return dm, tgt
}

func TestDTMOpen(t *testing.T) {
	dm, _ := setup(t, rvsim.Config{})
	cs := dm.DTM().CS()
	if cs.Version != 1 || cs.ABits != 7 || cs.Idle != 1 {
		t.Fatalf("%+v", cs)
	}
	id, err := dm.DTM().IDCode(ctx)
	if err != nil || id != 0x0000dc25 {
		t.Fatalf("%#x %v", id, err)
	}
}

func TestDTMRejectsBadIRLengthAndNonDTM(t *testing.T) {
	tgt := rvsim.New(rvsim.Config{})
	if _, err := riscv.NewDTM(ctx, jtag.NewDriver(rvsim.Port{T: tgt}), 3); err == nil {
		t.Fatal("IR length 3 accepted")
	}
	// A TAP without a RISC-V DTM: its dtmcs instruction reads as BYPASS (0).
	tgt = rvsim.New(rvsim.Config{IRLen: 8})
	if _, err := riscv.NewDTM(ctx, jtag.NewDriver(rvsim.Port{T: tgt}), 8); err != nil {
		t.Fatalf("8-bit IR with the same instruction codes is still a DTM: %v", err)
	}
}

func TestIdentify(t *testing.T) {
	dm, tgt := setup(t, rvsim.Config{Harts: 2, Memory: mem()})
	info, err := dm.Identify(ctx, 4)
	if err != nil {
		t.Fatal(err)
	}
	if info.Status.Version != 2 || info.Status.VersionName() != "0.13" || info.Harts != 2 || info.ProgBufSize != 16 || info.DataCount != 2 ||
		!info.SBA32 || info.SBASize != 32 || info.SBVersion != 1 || info.Status.State != riscv.HartRunning || !info.ActivatedByUs {
		t.Fatalf("%+v", info)
	}
	if tgt.Halted(0) || tgt.Halts != 0 {
		t.Fatal("identify must not halt")
	}
	if !tgt.Active() {
		t.Fatal("module not active")
	}
}

func TestActivateRelease(t *testing.T) {
	dm, tgt := setup(t, rvsim.Config{})
	if _, err := dm.Status(ctx); err != nil {
		t.Fatal(err)
	}
	if !tgt.Active() {
		t.Fatal("status must activate the module")
	}
	if err := dm.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if tgt.Active() {
		t.Fatal("a module this session activated is deactivated again while the hart runs")
	}
	// Already active before the session: left alone.
	dm, tgt = setup(t, rvsim.Config{Active: true})
	if _, err := dm.Status(ctx); err != nil {
		t.Fatal(err)
	}
	if err := dm.Release(ctx); err != nil || !tgt.Active() {
		t.Fatalf("pre-active module touched: %v", err)
	}
	// A halted hart keeps the module active.
	dm, tgt = setup(t, rvsim.Config{})
	if _, err := dm.Halt(ctx); err != nil {
		t.Fatal(err)
	}
	if err := dm.Release(ctx); err != nil || !tgt.Active() || !tgt.Halted(0) {
		t.Fatalf("release disturbed a halted hart: %v", err)
	}
}

func TestHaltResume(t *testing.T) {
	dm, tgt := setup(t, rvsim.Config{})
	st, err := dm.Halt(ctx)
	if err != nil || st.State != riscv.HartHalted || !tgt.Halted(0) {
		t.Fatalf("%+v %v", st, err)
	}
	// haltreq must be dropped again so a later resume sticks.
	st, err = dm.Resume(ctx)
	if err != nil || st.State != riscv.HartRunning || tgt.Halted(0) || !st.ResumeAck {
		t.Fatalf("%+v %v", st, err)
	}
	if tgt.Halts != 1 || tgt.Resumes != 1 {
		t.Fatalf("halts %d resumes %d", tgt.Halts, tgt.Resumes)
	}
}

func TestNonexistentHart(t *testing.T) {
	dm, _ := setup(t, rvsim.Config{Harts: 1})
	if err := dm.Select(ctx, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := dm.Halt(ctx); !errors.Is(err, riscv.ErrNoHart) {
		t.Fatalf("%v", err)
	}
}

func TestReadRegs(t *testing.T) {
	dm, tgt := setup(t, rvsim.Config{})
	tgt.SetGPR(10, 0xDEADBEEF)
	if _, err := dm.ReadReg(ctx, riscv.GPR(10)); !errors.Is(err, riscv.ErrNotHalted) {
		t.Fatalf("running hart: %v", err)
	}
	if _, err := dm.Halt(ctx); err != nil {
		t.Fatal(err)
	}
	v, err := dm.ReadReg(ctx, riscv.GPR(10))
	if err != nil || v != 0xDEADBEEF {
		t.Fatalf("%#x %v", v, err)
	}
	if v, err = dm.ReadReg(ctx, riscv.CSRMVendorID); err != nil || v != 0x612 {
		t.Fatalf("%#x %v", v, err)
	}
	// An unknown CSR raises an exception; the error is cleared so the next
	// access works.
	if _, err = dm.ReadReg(ctx, 0x0BAD); !errors.Is(err, riscv.ErrAbstract) {
		t.Fatalf("%v", err)
	}
	if v, err = dm.ReadReg(ctx, riscv.CSRMISA); err != nil || v != 0x40903105 {
		t.Fatalf("after cmderr: %#x %v", v, err)
	}
}

func TestReadMem(t *testing.T) {
	dm, tgt := setup(t, rvsim.Config{Memory: mem()})
	want := func(off, n int) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(0x10 + off + i)
		}
		return b
	}
	for _, c := range []struct {
		off, n int
	}{{0, 4}, {0, 16}, {4, 20}, {2, 9}, {1, 1}, {0, 64}} {
		tgt.SBAReads = 0
		got, err := dm.ReadMem(ctx, 0x40800000+uint32(c.off), c.n)
		if err != nil || !bytes.Equal(got, want(c.off, c.n)) {
			t.Fatalf("%+v: % x %v", c, got, err)
		}
		words := (c.off+c.n+3)/4 - c.off/4
		if tgt.SBAReads != words {
			t.Fatalf("%+v: %d bus reads for %d words (an extra read-on-data leaked)", c, tgt.SBAReads, words)
		}
	}
	if tgt.Halted(0) {
		t.Fatal("system bus reads must not halt the hart")
	}
}

func TestReadMemErrors(t *testing.T) {
	dm, _ := setup(t, rvsim.Config{Memory: mem()})
	if _, err := dm.ReadMem(ctx, 0x40800000+60, 16); !errors.Is(err, riscv.ErrBusAccess) {
		t.Fatalf("read past the region: %v", err)
	}
	// The sticky bus error was cleared.
	if b, err := dm.ReadMem(ctx, 0x40800000, 4); err != nil || len(b) != 4 {
		t.Fatalf("%v", err)
	}
	if _, err := dm.ReadMem(ctx, 0xFFFFFFF0, 32); err == nil {
		t.Fatal("wrap accepted")
	}
	if _, err := dm.ReadMem(ctx, 0, 1<<27); err == nil {
		t.Fatal("oversize accepted")
	}
	if b, err := dm.ReadMem(ctx, 0, 0); err != nil || b != nil {
		t.Fatal("empty read")
	}
	dm, _ = setup(t, rvsim.Config{NoSBA: true, Memory: mem()})
	if _, err := dm.ReadMem(ctx, 0x40800000, 4); !errors.Is(err, riscv.ErrNoSBA) {
		t.Fatalf("%v", err)
	}
}

func TestBusyRetry(t *testing.T) {
	dm, tgt := setup(t, rvsim.Config{BusyOps: 3, Memory: mem()})
	before := dm.DTM().Idle()
	b, err := dm.ReadMem(ctx, 0x40800000, 8)
	if err != nil || len(b) != 8 {
		t.Fatalf("%v", err)
	}
	if tgt.BusyCount != 3 || dm.DTM().Idle() <= before {
		t.Fatalf("busy %d, idle %d -> %d", tgt.BusyCount, before, dm.DTM().Idle())
	}
}

func TestPersistentBusy(t *testing.T) {
	dm, _ := setup(t, rvsim.Config{BusyOps: 1000})
	if _, err := dm.Status(ctx); !errors.Is(err, riscv.ErrBusy) {
		t.Fatalf("%v", err)
	}
}
