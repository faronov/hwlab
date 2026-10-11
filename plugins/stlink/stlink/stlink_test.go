package stlink_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/faronov/hwlab/internal/cortexm"
	"github.com/faronov/hwlab/internal/usb"
	"github.com/faronov/hwlab/plugins/stlink/sim"
	"github.com/faronov/hwlab/plugins/stlink/stlink"
)

func ifc() usb.Interface {
	return usb.Interface{Number: 0, Class: 0xff, Endpoints: []usb.Endpoint{
		{Address: 0x02, Type: "Bulk", MaxPacket: 64}, {Address: 0x81, Type: "Bulk", MaxPacket: 64}, {Address: 0x83, Type: "Bulk", MaxPacket: 64}}}
}

func open(t *testing.T, cfg sim.Config) (*stlink.Conn, *sim.Device) {
	t.Helper()
	d := sim.New(cfg)
	c, err := stlink.Open(context.Background(), d, ifc(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return c, d
}

func TestParseVersionRealBytes(t *testing.T) {
	v, err := stlink.ParseVersion(sim.RealVersion[:], 0x3748)
	if err != nil {
		t.Fatal(err)
	}
	if v.STLinkV != 2 || v.JTAGV != 39 || v.SWIMV != 7 || v.Name != "V2J39S7" || v.Generation != "V2" || v.VID != 0x0483 || v.PID != 0x3748 {
		t.Fatalf("%+v", v)
	}
	if !v.HasSWDFreq() || !v.HasRWStatus2() || !v.HasTrace() {
		t.Fatal("feature levels")
	}
	v21, _ := stlink.ParseVersion([]byte{0x29, 0xc7, 0x83, 0x04, 0x4b, 0x37}, 0x374b)
	if v21.Generation != "V2-1" {
		t.Fatalf("%+v", v21)
	}
	if _, err := stlink.ParseVersion([]byte{1, 2}, 0); !errors.Is(err, stlink.ErrShortReply) {
		t.Fatalf("short: %v", err)
	}
}

func TestUnsupportedGenerationsRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    [6]byte
	}{{"V3", [6]byte{0x30, 0x00, 0x83, 0x04, 0x74, 0x37}}, {"V1", [6]byte{0x15, 0xc7, 0x83, 0x04, 0x44, 0x37}}} {
		v := tc.v
		d := sim.New(sim.Config{Version: &v})
		_, err := stlink.Open(context.Background(), d, ifc(), time.Second)
		if !errors.Is(err, stlink.ErrUnsupportedGeneration) {
			t.Fatalf("%s: %v", tc.name, err)
		}
	}
}

func TestConnectLeavesDFUAndReadsIDCode(t *testing.T) {
	c, d := open(t, sim.Config{})
	if d.Mode() != 0 {
		t.Fatalf("mode %d", d.Mode())
	}
	info, err := c.Connect(context.Background(), stlink.ConnectConfig{KHz: 1800})
	if err != nil {
		t.Fatal(err)
	}
	if !info.DFUExit || info.ModeBefore != stlink.ModeDFU || info.ModeAfter != stlink.ModeDebug || info.DPIDCode != 0x2BA01477 || info.ActualKHz != 1800 {
		t.Fatalf("%+v", info)
	}
	if info.VoltageMV < 3200 || info.VoltageMV > 3300 || d.FreqDiv != 1 || !d.EnteredSWD {
		t.Fatalf("%+v div=%d", info, d.FreqDiv)
	}
}

func TestConnectNoTarget(t *testing.T) {
	c, _ := open(t, sim.Config{NoTarget: true})
	_, err := c.Connect(context.Background(), stlink.ConnectConfig{})
	if !errors.Is(err, stlink.ErrNoTarget) {
		t.Fatalf("%v", err)
	}
}

func TestStaleReplyDrainedAndWedgeAvoided(t *testing.T) {
	d := sim.New(sim.Config{WedgeOnStale: true, StaleIn: []byte{0x80, 0, 0, 0}})
	c, err := stlink.Open(context.Background(), d, ifc(), time.Second)
	if err != nil {
		t.Fatalf("a stale reply from an earlier owner must be drained: %v", err)
	}
	if c.Ver.Name != "V2J39S7" {
		t.Fatal(c.Ver.Name)
	}
}

func TestStallRecovery(t *testing.T) {
	c, d := open(t, sim.Config{})
	// A debug command in DFU mode stalls the pipes; the next call must work.
	_, err := c.Read32(context.Background(), 0x20000000)
	if err == nil || !errors.Is(err, usb.ErrStall) {
		t.Fatalf("want a stall error, got %v", err)
	}
	if d.Stalls != 1 {
		t.Fatalf("stalls %d", d.Stalls)
	}
	if _, err := c.Connect(context.Background(), stlink.ConnectConfig{}); err != nil {
		t.Fatalf("connect after stall: %v", err)
	}
}

func connected(t *testing.T) (*stlink.Conn, *sim.Device) {
	c, d := open(t, sim.Config{})
	if _, err := c.Connect(context.Background(), stlink.ConnectConfig{}); err != nil {
		t.Fatal(err)
	}
	return c, d
}

func TestMemoryBlockWordAndByte(t *testing.T) {
	ctx := context.Background()
	c, _ := connected(t)
	pat := make([]byte, 3000)
	for i := range pat {
		pat[i] = byte(i * 7)
	}
	pat = pat[:2996]
	// An address that is not 1 KiB aligned forces the chunker to split at boundaries.
	if err := c.WriteBlock(ctx, 0x20000100, pat); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(pat))
	if err := c.ReadBlock(ctx, 0x20000100, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, pat) {
		t.Fatal("block round trip differs")
	}
	if err := c.WriteBytes(ctx, 0x20000203, []byte{0xAA, 0xBB, 0xCC}); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 8)
	if err := c.ReadBlock(ctx, 0x20000200, b); err != nil {
		t.Fatal(err)
	}
	want := make([]byte, 8)
	if err := c.ReadBlock(ctx, 0x20000200, want); err != nil {
		t.Fatal(err)
	}
	if b[3] != 0xAA || b[4] != 0xBB || b[5] != 0xCC {
		t.Fatalf("% x", b)
	}
}

func TestMemoryFaultSurfacesAddress(t *testing.T) {
	ctx := context.Background()
	c, d := connected(t)
	d.T.FailAddr = map[uint32]error{0x20000400: errors.New("bus fault")}
	buf := make([]byte, 16)
	err := c.ReadBlock(ctx, 0x20000400, buf)
	var se *stlink.StatusError
	if !errors.As(err, &se) || !se.Have || se.Addr != 0x20000400 || se.Code != stlink.StatusAPError {
		t.Fatalf("%v", err)
	}
	if err := c.WriteBytes(ctx, 0x20000400, []byte{1}); err == nil {
		t.Fatal("a faulting byte write must not report success")
	}
}

func TestBadAlignmentRejectedBeforeUSB(t *testing.T) {
	c, d := connected(t)
	n := d.Commands
	if err := c.ReadBlock(context.Background(), 0x20000001, make([]byte, 4)); err == nil {
		t.Fatal("misaligned block read accepted")
	}
	if d.Commands != n {
		t.Fatal("a rejected request must not reach the probe")
	}
}

func TestCoreOverConn(t *testing.T) {
	ctx := context.Background()
	c, _ := connected(t)
	core := cortexm.New(c)
	id, err := core.ReadIdentity(ctx)
	if err != nil || id.Name != "Cortex-M4" {
		t.Fatalf("%+v %v", id, err)
	}
	if _, err := core.Halt(ctx); err != nil {
		t.Fatal(err)
	}
	regs, err := core.ReadAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pc, err := core.ReadRegisterDCRSR(ctx, cortexm.RegPC)
	if err != nil || pc != regs.PC {
		t.Fatalf("native PC 0x%x != DCRSR PC 0x%x (%v)", regs.PC, pc, err)
	}
	if err := core.WriteRegister(ctx, 4, 0xC0FFEE); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		r, err := core.Step(ctx, true)
		if err != nil {
			t.Fatal(err)
		}
		if d := r.PCAfter - r.PCBefore; d != 2 && d != 4 {
			t.Fatalf("step %d: pc delta %d", i, d)
		}
	}
	if _, err := core.Resume(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestUnplugIsDeviceGone(t *testing.T) {
	c, d := connected(t)
	d.Unplug()
	_, err := c.Read32(context.Background(), 0x20000000)
	if !errors.Is(err, usb.ErrDisconnected) {
		t.Fatalf("%v", err)
	}
}

func TestSWDClockQuantize(t *testing.T) {
	for _, tc := range []struct{ in, khz, div int }{{4000, 4000, 0}, {1800, 1800, 1}, {1000, 950, 3}, {5, 5, 798}, {9000, 4000, 0}} {
		k, dv := stlink.QuantizeSWD(tc.in)
		if k != tc.khz || dv != tc.div {
			t.Fatalf("%d -> %d/%d want %d/%d", tc.in, k, dv, tc.khz, tc.div)
		}
	}
}

func TestStatusNames(t *testing.T) {
	if stlink.StatusName(stlink.StatusAPFault) == "" || stlink.StatusName(0x55) != "UNKNOWN_0x55" {
		t.Fatalf("%q %q", stlink.StatusName(stlink.StatusAPFault), stlink.StatusName(0x55))
	}
}
