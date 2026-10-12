package dap

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/faronov/hwlab/internal/usb"
)

// fakePort is a scripted usb.Port: each BulkOut is recorded, each BulkIn pops
// the next scripted answer.
type fakePort struct {
	mu     sync.Mutex
	outs   [][]byte
	ins    []inAnswer
	inLens []int
	halts  []uint8
	outErr error
	closed bool
}

type inAnswer struct {
	data []byte
	err  error
}

func (f *fakePort) ControlIn(context.Context, uint8, uint8, uint16, uint16, int) ([]byte, error) {
	return nil, errors.New("not used")
}
func (f *fakePort) ControlOut(context.Context, uint8, uint8, uint16, uint16, []byte) error {
	return errors.New("not used")
}
func (f *fakePort) BulkOut(_ context.Context, _ uint8, data []byte, _ time.Duration) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.outErr != nil {
		return 0, f.outErr
	}
	f.outs = append(f.outs, append([]byte(nil), data...))
	return len(data), nil
}
func (f *fakePort) BulkIn(_ context.Context, _ uint8, length int, _ time.Duration) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inLens = append(f.inLens, length)
	if len(f.ins) == 0 {
		return nil, usb.ErrTimeout
	}
	a := f.ins[0]
	f.ins = f.ins[1:]
	return a.data, a.err
}
func (f *fakePort) ClearHalt(ep uint8) error { f.halts = append(f.halts, ep); return nil }
func (f *fakePort) Describe() string         { return "fake" }
func (f *fakePort) DetachedDriver() string   { return "" }
func (f *fakePort) Close() error             { f.closed = true; return nil }

func newFakeBulk(f *fakePort) *bulkTransport {
	tr := newBulk(f, UsbDevice{VID: "d6e7", PID: "3507", Bus: 1, Address: 2, EPIn: 0x82, EPOut: 0x01, MaxPacket: 64})
	tr.dirty = false // scripted answers must not be taken for stale ones
	return tr
}

// A previous owner (or a crashed process) can leave responses queued in the
// probe. The first exchange of a new handle discards them; without it the
// BL702-DAPLink answered DAP_Info with a stale DAP_Disconnect reply.
func TestBulkFreshOpenDrainsStaleResponses(t *testing.T) {
	f := &fakePort{ins: []inAnswer{{data: []byte{cmdDisconnect, 0}}, {data: []byte{cmdInfo, 0}}, {err: usb.ErrTimeout}, {data: []byte{cmdInfo, 1, 0x33}}}}
	tr := newBulk(f, UsbDevice{EPIn: 0x82, EPOut: 0x01, MaxPacket: 64})
	resp, err := tr.Exchange([]byte{cmdInfo, InfoCaps}, time.Second)
	if err != nil || len(resp) != 3 || resp[2] != 0x33 {
		t.Fatalf("got % x err %v", resp, err)
	}
}

func TestOutOfSyncResponseTriggersDrainOnNextCommand(t *testing.T) {
	f := &fakePort{ins: []inAnswer{{data: []byte{cmdDisconnect, 0}}}}
	tr := newFakeBulk(f)
	p := &Probe{t: tr, ps: 64, timeout: time.Second}
	if _, _, err := p.InfoRaw(InfoCaps); err == nil || !strings.Contains(err.Error(), "out of sync") {
		t.Fatalf("got %v", err)
	}
	f.ins = []inAnswer{{data: []byte{cmdInfo, 0}}, {err: usb.ErrTimeout}, {data: []byte{cmdInfo, 1, 0x33}}}
	raw, ok, err := p.InfoRaw(InfoCaps)
	if err != nil || !ok || raw[0] != 0x33 {
		t.Fatalf("after resync: % x ok=%v err=%v", raw, ok, err)
	}
}

func TestDelayZeroIsNeverSent(t *testing.T) {
	f := &fakePort{ins: []inAnswer{{data: []byte{cmdDelay, 0}}}}
	p := &Probe{t: newFakeBulk(f), ps: 64, timeout: time.Second}
	if err := p.Delay(0); err != nil {
		t.Fatal(err)
	}
	if got := f.outs[0]; got[1] != 1 || got[2] != 0 {
		t.Fatalf("DAP_Delay(0) hung the BL702-DAPLink firmware, got % x", got)
	}
}

func TestBulkReadSizedToPacketSize(t *testing.T) {
	f := &fakePort{}
	tr := newFakeBulk(f)
	tr.SetPacketSize(64)
	// A 64-byte response fills a full-speed USB packet exactly and is not
	// followed by a short packet; the read must not ask for more than 64.
	full := make([]byte, 64)
	full[0] = cmdTransferBlk
	f.ins = []inAnswer{{data: full}}
	resp, err := tr.Exchange([]byte{cmdTransferBlk}, time.Second)
	if err != nil || len(resp) != 64 {
		t.Fatalf("resp %d bytes err %v", len(resp), err)
	}
	if f.inLens[0] != 64 {
		t.Fatalf("read buffer %d, want the 64-byte DAP packet size", f.inLens[0])
	}
}

func TestBulkRejectsOversizeCommand(t *testing.T) {
	f := &fakePort{}
	tr := newFakeBulk(f)
	if _, err := tr.Exchange(make([]byte, 65), time.Second); err == nil {
		t.Fatal("a command larger than the packet size must be refused")
	}
	if len(f.outs) != 0 {
		t.Fatal("nothing may reach the USB endpoint")
	}
}

func TestBulkTimeoutThenStaleResponseDrained(t *testing.T) {
	f := &fakePort{}
	tr := newFakeBulk(f)
	if _, err := tr.Exchange([]byte{cmdInfo, InfoProtocolVer}, time.Millisecond); !errors.Is(err, usb.ErrTimeout) {
		t.Fatalf("want timeout, got %v", err)
	}
	// The late answer of the first command shows up before the next command.
	f.ins = []inAnswer{{data: []byte{cmdInfo, 0}}, {err: usb.ErrTimeout}, {data: []byte{cmdConnect, 1}}}
	resp, err := tr.Exchange([]byte{cmdConnect, 1}, time.Second)
	if err != nil || len(resp) != 2 || resp[0] != cmdConnect {
		t.Fatalf("the stale DAP_Info answer must be drained, got % x err %v", resp, err)
	}
}

func TestBulkStallClearsHalt(t *testing.T) {
	f := &fakePort{ins: []inAnswer{{err: usb.ErrStall}}}
	tr := newFakeBulk(f)
	if _, err := tr.Exchange([]byte{cmdInfo, 1}, time.Second); !errors.Is(err, usb.ErrStall) {
		t.Fatalf("got %v", err)
	}
	if len(f.halts) != 2 {
		t.Fatalf("both endpoints must be cleared, got %v", f.halts)
	}
}

func TestBulkDisconnectIsTyped(t *testing.T) {
	f := &fakePort{outErr: usb.ErrDisconnected}
	tr := newFakeBulk(f)
	if _, err := tr.Exchange([]byte{cmdInfo, 1}, time.Second); !errors.Is(err, usb.ErrDisconnected) {
		t.Fatalf("got %v", err)
	}
	f2 := &fakePort{ins: []inAnswer{{err: usb.ErrDisconnected}}}
	if _, err := newFakeBulk(f2).Exchange([]byte{cmdInfo, 1}, time.Second); !errors.Is(err, usb.ErrDisconnected) {
		t.Fatalf("read side: %v", err)
	}
}

func TestBulkSkipsEmptyPacketsAndFailsOnOnlyEmpty(t *testing.T) {
	f := &fakePort{ins: []inAnswer{{data: nil}, {data: []byte{cmdInfo, 0}}}}
	resp, err := newFakeBulk(f).Exchange([]byte{cmdInfo, 1}, time.Second)
	if err != nil || len(resp) != 2 {
		t.Fatalf("% x %v", resp, err)
	}
	f = &fakePort{ins: []inAnswer{{}, {}, {}, {}}}
	if _, err := newFakeBulk(f).Exchange([]byte{cmdInfo, 1}, time.Second); err == nil {
		t.Fatal("only empty packets is not a response")
	}
}

func TestBulkClosedAndIdempotentClose(t *testing.T) {
	f := &fakePort{}
	tr := newFakeBulk(f)
	_ = tr.Close()
	_ = tr.Close()
	if !f.closed {
		t.Fatal("port not closed")
	}
	if _, err := tr.Exchange([]byte{cmdInfo, 1}, time.Second); !errors.Is(err, usb.ErrClosed) {
		t.Fatalf("got %v", err)
	}
}

func TestBulkExchangeMany(t *testing.T) {
	f := &fakePort{ins: []inAnswer{{data: []byte{cmdInfo, 0}}, {data: []byte{cmdConnect, 1}}}}
	tr := newFakeBulk(f)
	out, err := tr.ExchangeMany([][]byte{{cmdInfo, 1}, {cmdConnect, 1}}, time.Second)
	if err != nil || len(out) != 2 || out[1][0] != cmdConnect || len(f.outs) != 2 {
		t.Fatalf("%v %v", out, err)
	}
}

// Probe-level handling of replies a damaged link can produce.

type scriptTransport struct {
	resp [][]byte
	err  error
	i    int
}

func (s *scriptTransport) Exchange([]byte, time.Duration) ([]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	r := s.resp[s.i%len(s.resp)]
	s.i++
	return r, nil
}
func (s *scriptTransport) PacketSize() int  { return 64 }
func (s *scriptTransport) Describe() string { return "script" }
func (s *scriptTransport) Close() error     { return nil }

func probeWith(t *testing.T, resp ...[]byte) *Probe {
	t.Helper()
	p := &Probe{t: &scriptTransport{resp: resp}, ps: 64, timeout: time.Second}
	return p
}

func TestProbeMalformedReplies(t *testing.T) {
	if _, err := probeWith(t, []byte{}).Connect(1); err == nil {
		t.Fatal("empty reply")
	}
	if _, err := probeWith(t, []byte{cmdDisconnect, 0}).Connect(1); err == nil {
		t.Fatal("reply for another command must be reported as out of sync")
	}
	if _, err := probeWith(t, []byte{respInvalid}).Connect(1); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("0xFF is an unsupported command: %v", err)
	}
	if _, err := probeWith(t, []byte{cmdConnect}).Connect(1); err == nil {
		t.Fatal("short reply")
	}
	if _, err := probeWith(t, []byte{cmdConnect, 0}).Connect(1); err == nil {
		t.Fatal("connect status 0 is a refusal")
	}
	if _, _, err := probeWith(t, []byte{cmdInfo, 8, 1, 2}).InfoRaw(1); err == nil {
		t.Fatal("DAP_Info claiming 8 bytes with 2 present")
	}
	if _, err := probeWith(t, []byte{cmdTransfer, 1}).Transfer([]Op{{Read: true, Reg: 0}}); err == nil {
		t.Fatal("short DAP_Transfer")
	}
	if _, err := probeWith(t, []byte{cmdTransfer, 1, AckOK}).Transfer([]Op{{Read: true, Reg: 0}}); err == nil {
		t.Fatal("DAP_Transfer without the read data")
	}
}

func TestAckDecoding(t *testing.T) {
	for _, c := range []struct {
		resp byte
		want error
	}{
		{AckOK, nil}, {AckWait, ErrWait}, {AckFault, ErrFault}, {AckNone, ErrNoACK}, {0, ErrNoACK},
		{AckOK | 0x08, ErrProtocol}, {AckOK | 0x10, ErrMismatch},
	} {
		if got := ackError(c.resp); !errors.Is(got, c.want) {
			t.Errorf("ack %#x: got %v want %v", c.resp, got, c.want)
		}
	}
}

func TestDecodeCaps(t *testing.T) {
	c := DecodeCaps([]byte{0x33})
	if !c.SWD || !c.JTAG || c.SWOUART || c.SWOManchester || !c.AtomicCommands || !c.TestDomainTimer || c.SWOStreaming || c.UARTPort || c.USBCOMPort {
		t.Fatalf("0x33 decoded as %+v", c)
	}
	c = DecodeCaps([]byte{0xFF, 0x01})
	if !c.SWOUART || !c.SWOManchester || !c.SWOStreaming || !c.UARTPort || !c.USBCOMPort {
		t.Fatalf("0xFF01 decoded as %+v", c)
	}
	if c := DecodeCaps(nil); c.SWD || c.JTAG {
		t.Fatal("no data means nothing is supported")
	}
}

func TestJTAGSequenceEncoding(t *testing.T) {
	// 64 bits encode as 0; the capture byte count follows the bit count.
	tr := &scriptTransport{resp: [][]byte{{cmdJTAGSequence, 0, 1, 2, 3, 4, 5, 6, 7, 8, 0xAA}}}
	p := &Probe{t: tr, ps: 64, timeout: time.Second}
	got, err := p.JTAGSequence([]JTAGSeq{{Bits: 64, Capture: true, TDI: make([]byte, 8)}, {Bits: 3, TMS: true, Capture: true, TDI: []byte{5}}})
	if err != nil || len(got) != 2 || len(got[0]) != 8 || got[1][0] != 0xAA {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := p.JTAGSequence([]JTAGSeq{{Bits: 65}}); err == nil {
		t.Fatal("65 bits must be refused")
	}
}
