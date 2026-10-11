package espusb_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/faronov/hwlab/internal/jtag"
	"github.com/faronov/hwlab/internal/riscv"
	"github.com/faronov/hwlab/internal/riscv/rvsim"
	"github.com/faronov/hwlab/internal/usb"
	"github.com/faronov/hwlab/plugins/espusbjtag/espusb"
	"github.com/faronov/hwlab/plugins/espusbjtag/sim"
)

var ctx = context.Background()

func open(t *testing.T, cfg sim.Config, ec espusb.Config) (*espusb.Probe, *sim.Device) {
	t.Helper()
	d := sim.New(cfg)
	if ec.Timeout == 0 {
		ec.Timeout = 200 * time.Millisecond
	}
	p, err := espusb.Open(ctx, d, 0x83, 0x02, ec)
	if err != nil {
		t.Fatal(err)
	}
	return p, d
}

func TestEncoderRuns(t *testing.T) {
	for _, n := range []int{1, 2, 3, 4, 5, 16, 17, 64, 100, 1023, 1024, 1025, 3000} {
		var e espusb.Encoder
		for i := 0; i < n; i++ {
			e.Add(0x6)
		}
		got := espusb.Decode(e.Bytes())
		// A pad nibble (FLUSH) may follow.
		for len(got) > n && got[len(got)-1] == 0xA {
			got = got[:len(got)-1]
		}
		if len(got) != n {
			t.Fatalf("run of %d decoded to %d", n, len(got))
		}
		for _, c := range got {
			if c != 6 {
				t.Fatalf("run of %d: %#x", n, c)
			}
		}
	}
}

func TestEncoderExactBytes(t *testing.T) {
	var e espusb.Encoder
	e.Add(0x6)
	if b := e.Bytes(); !bytes.Equal(b, []byte{0x6A}) {
		t.Fatalf("single command pads with FLUSH: % x", b)
	}
	// Five identical commands: 6, then REP digit of N-1=4 -> (4&3)=0, (4>>2)=1.
	for i := 0; i < 5; i++ {
		e.Add(0x6)
	}
	if b := e.Bytes(); !bytes.Equal(b, []byte{0x6C, 0xDA}) {
		t.Fatalf("% x", b)
	}
	e.Add(0x1)
	e.Add(0x2)
	e.Add(0xA)
	if b := e.Bytes(); !bytes.Equal(b, []byte{0x12, 0xAA}) {
		t.Fatalf("% x", b)
	}
}

func TestDecodeIgnoresStrayRep(t *testing.T) {
	if got := espusb.Decode([]byte{0xCD}); len(got) != 0 {
		t.Fatalf("% x", got)
	}
}

func TestParseCaps(t *testing.T) {
	c, err := espusb.ParseCaps(sim.RealCaps)
	if err != nil || c.APBKHz != 48000 || c.BaseKHz != 24000 || c.DivMin != 1 || c.DivMax != 255 || c.ProtoVersion != 1 {
		t.Fatalf("%+v %v", c, err)
	}
	for name, b := range map[string][]byte{
		"empty":     nil,
		"version":   {2, 10, 1, 8, 0xc0, 0x12, 1, 0, 0xff, 0},
		"truncated": {1, 10, 1, 8, 0xc0},
		"no speed":  {1, 6, 9, 4, 0, 0},
		"bad len":   {1, 10, 1, 1, 0xc0, 0x12, 1, 0, 0xff, 0},
		"zero div":  {1, 10, 1, 8, 0xc0, 0x12, 0, 0, 0xff, 0},
	} {
		if _, err := espusb.ParseCaps(b); !errors.Is(err, espusb.ErrCaps) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Unknown records are skipped.
	b := []byte{1, 14, 9, 4, 0, 0, 1, 8, 0xc0, 0x12, 1, 0, 0xff, 0}
	if c, err = espusb.ParseCaps(b); err != nil || c.DivMax != 255 {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestDivisor(t *testing.T) {
	c, _ := espusb.ParseCaps(sim.RealCaps)
	for _, tc := range []struct{ khz, div, actual int }{
		{1000, 24, 1000}, {24000, 1, 24000}, {40000, 1, 24000}, {1, 255, 94}, {3000, 8, 3000}, {7000, 4, 6000}, {0, 255, 94},
	} {
		div, act := c.Divisor(tc.khz)
		if div != tc.div || act != tc.actual {
			t.Errorf("%d kHz -> div %d (%d kHz), want %d (%d)", tc.khz, div, act, tc.div, tc.actual)
		}
	}
}

func TestOpenProgramsDivider(t *testing.T) {
	p, d := open(t, sim.Config{}, espusb.Config{ClockKHz: 1000})
	if d.Divider() != 24 || p.Divisor() != 24 || p.ClockKHz() != 1000 {
		t.Fatalf("div %d/%d khz %d", d.Divider(), p.Divisor(), p.ClockKHz())
	}
	if err := p.SetClock(ctx, 4000); err != nil || d.Divider() != 6 {
		t.Fatalf("%v %d", err, d.Divider())
	}
	for _, c := range d.Controls() {
		if c.Request == 1 || c.Request == 3 {
			t.Fatalf("driver issued SETIO/SET_CHIPID: %+v", c)
		}
		if c.Index != 0 {
			t.Fatalf("wIndex %d", c.Index)
		}
	}
}

func TestOpenRejectsBadCaps(t *testing.T) {
	d := sim.New(sim.Config{Caps: []byte{1, 2}})
	if _, err := espusb.Open(ctx, d, 0x83, 0x02, espusb.Config{}); !errors.Is(err, espusb.ErrCaps) {
		t.Fatalf("%v", err)
	}
}

func TestOpenDrainsStaleData(t *testing.T) {
	p, d := open(t, sim.Config{StaleIn: bytes.Repeat([]byte{0xFF}, 130)}, espusb.Config{})
	if d.Pending() != 0 {
		t.Fatalf("%d stale byte(s) left", d.Pending())
	}
	id := scanID(t, p)
	if id != 0x0000dc25 {
		t.Fatalf("%#x", id)
	}
}

func scanID(t *testing.T, p *espusb.Probe) uint32 {
	t.Helper()
	drv := jtag.NewDriver(p)
	if err := drv.Reset(ctx); err != nil {
		t.Fatal(err)
	}
	b, err := drv.ShiftDR(ctx, nil, 32, true)
	if err != nil {
		t.Fatal(err)
	}
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

func TestIDCodeBitOrder(t *testing.T) {
	p, _ := open(t, sim.Config{Target: rvsim.Config{IDCode: 0x1234A5C3 | 1}}, espusb.Config{})
	if id := scanID(t, p); id != 0x1234A5C3|1 {
		t.Fatalf("%#x", id)
	}
}

func TestOddLengthShift(t *testing.T) {
	p, _ := open(t, sim.Config{}, espusb.Config{})
	drv := jtag.NewDriver(p)
	if err := drv.Reset(ctx); err != nil {
		t.Fatal(err)
	}
	// Shift 13 IDCODE bits only, then leave: the capture must have exactly 13
	// significant bits.
	b, err := drv.ShiftDR(ctx, nil, 13, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 2 || uint16(b[0])|uint16(b[1])<<8 != 0xdc25&0x1fff {
		t.Fatalf("% x", b)
	}
}

func TestLongShiftSegments(t *testing.T) {
	// 1500 bits in BYPASS echo TDI delayed by one: exercises segmentation of the
	// capture (480 bits) and the stitching of segments.
	p, d := open(t, sim.Config{FIFOLimit: 64}, espusb.Config{})
	drv := jtag.NewDriver(p)
	if _, err := drv.ShiftIR(ctx, []byte{0x1f}, 5, false); err != nil {
		t.Fatal(err)
	}
	const n = 1500
	tdi := make([]byte, (n+7)/8)
	for i := range tdi {
		tdi[i] = byte(i*37 + 11)
	}
	before := d.BulkOuts()
	got, err := drv.ShiftDR(ctx, tdi, n, true)
	if err != nil {
		t.Fatal(err)
	}
	if d.BulkOuts()-before < 3 {
		t.Fatalf("only %d bulk transfer(s) for %d captured bits", d.BulkOuts()-before, n)
	}
	for i := 0; i < n; i++ {
		want := false
		if i > 0 {
			want = tdi[(i-1)/8]>>((i-1)%8)&1 == 1
		}
		if got[i/8]>>(i%8)&1 == 1 != want {
			t.Fatalf("bit %d", i)
		}
	}
}

func TestShiftErrors(t *testing.T) {
	p, _ := open(t, sim.Config{}, espusb.Config{})
	if _, err := p.Shift(ctx, nil, 0, false, true); err == nil {
		t.Fatal("zero bits")
	}
	if _, err := p.Shift(ctx, []byte{1}, 20, false, true); err == nil {
		t.Fatal("short data")
	}
}

func TestTimeoutThenRecover(t *testing.T) {
	p, d := open(t, sim.Config{}, espusb.Config{Timeout: 30 * time.Millisecond})
	d.Hang(true)
	drv := jtag.NewDriver(p)
	if err := drv.Reset(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := drv.ShiftDR(ctx, nil, 32, true); !errors.Is(err, usb.ErrTimeout) {
		t.Fatalf("%v", err)
	}
	d.Hang(false)
	if id := scanID(t, p); id != 0x0000dc25 {
		t.Fatalf("after recovery: %#x", id)
	}
}

func TestDisconnect(t *testing.T) {
	p, d := open(t, sim.Config{}, espusb.Config{})
	d.DisconnectAfterOuts(1)
	drv := jtag.NewDriver(p)
	if err := drv.Reset(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := drv.ShiftDR(ctx, nil, 32, true); !errors.Is(err, usb.ErrDisconnected) {
		t.Fatalf("%v", err)
	}
	if _, err := drv.ShiftDR(ctx, nil, 32, true); !errors.Is(err, usb.ErrDisconnected) {
		t.Fatalf("stays disconnected: %v", err)
	}
}

func TestCancel(t *testing.T) {
	p, _ := open(t, sim.Config{}, espusb.Config{})
	c, cancel := context.WithCancel(ctx)
	cancel()
	if err := p.TMS(c, []bool{true}, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
	// A cancelled call leaves the probe usable.
	if id := scanID(t, p); id != 0x0000dc25 {
		t.Fatalf("%#x", id)
	}
}

func TestCloseIdempotent(t *testing.T) {
	p, d := open(t, sim.Config{}, espusb.Config{})
	if err := p.Close(); err != nil || !d.Closed() {
		t.Fatalf("%v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if err := p.TMS(ctx, []bool{true}, false); !errors.Is(err, usb.ErrClosed) {
		t.Fatalf("%v", err)
	}
	if _, err := p.TDO(ctx); !errors.Is(err, usb.ErrClosed) {
		t.Fatalf("%v", err)
	}
}

func TestTDOPin(t *testing.T) {
	p, _ := open(t, sim.Config{}, espusb.Config{})
	if _, err := p.TDO(ctx); err != nil {
		t.Fatal(err)
	}
}

// The whole stack: chain scan, DTM, DM and memory read over the nibble
// protocol.
func TestStackOverEspusb(t *testing.T) {
	mem := make([]byte, 32)
	for i := range mem {
		mem[i] = byte(0xA0 + i)
	}
	p, _ := open(t, sim.Config{FIFOLimit: 64, Target: rvsim.Config{Harts: 2, Memory: []rvsim.Region{{Base: 0x40800000, Data: mem}}}}, espusb.Config{})
	drv := jtag.NewDriver(p)
	ch, err := jtag.ScanChain(ctx, drv, jtag.ScanOptions{MeasureIR: true})
	if err != nil || len(ch.Devices) != 1 || ch.Devices[0].IDCode == nil || ch.Devices[0].IDCode.Raw != 0x0000dc25 || ch.TotalIR != 5 {
		t.Fatalf("%+v %v", ch, err)
	}
	dtm, err := riscv.NewDTM(ctx, drv, 5)
	if err != nil {
		t.Fatal(err)
	}
	dm := riscv.NewDM(dtm)
	info, err := dm.Identify(ctx, 4)
	if err != nil || info.Harts != 2 {
		t.Fatalf("%+v %v", info, err)
	}
	got, err := dm.ReadMem(ctx, 0x40800004, 12)
	if err != nil || !bytes.Equal(got, mem[4:16]) {
		t.Fatalf("% x %v", got, err)
	}
	if _, err := dm.Halt(ctx); err != nil {
		t.Fatal(err)
	}
	if v, err := dm.ReadReg(ctx, riscv.CSRMArchID); err != nil || v != 0x80000002 {
		t.Fatalf("%#x %v", v, err)
	}
	if _, err := dm.Resume(ctx); err != nil {
		t.Fatal(err)
	}
}
