# ADR 0035: macOS native USB backend (IOKit through purego, no cgo)

Status: accepted

## Context

`internal/usb` (discovery and a one-interface transport) was Linux only:
sysfs for discovery, usbfs ioctls for transfers. On macOS every native probe
plugin (`jlinknative`, `xds110`, `cmsisdap`, `stlink`, `espusbjtag`, `ftdi`)
returned `ErrUnsupported`. The project builds without cgo on linux and windows
and keeps cgo on darwin to the two serial plugins that need VID/PID lookup
(ADR 0026).

## Decision

### No cgo: IOKit is called through purego

The darwin backend loads `IOKit.framework` and `CoreFoundation.framework` at
run time with `github.com/ebitengine/purego` (Apache-2.0) and calls their C
functions and the COM-style plug-in vtables (`IOCFPlugInInterface`,
`IOUSBDeviceInterface`, `IOUSBInterfaceInterface`) by slot. So:

* `CGO_ENABLED=0` builds for darwin/amd64 and darwin/arm64 keep working; no
  Xcode or C toolchain is needed to build a native probe plugin.
* The vtable slot numbers and struct layouts (`IOUSBDevRequestTO`, UUID byte
  order) were checked against the SDK headers with a throw-away C program
  built with clang and run on x86_64 (an arm64 build compiled but was not
  run; both are LP64 with the same layout); they are constants in
  `iokit_darwin.go`.
* cgo was not needed: every call is a plain C ABI call with integer, pointer
  or by-value 16-byte UUID arguments (passed as two 64-bit registers on both
  architectures). A `//go:build darwin && cgo` alternative would only add a
  second code path.

The cost is that the ABI is not checked by a compiler. Pure parsing and error
mapping are unit-tested; the bindings themselves are only exercised on real
hardware (see results).

### Discovery

`usb.Scan("/sys", …)` on darwin reads the IORegistry (`IOUSBHostDevice`
services) instead of sysfs and fills the same `Device`/`Interface`/`Endpoint`
structures: VID/PID, bcdDevice, strings, serial, speed, active configuration,
interface class/number/name, the bound macOS driver (or the process holding
it) and the tty of a serial interface. Endpoints come from the cached
configuration descriptor, read through a device user client that is never
opened and parsed by the platform-independent `parseConfigDescriptor`.

Identity stays VID:PID + USB serial, as on Linux. `SysName` is derived from the
locationID and, with `LocationID`/`RegistryID`, is diagnostics only: it changes
with the port and at every re-enumeration. Any other root (test fixtures) is
still read as a sysfs tree, so existing tests run unchanged on macOS.

### Transport: exactly one interface

`Open` finds the interface's `IOUSBHostInterface` service and calls
`USBInterfaceOpen` on that interface only. The device is never opened, its
configuration is never set and other interfaces keep their drivers (on the
J-Link OB both CDC ttys stayed usable while interface 4 was claimed).

* **Driver-held interfaces are refused.** An idle macOS driver (AppleUSBACMData
  with its tty closed) does not hold the interface open, so `USBInterfaceOpen`
  would succeed and silently take it away from the driver. `Open` therefore
  checks the registry first and returns `ErrBusy` if a driver is bound or a
  process owns the interface. macOS cannot detach a kernel driver from user
  space, so `OpenOptions.DetachDriver` is reported as impossible rather than
  honoured. (FTDI channel A bound to AppleUSBFTDI is busy on macOS.)
* **Interface version 300 or later.** libusb's darwin backend documents that
  with IOUSBInterfaceInterface 245 or older the OS clears the host-side data
  toggle on `AbortPipe` (it then clears the device side too). A transfer that
  times out is ended by an abort, so the host and device toggles can diverge
  and the next packet is dropped as a duplicate. The transport asks for
  versions 942, 800, 700, 650, 550, 500 and 300 in that order and refuses
  older ones, as libusb prefers. All of them share the vtable slots used here.
  (The J-Link bootloader falls first blamed on this came from SIGURG, below.)
* **Blocking calls run on a thread without SIGURG.** Each open interface has
  one locked OS thread with SIGURG blocked, and every control and pipe call
  runs there; the abort is issued from another thread. Issued from ordinary
  goroutines, the synchronous pipe calls made a J-Link OB stop answering and
  reboot, often into its bootloader, every few to few hundred 16 KiB reads.
  The command bytes were intact, the same stream replayed through libusb ran
  clean and `GODEBUG=asyncpreemptoff=1` fixed it; blocking SIGURG only around
  each call did not, so the thread must stay the same and never take the
  signal. libusb avoids this with the asynchronous API and a run loop thread.
* **Timeouts.** `ReadPipeTO`/`WritePipeTO` honour only the completion timeout,
  so each bulk call is one transfer with the full timeout. Cutting the wait
  into short slices was rejected: aborting a slice can lose a reply that is
  already in flight. Context cancellation aborts the pipe from a goroutine, so
  a cancelled call returns promptly. Interrupt pipes have no timeout variant
  and use `ReadPipe`/`WritePipe` with the same abort helper for the timeout.
* **Errors** map to the shared sentinels: `kIOReturnTimeout`/`kIOUSBTransactionTimeout`
  → `ErrTimeout`; `kIOReturnExclusiveAccess`/`kIOReturnBusy` → `ErrBusy`;
  `kIOReturnNotPrivileged`/`kIOReturnNotPermitted` → `ErrPermission`;
  `kIOUSBPipeStalled` → `ErrStall`; `kIOReturnNoDevice`/`kIOReturnOffline`
  → `ErrDisconnected`; `kIOReturnNotOpen` → `ErrClosed`. An abort, a
  not-responding or a generic error is reported as disconnected only when the
  device's registry entry disappears shortly after; otherwise the raw status
  is kept (an abort without cancellation is a timeout). Overrun and underrun
  stay generic errors. `ClearHalt` uses `ClearPipeStallBothEnds`
  (host pipe and `CLEAR_FEATURE(ENDPOINT_HALT)`, resets the toggle).

### Plugins

`jlinknative`, `xds110`, `stlink`, `espusbjtag` and `ftdi` use `usb.Scan` and
`usb.Open` and needed no code change. `cmsisdap` scanned sysfs itself; on
macOS it now picks the "CMSIS-DAP" interface from `usb.Scan` (v2 bulk
preferred) and opens v2 through the shared transport.

## Gaps

* **CMSIS-DAP v1 (HID)** is not implemented on macOS. It would need
  IOHIDManager/IOHIDDevice; no v1-only probe was available to test it, so v1
  probes are listed with a warning and refused.
* **CC-Debugger** has its own usbfs code and stays Linux only.
* No `DeviceResetter` on macOS (`USBDeviceReEnumerate` would need the device
  opened, which disturbs every interface).
* Only the J-Link was run on macOS; XDS110, CMSIS-DAP, ST-Link, ESP and FTDI
  are untested there.
* Why the signal on the thread that issues the calls upsets the probe is not
  known; the fix is empirical (see the results note).

## Consequences

* Native probes work on macOS without libusb, cgo or permission setup (IOKit
  needs no udev-like rule for a user process).
* `hwlab doctor` on macOS reports the native backends from the IORegistry,
  including which macOS driver holds which interface, and a J-Link in its
  bootloader.
* Evidence: [phase-macos-iokit-results](../notes/phase-macos-iokit-results.md).
