# macOS native USB backend (IOKit through purego): results

Scope: the darwin transport of `internal/usb` ([ADR 0035](../adr/0035-macos-iokit-usb.md))
and the `jlinknative` plugin on top of it. Host: an Intel Mac (darwin/amd64),
Go 1.27.1, `CGO_ENABLED=0` and `=1` builds. Everything below was read-only on
the target: nothing was written, erased, programmed or reset.

## Hardware

| Device | VID:PID | Interfaces (macOS driver) |
|---|---|---|
| SEGGER J-Link OB-nRF5340 on a Nordic DK, serial 000000000001 | 1366:1061 | 0/1 CDC (AppleUSBACMControl/ACMData, `cu.usbmodem0000000000011`), 2/3 CDC (`…383`), 4 "BULK interface" vendor 0xff (IN 0x85 / OUT 0x03, 64 B, no driver), 5 mass storage (IOUSBMassStorageInterfaceNub) |
| CH340 serial adapter | 1a86:7523 | AppleUSBCHCOM, `cu.usbserial-EXAMPLE` |
| built-in camera, T2, headset, ambient light sensor | Apple | (listed, never opened) |

The target behind the J-Link is an unidentified nRF52-class Cortex-M4.

## Discovery

The IORegistry scan listed every device above with VID/PID, strings, serial,
speed, interface class/number/name, endpoints (from the cached configuration
descriptor, read through an unopened device user client), the bound macOS
driver and the tty of CDC interfaces. Identity is VID:PID + USB serial; the
locationID (0x14200000 here) is only diagnostics. A whole scan process ran in
27 ms warm (0.7 s on the first launch of a fresh binary).

## jlinknative on the J-Link (IOUSBInterfaceInterface190, first build)

| Check | Result |
|---|---|
| probe info | firmware `J-Link OB-nRF5340-NordicSemi compiled Jul 8 2025 10:15:34`, HW V1.00, VTref 3300 mV, SWD only, max 2000 kHz; transport `IOKit 1366:1061 location 0x14200000 address 17 interface 4` |
| SWD identify | DPIDR 0x2BA01477, AP IDR 0x24770011, Cortex-M4 r0p1 (CPUID 0x410FC241); the core was found halted at PC 0x0000124C |
| memory reads | CPUID word at 0xE000ED00, ROM table at 0xE00FF000, vector table at 0x0 (SP 0x20020000); 16 KiB at 400 kHz, 1 MHz and 2 MHz in 210-620 ms |
| halt/resume | resume reported running, halt returned to PC 0x124C; the core was left halted as found |
| second open | refused with `kIOReturnExclusiveAccess`, reported PROBE_BUSY |
| CDC coexistence | both `cu.usbmodem…` ttys opened while interface 4 was claimed |
| interface held by a driver | opening CDC interface 1 (AppleUSBACMData, tty closed) is refused as busy; without that check IOKit would have opened it |
| cancellation | a cancelled context ends a pending bulk read at once (measured 150 ms for a 150 ms cancel); before, IOKit honoured only the 5 s completion timeout |
| close/reopen | Close is idempotent, calls after it return ErrClosed, reopen works |
| repo hardware tests | `TestHWInfoAndStableID`, `TestHWConnectIdentify`, `TestHWSecondOpenIsBusy`, `TestHWRepeatedOpenConnectClose` (10 rounds) passed |

## Failure: probe in its bootloader

`TestHWSpeedLadder` read 16 KiB at 100 kHz, then reopened at 400 kHz; that read
timed out with 0 of 1021 reply bytes, the next connect got `kIOReturnOverrun`
and the next run's bulk OUT timed out. The probe then re-enumerated as
1366:0101 (`J-Link OB-nRF5340-NordicSemi compiled May 18 2021 BTL`, CDC ports
gone, same serial). This is the bootloader state described in ADR 0034;
`USBDeviceReEnumerate` does not leave it, only a physical replug does.
JLinkExe was not run at first (it may update the probe firmware); see below.

Probable cause (inferred, not proven by a bus trace): libusb's darwin backend
documents that with IOUSBInterfaceInterface 245 or older the OS clears the
host-side data toggle when a pipe is aborted, and a timed-out transfer is
ended by an abort. The device keeps its toggle, so the next packet is dropped
as a duplicate and the command stream is skewed, which matches the overrun
and the bootloader entry seen here. The transport now asks for 942, 800, 700, 650, 550, 500
or 300 and refuses older ones. Opening the bootloader probe confirmed
`IOUSBInterfaceInterface942` is obtained on this macOS; no transfer was sent.

This explanation turned out to be incomplete; see the next section.

## Root cause: SIGURG on the thread that issues the pipe calls

With version 942 the falls continued: a loop of 16 KiB reads at 1 MHz ended
after 2 to a few hundred reads (6 exchanges each). The last exchange was a
normal command whose OUT completed; the reply never came and the kernel
aborted the IN about 0.75 s later when the probe rebooted, often into its
bootloader. Checks on the same Mac and probe:

| Run | Result |
|---|---|
| JLinkExe V9.24a `savebin`, 200 x 16 KiB at 1 MHz | clean |
| probe-rs 0.31 `read`, 3 x 1 MiB at 1 MHz | clean |
| our recorded command stream replayed through pyusb/libusb | ~2960 exchanges clean |
| OUT buffer checksummed before and after each call | never changed |
| IN rounded up to the packet size, interface 700, calls without TO, 64-byte OUT chunks, SWD idle 3, RAM instead of flash, `GOGC=off` | still failed |
| `GODEBUG=asyncpreemptoff=1` | 4 x 600 x 16 KiB at 1 and 2 MHz clean |
| SIGURG (or all signals) blocked only around each IOKit call | still failed (150th read) |
| all calls on one locked thread, no mask | failed after 367 reads |
| all calls on one locked thread with SIGURG blocked | 6 x 600 x 16 KiB clean |

The transport now runs every control and pipe call of an open interface on
such a thread (`ioThread` in `internal/usb/iokit_darwin.go`). Why the signal
upsets the probe is not known.

After the fix the read-only hardware tests (`TestHWInfoAndStableID`,
`TestHWConnectIdentify`, `TestHWSecondOpenIsBusy`,
`TestHWRepeatedOpenConnectClose`, `TestHWClockProbe`, `TestHWSpeedLadder`)
passed three times in a row. `TestHWRead64KAt4MHz` needs a 4 MHz probe and is
skipped on this 2 MHz one.

## Leaving the bootloader without a replug

`JLinkExe -NoGui 1 -AutoConnect 0 -CommandFile f.jlink` with `f`, `hwinfo` and
`exit` in the file finds the probe in its bootloader and installs its
firmware again ("Replacing firmware"); it re-enumerates as 1366:1061 after a
few seconds. This rewrites the probe firmware, so it needs the owner's
consent.

## doctor

`hwlab doctor` on macOS lists the native backends from the IORegistry without
opening anything; with the probe in its bootloader it reported
`1366:0101 … runs its bootloader and answers no commands: unplug and replug it`.

## Not run on macOS

XDS110, CMSIS-DAP (v2 bulk), ST-Link, ESP USB Serial/JTAG and FTDI: their
plugins use the same transport but no such device was attached to the Mac.
CMSIS-DAP v1 (HID) and the CC-Debugger native backend are not implemented on
macOS.
