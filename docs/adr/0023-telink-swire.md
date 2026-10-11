# ADR 0023: Telink SWire (TLSR8258) as a native Go backend

Status: accepted (implementation **unvalidated on real hardware**)

## Context

The maintainer's zigbee-rs lab flashes and debugs TLSR8258 boards with
`TlsrPgm.py`, `tlsr_debug.py` and a tc32-enabled probe-rs fork. Those are Python
and fork-specific tools. hwlab needs Telink support on the generic
`DebugProbe`, `MemoryAccess` and `FlashProgramming` capabilities without
Telink assumptions in the core, and without Python at runtime.

No Telink SWire programmer or sacrificial TLSR825x board is attached to the
development host. Nothing here has met real hardware.

## Decision

`hwlab-plugin-telink` is a pure-Go plugin in two layers:

- `plugins/telink/sws`: the programmer protocol (a USB-serial SWire
  programmer at 230400 baud, CRC-16/MODBUS framed requests), target debug
  registers, flash primitives and the flash layout guard. It talks to a small
  `Link` interface that `uart.Port` satisfies, so there is no second serial
  implementation.
- `plugins/telink`: the plugin (listing, options, leasing, methods).

### Capabilities and danger levels

| Capability | SAFE | CONFIRM |
|---|---|---|
| DebugProbe | info, connect, status, read_registers | attach (pulses RST), halt, resume, step, reset, set_breakpoint, clear_breakpoint |
| MemoryAccess | read | write |
| FlashProgramming | read, verify | erase, program |

`connect` never activates the target; a target that does not answer is a result,
not an error. There is no chip erase and no UART baud switching.

### Flash safety

The only layout is `tb04-512k`, taken from the zigbee-rs platform guide:

| Range | Class | Rule |
|---|---|---|
| 0x00000-0x70000 | application | erase/program allowed |
| 0x70000-0x76000 | APS/child/security journals | needs `allow_journals` |
| 0x76000-0x77000 | factory EUI-64 | never modifiable, no override |
| 0x77000-0x78000 | factory config + ADC calibration | never modifiable, no override |
| 0x78000-0x80000 | undescribed | refused |

Ranges are expanded to 4 KiB sectors before checking. The layout is refused if
the JEDEC capacity does not match. Programming reads back and verifies. Unlocking
a protected flash (status register = 0) is a persistent side effect and is
reported as `flash_protection_cleared`. Memory writes are limited to SRAM
(0x840000-0x84ffff) unless `allow_registers` is given. Factory data stays
readable.

### Discovery

Serial ports are never scanned (opening a USB-serial port can pulse DTR/RTS and
reset unrelated boards, and `/dev/ttyACM0` here is the nRF DK console). Devices
come from `HWLAB_TELINK_PORTS=name=/dev/ttyUSB0[@baud]`, plus the SYNTHETIC
`sim-telink` with `HWLAB_TELINK_SIM=1`. DTR/RTS are opened low.

### Provenance and licences

- `pvvx/TLSRPGM` `TlsrPgm.py` (carries a permission notice allowing use,
  modification and distribution): read as protocol evidence for framing,
  command numbers and response layouts. The Go code is a reimplementation; the
  CRC is computed, not copied from the table.
- The tc32 probe-rs fork was not needed.
- cc-tool-style GPL code is not involved.
- zigbee-rs scripts and docs: workflow and flash-safety rules only.

## Consequences

- Capability sets differ by backend: no RTT, no chip erase.
- `halted` is inferred from the CPU control byte (0x05/0x06). The state after a
  breakpoint hit on real silicon is unknown; the simulator keeps 0x84.
- The simulator (`sws.Sim`) is synthetic. `testdata/synthetic-telink` proves
  guards and replay only. It is compiled in but only enabled by
  `HWLAB_TELINK_SIM=1`; `TestSimulatorOnlyWhenRequested` checks that it never
  appears in the production device list.
- Layers (phase 14): `plugins/uart` serial -> `sws` (programmer protocol) ->
  `target` (chip-family drivers: SRAM window, flash layouts, chip-id table) ->
  plugin methods. SWire is a transport, not a core abstraction.
- Fail-closed flash protection: a layout (e.g. `tb04-512k`) is applied only if
  it is chosen with the `layout` device option, the JEDEC flash size matches
  and its evidence region (factory EUI-64) is non-blank. Otherwise
  erase/program return PermissionDenied and capabilities report
  `allowed_on_current_target: false`. The connected real board has a blank
  0x76000.. area, so modification is disabled there.
- Discovery lists USB-UART VID/PID candidates without opening them.
- Real read-only validation: see `docs/notes/phase14-telink-real-hardware-results.md`
  and `testdata/real-telink`. Erase, program, memory write, step, breakpoints
  and attach remain unvalidated on hardware.
- Erase running from XIP can hang the bus on real hardware; recovery is untested.

## Deferred

WAIT_RESP (firmware 5+), SWS_PRINTF, FIFO commands, analog registers, OTP
(TLSR8208), programmer UART baud switching, a CMSIS-style adapter for other
Telink families, a tc32 disassembly-aware step. Chip erase is intentionally
never exposed.

## Alternatives considered

- Wrapping `TlsrPgm.py`: adds a Python runtime dependency and hides safety rules.
- probe-rs tc32 fork: heavy, fork-specific, hard to ship.
- A Telink-specific core API: rejected; generic capabilities cover it.
