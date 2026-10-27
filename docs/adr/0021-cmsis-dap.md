# ADR 0021: CMSIS-DAP as a DebugProbe backend (native SWD stack)

Status: accepted

## Context

CMSIS-DAP is the open probe protocol behind DAPLink, the Raspberry Pi Debug
Probe, many vendor on-board probes and others. The generic `DebugProbe` and
`MemoryAccess` capabilities were proven on J-Link (through SEGGER's CLI) and the
CC-Debugger (native USB). No CMSIS-DAP probe is attached to the development
host, so nothing in this ADR has met a real probe.

## Decision

- **Pure Go, no new runtime dependency.** The protocol is a small command/response
  set over USB bulk (v2) or HID (v1), so it is implemented directly rather than
  wrapped around openocd/pyocd/probe-rs. No CGO, no libusb, no Python.
- **Layers** in `plugins/cmsisdap/dap`:
  `Transport` (USB v2 usbfs bulk, USB v1 hidraw, and a simulator) →
  `Probe` (DAP commands: Info, Connect, SWJ_*, Transfer, TransferBlock, ...) →
  `Target` (SWD line reset, JTAG-to-SWD, DPIDR, power-up, DP/AP and memory
  access with sticky-error recovery) → `Core` (Cortex-M halt/resume/step/reset,
  core registers). `plugins/cmsisdap` only maps these onto the plugin protocol.
  Nothing CMSIS-DAP specific (packet sizes, command IDs, ACK codes) is visible in
  core; the plugin advertises `DebugProbe` and `MemoryAccess` with the same method
  names and result shapes as J-Link.
- **Scope: SWD only.** JTAG is reported as a probe capability bit but never
  driven. Flash programming, RTT and SWO are **not implemented and not
  advertised**; a backend advertises only what it does. (Superseded in part:
  CC23xx flash programming and target-memory RTT were added later, see the
  addenda.)
- **Discovery through sysfs**, not by parsing USB descriptors: the interface
  string must contain "CMSIS-DAP" and `bInterfaceClass` selects v2 (0xff, bulk)
  or v1 (0x03, HID). v2 is preferred when both exist. This is testable with a
  fake sysfs tree.
- **Safety.** `info`, `connect`, `status` and `MemoryAccess.read` are SAFE.
  `halt`, `resume`, `step`, `reset`, `read_registers`, `write_register` and
  `MemoryAccess.write` are CONFIRM (halting or resetting a running target
  changes it; reading registers needs a halt). Writes verify by read-back unless
  asked not to. A transient call that halts leaves the target halted; it never
  resumes behind the user's back.
- **Simulator.** `dap.Sim` is a wire-level DAP probe with a simulated SW-DP, AHB
  MEM-AP and Cortex-M4 (read-only flash, RAM, sticky errors, WAIT injection).
  `HWLAB_CMSISDAP_SIM=1` exposes it as `cmsisdap/sim-cmsisdap`. It keeps CI
  hardware-free and records the fixture, but it embodies the author's reading of
  the specification. **It is not evidence about real probes**; everything
  recorded from it is labelled synthetic and `validated: false`.

## Consequences

- Flash is the main gap. The intended path is CMSIS-Pack flash algorithms run
  from target RAM through the existing `Target`/`Core` layers, with an external
  probe-rs/pyocd fallback during bring-up. Neither is started.
- The usbfs ioctl code is duplicated from `ccdebugger/native` instead of shared,
  to avoid touching the hardware-tested CC-Debugger code. A shared package can
  follow once both have real mileage.
- Linux only: macOS and Windows need IOKit/libusb/hidapi (CGO). Non-Linux builds
  compile a stub that reports the transport unsupported.
- Non-root access needs a udev rule for the probe's usb node (v2) or hidraw node
  (v1); error messages say so.
- A USB transfer timeout may leave a stale response queued; the plugin does not
  yet drop the device on transport errors, so recovery is reopening the device.
- Real-world quirks (probes that cap packet size, ignore SWD_Configure, or answer
  Transfer differently after WAIT), multi-AP targets and reset-line behaviour
  are the likeliest places a real probe disagrees with this implementation.

## Alternatives considered

- Wrapping openocd or pyocd: heavy runtime dependencies, process management and
  output parsing for what is a compact protocol.
- Wrapping probe-rs: attractive for flash, but Rust toolchain and a different
  session model; kept as a possible fallback for flashing.

## Addendum (phase 22): real probe, shared layers

A BL702-DAPLink (`d6e7:3507`, v2 bulk, no target) was validated; see
[phase 22](../notes/phase22-cmsisdap-results.md). The SWD/ADIv5 and Cortex-M
logic moved out of `plugins/cmsisdap/dap` into `internal/adiv5` and
`internal/cortexm` (shared with ST-Link), and the usbfs code into `internal/usb`.
`dap.Probe` implements `adiv5.Link`. Findings that shaped the transport: a fresh
opener drains stale replies and any out-of-sync reply triggers a drain;
`DAP_Delay(0)` is never sent (it wedges this firmware). The statement above that
no probe is available is historical.

## Addendum: target-memory RTT

The plugin now offers SEGGER-compatible RTT read from target SRAM, using the
probe-independent engine `internal/rtt` and the plugin glue shared with
`jlinknative` and `xds110` (`internal/rtt/rttplugin`): `DebugProbe.rtt_info`
(safe), `DebugProbe.rtt_write` (confirm) and `StartCapture` as an RTT capture
(Log events on stream `rtt`, source `cmsisdap-rtt`; `stream: rtt` is optional).
The capability list gains `RTT` and `LiveRawStream` (`rtt: target-memory`).
The device lock is held per poll, so debug calls interleave with a capture. On a
CC23xx every RTT access leaves SACI exactly like `MemoryAccess.read`, and a
capture re-locates the control block after a target reset. **Status: simulator-
tested (synthetic Cortex-M and the CC23xx SACI model); not yet verified on any
real probe or target.** A debug-only device in `hwlab run` now needs
`control: true`, because the probe advertises a capture stream.
