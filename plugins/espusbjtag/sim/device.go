// Package sim is a SYNTHETIC Espressif USB-JTAG device: a usb.Port that
// answers the vendor control requests, decodes the nibble command stream and
// clocks an rvsim RISC-V target. It exists for tests, replay and demos.
// Nothing it reports is evidence about real hardware.
package sim

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/faronov/hwlab/internal/riscv/rvsim"
	"github.com/faronov/hwlab/internal/usb"
	"github.com/faronov/hwlab/plugins/espusbjtag/espusb"
)

var errStall = errors.New("sim: control request stalled")

// RealCaps is the capabilities descriptor the attached ESP32-C6 returned
// (copied from the REAL capture so the parser is tested on real bytes).
var RealCaps = []byte{0x01, 0x0a, 0x01, 0x08, 0xc0, 0x12, 0x01, 0x00, 0xff, 0x00}

// Config configures a Device.
type Config struct {
	Target rvsim.Config
	// Shared, when set, is the target behind the device instead of a fresh
	// one, so state (a halted hart) survives reopening the port.
	Shared *rvsim.Target
	// Caps overrides RealCaps.
	Caps []byte
	// MaxPacket is the IN packet size (default 64).
	MaxPacket int
	// FIFOLimit, when > 0, makes the device stall (BulkOut error) if more
	// than that many captured bytes are waiting unread, like the real tiny
	// endpoint FIFO.
	FIFOLimit int
	// StaleIn is returned by the first BulkIn calls, to exercise draining.
	StaleIn []byte
}

// Control records one control request.
type Control struct {
	Type, Request uint8
	Value, Index  uint16
	Data          []byte
}

// Device is the simulated port.
type Device struct {
	mu  sync.Mutex
	cfg Config
	T   *rvsim.Target

	closed       bool
	disconnected bool
	hangIn       bool
	failOutAfter int // fail BulkOut with disconnect after N more calls when >= 0
	controls     []Control
	outs         int
	maxIn        int // largest IN request seen
	bits         []bool
	fifo         []byte
	tdoPin       bool
	div          int
}

// New returns a Device.
func New(cfg Config) *Device {
	if cfg.Caps == nil {
		cfg.Caps = RealCaps
	}
	if cfg.MaxPacket == 0 {
		cfg.MaxPacket = 64
	}
	t := cfg.Shared
	if t == nil {
		t = rvsim.New(cfg.Target)
	}
	return &Device{cfg: cfg, T: t, fifo: append([]byte(nil), cfg.StaleIn...), failOutAfter: -1}
}

// Controls returns the control requests received.
func (d *Device) Controls() []Control {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]Control(nil), d.controls...)
}

// BulkOuts is the number of bulk OUT transfers received.
func (d *Device) BulkOuts() int { d.mu.Lock(); defer d.mu.Unlock(); return d.outs }

// Divider is the last divider programmed.
func (d *Device) Divider() int { d.mu.Lock(); defer d.mu.Unlock(); return d.div }

// Disconnect makes every later transfer fail with usb.ErrDisconnected.
func (d *Device) Disconnect() { d.mu.Lock(); d.disconnected = true; d.mu.Unlock() }

// DisconnectAfterOuts disconnects the device on the Nth next bulk OUT.
func (d *Device) DisconnectAfterOuts(n int) { d.mu.Lock(); d.failOutAfter = n; d.mu.Unlock() }

// Hang makes BulkIn time out regardless of pending data.
func (d *Device) Hang(h bool) { d.mu.Lock(); d.hangIn = h; d.mu.Unlock() }

// Closed reports whether Close was called.
func (d *Device) Closed() bool { d.mu.Lock(); defer d.mu.Unlock(); return d.closed }

// Pending is the number of reply bytes waiting.
func (d *Device) Pending() int { d.mu.Lock(); defer d.mu.Unlock(); return len(d.fifo) }

func (d *Device) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d.closed {
		return usb.ErrClosed
	}
	if d.disconnected {
		return usb.ErrDisconnected
	}
	return nil
}

// ControlIn implements usb.Port.
func (d *Device) ControlIn(ctx context.Context, rt, req uint8, value, index uint16, length int) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.check(ctx); err != nil {
		return nil, err
	}
	d.controls = append(d.controls, Control{Type: rt, Request: req, Value: value, Index: index})
	switch {
	case rt == 0x80 && req == 6 && value == 0x2000:
		return append([]byte(nil), d.cfg.Caps[:min(length, len(d.cfg.Caps))]...), nil
	case rt == 0xC0 && req == 2:
		d.tdoPin = !d.tdoPin
		if d.tdoPin {
			return []byte{1}, nil
		}
		return []byte{0}, nil
	}
	return nil, errStall
}

// ControlOut implements usb.Port.
func (d *Device) ControlOut(ctx context.Context, rt, req uint8, value, index uint16, data []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.check(ctx); err != nil {
		return err
	}
	d.controls = append(d.controls, Control{rt, req, value, index, append([]byte(nil), data...)})
	if rt == 0x40 && req == 0 {
		d.div = int(value)
		return nil
	}
	return errStall
}

// BulkOut implements usb.Port.
func (d *Device) BulkOut(ctx context.Context, ep uint8, data []byte, _ time.Duration) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.check(ctx); err != nil {
		return 0, err
	}
	if d.failOutAfter == 0 {
		d.disconnected = true
		return 0, usb.ErrDisconnected
	}
	if d.failOutAfter > 0 {
		d.failOutAfter--
	}
	d.outs++
	if d.cfg.FIFOLimit > 0 && len(d.fifo) > d.cfg.FIFOLimit {
		return 0, usb.ErrTimeout
	}
	for _, c := range espusb.Decode(data) {
		switch {
		case c < 8:
			tdo := d.T.Clock(c&2 != 0, c&1 != 0)
			if c&4 != 0 {
				d.bits = append(d.bits, tdo)
			}
		case c == 0xA:
			d.flush()
		}
	}
	return len(data), nil
}

func (d *Device) flush() {
	for i := 0; i < len(d.bits); i += 8 {
		var b byte
		for j := 0; j < 8 && i+j < len(d.bits); j++ {
			if d.bits[i+j] {
				b |= 1 << j
			}
		}
		d.fifo = append(d.fifo, b)
	}
	d.bits = d.bits[:0]
}

// BulkIn implements usb.Port.
func (d *Device) BulkIn(ctx context.Context, ep uint8, length int, timeout time.Duration) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.check(ctx); err != nil {
		return nil, err
	}
	d.maxIn = max(d.maxIn, length)
	if d.hangIn || len(d.fifo) == 0 {
		time.Sleep(min(timeout, time.Millisecond))
		return nil, usb.ErrTimeout
	}
	n := min(length, d.cfg.MaxPacket, len(d.fifo))
	out := append([]byte(nil), d.fifo[:n]...)
	d.fifo = d.fifo[n:]
	return out, nil
}

// Describe implements usb.Port.
func (d *Device) Describe() string { return "SYNTHETIC espusb device" }

// DetachedDriver implements usb.Port.
func (d *Device) DetachedDriver() string { return "" }

// Close implements usb.Port.
func (d *Device) Close() error { d.mu.Lock(); d.closed = true; d.mu.Unlock(); return nil }
