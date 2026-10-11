package swd_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/faronov/hwlab/internal/adiv5"
	"github.com/faronov/hwlab/plugins/jlinknative/jlerr"
	"github.com/faronov/hwlab/plugins/jlinknative/protocol"
	"github.com/faronov/hwlab/plugins/jlinknative/sim"
	"github.com/faronov/hwlab/plugins/jlinknative/swd"
)

func rig(t *testing.T) (*sim.Sim, *protocol.Client, *swd.Link) {
	t.Helper()
	s := sim.New()
	c := protocol.New(s)
	ctx := context.Background()
	if _, err := c.SelectInterface(ctx, protocol.IfSWD); err != nil {
		t.Fatal(err)
	}
	return s, c, swd.New(c, c)
}

func TestConnectAndMemory(t *testing.T) {
	_, _, l := rig(t)
	tg, err := adiv5.Connect(l, 0)
	if err != nil {
		t.Fatal(err)
	}
	if tg.DPIDR != 0x2BA01477 || tg.APIDR != 0x24770011 {
		t.Fatalf("DPIDR %08x APIDR %08x", tg.DPIDR, tg.APIDR)
	}
	want := make([]byte, 300)
	for i := range want {
		want[i] = byte(i*7 + 1)
	}
	if err := tg.WriteMem(0x20000001, want); err != nil {
		t.Fatal(err)
	}
	got, err := tg.ReadMem(0x20000001, len(want))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("readback differs")
	}
}

func TestBlockSpansCommands(t *testing.T) {
	s, _, l := rig(t)
	tg, err := adiv5.Connect(l, 0)
	if err != nil {
		t.Fatal(err)
	}
	before := s.Sequences()
	data := make([]byte, 1024)
	for i := range data {
		data[i] = byte(i ^ 0x5A)
	}
	if err := tg.WriteMem(0x20000000, data); err != nil {
		t.Fatal(err)
	}
	got, err := tg.ReadMem(0x20000000, len(data))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("readback: %v", err)
	}
	if n := s.Sequences() - before; n > 40 {
		t.Fatalf("1 KiB write+read took %d probe commands; batching is not working", n)
	}
}

func TestWaitIsRetried(t *testing.T) {
	s, _, l := rig(t)
	tg, err := adiv5.Connect(l, 0)
	if err != nil {
		t.Fatal(err)
	}
	s.InjectWait(3)
	if _, err := tg.ReadWord(0x20000000); err != nil {
		t.Fatalf("WAIT was not recovered: %v", err)
	}
	if l.Stats().Waits == 0 || l.Stats().Resyncs == 0 {
		t.Fatalf("stats %+v", l.Stats())
	}
	// State survived the resync: memory still reads correctly.
	if err := tg.WriteWord(0x20000010, 0xCAFEF00D); err != nil {
		t.Fatal(err)
	}
	if v, err := tg.ReadWord(0x20000010); err != nil || v != 0xCAFEF00D {
		t.Fatalf("%08x %v", v, err)
	}
}

func TestFaultIsReportedNotSwallowed(t *testing.T) {
	s, _, l := rig(t)
	tg, err := adiv5.Connect(l, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.ReadMem(0x70000000, 4); err == nil {
		t.Fatal("read of an unmapped address succeeded")
	}
	// The target recovers and works again.
	if _, err := tg.ReadWord(0x20000000); err != nil {
		t.Fatalf("not recovered: %v", err)
	}
	s.InjectFault(1)
	if _, err = tg.ReadMem(0x20000000, 4); err == nil {
		t.Fatal("injected FAULT was swallowed")
	}
}

func TestNoTarget(t *testing.T) {
	s, _, l := rig(t)
	s.SetNoTarget(true)
	_, err := adiv5.Connect(l, 0)
	if err == nil {
		t.Fatal("connect without a target succeeded")
	}
	if !errors.Is(err, adiv5.ErrNoACK) {
		t.Fatalf("want ErrNoACK, got %v", err)
	}
	if !jlerr.Is(err, jlerr.SWDNoACK) {
		t.Fatalf("class mismatch, want %v: %v", jlerr.SWDNoACK, err)
	}
}

func TestTargetPowerOffClass(t *testing.T) {
	s, _, l := rig(t)
	s.SetNoTarget(true)
	s.SetVTarget(0)
	_, err := adiv5.Connect(l, 0)
	if !jlerr.Is(err, jlerr.TargetPowerOff) {
		t.Fatalf("class mismatch, want %v: %v", jlerr.TargetPowerOff, err)
	}
}

func TestParityErrorIsNeverSuccess(t *testing.T) {
	s, _, l := rig(t)
	if _, err := adiv5.Connect(l, 0); err != nil {
		t.Fatal(err)
	}
	s.CorruptParity(1)
	_, err := l.Transfer([]adiv5.Op{{Read: true, Reg: 0}})
	if err == nil {
		t.Fatal("bad parity accepted")
	}
	if !jlerr.Is(err, jlerr.ProtocolError) {
		t.Fatalf("class mismatch, want %v: %v", jlerr.ProtocolError, err)
	}
}

func TestShortReplyIsNeverSuccess(t *testing.T) {
	l := swd.New(shortRunner{}, nil)
	if _, err := l.Transfer([]adiv5.Op{{Read: true, Reg: 0}}); err == nil {
		t.Fatal("short reply accepted")
	}
}

type shortRunner struct{}

func (shortRunner) Run(context.Context, protocol.Sequence) (protocol.SequenceResult, error) {
	return protocol.SequenceResult{In: []byte{0}}, nil
}

func TestRequestEncoding(t *testing.T) {
	// DPIDR read: start, DP, read, A=0, parity 1, stop, park = 0xA5.
	if got := swd.Request(false, true, 0); got != 0xA5 {
		t.Fatalf("DPIDR read request %#x", got)
	}
	// AP write to 0x0C? A2=1,A3=1: APnDP=1, RnW=0 -> parity 1^0^1^1 = 1.
	if got := swd.Request(true, false, 0x0C); got != 0xBB {
		t.Fatalf("AP write request %#x", got)
	}
}
