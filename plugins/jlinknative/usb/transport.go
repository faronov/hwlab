// Package usb is the USB transport of the native J-Link backend: discovery of
// SEGGER probes (sysfs on Linux, the IORegistry on macOS), opening one probe
// by its serial number, and a command/reply exchange over the vendor bulk
// interface. It sits on internal/usb (Linux usbfs or macOS IOKit through
// purego; no libusb, no cgo) and knows nothing about the meaning of the bytes
// it carries.
//
// A probe is identified by its USB serial number only. The bus and device
// numbers are reported for diagnostics and are never used as identity: they
// change at every re-enumeration.
package usb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	iusb "github.com/faronov/hwlab/internal/usb"
	"github.com/faronov/hwlab/plugins/jlinknative/jlerr"
)

// VID is the USB vendor id SEGGER probes enumerate with.
const VID = 0x1366

// DefaultTimeout bounds one USB transfer.
const DefaultTimeout = 2 * time.Second

// NormalizeSerial strips the leading zeros the USB serial descriptor is padded
// with, which is how SEGGER tools print and accept a serial number.
func NormalizeSerial(s string) string {
	s = strings.TrimSpace(s)
	t := strings.TrimLeft(s, "0")
	if t == "" && s != "" {
		return "0"
	}
	return t
}

// Info is one attached probe as seen in sysfs or the IORegistry. Nothing is
// opened to build it.
type Info struct {
	Device    iusb.Device
	Serial    string // normalized USB serial: the stable identity
	Interface int    // vendor interface carrying the command pipe
	In, Out   uint8  // bulk endpoint addresses
	MaxPacket int
}

// ID is the stable device id derived from the serial.
func (i Info) ID() string { return "jlink-native:" + i.Serial }

// Scan lists SEGGER probes that expose a vendor-class interface with one bulk
// IN and one bulk OUT endpoint. Interfaces bound to kernel drivers (the CDC
// UART of a SAM-ICE) are never touched.
func Scan(root string) []Info {
	var out []Info
	for _, d := range iusb.Scan(root, func(v, _ uint16) bool { return v == VID }) {
		var best *Info
		for _, ifc := range d.Interfaces {
			if ifc.Class != 0xff {
				continue
			}
			in, o, mp := ifc.BulkEndpoints()
			if in == 0 || o == 0 {
				continue
			}
			best = &Info{Device: d, Serial: NormalizeSerial(d.Serial), Interface: ifc.Number, In: in, Out: o, MaxPacket: mp}
		}
		if best != nil {
			out = append(out, *best)
		}
	}
	return out
}

// Find returns the probe with the given serial. It never falls back to "the
// first probe": a serial is required, and a duplicated serial is an error.
func Find(infos []Info, serial string) (Info, error) {
	want := NormalizeSerial(serial)
	var hit []Info
	for _, i := range infos {
		if i.Serial == want {
			hit = append(hit, i)
		}
	}
	switch len(hit) {
	case 1:
		return hit[0], nil
	case 0:
		return Info{}, jlerr.New(jlerr.ProbeGone, "find probe", fmt.Errorf("no J-Link with serial %q is attached", want))
	}
	return Info{}, jlerr.New(jlerr.ProtocolError, "find probe", fmt.Errorf("%d probes report serial %q; select by USB bus path is not supported", len(hit), want))
}

// Transport is the byte pipe the protocol layer talks through. Conn implements
// it on real hardware and the simulator implements it for tests.
type Transport interface {
	// Exchange sends cmd (nothing when empty) and reads exactly replyLen
	// bytes. A short reply is an error, never a success.
	Exchange(ctx context.Context, cmd []byte, replyLen int) ([]byte, error)
	Describe() string
	Close() error
}

// Conn is an open probe.
type Conn struct {
	info    Info
	port    iusb.Port
	timeout time.Duration

	mu sync.Mutex
	// dirty is set when an exchange ended with bytes possibly still in
	// flight; the next exchange drains the pipe first. A probe left with an
	// unread reply stops accepting commands.
	dirty  bool
	closed bool
}

// Open claims the probe's command interface. A second open of the same probe,
// from this or another process, reports PROBE_BUSY.
func Open(info Info) (*Conn, error) {
	p, err := iusb.Open(info.Device, info.Interface, iusb.OpenOptions{})
	if err != nil {
		return nil, mapUSB("open "+info.Serial, err)
	}
	return &Conn{info: info, port: p, timeout: DefaultTimeout}, nil
}

// OpenSerial opens the probe with the given serial found below sysfs root.
func OpenSerial(root, serial string) (*Conn, error) {
	info, err := Find(Scan(root), serial)
	if err != nil {
		return nil, err
	}
	return Open(info)
}

// Info returns the discovery record the connection was opened from.
func (c *Conn) Info() Info { return c.info }

// Describe implements Transport.
func (c *Conn) Describe() string { return c.port.Describe() }

// SetTimeout sets the per-transfer timeout.
func (c *Conn) SetTimeout(d time.Duration) {
	c.mu.Lock()
	c.timeout = d
	c.mu.Unlock()
}

func mapUSB(op string, err error) error {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.Is(err, iusb.ErrDisconnected):
		return jlerr.New(jlerr.ProbeGone, op, err)
	case errors.Is(err, iusb.ErrBusy):
		return jlerr.New(jlerr.ProbeBusy, op, err)
	case errors.Is(err, iusb.ErrTimeout):
		return jlerr.New(jlerr.USBTimeout, op, err)
	case errors.Is(err, iusb.ErrStall):
		return jlerr.New(jlerr.USBStall, op, err)
	case errors.Is(err, iusb.ErrPermission):
		return jlerr.New(jlerr.USBPermission, op, err)
	case errors.Is(err, iusb.ErrClosed):
		return jlerr.New(jlerr.ProbeGone, op, err)
	}
	return jlerr.New(jlerr.ProtocolError, op, err)
}

// Exchange implements Transport.
func (c *Conn) Exchange(ctx context.Context, cmd []byte, replyLen int) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, jlerr.New(jlerr.ProbeGone, "exchange", iusb.ErrClosed)
	}
	if c.dirty {
		if err := c.drain(ctx); err != nil {
			return nil, err
		}
	}
	if len(cmd) > 0 {
		if _, err := c.port.BulkOut(ctx, c.info.Out, cmd, c.timeout); err != nil {
			c.afterError(err)
			return nil, mapUSB("bulk out", err)
		}
	}
	if replyLen == 0 {
		return nil, nil
	}
	buf := make([]byte, 0, replyLen)
	zlps := 0
	for len(buf) < replyLen {
		b, err := c.port.BulkIn(ctx, c.info.In, replyLen-len(buf), c.timeout)
		if err != nil {
			c.afterError(err)
			if errors.Is(err, iusb.ErrTimeout) {
				return buf, jlerr.New(jlerr.USBTimeout, fmt.Sprintf("bulk in: %d of %d reply bytes", len(buf), replyLen), err)
			}
			return buf, mapUSB("bulk in", err)
		}
		if len(b) == 0 {
			if zlps++; zlps > 4 {
				c.dirty = true
				return buf, jlerr.New(jlerr.ProtocolError, "bulk in", io.ErrNoProgress)
			}
			continue
		}
		buf = append(buf, b...)
	}
	return buf, nil
}

// afterError records what the failed transfer left behind.
func (c *Conn) afterError(err error) {
	switch {
	case errors.Is(err, iusb.ErrStall):
		if hc, ok := c.port.(iusb.HaltClearer); ok {
			_ = hc.ClearHalt(c.info.In)
			_ = hc.ClearHalt(c.info.Out)
		}
		c.dirty = true
	case errors.Is(err, iusb.ErrTimeout), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		c.dirty = true
	}
}

// drain discards stale bytes the probe still holds so that a late reply is
// not mistaken for the answer to the next command.
func (c *Conn) drain(ctx context.Context) error {
	for i := 0; i < 64; i++ {
		b, err := c.port.BulkIn(ctx, c.info.In, 4096, 30*time.Millisecond)
		if errors.Is(err, iusb.ErrTimeout) {
			c.dirty = false
			return nil
		}
		if err != nil {
			return mapUSB("drain", err)
		}
		_ = b
	}
	return jlerr.New(jlerr.ProtocolError, "drain", errors.New("the probe keeps sending data"))
}

// Close releases the interface. It is idempotent.
func (c *Conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	return c.port.Close()
}
