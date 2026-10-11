package ftdi_test

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

func TestStripStatus(t *testing.T) {
	// two full 8-byte packets and a short one
	raw := []byte{
		1, 0x60, 'a', 'b', 'c', 'd', 'e', 'f',
		2, 0x61, 'g', 'h', 'i', 'j', 'k', 'l',
		3, 0x62, 'm',
	}
	pl, st, err := ftdi.StripStatus(raw, 8)
	if err != nil {
		t.Fatal(err)
	}
	if string(pl) != "abcdefghijklm" {
		t.Fatalf("payload %q", pl)
	}
	if st != [2]byte{3, 0x62} {
		t.Fatalf("status %x", st)
	}
	// status-only packet yields no payload
	pl, _, err = ftdi.StripStatus([]byte{1, 0x60}, 64)
	if err != nil || len(pl) != 0 {
		t.Fatalf("status-only: %q %v", pl, err)
	}
	// a 1-byte packet is malformed
	if _, _, err := ftdi.StripStatus([]byte{1, 0x60, 'x', 'y', 'z', 'w', 'q', 'r', 9}, 8); err == nil {
		t.Fatal("expected a truncated-packet error")
	}
	if _, _, err := ftdi.StripStatus([]byte{1, 2}, 2); err == nil {
		t.Fatal("expected an invalid packet size error")
	}
}

func TestChipFromBCD(t *testing.T) {
	cases := map[uint16]ftdi.ChipType{0x0500: ftdi.ChipFT2232C, 0x0700: ftdi.ChipFT2232H, 0x0800: ftdi.ChipFT4232H, 0x0900: ftdi.ChipFT232H, 0x0200: ftdi.ChipUnknown}
	for bcd, want := range cases {
		if got := ftdi.ChipFromBCD(bcd); got != want {
			t.Errorf("bcd %04x: %v want %v", bcd, got, want)
		}
	}
	if !ftdi.ChipFT2232C.HasMPSSE(0) || ftdi.ChipFT2232C.HasMPSSE(1) {
		t.Error("FT2232C/D has MPSSE on channel A only")
	}
	if !ftdi.IsKnown(0x0403, 0x6010) || ftdi.IsKnown(0x0403, 0x1234) {
		t.Error("known table")
	}
}

func newConn(t *testing.T, cfg sim.Config, opt ftdi.Options) (*ftdi.Conn, *sim.Device) {
	t.Helper()
	d := sim.New(cfg)
	c, err := ftdi.New(d, sim.USBDevice(cfg.MaxPacket), opt)
	if err != nil {
		t.Fatal(err)
	}
	return c, d
}

func TestChannelSelection(t *testing.T) {
	ctx := context.Background()
	for _, ch := range []int{0, 1} {
		c, d := newConn(t, sim.Config{Channel: ch}, ftdi.Options{Channel: ch})
		in, out := c.Endpoints()
		if wantIn, wantOut := uint8(0x81+2*ch), uint8(0x02+2*ch); in != wantIn || out != wantOut {
			t.Errorf("channel %d endpoints in=%#x out=%#x", ch, in, out)
		}
		if err := c.Reset(ctx); err != nil {
			t.Fatalf("channel %d reset: %v", ch, err)
		}
		if err := c.SetBitMode(ctx, 0, ftdi.BitModeMPSSE); err != nil {
			t.Fatal(err)
		}
		cs := d.Controls()
		if len(cs) != 2 || cs[0].Index != uint16(ch+1) || cs[1].Index != uint16(ch+1) {
			t.Fatalf("channel %d: wIndex wrong: %+v", ch, cs)
		}
		if cs[1].Value != 0x0200 || cs[1].Request != ftdi.ReqSetBitMode || cs[1].Type != 0x40 {
			t.Fatalf("bitmode request: %+v", cs[1])
		}
	}
	if _, err := ftdi.New(sim.New(sim.Config{}), sim.USBDevice(0), ftdi.Options{Channel: 5}); err == nil {
		t.Fatal("expected error for a missing channel")
	}
}

func TestOpenSequenceAndLatency(t *testing.T) {
	ctx := context.Background()
	c, d := newConn(t, sim.Config{}, ftdi.Options{})
	if err := c.SetLatency(ctx, 0); err == nil {
		t.Fatal("latency 0 must be rejected before any USB traffic")
	}
	if err := c.SetLatency(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if got, err := c.Latency(ctx); err != nil || got != 2 {
		t.Fatalf("latency %d %v", got, err)
	}
	if err := c.DisableSpecialChars(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.SetBitMode(ctx, 0, ftdi.BitModeMPSSE); err != nil {
		t.Fatal(err)
	}
	if !d.MPSSE() {
		t.Fatal("device should be in MPSSE mode")
	}
	if len(c.Log()) == 0 {
		t.Fatal("protocol log empty")
	}
}

func TestOptionalControlStall(t *testing.T) {
	ctx := context.Background()
	cfg := sim.Config{StallRequests: []uint8{ftdi.ReqSetLatency}}
	c, _ := newConn(t, cfg, ftdi.Options{})
	if err := c.SetLatency(ctx, 2); err == nil {
		t.Fatal("a stall on a required request must be an error")
	}
	c, _ = newConn(t, cfg, ftdi.Options{Quirks: ftdi.Quirks{OptionalControl: []uint8{ftdi.ReqSetLatency}}})
	if err := c.SetLatency(ctx, 2); err != nil {
		t.Fatalf("an optional request may stall: %v", err)
	}
	if got := c.Log(); got[len(got)-1].Error == "" {
		t.Fatal("the tolerated stall should still be logged")
	}
}

// echo sets up an MPSSE channel and returns a function that makes the device
// queue n bytes of reply (via loopback shifts).
func queueReply(t *testing.T, c *ftdi.Conn, n int) []byte {
	t.Helper()
	ctx := context.Background()
	if err := c.SetBitMode(ctx, 0, ftdi.BitModeMPSSE); err != nil {
		t.Fatal(err)
	}
	pat := make([]byte, n)
	for i := range pat {
		pat[i] = byte(i*7 + 1)
	}
	b := mpsse.NewBuilder()
	b.Loopback(true)
	b.MaxCmdBytes = 4096
	b.ShiftBytes(pat, true)
	if err := c.Write(ctx, append(b.Bytes(), mpsse.OpSendImmediate)); err != nil {
		t.Fatal(err)
	}
	return pat
}

func TestFragmentedReads(t *testing.T) {
	ctx := context.Background()
	// 200 bytes arrive as 62-byte payload packets, one packet per read.
	c, _ := newConn(t, sim.Config{MaxPacketsPerRead: 1}, ftdi.Options{})
	pat := queueReply(t, c, 200)
	got, err := c.Read(ctx, 200, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, pat) {
		t.Fatalf("payload mismatch")
	}
}

func TestSurplusPayloadKept(t *testing.T) {
	ctx := context.Background()
	c, _ := newConn(t, sim.Config{}, ftdi.Options{})
	pat := queueReply(t, c, 100)
	a, err := c.Read(ctx, 30, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.Read(ctx, 70, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(append(a, b...), pat) {
		t.Fatal("surplus bytes were lost between reads")
	}
}

func TestIdleStatusPacketsSkipped(t *testing.T) {
	ctx := context.Background()
	c, _ := newConn(t, sim.Config{IdleStatusPackets: true}, ftdi.Options{})
	_, err := c.Read(ctx, 1, 20*time.Millisecond)
	if !errors.Is(err, usb.ErrTimeout) {
		t.Fatalf("want timeout, got %v", err)
	}
	if st := c.ModemStatus(); st != [2]byte{0x01, 0x60} {
		t.Fatalf("status %x", st)
	}
}

func TestTimeoutReturnsPartial(t *testing.T) {
	ctx := context.Background()
	c, _ := newConn(t, sim.Config{}, ftdi.Options{Timeout: 10 * time.Millisecond})
	queueReply(t, c, 10)
	got, err := c.Read(ctx, 20, 50*time.Millisecond)
	if !errors.Is(err, usb.ErrTimeout) {
		t.Fatalf("want timeout, got %v", err)
	}
	if len(got) != 10 {
		t.Fatalf("partial data: %d bytes", len(got))
	}
}

func TestCancel(t *testing.T) {
	c, _ := newConn(t, sim.Config{}, ftdi.Options{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	start := time.Now()
	_, err := c.Read(ctx, 1, 5*time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want canceled, got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("cancel was not prompt")
	}
}

func TestDisconnect(t *testing.T) {
	ctx := context.Background()
	c, d := newConn(t, sim.Config{}, ftdi.Options{})
	queueReply(t, c, 4)
	d.Disconnect()
	if _, err := c.Read(ctx, 4, time.Second); !errors.Is(err, usb.ErrDisconnected) {
		t.Fatalf("read after unplug: %v", err)
	}
	if err := c.Write(ctx, []byte{0x87}); !errors.Is(err, usb.ErrDisconnected) {
		t.Fatalf("write after unplug: %v", err)
	}
	// Close must still release the port.
	if err := c.Close(ctx); err != nil {
		t.Fatalf("close after unplug: %v", err)
	}
	if !d.Closed() {
		t.Fatal("port not closed")
	}
	if _, err := c.Read(ctx, 1, time.Second); !errors.Is(err, usb.ErrClosed) {
		t.Fatalf("read after close: %v", err)
	}
}

func TestDisconnectMidRead(t *testing.T) {
	c, d := newConn(t, sim.Config{}, ftdi.Options{})
	queueReply(t, c, 4)
	d.DisconnectOnBulkIn(1)
	if _, err := c.Read(context.Background(), 4, time.Second); !errors.Is(err, usb.ErrDisconnected) {
		t.Fatalf("got %v", err)
	}
}

func TestCloseIsIdempotentAndResetsBitMode(t *testing.T) {
	ctx := context.Background()
	c, d := newConn(t, sim.Config{DetachedDriver: "ftdi_sio"}, ftdi.Options{})
	if c.DetachedDriver() != "ftdi_sio" {
		t.Fatal("detached driver not reported")
	}
	if err := c.SetBitMode(ctx, 0, ftdi.BitModeMPSSE); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if d.MPSSE() {
		t.Fatal("close should leave MPSSE mode")
	}
}
