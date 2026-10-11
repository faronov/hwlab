# ADR 0028: native FTDI / MPSSE / JTAG probe backend

Status: accepted

## Context

Many debug probes are FTDI FT2232-class devices driven in MPSSE mode. The
attached adapter (XT-ZB1, a BL702 running Sipeed RV-Debugger-BL702 firmware)
enumerates as `0403:6010` "SIPEED JTAG Debugger" and emulates an FT2232D. Two
interfaces are exposed: interface 0 (channel A, JTAG) and interface 1
(channel B, UART). The goal is a generic backend, not an XT-ZB1 debugger.

> Update (ADR 0029): the generic `usb` and `jtag` layers described below now
> live in `internal/usb` and `internal/jtag` (the JTAG `Port` interface in
> `internal/jtag/port.go`); the MPSSE-backed port is `plugins/ftdi/jtagport`.

## Decision

1. `plugins/ftdi` is native Go on Linux usbfs (the `usb` layer reuses the
   direct-usbfs approach of `ccdebugger`/`cmsisdap`): no CGO, no libusb, no
   OpenOCD and no Python at run time.
2. Strict layering, each layer testable alone:
   `usb` (usbfs) -> `ftdi` (control/bulk protocol, status-byte stripping,
   bounded reads, cancellation) -> `mpsse` (command generation and response
   parsing, independent of JTAG) -> `jtag` (TAP state machine, IR/DR scans,
   IDCODE, chain parsing) -> plugin. `profiles` holds adapter facts and quirks;
   `sim` is a SYNTHETIC FT2232 + TAP model for CI.
3. Only the JTAG interface is claimed. `ftdi_sio` is detached from that
   interface only (`USBDEVFS_DISCONNECT_CLAIM` with a driver filter) and
   re-bound on close. Interface 1 stays bound so `/dev/ttyUSBx` remains usable.
   Only a driver named `ftdi_sio` is ever detached: a hardware test showed that
   detaching "whatever is bound" lets a second open steal the first session's
   claimed interface (`usbfs` was bound at that moment). A second open now
   fails with EBUSY.
4. No second serial stack: UART I/O stays in the `uart` plugin. The `ftdi`
   device exposes `companion_uart_*` properties; both plugins expose
   `usb_device` and `usb_interface` so the two presentations can be correlated.
5. Capabilities: `JTAGTransport`, `JTAGScan`, `TargetIdentify` only. Memory,
   flash, halt, resume and registers are not claimed: JTAG is a probe transport,
   not a CPU debug protocol. Later RISC-V DM / ARM JTAG-DP layers sit above it.
6. Profiles carry `implementation: real-ftdi | compatible-emulator`. The
   Sipeed profile is a `compatible-emulator`; behaviour is never gated on
   detecting real FTDI silicon. Quirks (fixed ~6 MHz clock, ignored loopback)
   live in the profile.
7. Safety: only TAP reset and scans run by default; arbitrary `shift_ir` and
   `shift_dr` need CONFIRM. Nothing resets, halts or writes a target.

## Clean implementation / licenses

OpenOCD (GPL-2.0) was used only for configuration facts (VID/PID, channel,
`layout_init` values, speed). No OpenOCD, Sipeed firmware or other source was
copied. Protocol behaviour follows FTDI AN_108 (MPSSE command processor) and
AN_135, and the firmware's Apache-2.0 headers as behavioural evidence. See
`plugins/ftdi/NOTICE` and [docs/licenses.md](../licenses.md).

## Consequences

* Linux only; other platforms build a stub that reports unsupported.
* A udev rule (or root) is needed for usbfs access; `hwlab doctor` reports it.
* The JTAG target scan is IMPLEMENTED_UNTESTED on real hardware: no target was
  attached, so only the empty-chain result (TDO all ones) was observed.
* The Sipeed firmware ignores MPSSE loopback and the clock divisor; the
  selftest reports that rather than failing.
