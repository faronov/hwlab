package jtagport_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/faronov/hwlab/internal/jtag"
	"github.com/faronov/hwlab/plugins/ftdi/ftdi"
	"github.com/faronov/hwlab/plugins/ftdi/jtagport"
	"github.com/faronov/hwlab/plugins/ftdi/mpsse"
	"github.com/faronov/hwlab/plugins/ftdi/sim"
)

func tms(bits string) []bool {
	var o []bool
	for _, c := range bits {
		o = append(o, c == '1')
	}
	return o
}

func TestTAPTransitions(t *testing.T) {
	// Well-known sequences from IEEE 1149.1.
	walk := func(from jtag.State, bits string) jtag.State {
		for _, b := range tms(bits) {
			from = jtag.Next(from, b)
		}
		return from
	}
	if s := walk(jtag.TestLogicReset, "0"); s != jtag.RunTestIdle {
		t.Fatal(s)
	}
	if s := walk(jtag.RunTestIdle, "100"); s != jtag.ShiftDR {
		t.Fatalf("RTI -> 100 = %v", s)
	}
	if s := walk(jtag.RunTestIdle, "1100"); s != jtag.ShiftIR {
		t.Fatalf("RTI -> 1100 = %v", s)
	}
	if s := walk(jtag.ShiftDR, "110"); s != jtag.RunTestIdle {
		t.Fatalf("ShiftDR -> 110 = %v", s)
	}
	if s := walk(jtag.PauseIR, "11"); s != jtag.UpdateIR {
		t.Fatalf("%v", s)
	}
	// five ones reach Test-Logic-Reset from every state
	for s := jtag.TestLogicReset; s <= jtag.UpdateIR; s++ {
		if got := walk(s, "11111"); got != jtag.TestLogicReset {
			t.Errorf("%v + 11111 = %v", s, got)
		}
	}
	if jtag.Next(jtag.StateUnknown, true) != jtag.StateUnknown {
		t.Error("unknown stays unknown")
	}
}

func TestPaths(t *testing.T) {
	cases := []struct {
		from, to jtag.State
		want     string
	}{
		{jtag.TestLogicReset, jtag.RunTestIdle, "0"},
		{jtag.RunTestIdle, jtag.ShiftDR, "100"},
		{jtag.RunTestIdle, jtag.ShiftIR, "1100"},
		{jtag.Exit1DR, jtag.RunTestIdle, "10"},
		{jtag.PauseDR, jtag.ShiftDR, "10"},
		{jtag.ShiftDR, jtag.ShiftDR, ""},
		{jtag.TestLogicReset, jtag.ShiftIR, "01100"},
	}
	for _, c := range cases {
		p, err := jtag.Path(c.from, c.to)
		if err != nil {
			t.Fatal(err)
		}
		var got string
		for _, b := range p {
			if b {
				got += "1"
			} else {
				got += "0"
			}
		}
		if got != c.want {
			t.Errorf("%v -> %v: %q want %q", c.from, c.to, got, c.want)
		}
		// applying the path really arrives
		s := c.from
		for _, b := range p {
			s = jtag.Next(s, b)
		}
		if s != c.to {
			t.Errorf("path %q lands in %v, want %v", got, s, c.to)
		}
	}
	if _, err := jtag.Path(jtag.StateUnknown, jtag.RunTestIdle); err == nil {
		t.Error("unknown source must fail")
	}
}

func TestIDCodeDecode(t *testing.T) {
	id, err := jtag.DecodeIDCode(0x4BA00477) // ARM Cortex-M3 DAP, a well-known value
	if err != nil {
		t.Fatal(err)
	}
	if id.Manufacturer != 0x23B || id.Part != 0xBA00 || id.Version != 4 || id.ManufacturerName != "ARM" {
		t.Fatalf("%+v", id)
	}
	if _, err := jtag.DecodeIDCode(0x4BA00476); err == nil {
		t.Fatal("bit 0 clear is not an IDCODE")
	}
	id, _ = jtag.DecodeIDCode(0x00000FFF | 1)
	if id.ManufacturerName != "" {
		t.Fatal("unknown manufacturers have no name")
	}
}

func TestBitHelpers(t *testing.T) {
	b := make([]byte, 3)
	jtag.SetBit(b, 0, true)
	jtag.SetBit(b, 9, true)
	jtag.SetBit(b, 23, true)
	if !bytes.Equal(b, []byte{0x01, 0x02, 0x80}) {
		t.Fatalf("% x", b)
	}
	jtag.SetBit(b, 9, false)
	if jtag.GetBit(b, 9) || !jtag.GetBit(b, 23) {
		t.Fatal("get/set")
	}
	if o := jtag.Ones(11); !bytes.Equal(o, []byte{0xFF, 0x07}) {
		t.Fatalf("% x", o)
	}
	if v := jtag.Field([]byte{0xF0, 0x0F}, 4, 8); v != 0xFF {
		t.Fatalf("%#x", v)
	}
}

func TestParseChain(t *testing.T) {
	pack := func(words ...any) ([]byte, int) {
		var bits []bool
		for _, w := range words {
			switch v := w.(type) {
			case uint32:
				for i := 0; i < 32; i++ {
					bits = append(bits, v>>i&1 == 1)
				}
			case string: // explicit bits
				for _, c := range v {
					bits = append(bits, c == '1')
				}
			}
		}
		out := make([]byte, (len(bits)+7)/8)
		for i, b := range bits {
			jtag.SetBit(out, i, b)
		}
		return out, len(bits)
	}
	ones := func(n int) string { return string(bytes.Repeat([]byte("1"), n)) }
	zeros := func(n int) string { return string(bytes.Repeat([]byte("0"), n)) }

	t.Run("two idcodes then flush", func(t *testing.T) {
		raw, n := pack(uint32(0x4BA00477), uint32(0x1234_5001), ones(200))
		c := jtag.ParseChain(raw, n, 32)
		if c.Status != jtag.StatusOK || len(c.Devices) != 2 {
			t.Fatalf("%+v", c)
		}
		if c.Devices[0].IDCode.Raw != 0x4BA00477 || c.Devices[1].IDCode.Raw != 0x12345001 || c.Devices[1].Index != 1 {
			t.Fatalf("%+v %+v", c.Devices[0], c.Devices[1])
		}
	})
	t.Run("bypass device between idcodes", func(t *testing.T) {
		raw, n := pack(uint32(0x4BA00477), "0", uint32(0x0362D093), ones(100))
		c := jtag.ParseChain(raw, n, 32)
		if len(c.Devices) != 3 || !c.Devices[1].Bypass || c.Devices[1].IDCode != nil || c.Devices[2].IDCode.Raw != 0x0362D093 {
			t.Fatalf("%+v", c)
		}
	})
	t.Run("no device", func(t *testing.T) {
		raw, n := pack(ones(1088))
		c := jtag.ParseChain(raw, n, 32)
		if c.Status != jtag.StatusNoDevice || len(c.Devices) != 0 {
			t.Fatalf("%+v", c)
		}
	})
	t.Run("stuck low", func(t *testing.T) {
		raw, n := pack(zeros(1088))
		if c := jtag.ParseChain(raw, n, 32); c.Status != jtag.StatusStuckLow {
			t.Fatalf("%+v", c)
		}
	})
	t.Run("garbage that parses as too many TAPs", func(t *testing.T) {
		raw, n := pack(zeros(100), "1", ones(40)) // 100 bypass bits then noise
		if c := jtag.ParseChain(raw, n, 32); c.Status != jtag.StatusAmbiguous || len(c.Devices) != 0 {
			t.Fatalf("%+v", c)
		}
	})
	t.Run("truncated idcode", func(t *testing.T) {
		raw, n := pack("0", "1010")
		if c := jtag.ParseChain(raw, n, 32); c.Status != jtag.StatusAmbiguous {
			t.Fatalf("%+v", c)
		}
	})
}

func stack(t *testing.T, cfg sim.Config) (*jtag.Driver, *sim.Device) {
	t.Helper()
	d := sim.New(cfg)
	c, err := ftdi.New(d, sim.USBDevice(cfg.MaxPacket), ftdi.Options{Timeout: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetBitMode(context.Background(), 0, ftdi.BitModeMPSSE); err != nil {
		t.Fatal(err)
	}
	e := mpsse.NewEngine(c, mpsse.Config{Timeout: 500 * time.Millisecond})
	return jtag.NewDriver(jtagport.NewMPSSEPort(e)), d
}

var armTAP = sim.TAP{IDCode: 0x4BA00477, IRLen: 4}

func TestResetAndGoTo(t *testing.T) {
	ctx := context.Background()
	drv, d := stack(t, sim.Config{Chain: []sim.TAP{armTAP}})
	if drv.State() != jtag.StateUnknown {
		t.Fatal("state unknown before reset")
	}
	if err := drv.Reset(ctx); err != nil {
		t.Fatal(err)
	}
	if d.TAPState() != "TLR" || drv.State() != jtag.TestLogicReset {
		t.Fatalf("%s %v", d.TAPState(), drv.State())
	}
	for _, c := range []struct {
		to   jtag.State
		want string
	}{
		{jtag.ShiftIR, "ShIR"}, {jtag.PauseIR, "PauseIR"}, {jtag.ShiftDR, "ShDR"}, {jtag.RunTestIdle, "RTI"}, {jtag.PauseDR, "PauseDR"},
	} {
		if err := drv.GoTo(ctx, c.to); err != nil {
			t.Fatal(err)
		}
		if d.TAPState() != c.want || drv.State() != c.to {
			t.Fatalf("GoTo(%v): device is in %s, driver says %v", c.to, d.TAPState(), drv.State())
		}
	}
	if err := drv.RunTest(ctx, 20); err != nil {
		t.Fatal(err)
	}
	if d.TAPState() != "RTI" {
		t.Fatal(d.TAPState())
	}
}

func TestGoToFromUnknownResets(t *testing.T) {
	drv, d := stack(t, sim.Config{Chain: []sim.TAP{armTAP}})
	if err := drv.GoTo(context.Background(), jtag.ShiftDR); err != nil {
		t.Fatal(err)
	}
	if d.TAPState() != "ShDR" {
		t.Fatal(d.TAPState())
	}
}

func TestIDCodeScanSingleDevice(t *testing.T) {
	drv, d := stack(t, sim.Config{Chain: []sim.TAP{armTAP}})
	chain, err := jtag.ScanChain(context.Background(), drv, jtag.ScanOptions{MeasureIR: true})
	if err != nil {
		t.Fatal(err)
	}
	if chain.Status != jtag.StatusOK || len(chain.Devices) != 1 || chain.Devices[0].IDCode.Raw != 0x4BA00477 {
		t.Fatalf("%+v", chain)
	}
	if chain.TotalIR != 4 || chain.BypassBits != 1 || chain.Devices[0].IRLength != 4 {
		t.Fatalf("lengths: %+v", chain)
	}
	if d.TAPState() != "TLR" {
		t.Fatalf("scan must end with a TAP reset, device is in %s", d.TAPState())
	}
}

func TestScanMultiDeviceChain(t *testing.T) {
	cfg := sim.Config{Chain: []sim.TAP{
		{IDCode: 0x1BA01477, IRLen: 4},
		{IRLen: 5}, // BYPASS-only TAP
		{IDCode: 0x0362D093, IRLen: 6},
	}}
	drv, _ := stack(t, cfg)
	chain, err := jtag.ScanChain(context.Background(), drv, jtag.ScanOptions{MeasureIR: true, IRLengths: []int{4, 5, 6}})
	if err != nil {
		t.Fatal(err)
	}
	if len(chain.Devices) != 3 || chain.Devices[0].IDCode.Raw != 0x1BA01477 || !chain.Devices[1].Bypass || chain.Devices[2].IDCode.Raw != 0x0362D093 {
		t.Fatalf("%+v", chain)
	}
	if chain.TotalIR != 15 || chain.BypassBits != 3 {
		t.Fatalf("lengths %d %d notes %v", chain.TotalIR, chain.BypassBits, chain.Notes)
	}
	if len(chain.Notes) != 0 {
		t.Fatalf("unexpected notes %v", chain.Notes)
	}
	// wrong configured lengths are reported, not silently accepted
	chain, _ = jtag.ScanChain(context.Background(), drv, jtag.ScanOptions{MeasureIR: true, IRLengths: []int{4, 5, 7}})
	if len(chain.Notes) == 0 {
		t.Fatal("expected a note about mismatching IR lengths")
	}
}

func TestScanStable(t *testing.T) {
	drv, _ := stack(t, sim.Config{Chain: []sim.TAP{armTAP, {IDCode: 0x0362D093, IRLen: 6}}})
	first, err := jtag.ScanChain(context.Background(), drv, jtag.ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		again, err := jtag.ScanChain(context.Background(), drv, jtag.ScanOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(again.Devices) != len(first.Devices) || again.Devices[0].IDCode.Raw != first.Devices[0].IDCode.Raw || again.Devices[1].IDCode.Raw != first.Devices[1].IDCode.Raw {
			t.Fatalf("scan %d differs: %+v vs %+v", i, again, first)
		}
	}
}

func TestScanNoTarget(t *testing.T) {
	drv, _ := stack(t, sim.Config{FloatHigh: true})
	c, err := jtag.ScanChain(context.Background(), drv, jtag.ScanOptions{MeasureIR: true})
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != jtag.StatusNoDevice || len(c.Devices) != 0 || c.BypassBits != -1 {
		t.Fatalf("%+v", c)
	}
	drv, _ = stack(t, sim.Config{FloatHigh: false})
	if c, _ = jtag.ScanChain(context.Background(), drv, jtag.ScanOptions{}); c.Status != jtag.StatusStuckLow {
		t.Fatalf("%+v", c)
	}
}

func TestIRScanCapturePattern(t *testing.T) {
	ctx := context.Background()
	drv, _ := stack(t, sim.Config{Chain: []sim.TAP{armTAP}})
	// Capture-IR loads ...01: reading 4 IR bits back yields 0b0001.
	out, err := drv.ShiftIR(ctx, []byte{0x0F}, 4, true)
	if err != nil {
		t.Fatal(err)
	}
	if out[0]&0x0F != 0x01 {
		t.Fatalf("IR capture % x", out)
	}
}

func TestDRScanArbitraryLengths(t *testing.T) {
	ctx := context.Background()
	// One BYPASS TAP: DR is a single bit register, so data comes out delayed
	// by one clock, a convenient known-answer test for any length.
	for _, n := range []int{1, 2, 7, 8, 9, 15, 16, 17, 31, 33, 63, 64, 65, 127, 129, 300} {
		drv, _ := stack(t, sim.Config{Chain: []sim.TAP{{IRLen: 4}}})
		pat := make([]byte, (n+7)/8)
		for i := range pat {
			pat[i] = byte(0xA5 ^ i*31)
		}
		if n%8 != 0 {
			pat[len(pat)-1] &= 1<<(n%8) - 1
		}
		got, err := drv.ShiftDR(ctx, pat, n, true)
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if len(got) != len(pat) {
			t.Fatalf("n=%d: %d bytes", n, len(got))
		}
		// expected: bit0 = captured 0, bit i = pat[i-1]
		want := make([]byte, len(pat))
		for i := 1; i < n; i++ {
			jtag.SetBit(want, i, jtag.GetBit(pat, i-1))
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("n=%d\n got % x\nwant % x", n, got, want)
		}
	}
}

func TestShiftWithoutCapture(t *testing.T) {
	drv, d := stack(t, sim.Config{Chain: []sim.TAP{armTAP}})
	out, err := drv.ShiftDR(context.Background(), []byte{0xFF}, 8, false)
	if err != nil || out != nil {
		t.Fatalf("%v %v", out, err)
	}
	if d.TAPState() != "RTI" {
		t.Fatal(d.TAPState())
	}
	if _, err := drv.ShiftDR(context.Background(), nil, 0, false); err == nil {
		t.Fatal("zero-bit scans are errors")
	}
	if _, err := drv.ShiftDR(context.Background(), []byte{1}, 20, false); err == nil {
		t.Fatal("short data must be rejected")
	}
}

func TestTMSBitsAcrossCommandBoundary(t *testing.T) {
	// 20 TMS clocks need three TMS commands (7+7+6); the TAP must still end
	// in the state the sequence implies.
	drv, d := stack(t, sim.Config{Chain: []sim.TAP{armTAP}})
	if err := drv.GoTo(context.Background(), jtag.RunTestIdle); err != nil {
		t.Fatal(err)
	}
	if err := drv.RunTest(context.Background(), 20); err != nil {
		t.Fatal(err)
	}
	if d.TAPState() != "RTI" {
		t.Fatal(d.TAPState())
	}
}

// The Sipeed firmware implements only write/write+read shift opcodes; the
// JTAG layer must work unchanged on it.
func TestScanOnSipeedSubset(t *testing.T) {
	drv, _ := stack(t, sim.Config{Sipeed: true, FloatHigh: true, Chain: []sim.TAP{armTAP}})
	c, err := jtag.ScanChain(context.Background(), drv, jtag.ScanOptions{MeasureIR: true})
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != jtag.StatusOK || len(c.Devices) != 1 || c.Devices[0].IDCode.Raw != 0x4BA00477 || c.TotalIR != 4 {
		t.Fatalf("%+v", c)
	}
}

func TestSipeedRejectsReadOnlyShift(t *testing.T) {
	d := sim.New(sim.Config{Sipeed: true})
	c, _ := ftdi.New(d, sim.USBDevice(0), ftdi.Options{Timeout: 100 * time.Millisecond})
	ctx := context.Background()
	_ = c.SetBitMode(ctx, 0, ftdi.BitModeMPSSE)
	if err := c.Write(ctx, []byte{0x28, 0x00, 0x00, 0x87}); err != nil {
		t.Fatal(err)
	}
	got, err := c.Read(ctx, 2, 100*time.Millisecond)
	if err != nil || !bytes.Equal(got, []byte{0xFA, 0x28}) {
		t.Fatalf("% x %v", got, err)
	}
}
