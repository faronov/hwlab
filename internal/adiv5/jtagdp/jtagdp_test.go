package jtagdp

import (
	"context"
	"errors"
	"testing"

	"github.com/faronov/hwlab/internal/adiv5"
	"github.com/faronov/hwlab/internal/jtag"
)

// fakeDAP is a JTAG-DP (DPv0: DP address 0 reads 0) with a MEM-AP over a
// small memory, behind a one-bit BYPASS TAP on the TDI side.
type fakeDAP struct {
	t        *testing.T
	ir       uint32
	res      uint32
	ctrl     uint32
	sel, tar uint32
	mem      map[uint32]uint32
	// waitAt answers WAIT (and ignores the request) for those DR scan
	// numbers; executed counts accepted requests.
	waitAt   map[int]bool
	drScans  int
	executed int
	writes   []uint32
}

func newFake(t *testing.T) *fakeDAP {
	return &fakeDAP{t: t, mem: map[uint32]uint32{0x1000: 0xAAAA0001, 0x1004: 0xBBBB0002, 0x1008: 0xCCCC0003}, waitAt: map[int]bool{}}
}

func bits(b []byte, off, n int) uint64 {
	var v uint64
	for i := 0; i < n; i++ {
		if j := off + i; b[j/8]>>(j%8)&1 == 1 {
			v |= 1 << i
		}
	}
	return v
}

func (f *fakeDAP) Scans(_ context.Context, ops []jtag.ScanOp) ([][]byte, error) {
	var out [][]byte
	for _, op := range ops {
		if op.Bits == 0 {
			continue
		}
		b := make([]byte, (op.Bits+7)/8)
		if op.IR {
			if op.Bits != 10 || bits(op.Out, 4, 6) != 0x3F {
				f.t.Fatalf("IR scan %d bits %x: the ICEPick must stay in BYPASS", op.Bits, op.Out)
			}
			f.ir = uint32(bits(op.Out, 0, 4))
			b[0] = 0x11
			out = append(out, b)
			continue
		}
		if op.Bits != 36 {
			f.t.Fatalf("DR scan of %d bits", op.Bits)
		}
		n := f.drScans
		f.drScans++
		if f.waitAt[n] {
			b[0] = 1
			out = append(out, b)
			continue
		}
		v := uint64(2) | uint64(f.res)<<3
		for i := 0; i < 35; i++ {
			if v>>i&1 == 1 {
				b[i/8] |= 1 << (i % 8)
			}
		}
		out = append(out, b)
		f.exec(bits(op.Out, 0, 35))
	}
	return out, nil
}

func (f *fakeDAP) exec(req uint64) {
	f.executed++
	rnw, a, data := req&1 == 1, byte(req>>1&3)<<2, uint32(req>>3)
	switch f.ir {
	case IRDPACC:
		switch {
		case rnw && a == 0x4:
			f.res = f.ctrl
		case rnw:
			f.res = 0
		case a == 0x4:
			f.writes = append(f.writes, data)
			f.ctrl = data&0x50000000 | (data>>1&0x50000000)<<1 // power acks follow requests
		case a == 0x8:
			f.sel = data
		}
	case IRAPACC:
		reg := f.sel&0xF0 | uint32(a)
		switch {
		case rnw && reg == 0xFC:
			f.res = 0x24770011
		case rnw && reg == 0x0C:
			f.res = f.mem[f.tar]
			f.tar += 4
		case rnw:
			f.res = 0
		case reg == 0x04:
			f.tar = data
		case reg == 0x0C:
			f.mem[f.tar] = data
			f.tar += 4
		}
	}
}

func newLink(f *fakeDAP) *Link {
	return &Link{S: f, IRPost: 6, DRPost: 1, IDCode: 0x4BA00477}
}

func TestPostedReadsLandOnTheirOps(t *testing.T) {
	f := newFake(t)
	f.ctrl = 0xF0000000
	l := newLink(f)
	v, err := l.Transfer([]adiv5.Op{{Read: true, Reg: 0x4}, {AP: true, Read: true, Reg: 0xC}, {Read: true, Reg: 0x0}})
	if err != nil {
		t.Fatal(err)
	}
	// DP 0 reads 0 on a DPv0 JTAG-DP: the IDCODE stands in for DPIDR.
	if len(v) != 3 || v[0] != 0xF0000000 || v[1] != 0 || v[2] != 0x4BA00477 || !l.DPIDRFromIDCode {
		t.Fatalf("%x", v)
	}
}

func TestBlockReadWrite(t *testing.T) {
	f := newFake(t)
	l := newLink(f)
	if _, err := l.Transfer([]adiv5.Op{{AP: true, Reg: 0x4, Value: 0x1000}}); err != nil {
		t.Fatal(err)
	}
	got, err := l.BlockRead(true, 0xC, 3)
	if err != nil || len(got) != 3 || got[0] != 0xAAAA0001 || got[2] != 0xCCCC0003 {
		t.Fatalf("%x %v", got, err)
	}
	if _, err := l.Transfer([]adiv5.Op{{AP: true, Reg: 0x4, Value: 0x2000}}); err != nil {
		t.Fatal(err)
	}
	if err := l.BlockWrite(true, 0xC, []uint32{1, 2}); err != nil || f.mem[0x2000] != 1 || f.mem[0x2004] != 2 {
		t.Fatalf("%v %v", err, f.mem)
	}
}

func TestWaitIsRetriedInOrder(t *testing.T) {
	f := newFake(t)
	l := newLink(f)
	if _, err := l.Transfer([]adiv5.Op{{AP: true, Reg: 0x4, Value: 0x1000}}); err != nil {
		t.Fatal(err)
	}
	// The second and third DRW reads see WAIT, and so does the RDBUFF
	// scan after them: the whole tail is ignored by the DP and re-issued.
	base := f.drScans
	f.waitAt[base+1], f.waitAt[base+2], f.waitAt[base+3] = true, true, true
	got, err := l.BlockRead(true, 0xC, 3)
	if err != nil || len(got) != 3 || got[0] != 0xAAAA0001 || got[1] != 0xBBBB0002 || got[2] != 0xCCCC0003 {
		t.Fatalf("%x %v", got, err)
	}
	if f.executed != 1+4 {
		t.Fatalf("%d requests executed, want each exactly once", f.executed)
	}
}

func TestWaitThenAcceptedIsReordered(t *testing.T) {
	f := newFake(t)
	l := newLink(f)
	f.waitAt[1] = true // a WAIT in the middle, the next request accepted
	_, err := l.BlockRead(true, 0xC, 3)
	if !errors.Is(err, ErrReordered) || errors.Is(err, adiv5.ErrWait) {
		t.Fatalf("%v", err)
	}
}

func TestStuckTDOIsNoAck(t *testing.T) {
	l := &Link{S: stuck{}, IRPost: 6, DRPost: 1}
	_, err := l.Transfer([]adiv5.Op{{Read: true, Reg: 0x4}})
	if !errors.Is(err, adiv5.ErrNoACK) {
		t.Fatalf("%v", err)
	}
}

type stuck struct{}

func (stuck) Scans(_ context.Context, ops []jtag.ScanOp) ([][]byte, error) {
	var out [][]byte
	for _, op := range ops {
		if op.Bits > 0 {
			out = append(out, make([]byte, (op.Bits+7)/8))
		}
	}
	return out, nil
}

func TestWriteABORTClearsStickyThroughCTRL(t *testing.T) {
	f := newFake(t)
	f.ctrl = 0xF0000020 // powered, STICKYERR
	l := newLink(f)
	if err := l.WriteABORT(0x1E); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 1 || f.writes[0] != 0x50000032 {
		t.Fatalf("CTRL/STAT writes %x: want power requests kept and the sticky flags written as ones", f.writes)
	}
}
