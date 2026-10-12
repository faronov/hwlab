//go:build linux

package usb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// usbfs ioctl numbers (linux/usbdevice_fs.h), built from the generic _IOC
// encoding so struct sizes follow the host pointer size.
const (
	iocNone  = 0
	iocWrite = 1
	iocRead  = 2
)

func ioc(dir, nr, size uintptr) uintptr { return dir<<30 | size<<16 | 'U'<<8 | nr }

type ctrlTransfer struct {
	requestType uint8
	request     uint8
	value       uint16
	index       uint16
	length      uint16
	timeout     uint32 // ms
	data        unsafe.Pointer
}

type bulkTransfer struct {
	endpoint uint32
	length   uint32
	timeout  uint32 // ms
	data     unsafe.Pointer
}

type disconnectClaim struct {
	iface  uint32
	flags  uint32
	driver [256]byte
}

type usbfsIoctl struct {
	ifno int32
	code int32
	data unsafe.Pointer
}

const (
	disconnectClaimIfDriver = 0x01
	usbdevfsConnectCode     = 'U'<<8 | 23 // _IO('U', 23)
)

var (
	ioctlControl         = ioc(iocRead|iocWrite, 0, unsafe.Sizeof(ctrlTransfer{}))
	ioctlBulk            = ioc(iocRead|iocWrite, 2, unsafe.Sizeof(bulkTransfer{}))
	ioctlClaim           = ioc(iocRead, 15, unsafe.Sizeof(uint32(0)))
	ioctlRelease         = ioc(iocRead, 16, unsafe.Sizeof(uint32(0)))
	ioctlIoctl           = ioc(iocRead|iocWrite, 18, unsafe.Sizeof(usbfsIoctl{}))
	ioctlDisconnectClaim = ioc(iocRead, 27, unsafe.Sizeof(disconnectClaim{}))
	ioctlReset           = ioc(iocNone, 20, 0)
	ioctlClearHalt       = ioc(iocRead, 21, unsafe.Sizeof(uint32(0)))
	ioctlCapabilities    = ioc(iocRead, 26, unsafe.Sizeof(uint32(0)))
)

// slice is the longest single blocking transfer for IN reads, so that a
// cancelled context is noticed promptly.
const slice = 100 * time.Millisecond

type port struct {
	mu       sync.Mutex
	f        *os.File
	dev      Device
	iface    int
	claimed  bool
	detached string
}

// Open claims interface ifnum of d through usbfs. It never changes the USB
// configuration. A kernel driver bound to the interface is detached only when
// opt.DetachDriver names it, and only from this interface.
func Open(d Device, ifnum int, opt OpenOptions) (Port, error) {
	if d.Bus <= 0 || d.Address <= 0 {
		return nil, fmt.Errorf("usb: invalid device address %d:%d", d.Bus, d.Address)
	}
	if _, ok := d.Iface(ifnum); !ok {
		return nil, fmt.Errorf("usb: device %04x:%04x has no interface %d", d.VID, d.PID, ifnum)
	}
	f, err := os.OpenFile(d.Path(), os.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return nil, fmt.Errorf("%w: %w (grant access with a udev rule for %04x:%04x, see docs/hardware.md)", ErrPermission, err, d.VID, d.PID)
		}
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %w", ErrDisconnected, err)
		}
		return nil, err
	}
	p := &port{f: f, dev: d, iface: ifnum}
	ok := false
	defer func() {
		if !ok {
			_ = p.Close()
		}
	}()
	// Reading the usbfs node returns the device descriptor first.
	var desc [18]byte
	if n, err := f.Read(desc[:]); err != nil {
		return nil, fmt.Errorf("usb: cannot read the device descriptor: %w", err)
	} else if n != len(desc) {
		return nil, fmt.Errorf("usb: device descriptor has %d bytes, expected %d", n, len(desc))
	}
	if vid, pid := uint16(desc[8])|uint16(desc[9])<<8, uint16(desc[10])|uint16(desc[11])<<8; vid != d.VID || pid != d.PID {
		return nil, fmt.Errorf("usb: device %d:%d is %04x:%04x, expected %04x:%04x (re-enumerated?)", d.Bus, d.Address, vid, pid, d.VID, d.PID)
	}
	if opt.DetachDriver != "" {
		dc := disconnectClaim{iface: uint32(ifnum), flags: disconnectClaimIfDriver}
		copy(dc.driver[:len(dc.driver)-1], opt.DetachDriver)
		bound := ""
		if i, ok := d.Iface(ifnum); ok {
			bound = i.Driver
		}
		if err := p.ioctl(ioctlDisconnectClaim, unsafe.Pointer(&dc)); err != nil {
			return nil, claimErr(ifnum, opt.DetachDriver, err)
		}
		if bound == opt.DetachDriver {
			p.detached = bound
		}
	} else {
		iface := uint32(ifnum)
		if err := p.ioctl(ioctlClaim, unsafe.Pointer(&iface)); err != nil {
			return nil, claimErr(ifnum, "", err)
		}
	}
	p.claimed = true
	ok = true
	return p, nil
}

func claimErr(ifnum int, driver string, err error) error {
	if errors.Is(err, syscall.EBUSY) {
		if driver != "" {
			return fmt.Errorf("%w: interface %d is bound to a driver other than %s", ErrBusy, ifnum, driver)
		}
		return fmt.Errorf("%w: interface %d is claimed by a kernel driver or another process", ErrBusy, ifnum)
	}
	if errors.Is(err, syscall.ENODEV) {
		return fmt.Errorf("%w: %w", ErrDisconnected, err)
	}
	return fmt.Errorf("usb: cannot claim interface %d: %w", ifnum, err)
}

func (p *port) ioctl(req uintptr, arg unsafe.Pointer) error {
	_, err := p.ioctlN(req, arg)
	return err
}

func (p *port) ioctlN(req uintptr, arg unsafe.Pointer) (int, error) {
	for {
		n, _, e := unix.Syscall(unix.SYS_IOCTL, p.f.Fd(), req, uintptr(arg))
		if e == unix.EINTR {
			continue
		}
		if e != 0 {
			return 0, e
		}
		return int(n), nil
	}
}

func millis(d time.Duration) uint32 {
	ms := (d + time.Millisecond - 1) / time.Millisecond
	if ms < 1 {
		ms = 1
	}
	return uint32(ms)
}

// mapErr turns an errno into the package's sentinel errors, additionally
// recognising an unplug that surfaced as a low-level bus error.
func (p *port) mapErr(what string, err error) error {
	if errors.Is(err, syscall.EPROTO) || errors.Is(err, syscall.EILSEQ) || errors.Is(err, syscall.EIO) || errors.Is(err, syscall.ETIME) {
		if p.vanished() {
			return fmt.Errorf("%w: %s: %w (the device left the bus)", ErrDisconnected, what, err)
		}
	}
	return mapErr(what, err)
}

// vanished reports whether the device behind this handle is gone. While a
// transfer is in flight, an unplug is often reported by the host controller
// as EPROTO/EILSEQ/EIO/ETIME before usbfs starts returning ENODEV, so the
// handle is probed for a short, bounded time: a healthy device answers the
// probe at once.
func (p *port) vanished() bool {
	deadline := time.Now().Add(750 * time.Millisecond)
	for {
		if p.f != nil {
			var caps uint32
			if err := p.ioctl(ioctlCapabilities, unsafe.Pointer(&caps)); err != nil {
				if errors.Is(err, syscall.ENODEV) || errors.Is(err, syscall.ESHUTDOWN) || errors.Is(err, syscall.ENOENT) {
					return true
				}
				return false
			}
			if _, err := os.Stat(p.f.Name()); err != nil {
				return true
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func mapErr(what string, err error) error {
	switch {
	case errors.Is(err, syscall.ETIMEDOUT):
		return fmt.Errorf("%w: %s", ErrTimeout, what)
	case errors.Is(err, syscall.EPIPE):
		return fmt.Errorf("%w: %s", ErrStall, what)
	case errors.Is(err, syscall.ENODEV), errors.Is(err, syscall.ESHUTDOWN), errors.Is(err, syscall.ENOENT):
		return fmt.Errorf("%w: %s: %w", ErrDisconnected, what, err)
	}
	return fmt.Errorf("usb: %s: %w", what, err)
}

func (p *port) open() error {
	if p.f == nil {
		return ErrClosed
	}
	return nil
}

func (p *port) ControlIn(ctx context.Context, requestType, request uint8, value, index uint16, length int) ([]byte, error) {
	buf := make([]byte, length)
	n, err := p.control(ctx, requestType|0x80, request, value, index, buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

func (p *port) ControlOut(ctx context.Context, requestType, request uint8, value, index uint16, data []byte) error {
	_, err := p.control(ctx, requestType&^0x80, request, value, index, append([]byte(nil), data...))
	return err
}

func (p *port) control(ctx context.Context, requestType, request uint8, value, index uint16, buf []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.open(); err != nil {
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if len(buf) > 0xFFFF {
		return 0, errors.New("usb: control payload too large")
	}
	c := ctrlTransfer{requestType: requestType, request: request, value: value, index: index, length: uint16(len(buf)), timeout: 1000}
	if len(buf) > 0 {
		c.data = unsafe.Pointer(&buf[0])
	}
	n, err := p.ioctlN(ioctlControl, unsafe.Pointer(&c))
	if err != nil {
		return 0, p.mapErr(fmt.Sprintf("control request 0x%02x", request), err)
	}
	return n, nil
}

func (p *port) BulkOut(ctx context.Context, endpoint uint8, data []byte, timeout time.Duration) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.open(); err != nil {
		return 0, err
	}
	if len(data) == 0 {
		return 0, nil
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	buf := append([]byte(nil), data...)
	b := bulkTransfer{endpoint: uint32(endpoint &^ 0x80), length: uint32(len(buf)), timeout: millis(timeout), data: unsafe.Pointer(&buf[0])}
	n, err := p.ioctlN(ioctlBulk, unsafe.Pointer(&b))
	if err != nil {
		return 0, p.mapErr(fmt.Sprintf("bulk OUT 0x%02x", endpoint), err)
	}
	return n, nil
}

func (p *port) BulkIn(ctx context.Context, endpoint uint8, length int, timeout time.Duration) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.open(); err != nil {
		return nil, err
	}
	if length <= 0 {
		return nil, nil
	}
	deadline := time.Now().Add(timeout)
	buf := make([]byte, length)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		wait := time.Until(deadline)
		if wait <= 0 {
			return nil, fmt.Errorf("%w: bulk IN 0x%02x", ErrTimeout, endpoint)
		}
		if wait > slice {
			wait = slice
		}
		b := bulkTransfer{endpoint: uint32(endpoint | 0x80), length: uint32(len(buf)), timeout: millis(wait), data: unsafe.Pointer(&buf[0])}
		n, err := p.ioctlN(ioctlBulk, unsafe.Pointer(&b))
		if err == nil {
			return buf[:n], nil
		}
		if errors.Is(err, syscall.ETIMEDOUT) {
			continue
		}
		return nil, p.mapErr(fmt.Sprintf("bulk IN 0x%02x", endpoint), err)
	}
}

// ClearHalt clears a halted endpoint (usbfs CLEAR_HALT), which also resets
// the host-side data toggle.
func (p *port) ClearHalt(endpoint uint8) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.open(); err != nil {
		return err
	}
	ep := uint32(endpoint)
	if err := p.ioctl(ioctlClearHalt, unsafe.Pointer(&ep)); err != nil {
		return p.mapErr(fmt.Sprintf("clear halt 0x%02x", endpoint), err)
	}
	return nil
}

// ResetDevice performs a USB port reset of the whole device.
func (p *port) ResetDevice() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.open(); err != nil {
		return err
	}
	if err := p.ioctl(ioctlReset, nil); err != nil {
		return p.mapErr("device reset", err)
	}
	return nil
}

func (p *port) Describe() string {
	return fmt.Sprintf("usbfs %04x:%04x bus %d dev %d interface %d", p.dev.VID, p.dev.PID, p.dev.Bus, p.dev.Address, p.iface)
}

func (p *port) DetachedDriver() string { return p.detached }

func (p *port) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.f == nil {
		return nil
	}
	if p.claimed {
		iface := uint32(p.iface)
		_ = p.ioctl(ioctlRelease, unsafe.Pointer(&iface))
		p.claimed = false
		if p.detached != "" {
			// Ask the kernel to bind a driver to this interface again.
			c := usbfsIoctl{ifno: int32(p.iface), code: usbdevfsConnectCode}
			_ = p.ioctl(ioctlIoctl, unsafe.Pointer(&c))
		}
	}
	err := p.f.Close()
	p.f = nil
	return err
}
