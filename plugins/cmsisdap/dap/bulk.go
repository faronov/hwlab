package dap

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/faronov/hwlab/internal/usb"
)

// PacketSizer is implemented by transports that size their read buffer to the
// DAP packet size the probe reported.
type PacketSizer interface {
	SetPacketSize(n int)
}

// Pipeliner is implemented by transports that can keep several commands in
// flight (the probe's DAP_Info packet count bounds how many).
type Pipeliner interface {
	// ExchangeMany writes every request and then reads one response each. The
	// probe answers in order.
	ExchangeMany(reqs [][]byte, timeout time.Duration) ([][]byte, error)
}

// Resyncer is a transport that can be told its response stream is out of step.
type Resyncer interface{ Resync() }

// bulkTransport is CMSIS-DAP v2 over one claimed vendor-class interface. It
// claims only that interface (internal/usb never detaches a kernel driver
// unless asked), so a CDC-ACM function of a composite probe stays untouched.
type bulkTransport struct {
	mu     sync.Mutex
	port   usb.Port
	d      UsbDevice
	ps     int  // DAP packet size: the largest command and response
	dirty  bool // an earlier exchange ended without its response
	closed bool
}

// newBulk wraps an opened port.
func newBulk(port usb.Port, d UsbDevice) *bulkTransport {
	ps := d.MaxPacket
	if ps < minPacket {
		ps = 64
	}
	// dirty: a previous owner may have left responses in the probe's queue
	// (this firmware silently ignores undefined commands and keeps stale
	// replies), which would otherwise be read as answers to our first commands.
	return &bulkTransport{port: port, d: d, ps: ps, dirty: true}
}

// Resync implements Resyncer: discard whatever the probe still has queued
// before the next command.
func (t *bulkTransport) Resync() {
	t.mu.Lock()
	t.dirty = true
	t.mu.Unlock()
}

// SetPacketSize implements PacketSizer. A response never exceeds the DAP
// packet size, and the read must not ask for more: a response that fills the
// last USB packet exactly has no short packet to end the transfer, so a larger
// buffer would wait until the timeout.
func (t *bulkTransport) SetPacketSize(n int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if n >= minPacket {
		t.ps = n
	}
}

func (t *bulkTransport) PacketSize() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ps
}

func (t *bulkTransport) Describe() string {
	return fmt.Sprintf("CMSIS-DAP v2 bulk %s:%s bus %d dev %d if %d", t.d.VID, t.d.PID, t.d.Bus, t.d.Address, t.d.Interface)
}

// recoverStall clears a halted endpoint so the next command can run.
func (t *bulkTransport) recoverStall() {
	if hc, ok := t.port.(usb.HaltClearer); ok {
		_ = hc.ClearHalt(t.d.EPOut)
		_ = hc.ClearHalt(t.d.EPIn)
	}
	t.dirty = true
}

// drain discards responses left over from a command whose answer was never
// read, so the next command is not paired with a stale reply.
func (t *bulkTransport) drain() {
	for i := 0; i < 16; i++ {
		if _, err := t.port.BulkIn(context.Background(), t.d.EPIn, t.ps, 15*time.Millisecond); err != nil {
			break
		}
	}
	t.dirty = false
}

func (t *bulkTransport) classify(err error) error {
	switch {
	case errors.Is(err, usb.ErrStall):
		t.recoverStall()
	case errors.Is(err, usb.ErrTimeout):
		t.dirty = true
	}
	return err
}

func (t *bulkTransport) write(req []byte, timeout time.Duration) error {
	if len(req) > t.ps {
		return fmt.Errorf("command of %d bytes exceeds the %d-byte DAP packet size", len(req), t.ps)
	}
	n, err := t.port.BulkOut(context.Background(), t.d.EPOut, req, timeout)
	if err != nil {
		return t.classify(err)
	}
	if n != len(req) {
		return fmt.Errorf("short USB write: %d of %d bytes", n, len(req))
	}
	return nil
}

func (t *bulkTransport) read(timeout time.Duration) ([]byte, error) {
	// A zero-length packet is not a DAP response; skip a few.
	for i := 0; i < 4; i++ {
		resp, err := t.port.BulkIn(context.Background(), t.d.EPIn, t.ps, timeout)
		if err != nil {
			return nil, t.classify(err)
		}
		if len(resp) > 0 {
			return resp, nil
		}
	}
	return nil, errors.New("the probe sent only empty USB packets")
}

// Exchange implements Transport.
func (t *bulkTransport) Exchange(req []byte, timeout time.Duration) ([]byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil, usb.ErrClosed
	}
	if t.dirty {
		t.drain()
	}
	if err := t.write(req, timeout); err != nil {
		return nil, err
	}
	return t.read(timeout)
}

// ExchangeMany implements Pipeliner.
func (t *bulkTransport) ExchangeMany(reqs [][]byte, timeout time.Duration) ([][]byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil, usb.ErrClosed
	}
	if t.dirty {
		t.drain()
	}
	sent := 0
	var first error
	for _, r := range reqs {
		if err := t.write(r, timeout); err != nil {
			first = err
			break
		}
		sent++
	}
	out := make([][]byte, 0, sent)
	for i := 0; i < sent; i++ {
		resp, err := t.read(timeout)
		if err != nil {
			if first == nil {
				first = err
			}
			break
		}
		out = append(out, resp)
	}
	return out, first
}

// Send implements Sender: a command with no response.
func (t *bulkTransport) Send(req []byte, timeout time.Duration) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return usb.ErrClosed
	}
	return t.write(req, timeout)
}

func (t *bulkTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	t.closed = true
	return t.port.Close()
}
