# ADR 0018 — Native direct-USB CC-Debugger backend

Status: accepted. Supersedes the "cc-tool is the primary backend" decision of
[ADR 0013](0013-ccdebugger-backend.md); cc-tool stays as an optional fallback.
Hardware-validated read-only on a CC-Debugger (0451:16a2) with a CC2530.

## Context

ADR 0013 chose `cc-tool` as a subprocess because no maintained host tool offered
halt/resume/memory access. Since then a hardware-tested, BSD-3-Clause direct-USB
implementation exists in the maintainer's own `cc2530-zigbee` repository
(`tools/cc_debugger.py`, `tools/cc2530_debug.py`, `tools/check_debug_hardware.py`,
`tools/cc_tool_no_run_guard.c`). It speaks the adapter's USB control/bulk protocol
directly and implements target state, reset-attach, halt/resume/step, PC and
register reads, CODE/XDATA/banked-flash reads, XDATA writes and hardware
breakpoints, with an explicit permission model.

## Decision

`plugins/ccdebugger/native` is the **primary** backend for the CC-Debugger
(PID 16a2). It is a pure-Go port, with no cgo, no libusb and no Python:

- **Transport**: `usbfs_linux.go` talks to `/dev/bus/usb/BBB/DDD` with x/sys
  ioctls. It checks the descriptor VID/PID, requires configuration 1, claims
  interface 0 and **refuses** a busy interface (it never detaches another
  driver). Other OSes build a stub (`Supported=false`) and fall back to cc-tool;
  a libusb/IOKit transport can later slot behind the `Transport` interface.
- **Port, not a script wrapper**: `Debugger` owns the protocol. It has
  Open/Faulted/Closed states, a single in-flight operation (`TryLock`, busy is
  refused), a per-operation deadline, and a session that is **never silently
  reconnected** after a transport fault.
- **Permission model preserved**: access mode `adapter-only` /
  `existing-debug-session` / `reset-debug-session`, plus independent
  `allow_cpu_control`, `allow_target_reset`, `allow_memory_access`,
  `allow_memory_write`, `allow_breakpoints`, `allow_dma_enable`. These are
  plugin config keys declared as `DangerousConfigKeys`, so `hwlab run` demands
  confirmation when they are set. Adapter-only sends nothing to the target.
  Only the reviewed reset form (`C9`, request `0x40`, value 0, index 1) is ever
  sent; any other `C9` form resets the target into run mode and is unreachable.
- **Methods**: `attach`, `status`, `reset_halt`, `halt`, `resume`, `step`,
  `read_registers`, `read_sfr` (allow-listed core SFRs), `set_breakpoint`,
  `wait_halted`, `enable_dma`, and `MemoryAccess` read (xdata/code/flash, SAFE)
  and write (xdata below 0x1E00 only, readback verified, CONFIRM).
  State-changing methods are CONFIRM; reads are SAFE.
- **Testing**: an in-process 8051/CC2530 debug **simulator** (`NewSimulator`)
  implements the adapter protocol; CI covers policy, sequences, banks and fault
  handling without hardware.

### Fallback: cc-tool

`cc-tool` remains an optional subprocess backend (ADR 0013) for what native does
not do: `read_mac`, `reset` to run, flash `read`, `read_info_page`, `erase`,
`program`, and for the legacy PID 16a0. `backend=auto` uses native when it is
available and the op is native; `backend=cc-tool` forces the fallback. A native
session holds USB interface 0, so any cc-tool operation first drops the native
session (the debug session is lost and must be re-attached).

### Native flash erase/program (phase 15)

Chip erase, page erase, flash program, readback and CRC verify, and config
(lock/secondary IEEE) write are implemented natively in Go
(`native/flash.go`, `flashproto.go`, `stream.go`, `ident.go`, `target/`,
`flashplan/`), written from TI SWRU191F and the BSD reference, with cc-tool used
only as a behavioural reference and a read-only cross-check. They are exercised by
the simulator only: **no erase, program or config write has run on real hardware**
because the only CC2530 is not sacrificial. The flash-controller write path
(FCTL/FWDATA, DMA channel 2) and the chip-erase command are inferred and stay
"implemented, unverified". Default policy is `preserve-device-config`; see
[phase 15 results](../notes/phase15-ccdebugger-results.md).

## Provenance and licensing

- The Go code derives from the maintainer's BSD-3-Clause `cc_debugger.py` /
  `cc2530_debug.py` and TI public documentation (SWRU191 debug interface, SWRA124
  flash/DMA notes).
- **No cc-tool (GPL-2.0) source was copied or mechanically translated.**
  cc-tool was consulted only as a behavioural reference (protocol sequences,
  device families, edge cases) and as an independent cross-check: native and
  cc-tool reads of the same CC2530 flash returned identical bytes.
- The erase/program code is written independently from TI documents, the BSD
  reference and behaviour observed on real hardware (pipelined framing, burst
  write, hardware CRC polynomial).

## Consequences

- Real debug-session features (halt, step, registers, memory, breakpoints) exist
  for the CC-Debugger without an external tool, on Linux.
- No RTT on this probe class; `attributes.rtt=no`.
- Linux only natively; flash erase/program is native but hardware-unverified (cc-tool remains an optional fallback).
- A native session and cc-tool contend for the interface; the plugin
  serialises them per device.

## Validation

Real hardware (CC2530, target left running after every run): adapter state,
reset attach, status, registers, SFR read, CODE and physical flash reads (256 KB
byte-identical to cc-tool), info page and primary IEEE (identical to cc-tool),
hardware CRC verify (poly 0x8005, init 0xFFFF; full image in ~3.4 s, bit flip
detected), burst write into scratch RAM, XDATA scratch write with restore, halt
and resume on a running target, one hardware breakpoint and one single step.
Replay fixtures: `testdata/real-ccdebugger-native`,
`testdata/real-ccdebugger-identify`.
Unverified on hardware: every flash-controller operation (chip/page erase,
program, config write), debug-locked targets and other chip families.
