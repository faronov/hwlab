// Package sim is a SYNTHETIC FTDI FT2232-class device: a usb.Port that
// answers FTDI control requests, runs an MPSSE command processor and drives a
// bit-accurate IEEE 1149.1 TAP chain. It exists so protocol, MPSSE and JTAG
// code can be tested, replayed and demonstrated without hardware. Nothing it
// reports is evidence about real hardware; real captures live in testdata
// under a REAL label.
//
// The TAP model is written independently of package jtag on purpose, so the
// two cross-check each other.
package sim

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/faronov/hwlab/internal/usb"
)

// TAP describes one simulated TAP.
type TAP struct {
	// IDCode of the device; 0 means it has no IDCODE register and selects
	// BYPASS after reset.
	IDCode uint32
	// IRLen is the instruction register length (>= 2).
	IRLen int
}

// Config configures a Device.
type Config struct {
	// Channel is the 0-based channel this port represents; SIO requests must
	// carry Channel+1 as wIndex.
	Channel int
	// MaxPacket is the bulk packet size (default 64).
	MaxPacket int
	// Chain lists TAPs from the one nearest TDO to the one nearest TDI.
	Chain []TAP
	// FloatHigh is the TDO level when no TAP drives it.
	FloatHigh bool
	// Sipeed enables the command-subset and no-op behaviour of the BL702
	// emulator firmware: ignored loopback/divisor/GPIO, no read-only shifts.
	Sipeed bool
	// HighSpeed enables H-part opcodes (clock bits, divide-by-5...).
	HighSpeed bool
	// IdleStatusPackets makes an empty BulkIn return a status-only packet
	// (as the latency timer does) instead of timing out.
	IdleStatusPackets bool
	// MaxPacketsPerRead, when > 0, limits how many packets one BulkIn
	// returns, to exercise fragmentation.
	MaxPacketsPerRead int
	// StallRequests lists SIO requests the device answers with a stall.
	StallRequests []uint8
	// DetachedDriver is reported by DetachedDriver().
	DetachedDriver string
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

	closed       bool
	disconnected bool
	failBulkIn   int // fail the Nth next BulkIn with disconnect when > 0

	controls []Control
	written  [][]byte

	mpsse    bool
	bitMode  uint8
	latency  uint8
	loopback bool
	divisor  uint16
	div5     bool
	gpioLow  [2]byte // value, direction
	gpioHigh [2]byte
	tmsPin   bool

	pend []byte // partial MPSSE command
	fifo []byte // reply payload waiting to be read

	tap tapModel
}

// New returns a Device.
func New(cfg Config) *Device {
	if cfg.MaxPacket == 0 {
		cfg.MaxPacket = 64
	}
	d := &Device{cfg: cfg, latency: 16, tmsPin: true}
	d.tap.init(cfg.Chain, cfg.FloatHigh)
	return d
}

// Controls returns a copy of the control requests received.
func (d *Device) Controls() []Control {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]Control(nil), d.controls...)
}

// Written returns a copy of the bulk OUT transfers received.
func (d *Device) Written() [][]byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([][]byte(nil), d.written...)
}

// MPSSE reports whether MPSSE mode is active.
func (d *Device) MPSSE() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.mpsse
}

// Divisor returns the last divisor the device accepted (Sipeed mode ignores it).
func (d *Device) Divisor() uint16 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.divisor
}

// Loopback reports whether internal loopback is on.
func (d *Device) Loopback() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.loopback
}

// TAPState returns the simulated TAP state name.
func (d *Device) TAPState() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.tap.state.String()
}

// Closed reports whether Close was called.
func (d *Device) Closed() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.closed
}

// Disconnect simulates unplugging the device: every later call fails with
// usb.ErrDisconnected.
func (d *Device) Disconnect() {
	d.mu.Lock()
	d.disconnected = true
	d.mu.Unlock()
}

// DisconnectOnBulkIn makes the nth next BulkIn (1 = the next) report a
// disconnect.
func (d *Device) DisconnectOnBulkIn(n int) {
	d.mu.Lock()
	d.failBulkIn = n
	d.mu.Unlock()
}

func (d *Device) check(ctx context.Context) error {
	if d.closed {
		return usb.ErrClosed
	}
	if d.disconnected {
		return usb.ErrDisconnected
	}
	return ctx.Err()
}

func (d *Device) stalled(req uint8) bool {
	for _, r := range d.cfg.StallRequests {
		if r == req {
			return true
		}
	}
	return false
}

var errStall = errors.New("usb: control request stalled")

// Describe implements usb.Port.
func (d *Device) Describe() string { return "synthetic FT2232-class device (sim)" }

// DetachedDriver implements usb.Port.
func (d *Device) DetachedDriver() string { return d.cfg.DetachedDriver }

// Close implements usb.Port.
func (d *Device) Close() error {
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()
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
	if d.stalled(req) {
		return nil, errStall
	}
	if index != uint16(d.cfg.Channel+1) {
		return nil, errStall
	}
	switch req {
	case 0x0A:
		return []byte{d.latency}, nil
	case 0x0C:
		return []byte{d.gpioLow[0]}, nil
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
	d.controls = append(d.controls, Control{Type: rt, Request: req, Value: value, Index: index, Data: append([]byte(nil), data...)})
	if d.stalled(req) {
		return errStall
	}
	if index != uint16(d.cfg.Channel+1) {
		return errStall
	}
	switch req {
	case 0x00: // reset
		switch value {
		case 0:
			d.fifo, d.pend = nil, nil
		case 1:
			d.fifo = nil
		case 2:
			d.pend = nil
		}
	case 0x02, 0x06, 0x07: // flow control, event char, error char: accepted
	case 0x09:
		if value < 1 || value > 255 {
			return errStall
		}
		d.latency = uint8(value)
	case 0x0B:
		d.bitMode = uint8(value >> 8)
		d.mpsse = d.bitMode == 0x02
		if !d.mpsse {
			d.loopback = false
		}
	default:
		return errStall
	}
	return nil
}

// BulkOut implements usb.Port.
func (d *Device) BulkOut(ctx context.Context, ep uint8, data []byte, timeout time.Duration) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.check(ctx); err != nil {
		return 0, err
	}
	if ep != 0x02+uint8(d.cfg.Channel)*2 {
		return 0, fmt.Errorf("sim: bulk OUT to unexpected endpoint 0x%02x", ep)
	}
	d.written = append(d.written, append([]byte(nil), data...))
	if d.mpsse {
		d.pend = append(d.pend, data...)
		d.process()
	}
	return len(data), nil
}

// BulkIn implements usb.Port: it packs queued reply bytes into packets of
// MaxPacket bytes, each starting with two status bytes.
func (d *Device) BulkIn(ctx context.Context, ep uint8, length int, timeout time.Duration) ([]byte, error) {
	d.mu.Lock()
	if err := d.check(ctx); err != nil {
		d.mu.Unlock()
		return nil, err
	}
	if d.failBulkIn > 0 {
		d.failBulkIn--
		if d.failBulkIn == 0 {
			d.disconnected = true
			d.mu.Unlock()
			return nil, usb.ErrDisconnected
		}
	}
	if ep != 0x81+uint8(d.cfg.Channel)*2 {
		d.mu.Unlock()
		return nil, fmt.Errorf("sim: bulk IN from unexpected endpoint 0x%02x", ep)
	}
	status := []byte{0x01, 0x60}
	if len(d.fifo) == 0 {
		idle := d.cfg.IdleStatusPackets
		d.mu.Unlock()
		wait := timeout
		if idle {
			wait = min(timeout, time.Millisecond)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
		if idle {
			return status, nil
		}
		return nil, usb.ErrTimeout
	}
	defer d.mu.Unlock()
	var out []byte
	packets := 0
	for len(d.fifo) > 0 && len(out)+2 < length {
		if d.cfg.MaxPacketsPerRead > 0 && packets >= d.cfg.MaxPacketsPerRead {
			break
		}
		room := min(d.cfg.MaxPacket-2, length-len(out)-2, len(d.fifo))
		if room <= 0 {
			break
		}
		out = append(out, status...)
		out = append(out, d.fifo[:room]...)
		d.fifo = d.fifo[room:]
		packets++
	}
	return out, nil
}

var _ usb.Port = (*Device)(nil)

// USBDevice returns the descriptor view of a synthetic dual-channel FT2232D
// laid out like the Sipeed adapter: channel A on interface 0 (0x81/0x02),
// channel B on interface 1 (0x83/0x04).
func USBDevice(maxPacket int) usb.Device {
	if maxPacket == 0 {
		maxPacket = 64
	}
	ifc := func(n int, in, out uint8, driver, tty string) usb.Interface {
		return usb.Interface{Number: n, Class: 0xFF, Driver: driver, TTY: tty, Endpoints: []usb.Endpoint{
			{Address: in, Type: "Bulk", MaxPacket: maxPacket},
			{Address: out, Type: "Bulk", MaxPacket: maxPacket},
		}}
	}
	return usb.Device{
		Bus: 1, Address: 2, VID: 0x0403, PID: 0x6010, BCD: 0x0500,
		Manufacturer: "SIPEED", Product: "JTAG Debugger", Serial: "SYNTH0001",
		SysName: "1-1", Configuration: 1, SpeedMbps: 12,
		Interfaces: []usb.Interface{
			ifc(0, 0x81, 0x02, "ftdi_sio", "ttyUSB1"),
			ifc(1, 0x83, 0x04, "ftdi_sio", "ttyUSB2"),
		},
	}
}
