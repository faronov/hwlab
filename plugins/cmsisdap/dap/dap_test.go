package dap

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/faronov/hwlab/internal/adiv5"
	"github.com/faronov/hwlab/internal/cortexm"
)

func attach(t *testing.T) (*adiv5.Target, *Sim, *cortexm.Core) {
	t.Helper()
	p, s, err := SimProbe()
	if err != nil {
		t.Fatal(err)
	}
	tg, err := Attach(p, AttachOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return tg, s, cortexm.New(adiv5.Bus{T: tg})
}

func TestProbeInfo(t *testing.T) {
	p, _, err := SimProbe()
	if err != nil {
		t.Fatal(err)
	}
	i := p.Info()
	if i.Product == "" || i.Serial != "SIM0001" || i.ProtocolVersion != "2.1.0" {
		t.Fatalf("info %+v", i)
	}
	if !i.SupportsSWD() || i.SupportsJTAG() || i.PacketSize != 64 || i.PacketCount != 1 {
		t.Fatalf("caps %+v", i)
	}
}

func TestUnsupportedCommand(t *testing.T) {
	_, s, _ := attach(t)
	resp, err := s.Exchange([]byte{0x7E}, time.Second)
	if err != nil || len(resp) != 1 || resp[0] != respInvalid {
		t.Fatalf("resp % x err %v", resp, err)
	}
}

func TestAttachNoTarget(t *testing.T) {
	p, s, _ := SimProbe()
	s.SetNoTarget(true)
	_, err := Attach(p, AttachOptions{})
	if !errors.Is(err, ErrNoACK) {
		t.Fatalf("want ErrNoACK, got %v", err)
	}
}

func TestAttachWakesDormantDP(t *testing.T) {
	p, s, _ := SimProbe()
	s.SetDormant(true)
	tg, err := Attach(p, AttachOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !tg.WokeFromDormant {
		t.Fatal("the attach did not go through the dormant wake-up")
	}
}

func TestAttachMissingAP(t *testing.T) {
	p, _, _ := SimProbe()
	_, err := Attach(p, AttachOptions{AP: 3})
	if err == nil {
		t.Fatal("expected a missing-AP error")
	}
}

func TestAttachIDs(t *testing.T) {
	tg, _, c := attach(t)
	if tg.DPIDR != simDPIDR || tg.APIDR != simAPIDR {
		t.Fatalf("ids %08x %08x", tg.DPIDR, tg.APIDR)
	}
	ci, err := c.ReadIdentity(context.Background())
	if err != nil || ci.Name != "Cortex-M4" || ci.Variant != 0 || ci.Revision != 1 {
		t.Fatalf("core %+v err %v", ci, err)
	}
}

func TestSWJSequenceValidation(t *testing.T) {
	p, _, _ := SimProbe()
	if err := p.SWJSequence(0, nil); err == nil {
		t.Fatal("0 bits must fail")
	}
	if err := p.SWJSequence(9, []byte{1}); err == nil {
		t.Fatal("length mismatch must fail")
	}
	if err := p.SWJSequence(256, make([]byte, 32)); err != nil {
		t.Fatalf("256 bits: %v", err)
	}
}

func TestReadFlashAndRAM(t *testing.T) {
	tg, _, _ := attach(t)
	b, err := tg.ReadMem(0, 8)
	if err != nil || !bytes.Equal(b, []byte{0, 0, 1, 0x20, 0x01, 0x01, 0, 0}) {
		t.Fatalf("vector table % x err %v", b, err)
	}
}

func TestMemoryWriteReadUnaligned(t *testing.T) {
	tg, s, _ := attach(t)
	base := uint32(simRAMBase + 0x101)
	data := make([]byte, 37)
	for i := range data {
		data[i] = byte(0x40 + i)
	}
	if err := tg.WriteMem(base, data); err != nil {
		t.Fatal(err)
	}
	got, err := tg.ReadMem(base, len(data))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("got % x err %v", got, err)
	}
	// neighbours untouched
	for _, a := range []uint32{base - 1, base + uint32(len(data))} {
		if b, _ := s.Byte(a); b != 0 {
			t.Fatalf("neighbour 0x%x modified: %x", a, b)
		}
	}
}

func TestMemoryLargeCrossesKiB(t *testing.T) {
	tg, s, _ := attach(t)
	data := make([]byte, 5000)
	for i := range data {
		data[i] = byte(i * 7)
	}
	base := uint32(simRAMBase + 0x3F0) // starts just before a 1 KiB boundary
	before := s.Packets()
	if err := tg.WriteMem(base, data); err != nil {
		t.Fatal(err)
	}
	got, err := tg.ReadMem(base, len(data))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("round trip over several KiB differs", err)
	}
	if s.Packets()-before < 50 {
		t.Fatalf("a 64-byte packet size must chunk 5000 B into many packets, got %d", s.Packets()-before)
	}
}

func TestSmallPacketChunking(t *testing.T) {
	s := NewSim()
	s.Packet = 32
	p, err := NewProbe(s)
	if err != nil {
		t.Fatal(err)
	}
	tg, err := Attach(p, AttachOptions{})
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte{0xAB, 0xCD, 0xEF, 0x01}, 100)
	if err := tg.WriteMem(simRAMBase, data); err != nil {
		t.Fatal(err)
	}
	got, err := tg.ReadMem(simRAMBase, len(data))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("round trip with a 32-byte packet size differs", err)
	}
}

func TestUnmappedFaultsThenRecovers(t *testing.T) {
	tg, _, _ := attach(t)
	if _, err := tg.ReadMem(0x60000000, 16); err == nil {
		t.Fatal("unmapped read must fail")
	}
	// the sticky error was cleared; the target is usable again
	if b, err := tg.ReadMem(0, 4); err != nil || len(b) != 4 {
		t.Fatalf("after recovery: % x %v", b, err)
	}
}

func TestFlashWriteRejected(t *testing.T) {
	tg, _, _ := attach(t)
	if err := tg.WriteMem(0x100, []byte{1, 2, 3, 4}); err == nil {
		t.Fatal("a plain write to flash must fail")
	}
	if b, err := tg.ReadMem(0x100, 4); err != nil || len(b) != 4 {
		t.Fatalf("the target must stay usable after the rejected write: % x %v", b, err)
	}
}

func TestWaitRetry(t *testing.T) {
	tg, s, _ := attach(t)
	s.InjectWait(3)
	if _, err := tg.ReadMem(0, 4); err != nil {
		t.Fatalf("WAIT must be retried: %v", err)
	}
	s.InjectWait(1000)
	if _, err := tg.ReadWord(0); !errors.Is(err, ErrWait) {
		t.Fatalf("persistent WAIT must surface, got %v", err)
	}
}

func TestUnalignedWordRejected(t *testing.T) {
	tg, _, _ := attach(t)
	if _, err := tg.ReadWord(2); err == nil {
		t.Fatal("unaligned word read must fail")
	}
}

// The generic Cortex-M layer runs through the DAP simulator exactly as it does
// through a real probe: DAP_Transfer -> ADIv5 -> SCS.

func TestCoreHaltResumeStepThroughDAP(t *testing.T) {
	_, _, c := attach(t)
	ctx := context.Background()
	if st, _ := c.Status(ctx); st.Halted {
		t.Fatal("the core starts running")
	}
	if _, err := c.Halt(ctx); err != nil {
		t.Fatal(err)
	}
	if st, _ := c.Status(ctx); !st.Halted || !st.DebugEnabled {
		t.Fatalf("state %+v", st)
	}
	pc0, err := c.ReadRegister(ctx, cortexm.RegPC)
	if err != nil {
		t.Fatal(err)
	}
	sr, err := c.Step(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if sr.PCAfter == pc0 {
		t.Fatalf("pc did not change: %+v", sr)
	}
	if _, err := c.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	if st, _ := c.Status(ctx); st.Halted {
		t.Fatal("still halted after resume")
	}
}

func TestCoreRegistersThroughDAP(t *testing.T) {
	_, _, c := attach(t)
	ctx := context.Background()
	if _, err := c.Halt(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.WriteRegister(ctx, 3, 0xDEADBEEF); err != nil {
		t.Fatal(err)
	}
	if v, err := c.ReadRegister(ctx, 3); err != nil || v != 0xDEADBEEF {
		t.Fatalf("r3 %x %v", v, err)
	}
	regs, err := c.ReadAll(ctx)
	if err != nil || regs.R[3] != 0xDEADBEEF {
		t.Fatalf("regs %+v err %v", regs, err)
	}
}

func TestCoreBreakpointAndWatchpointThroughDAP(t *testing.T) {
	tg, _, c := attach(t)
	ctx := context.Background()
	fpb, err := c.DiscoverFPB(ctx)
	if err != nil || fpb.Info.NumCode == 0 {
		t.Fatalf("FPB %+v %v", fpb, err)
	}
	dwt, err := c.DiscoverDWT(ctx)
	if err != nil || dwt.NumComp == 0 {
		t.Fatalf("DWT %+v %v", dwt, err)
	}
	if _, err := c.Halt(ctx); err != nil {
		t.Fatal(err)
	}
	// Find a real instruction boundary by stepping, then rewind to the start.
	start, _ := c.ReadRegister(ctx, cortexm.RegPC)
	var target uint32
	for i := 0; i < 4; i++ {
		sr, err := c.Step(ctx, false)
		if err != nil {
			t.Fatal(err)
		}
		target = sr.PCAfter
	}
	if err := c.WriteRegister(ctx, cortexm.RegPC, start); err != nil {
		t.Fatal(err)
	}
	if _, err := fpb.Set(ctx, target); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	st, err := c.WaitHalted(ctx, time.Second)
	if err != nil || !st.Halted {
		t.Fatalf("breakpoint not hit: %+v %v", st, err)
	}
	if got, _ := c.ReadRegister(ctx, cortexm.RegPC); got&^1 != target&^1 {
		t.Fatalf("halted at %x, want %x", got, target)
	}
	if err := fpb.Clear(ctx, target); err != nil {
		t.Fatal(err)
	}
	_ = tg
}

func TestResetThroughDAP(t *testing.T) {
	_, s, c := attach(t)
	rr, err := c.Reset(context.Background(), cortexm.ResetSystem, true)
	if err != nil || !rr.Halted || !rr.AtVector {
		t.Fatalf("reset %+v %v", rr, err)
	}
	if s.Resets() < 1 {
		t.Fatalf("resets %d", s.Resets())
	}
}

func TestTransferErrorDone(t *testing.T) {
	p, s, _ := SimProbe()
	_ = s
	_, err := p.Transfer([]Op{{Read: true, Reg: dpDPIDR}}) // before connect: no ACK
	var te *TransferError
	if !errors.As(err, &te) || !errors.Is(err, ErrNoACK) || te.Done != 0 {
		t.Fatalf("err %v", err)
	}
}
