//go:build darwin

package usb

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
	"unsafe"
)

// pipe is one endpoint of the opened interface.
type pipe struct {
	ref       uint8
	typ       uint8 // 0 control, 1 isoc, 2 bulk, 3 interrupt
	maxPacket int
}

const (
	pipeBulk      = 2
	pipeInterrupt = 3
)

// iokitPort is an interface opened through IOUSBInterfaceInterface (version
// 300 or later, see interfaceIIDs).
type iokitPort struct {
	mu    sync.Mutex
	intf  comObj
	io    *ioThread // runs the blocking calls; nil once closed
	open  bool
	dev   Device
	iface int
	ver   int            // IOUSBInterfaceInterface version in use
	pipes map[uint8]pipe // by endpoint address
}

// Open opens interface ifnum of d through IOKit (USBInterfaceOpen). Only that
// interface is opened: the device is not opened, its configuration is not
// changed and the kernel drivers of the other interfaces (CDC-ACM, mass
// storage, HID) stay attached. macOS cannot detach a kernel driver from a
// user process, so an interface a driver holds reports ErrBusy.
func Open(d Device, ifnum int, opt OpenOptions) (Port, error) {
	if _, ok := d.Iface(ifnum); !ok {
		return nil, fmt.Errorf("usb: device %04x:%04x has no interface %d", d.VID, d.PID, ifnum)
	}
	if err := loadIOKit(); err != nil {
		return nil, err
	}
	dev, err := findDevice(d)
	if err != nil {
		return nil, err
	}
	defer ioObjectRelease(dev)
	ifsvc := findInterface(dev, ifnum)
	if ifsvc == 0 {
		return nil, fmt.Errorf("usb: device %04x:%04x has no configured interface %d in the IORegistry", d.VID, d.PID, ifnum)
	}
	defer ioObjectRelease(ifsvc)
	// A kernel driver that is bound but idle (AppleUSBACMData while its tty
	// is closed) does not hold the interface open, so USBInterfaceOpen would
	// succeed and take the interface from under it. Refuse, as usbfs refuses
	// a claim of an interface with a bound driver.
	if owner := interfaceDriver(ifsvc); owner != "" {
		return nil, busyErr(ifnum, owner, opt)
	}
	intf, idx, err := plugin(ifsvc, cfuuidInterfaceUserClient, iidInterfaces...)
	if err != nil {
		if serviceByID(registryID(dev)) == 0 {
			return nil, fmt.Errorf("%w: %w", ErrDisconnected, err)
		}
		return nil, fmt.Errorf("usb: cannot create the IOKit user client for interface %d: %w", ifnum, err)
	}
	if kr := intf.call(slotIfOpen); kr != kIOReturnSuccess {
		intf.release()
		return nil, openErr(d, ifnum, opt, kr)
	}
	p := &iokitPort{intf: intf, io: newIOThread(), open: true, dev: d, iface: ifnum, ver: interfaceIIDs[idx].version, pipes: map[uint8]pipe{}}
	if d.RegistryID == 0 {
		p.dev.RegistryID = registryID(dev)
	}
	var n uint8
	if kr := intf.call(slotIfGetNumEndpoints, uintptr(unsafe.Pointer(&n))); kr != kIOReturnSuccess {
		_ = p.Close()
		return nil, mapIOReturn("count endpoints", kr)
	}
	for ref := uint8(1); ref <= n; ref++ {
		var dir, num, typ, interval uint8
		var mps uint16
		kr := intf.call(slotIfGetPipeProperties, uintptr(ref), uintptr(unsafe.Pointer(&dir)), uintptr(unsafe.Pointer(&num)),
			uintptr(unsafe.Pointer(&typ)), uintptr(unsafe.Pointer(&mps)), uintptr(unsafe.Pointer(&interval)))
		if kr != kIOReturnSuccess {
			continue
		}
		addr := num & 0x0F
		if dir == 1 { // kUSBIn
			addr |= 0x80
		}
		p.pipes[addr] = pipe{ref: ref, typ: typ, maxPacket: int(mps)}
	}
	return p, nil
}

func openErr(d Device, ifnum int, opt OpenOptions, kr uint32) error {
	switch kr {
	case kIOReturnExclusiveAccess, kIOReturnBusy:
		return fmt.Errorf("%w (%w)", busyErr(ifnum, "", opt), ioReturnError(kr))
	case kIOReturnNotPrivileged, kIOReturnNotPermitted:
		return fmt.Errorf("%w: interface %d of %04x:%04x: %w", ErrPermission, ifnum, d.VID, d.PID, ioReturnError(kr))
	case kIOReturnNoDevice, kIOReturnOffline:
		return fmt.Errorf("%w: %w", ErrDisconnected, ioReturnError(kr))
	}
	return fmt.Errorf("usb: cannot open interface %d: %w", ifnum, ioReturnError(kr))
}

// busyErr reports interface ifnum as held by owner: a kernel driver, or "pid
// N, name" for a process that opened it. macOS has no user-space equivalent
// of usbfs DISCONNECT, so OpenOptions.DetachDriver cannot be honoured.
func busyErr(ifnum int, owner string, opt OpenOptions) error {
	who := "another process or a kernel driver"
	if owner != "" {
		who = owner
	}
	note := ""
	if opt.DetachDriver != "" {
		note = "; macOS cannot detach a kernel driver from a user process"
	}
	return fmt.Errorf("%w: interface %d is held by %s%s", ErrBusy, ifnum, who, note)
}

// findDevice returns the live IOUSBHostDevice for d: the registry entry the
// scan saw when RegistryID is set, else the one at d.Bus/d.Address. A device
// that re-enumerated has a new entry and is reported as disconnected, as a
// changed bus address is on Linux.
func findDevice(d Device) (uint32, error) {
	if d.RegistryID != 0 {
		svc := serviceByID(d.RegistryID)
		if svc == 0 {
			return 0, fmt.Errorf("%w: device %04x:%04x (IORegistry entry 0x%x) is gone", ErrDisconnected, d.VID, d.PID, d.RegistryID)
		}
		vid, _ := numProp(svc, "idVendor")
		pid, _ := numProp(svc, "idProduct")
		if uint16(vid) != d.VID || uint16(pid) != d.PID {
			ioObjectRelease(svc)
			return 0, fmt.Errorf("usb: IORegistry entry 0x%x is %04x:%04x, expected %04x:%04x", d.RegistryID, vid, pid, d.VID, d.PID)
		}
		return svc, nil
	}
	if d.Bus <= 0 || d.Address <= 0 {
		return 0, fmt.Errorf("usb: invalid device address %d:%d", d.Bus, d.Address)
	}
	devs := services(ioServiceMatching("IOUSBHostDevice"))
	var hit uint32
	for _, svc := range devs {
		vid, _ := numProp(svc, "idVendor")
		pid, _ := numProp(svc, "idProduct")
		loc, _ := numProp(svc, "locationID")
		addr, _ := numProp(svc, "USB Address", "kUSBAddress")
		if hit == 0 && uint16(vid) == d.VID && uint16(pid) == d.PID && int(uint32(loc)>>24) == d.Bus && int(addr) == d.Address {
			hit = svc
			continue
		}
		ioObjectRelease(svc)
	}
	if hit == 0 {
		return 0, fmt.Errorf("%w: no %04x:%04x at bus %d address %d", ErrDisconnected, d.VID, d.PID, d.Bus, d.Address)
	}
	return hit, nil
}

func findInterface(dev uint32, ifnum int) uint32 {
	kids := children(dev)
	var hit uint32
	for _, k := range kids {
		if hit == 0 && ioObjectConformsTo(k, "IOUSBHostInterface") != 0 {
			if n, ok := numProp(k, "bInterfaceNumber"); ok && int(n) == ifnum {
				hit = k
				continue
			}
		}
		ioObjectRelease(k)
	}
	return hit
}

func (p *iokitPort) checkOpen() error {
	if !p.open {
		return ErrClosed
	}
	return nil
}

// mapErr maps a transfer status, recognising an unplug that surfaced as an
// abort or a bus error before the registry entry was terminated.
func (p *iokitPort) mapErr(what string, kr uint32) error {
	err := mapIOReturn(what, kr)
	if mayBeUnplug(err) && !errors.Is(err, ErrDisconnected) && p.vanished() {
		return fmt.Errorf("%w: %s: %w (the device left the bus)", ErrDisconnected, what, ioReturnError(kr))
	}
	return err
}

// vanished reports whether the device's registry entry is gone, polling for a
// short bounded time because termination trails the failed transfer.
func (p *iokitPort) vanished() bool {
	deadline := time.Now().Add(750 * time.Millisecond)
	for {
		svc := serviceByID(p.dev.RegistryID)
		if svc == 0 {
			return true
		}
		ioObjectRelease(svc)
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func (p *iokitPort) pipe(endpoint uint8) (pipe, error) {
	pp, ok := p.pipes[endpoint]
	if !ok {
		return pipe{}, fmt.Errorf("usb: interface %d has no endpoint 0x%02x", p.iface, endpoint)
	}
	return pp, nil
}

// devRequestTO is IOUSBDevRequestTO (32 bytes on 64-bit macOS).
type devRequestTO struct {
	bmRequestType     uint8
	bRequest          uint8
	wValue            uint16
	wIndex            uint16
	wLength           uint16
	pData             unsafe.Pointer
	wLenDone          uint32
	noDataTimeout     uint32
	completionTimeout uint32
	_                 uint32
}

func (p *iokitPort) ControlIn(ctx context.Context, requestType, request uint8, value, index uint16, length int) ([]byte, error) {
	buf := make([]byte, length)
	n, err := p.control(ctx, requestType|0x80, request, value, index, buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

func (p *iokitPort) ControlOut(ctx context.Context, requestType, request uint8, value, index uint16, data []byte) error {
	_, err := p.control(ctx, requestType&^0x80, request, value, index, append([]byte(nil), data...))
	return err
}

func (p *iokitPort) control(ctx context.Context, requestType, request uint8, value, index uint16, buf []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.checkOpen(); err != nil {
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if len(buf) > 0xFFFF {
		return 0, errors.New("usb: control payload too large")
	}
	req := &devRequestTO{bmRequestType: requestType, bRequest: request, wValue: value, wIndex: index,
		wLength: uint16(len(buf)), noDataTimeout: 1000, completionTimeout: 1000}
	if len(buf) > 0 {
		req.pData = unsafe.Pointer(&buf[0])
	}
	kr := p.io.do(func() uint32 {
		return p.intf.call(slotIfControlRequestTO, 0, uintptr(unsafe.Pointer(req)))
	})
	if kr != kIOReturnSuccess {
		return 0, p.mapErr(fmt.Sprintf("control request 0x%02x", request), kr)
	}
	return int(req.wLenDone), nil
}

func (p *iokitPort) BulkOut(ctx context.Context, endpoint uint8, data []byte, timeout time.Duration) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.checkOpen(); err != nil {
		return 0, err
	}
	if len(data) == 0 {
		return 0, nil
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	pp, err := p.pipe(endpoint &^ 0x80)
	if err != nil {
		return 0, err
	}
	buf := append([]byte(nil), data...)
	what := fmt.Sprintf("bulk OUT 0x%02x", endpoint)
	if pp.typ == pipeInterrupt {
		// Timeouts do not apply to interrupt pipes: abort on the deadline.
		kr, expired := p.withAbort(ctx, pp, timeout, func() uint32 {
			return p.intf.call(slotIfWritePipe, uintptr(pp.ref), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
		})
		if kr != kIOReturnSuccess {
			return 0, p.abortErr(ctx, what, kr, expired)
		}
		return len(buf), nil
	}
	ms := millis(timeout)
	kr, _ := p.withAbort(ctx, pp, 0, func() uint32 {
		return p.intf.call(slotIfWritePipeTO, uintptr(pp.ref), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), uintptr(ms), uintptr(ms))
	})
	if kr != kIOReturnSuccess {
		return 0, p.abortErr(ctx, what, kr, false)
	}
	return len(buf), nil
}

func (p *iokitPort) BulkIn(ctx context.Context, endpoint uint8, length int, timeout time.Duration) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.checkOpen(); err != nil {
		return nil, err
	}
	if length <= 0 {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pp, err := p.pipe(endpoint | 0x80)
	if err != nil {
		return nil, err
	}
	what := fmt.Sprintf("bulk IN 0x%02x", endpoint)
	buf := make([]byte, length)
	if pp.typ == pipeInterrupt {
		size := uint32(len(buf))
		kr, expired := p.withAbort(ctx, pp, timeout, func() uint32 {
			return p.intf.call(slotIfReadPipe, uintptr(pp.ref), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)))
		})
		if kr != kIOReturnSuccess {
			return nil, p.abortErr(ctx, what, kr, expired)
		}
		return buf[:size], nil
	}
	// One transfer for the whole timeout. The host stack applies only the
	// completion timeout (measured on this Mac: a 100 ms no-data timeout with
	// a 5 s completion timeout waited 5 s), so slicing the wait as the Linux
	// transport does would abort replies that are still arriving and lose
	// them. A cancelled ctx aborts the pipe instead.
	ms := millis(timeout)
	size := uint32(len(buf))
	kr, _ := p.withAbort(ctx, pp, 0, func() uint32 {
		return p.intf.call(slotIfReadPipeTO, uintptr(pp.ref), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)),
			uintptr(ms), uintptr(ms))
	})
	if kr != kIOReturnSuccess {
		return nil, p.abortErr(ctx, what, kr, false)
	}
	return buf[:size], nil
}

// withAbort runs a blocking pipe call and aborts the pipe when ctx is
// cancelled or, if timeout > 0, when it expires (interrupt pipes have no
// timeout variant). expired reports that the timeout caused the abort. The call runs on
// p.io; the abort is issued from another thread while it blocks.
func (p *iokitPort) withAbort(ctx context.Context, pp pipe, timeout time.Duration, call func() uint32) (kr uint32, expired bool) {
	if timeout <= 0 && ctx.Done() == nil {
		return p.io.do(call), false
	}
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		var timer <-chan time.Time
		if timeout > 0 {
			t := time.NewTimer(timeout)
			defer t.Stop()
			timer = t.C
		}
		select {
		case <-done:
			return
		case <-timer:
			expired = true
		case <-ctx.Done():
		}
		p.intf.call(slotIfAbortPipe, uintptr(pp.ref))
	}()
	kr = p.io.do(call)
	close(done)
	<-finished
	return kr, expired
}

func (p *iokitPort) abortErr(ctx context.Context, what string, kr uint32, expired bool) error {
	if kr == kIOReturnAborted || kr == kIOUSBTransactionReturned {
		if err := ctx.Err(); err != nil {
			return err
		}
		if expired || !p.vanished() {
			return fmt.Errorf("%w: %s", ErrTimeout, what)
		}
		return fmt.Errorf("%w: %s: %w", ErrDisconnected, what, ioReturnError(kr))
	}
	return p.mapErr(what, kr)
}

func millis(d time.Duration) uint32 {
	ms := (d + time.Millisecond - 1) / time.Millisecond
	if ms < 1 {
		ms = 1
	}
	return uint32(ms)
}

// ClearHalt clears a halted endpoint on both ends (host pipe and a
// CLEAR_FEATURE(ENDPOINT_HALT) to the device), which also resets the data
// toggle, like usbfs CLEAR_HALT.
func (p *iokitPort) ClearHalt(endpoint uint8) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.checkOpen(); err != nil {
		return err
	}
	pp, err := p.pipe(endpoint)
	if err != nil {
		return err
	}
	if kr := p.intf.call(slotIfClearPipeStallBothEnds, uintptr(pp.ref)); kr != kIOReturnSuccess {
		return p.mapErr(fmt.Sprintf("clear halt 0x%02x", endpoint), kr)
	}
	return nil
}

func (p *iokitPort) Describe() string {
	return fmt.Sprintf("IOKit %04x:%04x location 0x%08x address %d interface %d (IOUSBInterfaceInterface%d)", p.dev.VID, p.dev.PID, p.dev.LocationID, p.dev.Address, p.iface, p.ver)
}

func (p *iokitPort) DetachedDriver() string { return "" }

func (p *iokitPort) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.intf.p == nil {
		return nil
	}
	var err error
	if p.open {
		if kr := p.intf.call(slotIfClose); kr != kIOReturnSuccess && kr != kIOReturnNoDevice && kr != kIOReturnNotOpen {
			err = mapIOReturn("close interface", kr)
		}
		p.open = false
	}
	p.intf.release()
	p.intf = comObj{}
	if p.io != nil {
		p.io.stop()
		p.io = nil
	}
	return err
}
