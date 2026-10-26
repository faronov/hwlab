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
| DebugProbe | info, connect, status, read_registers, chip_info, list_breakpoints | attach (pulses RST), halt, resume, step, reset, set_breakpoint, clear_breakpoint, wait_halt |
| MemoryAccess | read | write, watch |
| FlashProgramming | read, verify, info | erase, program, backup, restore |

`connect` never activates the target; a target that does not answer is a result,
not an error. There is no chip erase and no UART baud switching.

`watch` samples one SRAM window back-to-back for up to 300 s and reports
awake/unreachable segments, so a deep-retention sleeper's short wakes can be
seen (a CLI invocation per sample is far too slow). Its optional `prime`
writes and verifies up to four SRAM blocks on the first awake sample; with
`halt_on_wake` the core is stalled for the writes and always resumed. It is
CONFIRM because `prime` writes memory and halts the core. Results:
`docs/notes/telink-watch-retention-hardware-results.md`.

### Flash safety

Two layouts describe the TB-04 map from the zigbee-rs platform guide:
`tb04-512k` (confirmed when the factory EUI-64 sector holds data) and
`tb04-512k-nofactory` (confirmed only when every byte of both factory sectors
reads 0xFF; zigbee-rs then derives the EUI-64 from the flash unique id). Both
keep the factory sectors unmodifiable:

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

### Backup gate (phase 15)

Erase, program and restore additionally require:

- the CPU captured (`DebugProbe.halt`), so the firmware cannot race the flash;
- `backup=<file>` written by `FlashProgramming.backup`: the whole flash read
  twice with identical SHA-256, stored 0600 with a manifest
  (`hwlab-telink-flash-backup/1`) binding it to the flash unique id, JEDEC id,
  chip id and size. The backup must match the connected board, its SHA-256 must
  match the manifest, and every factory region must still equal the backup.

`FlashProgramming.restore` rewrites only sectors that differ from the backup
and reads the whole flash back. Factory and undescribed sectors are never
written; journals need `allow_journals`, or `skip_journals` keeps the current
journals. Restoring an old security journal rolls back NWK/APS frame counters,
so it must never be done on a board that has been on a live network.

### Transport recovery (phase 15)

On the CH340 link about one session in 100-300 got a malformed first answer
right after the port was opened. The port now settles 50 ms and is flushed after
opening (as TlsrPgm does), stray bytes before an answer header are skipped (at
most 64; the CRC still has to match), and idempotent reads (version, flash
read/JEDEC/status/CRC/unique id, SRAM/debug-block/chip-id SWire reads) are
retried twice after draining the line. Writes, erases, flash status writes,
reset and activation are never retried. CPU capture retries the control write up
to 4 times and checks the control byte each time. Recoveries are reported as
`link_recoveries`.

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
- `halted` is the control byte 0x05/0x06 or bit 31 of 0x610 dword 0 (set while
  the core is stopped in debug). A comparator stop keeps control 0x08, so it is
  reported as halt_reason=breakpoint; step and flash operations first put it in
  stall (0x06). 0x05 holds the core at the reset vector (PC reads 0, resume
  reboots); attach leaves the core there. Three comparators (0x610 dwords 1..3,
  `addr | 0x01000000`) are live as soon as written. PC write (0x6bc) is not
  accepted by the hardware and is not offered. Reads below 0x800000 hit the
  core register block, so `MemoryAccess.read` requires `allow_registers` there.
  Details: `docs/notes/telink-debug-hardware-results.md`.
- The simulator (`sws.Sim`) is synthetic. `testdata/synthetic-telink` proves
  guards and replay only. It is compiled in but only enabled by
  `HWLAB_TELINK_SIM=1`; `TestSimulatorOnlyWhenRequested` checks that it never
  appears in the production device list.
- Layers (phase 14): `plugins/uart` serial -> `sws` (programmer protocol) ->
  `target` (chip-family drivers: SRAM window, flash layouts, chip-id table) ->
  plugin methods. SWire is a transport, not a core abstraction.
- Fail-closed flash protection: a layout (e.g. `tb04-512k`) is applied only if
  it is chosen with the `layout` device option, the JEDEC flash size matches
  and its evidence region (factory EUI-64) is non-blank (or, for
  `tb04-512k-nofactory`, both factory sectors are entirely blank). Otherwise
  erase/program return PermissionDenied and capabilities report
  `allowed_on_current_target: false`.
- Discovery lists USB-UART VID/PID candidates without opening them.
- Real validation: see `docs/notes/phase14-telink-real-hardware-results.md`,
  `docs/notes/phase15-telink-flash-hardware-results.md` and
  `testdata/real-telink`; step and breakpoints in
  `docs/notes/telink-debug-hardware-results.md`. Memory write remains
  unvalidated on hardware.
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
