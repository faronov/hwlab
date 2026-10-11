package rtt

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"
	"time"
)

// ram is a flat simulated target memory.
type ram struct {
	base   uint32
	b      []byte
	reads  int
	failAt uint32 // reads touching this address fail (0 = never)
}

func (r *ram) Read(_ context.Context, a uint32, n int) ([]byte, error) {
	r.reads++
	if a < r.base || uint64(a)+uint64(n) > uint64(r.base)+uint64(len(r.b)) {
		return nil, fmt.Errorf("bus fault at 0x%08x", a)
	}
	if r.failAt != 0 && r.failAt >= a && r.failAt < a+uint32(n) {
		return nil, errors.New("injected")
	}
	return append([]byte(nil), r.b[a-r.base:a-r.base+uint32(n)]...), nil
}

func (r *ram) Write(_ context.Context, a uint32, d []byte) error {
	if a < r.base || uint64(a)+uint64(len(d)) > uint64(r.base)+uint64(len(r.b)) {
		return fmt.Errorf("bus fault at 0x%08x", a)
	}
	copy(r.b[a-r.base:], d)
	return nil
}

func (r *ram) u32(a uint32) uint32 { return binary.LittleEndian.Uint32(r.b[a-r.base:]) }
func (r *ram) put(a, v uint32)     { binary.LittleEndian.PutUint32(r.b[a-r.base:], v) }

const base = 0x20000000

// build places a control block at cb with one up buffer (upSize) and one down
// buffer (downSize) and returns the memory.
func build(cb uint32, upSize, downSize uint32) *ram {
	r := &ram{base: base, b: make([]byte, 0x4000)}
	copy(r.b[cb-base:], ID)
	r.put(cb+16, 1)
	r.put(cb+20, 1)
	upDesc, downDesc := cb+24, cb+48
	upBuf, downBuf := cb+0x100, cb+0x100+upSize
	copy(r.b[cb+0x80-base:], "Terminal\x00")
	r.put(upDesc, cb+0x80)
	r.put(upDesc+4, upBuf)
	r.put(upDesc+8, upSize)
	r.put(downDesc, cb+0x80)
	r.put(downDesc+4, downBuf)
	r.put(downDesc+8, downSize)
	return r
}

var win = []Range{{base, 0x4000}}

func TestFindAndParse(t *testing.T) {
	ctx := context.Background()
	for _, cb := range []uint32{base, base + 0x3FC, base + 0x400 - 8, base + 0x1000} {
		r := build(cb, 64, 16)
		got, err := Find(ctx, r, win, 1024)
		if err != nil || got != cb {
			t.Fatalf("cb %#x: got %#x err %v", cb, got, err)
		}
		c, err := Parse(ctx, r, got, win)
		if err != nil {
			t.Fatal(err)
		}
		if len(c.Up) != 1 || len(c.Down) != 1 || c.Up[0].Name != "Terminal" || c.Up[0].Size != 64 {
			t.Fatalf("parse %+v", c)
		}
	}
}

func TestFindRejectsFalseAndCorrupt(t *testing.T) {
	ctx := context.Background()
	r := &ram{base: base, b: make([]byte, 0x4000)}
	if _, err := Find(ctx, r, win, 1024); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty RAM: %v", err)
	}
	copy(r.b[0x100:], ID) // identifier alone: zero buffer counts
	if _, err := Find(ctx, r, win, 1024); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bare identifier: %v", err)
	}
	for name, mut := range map[string]func(*ram){
		"too many buffers": func(r *ram) { r.put(base+0x200+16, 1000) },
		"storage outside":  func(r *ram) { r.put(base+0x200+24+4, 0x30000000) },
		"huge size":        func(r *ram) { r.put(base+0x200+24+8, 1<<30) },
		"offset over size": func(r *ram) { r.put(base+0x200+24+12, 9999) },
		"not zero padded":  func(r *ram) { r.b[0x200+12] = 'x' },
	} {
		r := build(base+0x200, 64, 16)
		mut(r)
		if _, err := Find(ctx, r, win, 1024); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: accepted (%v)", name, err)
		}
	}
	r = &ram{base: base, b: make([]byte, 0x4000)}
	if _, err := Find(ctx, r, []Range{{base, 0x8000}}, 1024); err == nil {
		t.Error("a scan past the end of memory must report the fault")
	}
}

func TestReadUpWrapAndOffsets(t *testing.T) {
	ctx := context.Background()
	cbAddr := uint32(base + 0x200)
	r := build(cbAddr, 16, 16)
	cb, err := Parse(ctx, r, cbAddr, win)
	if err != nil {
		t.Fatal(err)
	}
	ch := New(r, cb)
	up := cb.Up[0]
	if d, err := ch.ReadUp(ctx, 0); err != nil || d != nil {
		t.Fatalf("idle: %v %v", d, err)
	}
	write := func(wr uint32, data []byte) { // emulate the target producing data
		for i, b := range data {
			r.b[up.Storage-base+(wr+uint32(i))%16] = b
		}
		r.put(up.Desc+12, (wr+uint32(len(data)))%16)
	}
	write(0, []byte("hello "))
	d, err := ch.ReadUp(ctx, 0)
	if err != nil || string(d) != "hello " || r.u32(up.Desc+16) != 6 {
		t.Fatalf("first %q %v rd=%d", d, err, r.u32(up.Desc+16))
	}
	write(6, []byte("0123456789")) // wraps: rd=6, wr=0
	d, err = ch.ReadUp(ctx, 0)
	if err != nil || string(d) != "0123456789" || r.u32(up.Desc+16) != 0 {
		t.Fatalf("wrap %q %v rd=%d", d, err, r.u32(up.Desc+16))
	}
	write(0, []byte("abcdefghijklmn")) // wr=14, rd=0
	write(14, []byte("OP"))            // wr=0 again after wrap: rd=0 -> would look empty
	if d, _ := ch.ReadUp(ctx, 0); d != nil {
		t.Fatalf("wr==rd must read as empty, got %q", d)
	}
}

func TestReadUpBoundsAndFailure(t *testing.T) {
	ctx := context.Background()
	cbAddr := uint32(base + 0x200)
	r := build(cbAddr, 16, 16)
	cb, _ := Parse(ctx, r, cbAddr, win)
	ch := New(r, cb)
	up := cb.Up[0]
	r.put(up.Desc+12, 99) // corrupted WrOff
	if _, err := ch.ReadUp(ctx, 0); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("want ErrCorrupt, got %v", err)
	}
	r.put(up.Desc+12, 8)
	r.failAt = up.Storage + 2
	if _, err := ch.ReadUp(ctx, 0); err == nil {
		t.Fatal("a failed data read must be an error")
	}
	if r.u32(up.Desc+16) != 0 {
		t.Fatal("RdOff must not advance after a failed read")
	}
	r.failAt = 0
	if _, err := ch.ReadUp(ctx, 3); err == nil {
		t.Fatal("bad channel accepted")
	}
}

func TestWriteDown(t *testing.T) {
	ctx := context.Background()
	cbAddr := uint32(base + 0x200)
	r := build(cbAddr, 16, 8)
	cb, _ := Parse(ctx, r, cbAddr, win)
	ch := New(r, cb)
	dn := cb.Down[0]
	n, err := ch.WriteDown(ctx, 0, []byte("abcdefghij")) // capacity 7
	if err != nil || n != 7 || r.u32(dn.Desc+12) != 7 {
		t.Fatalf("n=%d err=%v wr=%d", n, err, r.u32(dn.Desc+12))
	}
	if n, _ := ch.WriteDown(ctx, 0, []byte("z")); n != 0 {
		t.Fatalf("full buffer accepted %d bytes", n)
	}
	r.put(dn.Desc+16, 5) // the target consumed 5
	n, err = ch.WriteDown(ctx, 0, []byte("XYZ"))
	if err != nil || n != 3 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	got := r.b[dn.Storage-base : dn.Storage-base+8]
	if !bytes.Equal(got, []byte("YZcdefgX")) {
		t.Fatalf("ring content %q", got)
	}
	if r.u32(dn.Desc+12) != 2 {
		t.Fatalf("wr=%d", r.u32(dn.Desc+12))
	}
}

func TestBackoff(t *testing.T) {
	b := Backoff{Min: 5 * time.Millisecond, Max: 40 * time.Millisecond}
	var got []time.Duration
	got = append(got, b.Next(true), b.Next(false), b.Next(false), b.Next(false), b.Next(false), b.Next(true))
	want := []time.Duration{5, 10, 20, 40, 40, 5}
	for i := range want {
		if got[i] != want[i]*time.Millisecond {
			t.Fatalf("step %d: %v want %v", i, got[i], want[i]*time.Millisecond)
		}
	}
}
