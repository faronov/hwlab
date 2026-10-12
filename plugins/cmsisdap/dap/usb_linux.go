//go:build linux

package dap

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/faronov/hwlab/internal/usb"
	"golang.org/x/sys/unix"
)

// OpenUSB opens the probe's transport: usbfs bulk endpoints for v2, hidraw for
// v1. It never changes the USB configuration and never detaches a kernel
// driver, so the CDC-ACM interfaces of a composite probe stay bound.
func OpenUSB(d UsbDevice) (Transport, error) {
	switch d.Version {
	case V2:
		return openBulk(d)
	case V1:
		return openHID(d)
	}
	return nil, fmt.Errorf("unknown CMSIS-DAP transport version %d", d.Version)
}

func openBulk(d UsbDevice) (Transport, error) {
	vid, err1 := strconv.ParseUint(d.VID, 16, 16)
	pid, err2 := strconv.ParseUint(d.PID, 16, 16)
	if err1 != nil || err2 != nil {
		return nil, fmt.Errorf("bad USB id %s:%s", d.VID, d.PID)
	}
	ud := usb.Device{
		Bus: d.Bus, Address: d.Address, VID: uint16(vid), PID: uint16(pid), Serial: d.Serial, SysName: d.SysName,
		Interfaces: []usb.Interface{{Number: d.Interface, Class: 0xff}},
	}
	port, err := usb.Open(ud, d.Interface, usb.OpenOptions{})
	if err != nil {
		return nil, err
	}
	return newBulk(port, d), nil
}

type hidTransport struct {
	mu     sync.Mutex
	f      *os.File
	d      UsbDevice
	report int
}

func openHID(d UsbDevice) (Transport, error) {
	if d.HIDRaw == "" {
		return nil, errors.New("no hidraw node for this CMSIS-DAP v1 probe (is the hid-generic driver bound?)")
	}
	f, err := os.OpenFile(d.HIDRaw, os.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return nil, fmt.Errorf("%w (grant access to %s, e.g. with a udev rule for %s:%s)", err, d.HIDRaw, d.VID, d.PID)
		}
		return nil, err
	}
	n := d.MaxPacket
	if n < minPacket {
		n = 64
	}
	return &hidTransport{f: f, d: d, report: n}, nil
}

func (t *hidTransport) Exchange(req []byte, timeout time.Duration) ([]byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.f == nil {
		return nil, os.ErrClosed
	}
	if len(req) > t.report {
		return nil, fmt.Errorf("request of %d bytes exceeds the %d-byte HID report", len(req), t.report)
	}
	// Report id 0 (the device has no numbered reports) precedes the payload.
	out := make([]byte, 1+t.report)
	copy(out[1:], req)
	if _, err := t.f.Write(out); err != nil {
		return nil, fmt.Errorf("hidraw write: %w", err)
	}
	pfd := []unix.PollFd{{Fd: int32(t.f.Fd()), Events: unix.POLLIN}}
	for {
		n, err := unix.Poll(pfd, int(timeout/time.Millisecond))
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, errors.New("timeout waiting for the probe's HID report")
		}
		break
	}
	in := make([]byte, t.report)
	n, err := t.f.Read(in)
	if err != nil {
		return nil, fmt.Errorf("hidraw read: %w", err)
	}
	return in[:n], nil
}

func (t *hidTransport) PacketSize() int { return t.report }

func (t *hidTransport) Describe() string {
	return fmt.Sprintf("CMSIS-DAP v1 HID %s:%s %s", t.d.VID, t.d.PID, t.d.HIDRaw)
}

func (t *hidTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.f == nil {
		return nil
	}
	err := t.f.Close()
	t.f = nil
	return err
}
