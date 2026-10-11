# ADR 0013 — TI CC-Debugger backend via cc-tool

Status: accepted; the "cc-tool is the primary backend" part is superseded by
[ADR 0018](0018-native-ccdebugger.md) (cc-tool is now an optional fallback).
Hardware-validated on a CC-Debugger (0451:16a2) with a CC2530
target for the read-only operations only (see the end).

## Context

The CC-Debugger is a legacy TI programmer for the 8051-based CC111x / CC243x /
CC253x / CC254x families over TI's two-wire debug interface. It is not an
ARM/SWD probe, has no RTT and no halt/resume/memory API in any maintained host
tool. The generic `DebugProbe` model must not assume otherwise.

## Options considered

1. **Reimplement the protocol over libusb** — the debug-interface commands are
   documented only through third-party code. High effort, high risk of corrupting
   a target's flash/lock bits, and it needs cgo or a pure-Go USB stack.
2. **OpenOCD / pyOCD** — no CC8051 support for this probe.
3. **TI SmartRF Flash Programmer / Windows tools** — Windows only.
4. **`cc-tool`** (open source, libusb, C++, packaged for Linux, buildable for
   macOS) — supports exactly the probe and chips we have. **Chosen.**

## Decision

`plugins/ccdebugger` drives `cc-tool` as a subprocess, like `jlink` drives
`JLinkExe` (ADR 0012): no cgo, no bundled third-party binary, every vendor
detail confined to the plugin.

- **Discovery needs no external tool**: sysfs scan for VID 0451 PID 16a2/16a0
  (the id is the USB serial, `serial@bus:dev` if serials collide). Without
  `cc-tool` the device is `operations=discovery-only`, `missing=cc-tool`, with an
  install hint.
- **Capabilities**: `DebugProbe` and `FlashProgramming` only. The plugin does not
  advertise `MemoryAccess`, `RTT` or register access, and `info` returns an
  explicit `unsupported` list (`memory_read`, `memory_write`, `registers`,
  `halt`, `resume`, `rtt`) so a client does not have to guess.
- **Methods**: SAFE — `DebugProbe.info`, `read_mac`, `FlashProgramming.read`
  (refuses to overwrite a file), `read_info_page`. CONFIRM — `DebugProbe.reset`,
  `FlashProgramming.erase`, `program` (erase + write + verify; the secondary MAC
  is preserved by default).
- **Safety**: all state-changing calls are CONFIRM and serialised per probe by a
  mutex; every invocation pins `-d bus:dev` so two probes cannot be confused.
  Flash read can contain firmware secrets, so it writes to a caller-chosen path
  and never into the timeline.
- **Speed**: cc-tool's default (slow) mode takes minutes for a 256 KB read; `fast`
  (`-f`) took about 26 s. `fast` defaults to true for reads and false for writes.

## Linux / macOS

- Linux: verified. Needs a udev rule for 0451:16a2 for non-root access.
- macOS: **not verified.** cc-tool is libusb-based and the probe is a plain USB
  bulk device, so it should work, but no macOS run exists. Discovery is sysfs
  based and therefore Linux only; an `ioreg`/`system_profiler` source is a TODO
  (ADR 0009 covers the CGO-free enumeration question).

## Consequences

- Capabilities differ per backend (J-Link: memory/registers/RTT; CC-Debugger:
  flash/identify only). Clients must read the advertised capabilities and method
  list instead of assuming a uniform probe.
- Command/response probes cannot be recorded by the capture pipeline alone; see
  [ADR 0014](0014-steps-and-control-devices.md).

## Validation status

Real hardware (CC-Debugger + CC2530): `info`, `read_mac`, `read_info_page`,
full flash read, and a `hwlab run` with recorded `info`/`read_mac` steps.
**Not run on real hardware** (destructive, target belongs to a user project):
`reset`, `erase`, `program`. Their argv is covered by unit tests only.
