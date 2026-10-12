//go:build linux

package native

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Supported reports whether this build has a native USB transport.
const Supported = true

// usbfs ioctl numbers (linux/usbdevice_fs.h), built from the generic
// _IOC encoding so the struct sizes follow the host pointer size.
const (
	iocWrite = 1
	iocRead  = 2
)

func ioc(dir, nr, size uintptr) uintptr {
	return dir<<30 | size<<16 | 'U'<<8 | nr
}

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

var (
	ioctlControl   = ioc(iocRead|iocWrite, 0, unsafe.Sizeof(ctrlTransfer{}))
	ioctlBulk      = ioc(iocRead|iocWrite, 2, unsafe.Sizeof(bulkTransfer{}))
	ioctlClaim     = ioc(iocRead, 15, unsafe.Sizeof(uint32(0)))
	ioctlRelease   = ioc(iocRead, 16, unsafe.Sizeof(uint32(0)))
	usbfsDeviceFmt = "/dev/bus/usb/%03d/%03d"
)

// usbfsTransport talks to one device through /dev/bus/usb without libusb.
type usbfsTransport struct {
	f        *os.File
	revision uint16
	claimed  bool
}

// Open opens the CC-Debugger at addr through usbfs and claims interface 0. It
// verifies the device identity and that configuration 1 is already active; it
// never changes the configuration or detaches a kernel driver.
func Open(addr Address) (_ Transport, openErr error) {
	if !addr.Valid() {
		return nil, invalidf("invalid USB address %d:%d", addr.Bus, addr.Address)
	}
	f, err := os.OpenFile(fmt.Sprintf(usbfsDeviceFmt, addr.Bus, addr.Address), os.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return nil, transportf("%v (grant access to the device, e.g. with a udev rule for 0451:16a2)", err)
		}
		return nil, transportf("%v", err)
	}
	t := &usbfsTransport{f: f}
	ok := false
	defer func() {
		if !ok {
			if err := t.Close(); err != nil {
				openErr = errors.Join(openErr, fmt.Errorf("close USB transport after failed open: %w", err))
			}
		}
	}()
	// Reading the usbfs node returns the device descriptor first.
	var desc [18]byte
	if n, err := f.Read(desc[:]); err != nil || n != len(desc) {
		return nil, transportf("cannot read the USB device descriptor: %v", err)
	}
	if vid, pid := uint16(desc[8])|uint16(desc[9])<<8, uint16(desc[10])|uint16(desc[11])<<8; vid != VID || pid != PID {
		return nil, transportf("USB device %d:%d is %04x:%04x, not a CC-Debugger %04x:%04x", addr.Bus, addr.Address, vid, pid, VID, PID)
	}
	t.revision = uint16(desc[12]) | uint16(desc[13])<<8
	cfg, err := t.ControlRead(0x80, 0x08, 0, 0, 1, time.Second)
	if err != nil {
		return nil, err
	}
	if len(cfg) != 1 || cfg[0] != 1 {
		return nil, transportf("USB configuration 1 is not active; refusing to change it")
	}
	iface := uint32(0)
	if err := t.ioctl(ioctlClaim, unsafe.Pointer(&iface)); err != nil {
		if errors.Is(err, syscall.EBUSY) {
			return nil, transportf("USB interface 0 is in use by another driver or process (cc-tool?); refusing to detach it")
		}
		return nil, transportf("cannot claim USB interface 0: %v", err)
	}
	t.claimed = true
	ok = true
	return t, nil
}

func (t *usbfsTransport) ioctl(req uintptr, arg unsafe.Pointer) error {
	for {
		_, _, e := unix.Syscall(unix.SYS_IOCTL, t.f.Fd(), req, uintptr(arg))
		if e == unix.EINTR {
			continue
		}
		if e != 0 {
			return e
		}
		return nil
	}
}

func (t *usbfsTransport) Revision() (uint16, error) { return t.revision, nil }

// millis converts a timeout for usbfs, where 0 means infinite.
func millis(d time.Duration) uint32 {
	ms := (d + time.Millisecond - 1) / time.Millisecond
	if ms < 1 {
		ms = 1
	}
	return uint32(ms)
}

func (t *usbfsTransport) ControlRead(requestType, request uint8, value, index uint16, length int, timeout time.Duration) ([]byte, error) {
	buf := make([]byte, length)
	n, err := t.control(requestType, request, value, index, buf, timeout)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

func (t *usbfsTransport) ControlWrite(requestType, request uint8, value, index uint16, data []byte, timeout time.Duration) (int, error) {
	return t.control(requestType, request, value, index, data, timeout)
}

func (t *usbfsTransport) control(requestType, request uint8, value, index uint16, buf []byte, timeout time.Duration) (int, error) {
	if len(buf) > 0xFFFF {
		return 0, invalidf("USB control payload too large")
	}
	c := ctrlTransfer{requestType: requestType, request: request, value: value, index: index,
		length: uint16(len(buf)), timeout: millis(timeout)}
	if len(buf) > 0 {
		c.data = unsafe.Pointer(&buf[0])
	}
	n, err := t.transfer(ioctlControl, unsafe.Pointer(&c))
	if err != nil {
		return 0, transportf("USB control request 0x%02x failed: %v", request, err)
	}
	return n, nil
}

func (t *usbfsTransport) BulkWrite(endpoint uint8, data []byte, timeout time.Duration) (int, error) {
	return t.bulk(endpoint, data, timeout)
}

func (t *usbfsTransport) BulkRead(endpoint uint8, length int, timeout time.Duration) ([]byte, error) {
	buf := make([]byte, length)
	n, err := t.bulk(endpoint, buf, timeout)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

func (t *usbfsTransport) bulk(endpoint uint8, buf []byte, timeout time.Duration) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}
	b := bulkTransfer{endpoint: uint32(endpoint), length: uint32(len(buf)), timeout: millis(timeout), data: unsafe.Pointer(&buf[0])}
	n, err := t.transfer(ioctlBulk, unsafe.Pointer(&b))
	if err != nil {
		return 0, transportf("USB bulk transfer on 0x%02x failed: %v", endpoint, err)
	}
	return n, nil
}

// transfer returns the byte count of a usbfs control or bulk ioctl. The kernel
// does not retry; EINTR is the only error treated as transient, and only
// before any data moved.
func (t *usbfsTransport) transfer(req uintptr, arg unsafe.Pointer) (int, error) {
	for {
		n, _, e := unix.Syscall(unix.SYS_IOCTL, t.f.Fd(), req, uintptr(arg))
		if e == unix.EINTR {
			continue
		}
		if e != 0 {
			return 0, e
		}
		return int(n), nil
	}
}

func (t *usbfsTransport) Close() error {
	if t.f == nil {
		return nil
	}
	var releaseErr error
	if t.claimed {
		iface := uint32(0)
		if err := t.ioctl(ioctlRelease, unsafe.Pointer(&iface)); err != nil {
			releaseErr = fmt.Errorf("release USB interface: %w", err)
		}
		t.claimed = false
	}
	err := t.f.Close()
	t.f = nil
	return errors.Join(releaseErr, err)
}
