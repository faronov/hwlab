# Telink / SWire backend — investigation (pre-design)

Status: **superseded by [ADR 0023](../adr/0023-telink-swire.md)** (implemented against a simulator; still no Telink hardware validated). Original status: investigation only.
Scheduled after the currently available real-hardware plugins and the generic
foundations (UART, CAN, SCPI, CMSIS-DAP, Modbus). It must not block them.

## What exists today (read, not copied)

| Source | License | What it gives |
|---|---|---|
| `zigbee-rs/tools/telink-tlsr8258-lab/scripts/tlsr8258.sh`, `tools/tlsr8258-firmware.sh` | maintainer's repo | The working lab workflow: flash (`TlsrPgm.py -m we 0 image.bin`), SRAM dumps, programmer info, breakpoint, step, go, `probe-rs` attach/debug/gdb via `sws:<serial-port>` / chip `TLSR8258`; post-link layout checks that keep images below `0x70000`. |
| `zigbee-rs/docs/book/src/platform-guides/telink.md` | maintainer's repo | Flash ownership and safety rules (below). |
| `pvvx/TLSRPGM` (`TlsrPgm.py`, 1861 lines) | MIT-style permission notice | The host side of the SWire programmer: CRC16-framed command protocol over a serial port (flash read/write/sector erase/chip erase/JEDEC id/status, SWire read/write/FIFO, version, SWire config, external power/reset, activate, breakpoints, step), plus the debug logic on top. |
| `modern-tc32/probe-rs` (`probe/telink_sws.rs`, ~1100 lines, `architecture/tc32`, `targets/TLSR8258.yaml`) | Apache-2.0 (probe-rs fork) | The same protocol re-implemented in Rust as a probe-rs `DebugProbe` plus a tc32 architecture layer. Evidence that the protocol is stable and re-implementable. |

The Python tools and the probe-rs binary are **not installed on this host**, and no
Telink SWire programmer is currently connected.

## Findings

1. **The SWS wire is not bit-banged by the host.** The host talks a framed
   request/response protocol (`cmd, sub-cmd, length, payload, CRC16`; default
   230400 baud, 6-byte minimum response, 1024-byte read blocks) to a Telink SWire
   master adapter that owns the single-wire timing. The adapter is a normal serial
   device from the host's point of view.
2. Therefore a **native Go transport is maintainable**: pure Go, over a serial
   port (reusing the generic `uart` transport/lister), no cgo, no Python, no
   Rust. Roughly 1–1.5 kLOC for transport + flash + SWire memory access.
3. The **debug layer** (halt/run/step/breakpoint on the tc32 core) is the part
   with real design risk: it is implemented via SWire register access in both
   references and is only exercised by the experimental tools. It needs a
   reviewed register-level spec and hardware evidence before it is exposed.
4. Both references are permissively licensed, so porting is allowed with
   attribution, unlike the GPL cc-tool case (ADR 0018). The ADR must still record
   provenance per file.

## Target architecture (proposal, to become an ADR when scheduled)

```text
hwlab-plugin-telink
    -> native Go SWS/SWire transport (framed protocol over serial; Transport interface + simulator)
    -> TLSR8258 (family table for other TLSR825x later)
optional fallbacks during bring-up: TlsrPgm.py, tc32 probe-rs
```

- Generic capabilities only, no Telink assumptions in core: `DebugProbe`
  (discovery/identify/attach/halt/resume/step/breakpoint), `MemoryAccess`
  (SRAM/registers read and write), `FlashProgramming` (erase/program/verify). A
  backend advertises only what it implements, as with J-Link and CC-Debugger.
- Discovery by serial port plus a `get_version` handshake; identification from
  programmer version and target chip registers. A port is never opened for
  writing during enumeration.
- Same explicit access/permission model as ADR 0018: separate allowances for CPU
  control, reset, memory read, memory write, flash write and erase.
- External TlsrPgm/probe-rs backends are optional fallbacks, never the final design,
  and are selected by config.
- A simulator of the programmer firmware gives hardware-independent CI; a short
  real fixture is recorded for replay as for every plugin.

## Flash safety rules to preserve (from the zigbee-rs Telink guide)

TB-04 / 512 KiB layout:

```text
0x00000..0x70000  application
0x70000..0x72000  APS binding/group/application-key journal
0x72000..0x74000  child-table journal
0x74000..0x76000  security journal
0x76000..0x77000  factory EUI-64
0x77000..0x78000  factory config and ADC calibration
```

- **Never erase or overwrite factory EUI/config/calibration (`0x76000..0x78000`)
  by default.** Chip erase is refused unless explicitly forced, and the factory
  region must be read back and saved to an artifact first when an override is used.
- Default program/erase range is the application area only; the NV journals are a
  separate explicit opt-in (flashing a lab image must not wipe a commissioned
  device's security state).
- Refuse an image that reaches `0x70000` unless the caller widens the range.
- Verify geometry (flash size, JEDEC id) before any factory-region access on
  non-512 KiB parts; the layout is per-board configuration, not hard-coded to one
  board.
- Flash erase/program routines must run from RAM on the target side; the adapter
  protocol hides this, but the guide records that erase from XIP hangs the bus, so
  recovery behavior must be tested.
- Zbit/low-voltage flash writes need the ADC/PC5 voltage guard in firmware; host
  tooling must surface a failed/low-voltage status rather than retry blindly.
- Every write, erase, flash, register write and reset/halt is CONFIRM; reads
  (info, memory/flash read, dump) are SAFE.

## Open questions before design

- Which SWire master hardware will be used (TB-04/03F + USB-UART, TLSR8269 module
  with direct USB, or a custom adapter) and its firmware version range.
- Exact framing and error codes, to be confirmed against the programmer firmware
  sources and independently observed serial traffic, not only the two clients.
- tc32 halt/step/breakpoint register semantics and a TLSR8258 target to test on
  without risking factory data.
- macOS/Windows: pure serial, so portable once the serial layer is.

## Roadmap placement

Added to [ROADMAP.md](../../ROADMAP.md) under Debug, ordered after CMSIS-DAP and
Modbus in the hardware plan. Validation status: **not started**.
