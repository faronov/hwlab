package icepick

import (
	"context"
	"testing"

	"github.com/faronov/hwlab/internal/jtag"
)

// recorder answers every scan with zeros and keeps the ops.
type recorder struct{ ops []jtag.ScanOp }

func (r *recorder) Scans(_ context.Context, ops []jtag.ScanOp) ([][]byte, error) {
	var out [][]byte
	for _, op := range ops {
		r.ops = append(r.ops, op)
		if op.Bits > 0 {
			out = append(out, make([]byte, (op.Bits+7)/8))
		}
	}
	return out, nil
}

func u32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

func TestLinkTAPSequence(t *testing.T) {
	r := &recorder{}
	if err := (&Router{S: r}).LinkTAP(context.Background(), PortCPUDAP); err != nil {
		t.Fatal(err)
	}
	type step struct {
		ir   bool
		bits int
		val  uint32
		end  jtag.State
	}
	want := []step{
		{true, 6, IRConnect, jtag.PauseIR}, {false, 8, ConnectKey, jtag.PauseDR},
		{true, 6, IRRouter, jtag.PauseIR}, {false, 32, 0x81001000, jtag.PauseDR},
		{true, 6, IRRouter, jtag.PauseIR}, {false, 32, 0xA0110048, jtag.PauseDR},
		{true, 6, IRRouter, jtag.PauseIR}, {false, 32, 0xA0112048, jtag.PauseDR},
		{true, 6, IRRouter, jtag.PauseIR}, {false, 32, 0xA0112148, jtag.PauseDR},
		{true, 6, IRBypass, jtag.RunTestIdle},
	}
	if len(r.ops) != len(want)+1 || r.ops[len(want)].Bits != 0 || r.ops[len(want)].Idle != 10 {
		t.Fatalf("ops %+v", r.ops)
	}
	for i, w := range want {
		op := r.ops[i]
		v := uint32(op.Out[0])
		if op.Bits == 32 {
			v = u32(op.Out)
		}
		if op.IR != w.ir || op.Bits != w.bits || v != w.val || op.End != w.end {
			t.Fatalf("op %d: %+v, want %+v", i, op, w)
		}
	}
	// No routing step may pass Run-Test/Idle before the selection is done.
	for _, op := range r.ops[:len(want)-1] {
		if op.End == jtag.RunTestIdle {
			t.Fatalf("Run-Test/Idle entered mid-routing: %+v", op)
		}
	}
}

func TestChipEraseUsesTestTAP(t *testing.T) {
	r := &recorder{}
	if err := (&Router{S: r}).ChipErase(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v := u32(r.ops[9].Out); v != 0x95112148 {
		t.Fatalf("test TAP 5 select 0x%08x", v)
	}
	n := len(r.ops)
	ir, dr := r.ops[n-3], r.ops[n-2]
	if !ir.IR || ir.Bits != 10 || ir.Out[0] != 0xF1 || ir.Out[1] != 0x03 || dr.Bits != 9 || dr.Out[0] != ChipEraseCommand || r.ops[n-1].Idle != 100 {
		t.Fatalf("erase scans %+v %+v %+v", ir, dr, r.ops[n-1])
	}
}

// chain answers ProbeLinked like a DAP (IR 4, IDCODE) behind the ICEPick.
type chain struct{ linked bool }

func (c chain) Scans(_ context.Context, ops []jtag.ScanOp) ([][]byte, error) {
	var out [][]byte
	for _, op := range ops {
		b := make([]byte, (op.Bits+7)/8)
		switch {
		case op.IR && c.linked:
			b[0], b[1] = 0x11, 0x00 // 0001 (DAP) then 000001 (ICEPick)
		case op.IR:
			b[0], b[1] = 0xC1, 0x03 // ICEPick 000001, then the shifted-in ones
		case c.linked:
			b[0], b[1], b[2], b[3] = 0x77, 0x04, 0xA0, 0x4B
		}
		out = append(out, b)
	}
	return out, nil
}

func TestProbeLinked(t *testing.T) {
	l, ok, err := (&Router{S: chain{linked: true}}).ProbeLinked(context.Background(), 4, 0xE)
	if err != nil || !ok || l.IDCode != 0x4BA00477 || l.IRCapture != 0x011 {
		t.Fatalf("%+v %v %v", l, ok, err)
	}
	l, ok, err = (&Router{S: chain{}}).ProbeLinked(context.Background(), 4, 0xE)
	if err != nil || ok {
		t.Fatalf("locked chain reported linked: %+v %v", l, err)
	}
}

func TestRouterValue(t *testing.T) {
	if v := RouterValue(true, BlockDebug, 0, TAPSelect); v != 0xA0112148 {
		t.Fatalf("0x%08x", v)
	}
	if v := RouterValue(false, BlockControl, RegControl, 0); v != 0x01000000 {
		t.Fatalf("0x%08x", v)
	}
}
