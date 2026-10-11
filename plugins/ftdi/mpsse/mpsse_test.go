package mpsse_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/faronov/hwlab/internal/usb"
	"github.com/faronov/hwlab/plugins/ftdi/ftdi"
	"github.com/faronov/hwlab/plugins/ftdi/mpsse"
	"github.com/faronov/hwlab/plugins/ftdi/sim"
)

func TestCommandEncoding(t *testing.T) {
	b := mpsse.NewBuilder()
	b.SetLow(0x08, 0x0B)
	b.SetHigh(0x00, 0xFF)
	b.Loopback(false)
	b.SetDivisor(0x0102)
	g := b.GetLow()
	b.TMS(0b101, 3, true, false)
	b.TMS(0b1, 1, false, true)
	b.ShiftBits(0xA5, 5, true)
	b.ShiftBits(0x01, 1, false)
	b.ShiftBytes([]byte{1, 2, 3}, true)
	b.ShiftBytes([]byte{9}, false)
	b.SendImmediate()
	want := []byte{
		0x80, 0x08, 0x0B,
		0x82, 0x00, 0xFF,
		0x85,
		0x86, 0x02, 0x01,
		0x81,
		0x4B, 2, 0x85, // TMS=101, TDI=1 in bit 7
		0x6B, 0, 0x01,
		0x3B, 4, 0xA5,
		0x1B, 0, 0x01,
		0x39, 2, 0, 1, 2, 3,
		0x19, 0, 0, 9,
		0x87,
	}
	if got := b.Bytes(); !bytes.Equal(got, want) {
		t.Fatalf("encoding\n got % x\nwant % x", got, want)
	}
	// replies: GetLow 1, TMS read 1, bits read 1, bytes read 3
	if b.Reply() != 6 {
		t.Fatalf("reply %d", b.Reply())
	}
	if g.Off != 0 || g.Len != 1 {
		t.Fatalf("GetLow resp %+v", g)
	}
	if b.Err() != nil {
		t.Fatal(b.Err())
	}
}

func TestEncodingRangeErrors(t *testing.T) {
	for name, f := range map[string]func(b *mpsse.Builder){
		"tms0":   func(b *mpsse.Builder) { b.TMS(0, 0, false, false) },
		"tms8":   func(b *mpsse.Builder) { b.TMS(0, 8, false, false) },
		"bits0":  func(b *mpsse.Builder) { b.ShiftBits(0, 0, false) },
		"bits9":  func(b *mpsse.Builder) { b.ShiftBits(0, 9, false) },
		"clkbit": func(b *mpsse.Builder) { b.ClockBits(9) },
	} {
		b := mpsse.NewBuilder()
		f(b)
		if b.Err() == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestLargeShiftIsSplit(t *testing.T) {
	b := mpsse.NewBuilder()
	b.MaxCmdBytes = 100
	rs := b.ShiftBytes(make([]byte, 250), true)
	if len(rs) != 3 || rs[0].Len != 100 || rs[2].Len != 50 || rs[2].Off != 200 {
		t.Fatalf("split: %+v", rs)
	}
	if b.Reply() != 250 {
		t.Fatal(b.Reply())
	}
	buf := b.Bytes()
	// first command header: 0x39, len-1 = 99
	if buf[0] != 0x39 || buf[1] != 99 || buf[2] != 0 {
		t.Fatalf("header % x", buf[:3])
	}
}

func TestDivisor(t *testing.T) {
	cases := []struct {
		base, hz uint32
		div      uint16
		actual   uint32
		bad      bool
	}{
		{6_000_000, 3_000_000, 0, 3_000_000, false},
		{6_000_000, 1_000_000, 2, 1_000_000, false},
		{6_000_000, 900_000, 3, 750_000, false}, // rounds the divisor up: never faster than asked
		{60_000_000, 30_000_000, 0, 30_000_000, false},
		{60_000_000, 6_000_000, 4, 6_000_000, false},
		{6_000_000, 4_000_000, 0, 0, true},
		{6_000_000, 0, 0, 0, true},
		{6_000_000, 10, 0, 0, true},
	}
	for _, c := range cases {
		div, act, err := mpsse.Divisor(c.base, c.hz)
		if c.bad {
			if err == nil {
				t.Errorf("%d/%d: expected an error", c.base, c.hz)
			}
			continue
		}
		if err != nil || div != c.div || act != c.actual {
			t.Errorf("%d/%d: div=%d act=%d err=%v; want %d %d", c.base, c.hz, div, act, err, c.div, c.actual)
		}
		if act > c.hz {
			t.Errorf("clock %d exceeds request %d", act, c.hz)
		}
	}
}

func engine(t *testing.T, cfg sim.Config) (*mpsse.Engine, *sim.Device) {
	t.Helper()
	d := sim.New(cfg)
	c, err := ftdi.New(d, sim.USBDevice(cfg.MaxPacket), ftdi.Options{Timeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetBitMode(context.Background(), 0, ftdi.BitModeMPSSE); err != nil {
		t.Fatal(err)
	}
	return mpsse.NewEngine(c, mpsse.Config{Timeout: 300 * time.Millisecond}), d
}

func TestSync(t *testing.T) {
	e, _ := engine(t, sim.Config{})
	if err := e.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSyncFailsWhenNotMPSSE(t *testing.T) {
	d := sim.New(sim.Config{})
	c, _ := ftdi.New(d, sim.USBDevice(0), ftdi.Options{Timeout: 20 * time.Millisecond})
	e := mpsse.NewEngine(c, mpsse.Config{Timeout: 50 * time.Millisecond})
	err := e.Sync(context.Background())
	if err == nil {
		t.Fatal("sync must fail when the device is not in MPSSE mode")
	}
}

func TestGPIOReadBack(t *testing.T) {
	e, _ := engine(t, sim.Config{})
	b := mpsse.NewBuilder()
	b.SetLow(0x48, 0x0B)
	r := b.GetLow()
	reply, err := e.Exec(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Slice(reply); len(got) != 1 || got[0] != 0x48 {
		t.Fatalf("got % x", got)
	}
}

func TestExecChunksLargeReplies(t *testing.T) {
	// 1000 reply bytes cannot be requested in one go: the engine must split
	// into groups that respect ChunkReply and still return everything in order.
	e, d := engine(t, sim.Config{})
	b := mpsse.NewBuilder()
	b.Loopback(true)
	pat := make([]byte, 1000)
	for i := range pat {
		pat[i] = byte(i * 13)
	}
	rs := b.ShiftBytes(pat, true)
	b.Loopback(false)
	reply, err := e.Exec(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mpsse.ReadAll(reply, rs), pat) {
		t.Fatal("loopback data mismatch")
	}
	if n := len(d.Written()); n < 8 {
		t.Fatalf("expected the stream to be split into several USB writes, got %d", n)
	}
	for _, w := range d.Written() {
		if len(w) > 1024+1 {
			t.Fatalf("write of %d bytes exceeds the chunk bound", len(w))
		}
	}
}

func TestLoopbackSupported(t *testing.T) {
	e, d := engine(t, sim.Config{})
	res, err := e.LoopbackTest(context.Background(), []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x01})
	if err != nil || !res.Supported {
		t.Fatalf("%+v %v", res, err)
	}
	if d.Loopback() {
		t.Fatal("loopback left enabled")
	}
}

func TestLoopbackIgnoredIsNotAnError(t *testing.T) {
	e, d := engine(t, sim.Config{Sipeed: true, FloatHigh: true})
	res, err := e.LoopbackTest(context.Background(), []byte{0x12, 0x34, 0x56})
	if err != nil {
		t.Fatal(err)
	}
	if res.Supported || res.Note == "" {
		t.Fatalf("an emulator ignoring loopback must be reported as unsupported: %+v", res)
	}
	if d.Loopback() {
		t.Fatal("loopback state")
	}
}

func TestBadCommandAnswer(t *testing.T) {
	e, _ := engine(t, sim.Config{})
	b := mpsse.NewBuilder()
	r := b.BadCommand()
	reply, err := e.Exec(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Slice(reply); !bytes.Equal(got, []byte{0xFA, 0xAA}) {
		t.Fatalf("% x", got)
	}
}

func TestExecPropagatesBuilderError(t *testing.T) {
	e, _ := engine(t, sim.Config{})
	b := mpsse.NewBuilder()
	b.TMS(0, 9, false, false)
	if _, err := e.Exec(context.Background(), b); err == nil {
		t.Fatal("expected the encoding error")
	}
}

func TestExecTimeoutWhenDeviceStaysSilent(t *testing.T) {
	e, _ := engine(t, sim.Config{})
	// 0x87 on its own produces nothing, so waiting for a reply must time out.
	if _, err := e.Link().Read(context.Background(), 1, 20*time.Millisecond); !errors.Is(err, usb.ErrTimeout) {
		t.Fatalf("want a timeout, got %v", err)
	}
}
