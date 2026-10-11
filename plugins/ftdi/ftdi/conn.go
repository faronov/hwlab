package ftdi

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/faronov/hwlab/internal/usb"
)

// Quirks hold deviations of a device from the FTDI documentation. They come
// from profiles, never from probing for "real" FTDI silicon.
type Quirks struct {
	// OptionalControl lists SIO requests whose failure is tolerated (the
	// device may stall requests it does not implement).
	OptionalControl []uint8
}

func (q Quirks) optional(req uint8) bool {
	for _, r := range q.OptionalControl {
		if r == req {
			return true
		}
	}
	return false
}

// Options configure a Conn.
type Options struct {
	// Channel is the 0-based FTDI channel (USB interface) to drive.
	Channel int
	// Timeout bounds one bulk transfer (default 1 s).
	Timeout time.Duration
	// MaxPacket overrides the bulk packet size taken from the endpoint
	// descriptor.
	MaxPacket int
	Quirks    Quirks
}

// LogEntry is one recorded protocol step, kept as evidence of what the
// initialisation did and what the device answered.
type LogEntry struct {
	Op     string `json:"op"`
	Detail string `json:"detail,omitempty"`
	Error  string `json:"error,omitempty"`
}

const maxLog = 512

// Conn is an FTDI channel on one claimed USB interface.
type Conn struct {
	mu     sync.Mutex
	port   usb.Port
	chip   ChipType
	opt    Options
	index  uint16 // wIndex for SIO requests: channel + 1
	epIn   uint8
	epOut  uint8
	maxPkt int
	status [2]byte
	rx     []byte // payload received but not yet consumed
	log    []LogEntry
	closed bool
}

// New binds a channel of dev to an already-claimed port. The interface
// number is opt.Channel. It performs no USB traffic.
func New(port usb.Port, dev usb.Device, opt Options) (*Conn, error) {
	ifc, ok := dev.Iface(opt.Channel)
	if !ok {
		return nil, fmt.Errorf("ftdi: device has no interface/channel %d", opt.Channel)
	}
	in, out, mp := ifc.BulkEndpoints()
	if in == 0 || out == 0 {
		return nil, fmt.Errorf("ftdi: channel %d has no bulk IN/OUT endpoint pair", opt.Channel)
	}
	if opt.MaxPacket > 0 {
		mp = opt.MaxPacket
	}
	if mp < 8 {
		return nil, fmt.Errorf("ftdi: implausible bulk packet size %d", mp)
	}
	if opt.Timeout <= 0 {
		opt.Timeout = time.Second
	}
	return &Conn{port: port, chip: ChipFromBCD(dev.BCD), opt: opt, index: uint16(opt.Channel + 1), epIn: in, epOut: out, maxPkt: mp}, nil
}

// Chip returns the generation derived from bcdDevice.
func (c *Conn) Chip() ChipType { return c.chip }

// Channel returns the 0-based channel.
func (c *Conn) Channel() int { return c.opt.Channel }

// MaxPacket returns the bulk packet size in use.
func (c *Conn) MaxPacket() int { return c.maxPkt }

// Endpoints returns the bulk IN and OUT addresses.
func (c *Conn) Endpoints() (in, out uint8) { return c.epIn, c.epOut }

// ModemStatus returns the last status bytes received.
func (c *Conn) ModemStatus() [2]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

// Log returns the recorded protocol steps.
func (c *Conn) Log() []LogEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]LogEntry(nil), c.log...)
}

func (c *Conn) note(op, detail string, err error) {
	e := LogEntry{Op: op, Detail: detail}
	if err != nil {
		e.Error = err.Error()
	}
	if len(c.log) < maxLog {
		c.log = append(c.log, e)
	}
}

func (c *Conn) ctrlOut(ctx context.Context, op string, req uint8, value uint16, index uint16) error {
	err := c.port.ControlOut(ctx, reqTypeOut, req, value, index, nil)
	c.note(op, fmt.Sprintf("req=0x%02x value=0x%04x index=0x%04x", req, value, index), err)
	if err != nil && c.opt.Quirks.optional(req) && !errors.Is(err, usb.ErrDisconnected) && ctx.Err() == nil {
		return nil
	}
	return err
}

// Reset issues SIO reset (not a USB port reset).
func (c *Conn) Reset(ctx context.Context) error {
	return c.ctrlOut(ctx, "reset", ReqReset, resetSIO, c.index)
}

// PurgeRX discards the device's receive buffer and anything buffered here.
func (c *Conn) PurgeRX(ctx context.Context) error {
	c.mu.Lock()
	c.rx = nil
	c.mu.Unlock()
	return c.ctrlOut(ctx, "purge_rx", ReqReset, resetPurgeRX, c.index)
}

// PurgeTX discards the device's transmit buffer.
func (c *Conn) PurgeTX(ctx context.Context) error {
	return c.ctrlOut(ctx, "purge_tx", ReqReset, resetPurgeTX, c.index)
}

// SetLatency sets the latency timer in milliseconds (1..255).
func (c *Conn) SetLatency(ctx context.Context, ms int) error {
	if ms < 1 || ms > 255 {
		return fmt.Errorf("ftdi: latency %d ms: want 1..255", ms)
	}
	return c.ctrlOut(ctx, "set_latency", ReqSetLatency, uint16(ms), c.index)
}

// Latency reads the latency timer in milliseconds.
func (c *Conn) Latency(ctx context.Context) (int, error) {
	b, err := c.port.ControlIn(ctx, reqTypeIn, ReqGetLatency, 0, c.index, 1)
	c.note("get_latency", "", err)
	if err != nil {
		return 0, err
	}
	if len(b) != 1 {
		return 0, fmt.Errorf("ftdi: latency reply of %d byte(s)", len(b))
	}
	return int(b[0]), nil
}

// SetBitMode selects a bit mode; mask is the output pin mask of the
// bit-bang modes (ignored for MPSSE, where it is conventionally 0).
func (c *Conn) SetBitMode(ctx context.Context, mask uint8, mode BitMode) error {
	return c.ctrlOut(ctx, "set_bitmode", ReqSetBitMode, uint16(mode)<<8|uint16(mask), c.index)
}

// DisableSpecialChars turns the event and error characters off, so payload
// bytes are never reinterpreted.
func (c *Conn) DisableSpecialChars(ctx context.Context) error {
	if err := c.ctrlOut(ctx, "event_char_off", ReqSetEventChar, 0, c.index); err != nil {
		return err
	}
	return c.ctrlOut(ctx, "error_char_off", ReqSetErrorChar, 0, c.index)
}

// ReadPins reads the current pin levels of the low byte (bit-bang and MPSSE
// modes).
func (c *Conn) ReadPins(ctx context.Context) (uint8, error) {
	b, err := c.port.ControlIn(ctx, reqTypeIn, ReqReadPins, 0, c.index, 1)
	c.note("read_pins", "", err)
	if err != nil {
		return 0, err
	}
	if len(b) != 1 {
		return 0, fmt.Errorf("ftdi: read-pins reply of %d byte(s)", len(b))
	}
	return b[0], nil
}

// maxWrite bounds one bulk OUT transfer.
const maxWrite = 4096

// Write sends data to the bulk OUT endpoint, in bounded transfers.
func (c *Conn) Write(ctx context.Context, data []byte) error {
	for len(data) > 0 {
		n := min(len(data), maxWrite)
		w, err := c.port.BulkOut(ctx, c.epOut, data[:n], c.opt.Timeout)
		if err != nil {
			return fmt.Errorf("ftdi write: %w", err)
		}
		if w <= 0 {
			return errors.New("ftdi write: no progress")
		}
		data = data[w:]
	}
	return nil
}

// readChunk is the size of one bulk IN request (a multiple of any packet size).
func (c *Conn) readChunk() int { return c.maxPkt * 8 }

// Read returns exactly n payload bytes, stripping the status bytes of every
// packet. Status-only packets (the latency timer firing with no data) are
// skipped. Extra payload that arrives with the last packet is kept for the
// next call. If the data does not arrive within timeout, the error wraps
// usb.ErrTimeout and reports how many bytes did arrive; those bytes are
// returned with the error so the caller can resynchronise.
func (c *Conn) Read(ctx context.Context, n int, timeout time.Duration) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, usb.ErrClosed
	}
	deadline := time.Now().Add(timeout)
	out := make([]byte, 0, n)
	take := func() {
		k := min(n-len(out), len(c.rx))
		out = append(out, c.rx[:k]...)
		c.rx = c.rx[k:]
	}
	take()
	for len(out) < n {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		wait := time.Until(deadline)
		if wait <= 0 {
			return out, fmt.Errorf("ftdi read: %w: got %d of %d byte(s)", usb.ErrTimeout, len(out), n)
		}
		raw, err := c.port.BulkIn(ctx, c.epIn, c.readChunk(), min(wait, c.opt.Timeout))
		if err != nil {
			if errors.Is(err, usb.ErrTimeout) {
				continue
			}
			return out, fmt.Errorf("ftdi read: %w", err)
		}
		payload, st, err := StripStatus(raw, c.maxPkt)
		if err != nil {
			return out, err
		}
		c.status = st
		c.rx = append(c.rx, payload...)
		take()
	}
	return out, nil
}

// Drain reads and discards whatever the device still has queued, waiting up
// to wait for new data; it returns the discarded payload.
func (c *Conn) Drain(ctx context.Context, wait time.Duration) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := append([]byte(nil), c.rx...)
	c.rx = nil
	deadline := time.Now().Add(wait)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return out, nil
		}
		raw, err := c.port.BulkIn(ctx, c.epIn, c.readChunk(), left)
		if err != nil {
			if errors.Is(err, usb.ErrTimeout) {
				return out, nil
			}
			return out, err
		}
		payload, st, err := StripStatus(raw, c.maxPkt)
		if err != nil {
			return out, err
		}
		c.status = st
		out = append(out, payload...)
	}
}

// Close returns the channel to its reset bit mode (best effort) and releases
// the USB interface, which re-binds a detached kernel driver.
func (c *Conn) Close(ctx context.Context) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	_ = c.port.ControlOut(ctx, reqTypeOut, ReqSetBitMode, uint16(BitModeReset)<<8, c.index, nil)
	return c.port.Close()
}

// DetachedDriver names the kernel driver detached from the interface.
func (c *Conn) DetachedDriver() string { return c.port.DetachedDriver() }

// Describe describes the underlying transport.
func (c *Conn) Describe() string { return c.port.Describe() }
