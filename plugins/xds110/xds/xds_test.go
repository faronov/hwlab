package xds

import (
	"context"
	"errors"
	"testing"

	"github.com/faronov/hwlab/internal/adiv5"
	"github.com/faronov/hwlab/internal/cortexm"
	"github.com/faronov/hwlab/internal/jtag"
)

func simProbe(t *testing.T) (*Probe, *Sim) {
	t.Helper()
	s := NewSim()
	p := NewProbe(s)
	if _, err := p.Connect(); err != nil {
		t.Fatal(err)
	}
	return p, s
}

func TestFirmwareString(t *testing.T) {
	// 0x03000022 is the value the real probe reported; it matches the
	// "03.00.00.22" in its USB product string.
	if got := FirmwareString(0x03000022); got != "3.0.0.22" {
		t.Fatalf("got %s", got)
	}
}

func TestFrame(t *testing.T) {
	got := frame(CmdSetTCK, le32(41))
	want := []byte{'*', 5, 0, 0x04, 41, 0, 0, 0}
	if string(got) != string(want) {
		t.Fatalf("got %x want %x", got, want)
	}
}

func TestParseHeaderRejectsMalformed(t *testing.T) {
	for name, pkt := range map[string][]byte{
		"empty":     nil,
		"bad start": {'x', 4, 0, 0, 0, 0, 0},
		"too short": {'*', 4, 0, 0, 0},
		"len<4":     {'*', 2, 0, 0, 0, 0, 0},
		"overlong":  {'*', 4, 0, 0, 0, 0, 0, 9, 9},
		"huge":      {'*', 0xff, 0xff, 0, 0, 0, 0},
	} {
		if _, err := parseHeader(pkt); !errors.Is(err, ErrShortReply) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Delay words for firmware 3.0.0.22. The fixed fast steps (5.5 MHz and above)
// are the documented encodings; below that the count is rounded up so the
// clock never exceeds the request.
func TestPlanClockEncodings(t *testing.T) {
	fw := uint32(0x03000022)
	for khz, delay := range map[uint32]uint32{100: 170, 400: 42, 1000: 17, 5500: 2, 8500: 1, 10000: 0xfffffffd, 12000: 0xfffffffe, 14000: 0} {
		if got := PlanClock(fw, khz); got.Delay != delay {
			t.Errorf("%d kHz: delay %d, want %d", khz, got.Delay, delay)
		}
	}
	if p := PlanClock(fw, 50); !p.Clamped || p.NominalKHz > 110 {
		t.Errorf("below the minimum: %+v", p)
	}
	if p := PlanClock(fw, 30000); !p.Clamped || p.NominalKHz != 14000 {
		t.Errorf("above the maximum: %+v", p)
	}
	// Never faster than requested below 5.5 MHz.
	for khz := uint32(100); khz < 5500; khz += 137 {
		if p := PlanClock(fw, khz); p.NominalKHz > khz {
			t.Errorf("%d kHz request planned as %d kHz", khz, p.NominalKHz)
		}
	}
	if p := PlanClock(0x02030011, 1000); p.NominalKHz > 1000 || p.Delay == 0 {
		t.Errorf("old firmware: %+v", p)
	}
}

func TestProbeInfoAndErrors(t *testing.T) {
	p, s := simProbe(t)
	if p.Info.FirmwareText != "3.0.0.22" || p.Info.Hardware != 0x23 {
		t.Fatalf("%+v", p.Info)
	}
	s.FailNext(StatusSWDFault)
	_, err := p.Do(CmdCycleTCK, le32(1))
	var xe *Error
	if !errors.As(err, &xe) || xe.Status != StatusSWDFault {
		t.Fatalf("%v", err)
	}
	if _, err := p.SetClock(1000); err != nil {
		t.Fatal(err)
	}
	if s.tck != 17 {
		t.Fatalf("tck delay %d", s.tck)
	}
}

func TestSupplyIsGatedByHardware(t *testing.T) {
	p, s := simProbe(t)
	s.Hardware = HWStandAlone
	if _, err := p.Version(); err != nil {
		t.Fatal(err)
	}
	if err := p.SetSupply(900, true); err == nil {
		t.Fatal("out-of-range voltage accepted")
	}
	if err := p.SetSupply(3300, true); err != nil {
		t.Fatal(err)
	}
	if mv, on := s.Supply(); mv != 3300 || !on {
		t.Fatalf("%d %v", mv, on)
	}
	for _, hw := range []uint16{HWLPXDS110ET, 0x30} {
		s.Hardware = hw
		if _, err := p.Version(); err != nil {
			t.Fatal(err)
		}
		if err := p.SetSupply(3300, true); !errors.Is(err, ErrNoSupply) {
			t.Fatalf("hardware 0x%02x: supply allowed or wrong error: %v", hw, err)
		}
	}
}

func TestSWDTargetOnSimulator(t *testing.T) {
	p, s := simProbe(t)
	tgt, err := adiv5.Connect(NewSWDLink(p), 0)
	if err != nil {
		t.Fatal(err)
	}
	if tgt.DPIDR != simDPIDR || tgt.APIDR != simAPIDR {
		t.Fatalf("DPIDR %08x APIDR %08x", tgt.DPIDR, tgt.APIDR)
	}
	c := cortexm.New(adiv5.Bus{T: tgt})
	ctx := context.Background()
	if _, err := c.Halt(ctx); err != nil {
		t.Fatal(err)
	}
	// Pipelined AP reads must return each word's own value.
	want := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	if err := tgt.WriteMem(simRAMBase, want); err != nil {
		t.Fatal(err)
	}
	got, err := tgt.ReadMem(simRAMBase, len(want))
	if err != nil || string(got) != string(want) {
		t.Fatalf("%x %v", got, err)
	}
	for i, b := range want {
		if v, ok := s.Byte(simRAMBase + uint32(i)); !ok || v != b {
			t.Fatalf("byte %d = %d", i, v)
		}
	}
	big := make([]byte, 6000)
	for i := range big {
		big[i] = byte(i * 7)
	}
	if err := tgt.WriteMem(simRAMBase+0x100, big); err != nil {
		t.Fatal(err)
	}
	if got, err := tgt.ReadMem(simRAMBase+0x100, len(big)); err != nil || string(got) != string(big) {
		t.Fatalf("bulk read mismatch: %v", err)
	}
}

func TestSWDErrorsAreClassified(t *testing.T) {
	p, s := simProbe(t)
	tgt, err := adiv5.Connect(NewSWDLink(p), 0)
	if err != nil {
		t.Fatal(err)
	}
	s.InjectWait(1) // the adiv5 layer retries a WAIT
	if _, err := tgt.ReadWord(simRAMBase); err != nil {
		t.Fatalf("WAIT was not retried: %v", err)
	}
	s.SetNoTarget(true)
	_, err = tgt.ReadWord(simRAMBase)
	if !errors.Is(err, adiv5.ErrNoACK) {
		t.Fatalf("no-target error %v", err)
	}
	s.SetNoTarget(false)
	if _, err := tgt.ReadMem(0xC0000000, 4); err == nil {
		t.Fatal("read of unmapped memory reported success")
	}
}

func TestNoTargetConnectIsExplicit(t *testing.T) {
	p, s := simProbe(t)
	s.SetNoTarget(true)
	_, err := adiv5.Connect(NewSWDLink(p), 0)
	if err == nil {
		t.Fatal("connected without a target")
	}
}

func TestDormantDPIsWokenThroughPathMove(t *testing.T) {
	p, s := simProbe(t)
	s.SetDormant(true)
	tgt, err := adiv5.Connect(NewSWDLink(p), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !tgt.WokeFromDormant {
		t.Fatal("connected without the wake-up")
	}
	if _, err := tgt.ReadMem(0x20000000, 4); err != nil {
		t.Fatal(err)
	}
}

func TestPathMoveRejectsNonAdjacentStates(t *testing.T) {
	p, _ := simProbe(t)
	if err := p.GotoState(StReset); err != nil {
		t.Fatal(err)
	}
	if err := p.PathMove([]uint32{StShiftDR}); err == nil {
		t.Fatal("Reset -> Shift-DR accepted as one step")
	}
}

func TestSWJSequenceRefusesArbitraryBits(t *testing.T) {
	p, _ := simProbe(t)
	if err := NewSWDLink(p).SWJSequence(13, []byte{0x55, 0x15}); err == nil {
		t.Fatal("arbitrary sequence accepted")
	}
}

func TestJTAGChainScanOnSimulator(t *testing.T) {
	p, _ := simProbe(t)
	if err := p.CJTAGConnect(); err != nil {
		t.Fatal(err)
	}
	d := jtag.NewDriver(NewJTAGPort(p))
	c, err := jtag.ScanChain(context.Background(), d, jtag.ScanOptions{MaxDevices: 4, MeasureIR: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Devices) != len(SimIDCodes) {
		t.Fatalf("%d devices: %+v", len(c.Devices), c)
	}
	for i, dev := range c.Devices {
		if dev.IDCode == nil || dev.IDCode.Raw != SimIDCodes[i] {
			t.Errorf("device %d idcode %+v", i, dev.IDCode)
		}
	}
}

func TestJTAGPortRejectsChunkedShift(t *testing.T) {
	p, _ := simProbe(t)
	j := NewJTAGPort(p)
	if _, err := j.Shift(context.Background(), []byte{0}, 8, false, true); err == nil {
		t.Fatal("non-exiting shift accepted")
	}
}
