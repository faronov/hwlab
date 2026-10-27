//go:build darwin

package usb

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// The macOS backend reaches IOKit and CoreFoundation through purego: the
// frameworks are dlopened at first use and called without cgo, so
// CGO_ENABLED=0 builds keep working (ADR 0035).

const (
	iokitPath = "/System/Library/Frameworks/IOKit.framework/IOKit"
	cfPath    = "/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation"
	sysPath   = "/usr/lib/libSystem.B.dylib"

	kIOMainPortDefault            = 0
	kIORegistryIterateRecursively = 1
	kCFStringEncodingUTF8         = 0x08000100
	kCFNumberSInt64Type           = 4
	kIOServicePlane               = "IOService"
)

// UUIDs from IOKit/usb/IOUSBLib.h and IOKit/IOCFPlugIn.h.
const (
	uuidUSBDeviceUserClientType    = "9DC7B780-9EC0-11D4-A54F-000A27052861"
	uuidUSBInterfaceUserClientType = "2D9786C6-9EF3-11D4-AD51-000A27052861"
	uuidCFPlugInInterface          = "C244E858-109C-11D4-91D4-0050E4C6426F"
	uuidUSBDeviceInterface100      = "5C8187D0-9EF3-11D4-8B45-000A27052861"
)

// interfaceIIDs are the IOUSBInterfaceInterface versions Open accepts, newest
// first. The vtable slots used here are the same in all of them. Versions up to
// 245 are deliberately absent: libusb's darwin backend documents that for those
// the OS clears the host-side data toggle when a pipe is aborted (a timed-out
// transfer ends in an abort), while the device keeps its toggle, so the next
// packet in that direction is dropped. (The J-Link bootloader falls first
// seen with version 190 came from SIGURG, see ioThread, and also happened
// with 942.)
var interfaceIIDs = []struct {
	version int
	uuid    string
}{
	{942, "8752663B-C07B-4BAE-9584-22032FAB9C5A"},
	{800, "33A85DB0-0C3B-4328-8F02-FDA81B117F4C"},
	{700, "17F9E59C-B0A1-401D-9AC0-8DE27AC6047E"},
	{650, "08151A89-8081-4087-8F9E-0AFEDFDB5D9F"},
	{550, "6AE44D3F-EB45-487F-8E8E-B93B99F8EA9E"},
	{500, "6C0D38C3-B093-4EA7-809B-09FB5DDDAC16"},
	{300, "BCEAADDC-884D-4F27-8340-36D69FAB90F6"},
}

// Vtable slots (in pointers) of the COM-style interfaces, checked against the
// macOS 15.5 SDK headers with offsetof. Slot 0 is the reserved pointer, 1-3
// are QueryInterface, AddRef and Release.
const (
	slotQueryInterface = 1
	slotRelease        = 3

	// IOUSBDeviceInterface
	slotDevGetConfigurationDescriptorPtr = 21

	// IOUSBInterfaceInterface300 through 942 (same slots in each)
	slotIfOpen                   = 8
	slotIfClose                  = 9
	slotIfGetNumEndpoints        = 19
	slotIfGetPipeProperties      = 26
	slotIfAbortPipe              = 28
	slotIfReadPipe               = 31
	slotIfWritePipe              = 32
	slotIfControlRequestTO       = 37
	slotIfReadPipeTO             = 39
	slotIfWritePipeTO            = 40
	slotIfClearPipeStallBothEnds = 45
)

// uuidBytes parses a canonical UUID string into its 16 bytes.
func uuidBytes(s string) ([16]byte, error) {
	var u [16]byte
	h := strings.ReplaceAll(s, "-", "")
	if len(h) != 32 {
		return u, fmt.Errorf("bad UUID %q", s)
	}
	if _, err := hex.Decode(u[:], []byte(h)); err != nil {
		return u, fmt.Errorf("bad UUID %q: %w", s, err)
	}
	return u, nil
}

// uuidRegs splits a CFUUIDBytes value into the two integer registers it is
// passed in by value (16-byte aggregate: rsi/rdx on amd64, x1/x2 on arm64).
func uuidRegs(u [16]byte) (lo, hi uintptr) {
	return uintptr(binary.LittleEndian.Uint64(u[:8])), uintptr(binary.LittleEndian.Uint64(u[8:]))
}

var (
	iokitOnce sync.Once
	iokitErr  error

	ioServiceMatching                 func(name string) uintptr
	ioServiceGetMatchingServices      func(mainPort uint32, matching uintptr, it *uint32) int32
	ioServiceGetMatchingService       func(mainPort uint32, matching uintptr) uint32
	ioRegistryEntryIDMatching         func(id uint64) uintptr
	ioIteratorNext                    func(it uint32) uint32
	ioObjectRelease                   func(o uint32) int32
	ioObjectConformsTo                func(o uint32, class string) uint32
	ioObjectGetClass                  func(o uint32, name *byte) int32
	ioRegistryEntryGetChildIterator   func(e uint32, plane string, it *uint32) int32
	ioRegistryEntryGetRegistryEntryID func(e uint32, id *uint64) int32
	ioRegistryEntryCreateCFProperty   func(e uint32, key uintptr, alloc uintptr, opts uint32) uintptr
	ioRegistryEntrySearchCFProperty   func(e uint32, plane string, key uintptr, alloc uintptr, opts uint32) uintptr
	ioCreatePlugInInterfaceForService func(svc uint32, pluginType, ifaceType uintptr, out *unsafe.Pointer, score *int32) int32

	cfRelease                         func(cf uintptr)
	cfGetTypeID                       func(cf uintptr) uint
	cfStringGetTypeID                 func() uint
	cfNumberGetTypeID                 func() uint
	cfStringCreateWithCString         func(alloc uintptr, s string, enc uint32) uintptr
	cfStringGetLength                 func(s uintptr) int
	cfStringGetMaximumSizeForEncoding func(n int, enc uint32) int
	cfStringGetCString                func(s uintptr, buf *byte, size int, enc uint32) uint8
	cfNumberGetValue                  func(n uintptr, typ int, out *int64) uint8
	cfUUIDCreateFromString            func(alloc uintptr, s uintptr) uintptr

	pthreadSigmask func(how int32, set, old *uint32) int32

	// CFUUIDRefs, created once and never released.
	cfuuidDeviceUserClient, cfuuidInterfaceUserClient, cfuuidPlugIn uintptr
	iidDevice100                                                    [16]byte
	iidInterfaces                                                   [][16]byte
)

// loadIOKit binds the IOKit and CoreFoundation symbols once.
func loadIOKit() error {
	iokitOnce.Do(func() { iokitErr = bindIOKit() })
	return iokitErr
}

func bindIOKit() (err error) {
	defer func() {
		// RegisterLibFunc panics on a missing symbol.
		if r := recover(); r != nil {
			err = fmt.Errorf("usb: IOKit binding failed: %v", r)
		}
	}()
	iokit, err := purego.Dlopen(iokitPath, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return fmt.Errorf("usb: cannot load IOKit: %w", err)
	}
	cf, err := purego.Dlopen(cfPath, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return fmt.Errorf("usb: cannot load CoreFoundation: %w", err)
	}
	for name, fn := range map[string]any{
		"IOServiceMatching":                 &ioServiceMatching,
		"IOServiceGetMatchingServices":      &ioServiceGetMatchingServices,
		"IOServiceGetMatchingService":       &ioServiceGetMatchingService,
		"IORegistryEntryIDMatching":         &ioRegistryEntryIDMatching,
		"IOIteratorNext":                    &ioIteratorNext,
		"IOObjectRelease":                   &ioObjectRelease,
		"IOObjectConformsTo":                &ioObjectConformsTo,
		"IOObjectGetClass":                  &ioObjectGetClass,
		"IORegistryEntryGetChildIterator":   &ioRegistryEntryGetChildIterator,
		"IORegistryEntryGetRegistryEntryID": &ioRegistryEntryGetRegistryEntryID,
		"IORegistryEntryCreateCFProperty":   &ioRegistryEntryCreateCFProperty,
		"IORegistryEntrySearchCFProperty":   &ioRegistryEntrySearchCFProperty,
		"IOCreatePlugInInterfaceForService": &ioCreatePlugInInterfaceForService,
	} {
		purego.RegisterLibFunc(fn, iokit, name)
	}
	for name, fn := range map[string]any{
		"CFRelease":                         &cfRelease,
		"CFGetTypeID":                       &cfGetTypeID,
		"CFStringGetTypeID":                 &cfStringGetTypeID,
		"CFNumberGetTypeID":                 &cfNumberGetTypeID,
		"CFStringCreateWithCString":         &cfStringCreateWithCString,
		"CFStringGetLength":                 &cfStringGetLength,
		"CFStringGetMaximumSizeForEncoding": &cfStringGetMaximumSizeForEncoding,
		"CFStringGetCString":                &cfStringGetCString,
		"CFNumberGetValue":                  &cfNumberGetValue,
		"CFUUIDCreateFromString":            &cfUUIDCreateFromString,
	} {
		purego.RegisterLibFunc(fn, cf, name)
	}
	sys, err := purego.Dlopen(sysPath, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return fmt.Errorf("usb: cannot load libSystem: %w", err)
	}
	purego.RegisterLibFunc(&pthreadSigmask, sys, "pthread_sigmask")
	mk := func(s string) (uintptr, error) {
		str := cfString(s)
		if str == 0 {
			return 0, fmt.Errorf("usb: CFStringCreateWithCString(%s) failed", s)
		}
		defer cfRelease(str)
		u := cfUUIDCreateFromString(0, str)
		if u == 0 {
			return 0, fmt.Errorf("usb: CFUUIDCreateFromString(%s) failed", s)
		}
		return u, nil
	}
	if cfuuidDeviceUserClient, err = mk(uuidUSBDeviceUserClientType); err != nil {
		return err
	}
	if cfuuidInterfaceUserClient, err = mk(uuidUSBInterfaceUserClientType); err != nil {
		return err
	}
	if cfuuidPlugIn, err = mk(uuidCFPlugInInterface); err != nil {
		return err
	}
	if iidDevice100, err = uuidBytes(uuidUSBDeviceInterface100); err != nil {
		return err
	}
	for _, v := range interfaceIIDs {
		b, err := uuidBytes(v.uuid)
		if err != nil {
			return err
		}
		iidInterfaces = append(iidInterfaces, b)
	}
	return nil
}

func cfString(s string) uintptr { return cfStringCreateWithCString(0, s, kCFStringEncodingUTF8) }

// goString converts a CFStringRef (not released here).
func goString(s uintptr) (string, bool) {
	if s == 0 || cfGetTypeID(s) != cfStringGetTypeID() {
		return "", false
	}
	n := cfStringGetMaximumSizeForEncoding(cfStringGetLength(s), kCFStringEncodingUTF8) + 1
	buf := make([]byte, n)
	if cfStringGetCString(s, &buf[0], n, kCFStringEncodingUTF8) == 0 {
		return "", false
	}
	if i := strings.IndexByte(string(buf), 0); i >= 0 {
		buf = buf[:i]
	}
	return string(buf), true
}

// goNumber converts a CFNumberRef (not released here).
func goNumber(n uintptr) (int64, bool) {
	if n == 0 || cfGetTypeID(n) != cfNumberGetTypeID() {
		return 0, false
	}
	var v int64
	if cfNumberGetValue(n, kCFNumberSInt64Type, &v) == 0 {
		return 0, false
	}
	return v, true
}

// prop reads one registry property of entry e; search also looks in the
// entry's children (recursively, IOService plane).
func prop(e uint32, key string, search bool) uintptr {
	k := cfString(key)
	if k == 0 {
		return 0
	}
	defer cfRelease(k)
	if search {
		return ioRegistryEntrySearchCFProperty(e, kIOServicePlane, k, 0, kIORegistryIterateRecursively)
	}
	return ioRegistryEntryCreateCFProperty(e, k, 0, 0)
}

// strProp returns the first of keys that is a string property of e.
func strProp(e uint32, keys ...string) (string, bool) {
	for _, key := range keys {
		v := prop(e, key, false)
		if v == 0 {
			continue
		}
		s, ok := goString(v)
		cfRelease(v)
		if ok {
			return s, true
		}
	}
	return "", false
}

// numProp returns the first of keys that is a number property of e.
func numProp(e uint32, keys ...string) (int64, bool) {
	for _, key := range keys {
		v := prop(e, key, false)
		if v == 0 {
			continue
		}
		n, ok := goNumber(v)
		cfRelease(v)
		if ok {
			return n, true
		}
	}
	return 0, false
}

func searchString(e uint32, key string) string {
	v := prop(e, key, true)
	if v == 0 {
		return ""
	}
	defer cfRelease(v)
	s, _ := goString(v)
	return s
}

func className(o uint32) string {
	var buf [128]byte
	if ioObjectGetClass(o, &buf[0]) != 0 {
		return ""
	}
	if i := strings.IndexByte(string(buf[:]), 0); i >= 0 {
		return string(buf[:i])
	}
	return string(buf[:])
}

func registryID(e uint32) uint64 {
	var id uint64
	if ioRegistryEntryGetRegistryEntryID(e, &id) != 0 {
		return 0
	}
	return id
}

// services returns the services matching dict (which is consumed). The caller
// releases each returned object.
func services(dict uintptr) []uint32 {
	if dict == 0 {
		return nil
	}
	var it uint32
	if ioServiceGetMatchingServices(kIOMainPortDefault, dict, &it) != 0 {
		return nil
	}
	defer ioObjectRelease(it)
	return drain(it)
}

func children(e uint32) []uint32 {
	var it uint32
	if ioRegistryEntryGetChildIterator(e, kIOServicePlane, &it) != 0 {
		return nil
	}
	defer ioObjectRelease(it)
	return drain(it)
}

func drain(it uint32) []uint32 {
	var out []uint32
	for {
		o := ioIteratorNext(it)
		if o == 0 {
			return out
		}
		out = append(out, o)
	}
}

func releaseAll(objs []uint32) {
	for _, o := range objs {
		ioObjectRelease(o)
	}
}

// serviceByID returns the live service with the given registry entry id, or 0
// once the entry has been terminated (the device left the bus).
func serviceByID(id uint64) uint32 {
	if id == 0 {
		return 0
	}
	return ioServiceGetMatchingService(kIOMainPortDefault, ioRegistryEntryIDMatching(id))
}

// comObj is a COM-style IOKit interface: a pointer to a pointer to a vtable.
type comObj struct{ p unsafe.Pointer }

func (o comObj) fn(slot int) uintptr {
	vt := *(*unsafe.Pointer)(o.p)
	return *(*uintptr)(unsafe.Add(vt, slot*int(unsafe.Sizeof(uintptr(0)))))
}

// call invokes vtable slot with the object as the first argument and returns
// the low 32 bits of the result (IOReturn, HRESULT or ULONG).
//
//go:uintptrescapes
func (o comObj) call(slot int, args ...uintptr) uint32 {
	all := make([]uintptr, 0, len(args)+1)
	all = append(all, uintptr(o.p))
	all = append(all, args...)
	r1, _, _ := purego.SyscallN(o.fn(slot), all...)
	return uint32(r1)
}

// ioThread runs the blocking pipe calls of one open interface on a dedicated
// OS thread that has SIGURG blocked.
//
// The Go runtime sends SIGURG to threads that run Go code to preempt
// goroutines. With the synchronous IOKit pipe calls issued from ordinary
// goroutines, a J-Link OB (nRF5340) stopped answering every few hundred
// 16 KiB reads at 1-2 MHz and rebooted, often into its bootloader. The
// command bytes were intact and the same command stream replayed through
// libusb ran clean. GODEBUG=asyncpreemptoff=1 fixed it, but blocking SIGURG
// only around each call did not: the thread has to stay the same and never
// take the signal. With this thread the reads ran clean (2400 x 16 KiB).
// Only SIGURG is blocked, so faults on the thread still reach the runtime;
// preemption of the loop below falls back to the cooperative kind.
type ioThread struct {
	reqs chan func()
}

func newIOThread() *ioThread {
	t := &ioThread{reqs: make(chan func())}
	ready := make(chan struct{})
	go func() {
		// Never unlocked: the thread exits with this goroutine and its
		// signal mask does not leak to other goroutines.
		runtime.LockOSThread()
		set := uint32(1) << (sigURG - 1)
		pthreadSigmask(sigBlock, &set, nil)
		close(ready)
		for f := range t.reqs {
			f()
		}
	}()
	<-ready
	return t
}

// do runs call on the thread and waits for it.
func (t *ioThread) do(call func() uint32) uint32 {
	var kr uint32
	done := make(chan struct{})
	t.reqs <- func() {
		defer close(done)
		kr = call()
	}
	<-done
	return kr
}

func (t *ioThread) stop() { close(t.reqs) }

// Darwin pthread_sigmask arguments.
const (
	sigBlock = 1
	sigURG   = 16
)

func (o comObj) release() {
	if o.p != nil {
		o.call(slotRelease)
	}
}

// plugin creates the user-client plug-in for svc and queries the first of iids
// it supports, returning its index. The intermediate IOCFPlugInInterface is
// released before returning.
func plugin(svc uint32, userClientType uintptr, iids ...[16]byte) (comObj, int, error) {
	var plug unsafe.Pointer
	var score int32
	if kr := ioCreatePlugInInterfaceForService(svc, userClientType, cfuuidPlugIn, &plug, &score); kr != 0 || plug == nil {
		return comObj{}, -1, ioReturn(uint32(kr))
	}
	p := comObj{plug}
	defer p.release()
	var hr uint32
	for i, iid := range iids {
		var obj unsafe.Pointer
		lo, hi := uuidRegs(iid)
		if hr = p.call(slotQueryInterface, lo, hi, uintptr(unsafe.Pointer(&obj))); hr == 0 && obj != nil {
			return comObj{obj}, i, nil
		}
	}
	return comObj{}, -1, fmt.Errorf("QueryInterface failed (HRESULT 0x%08x)", hr)
}
