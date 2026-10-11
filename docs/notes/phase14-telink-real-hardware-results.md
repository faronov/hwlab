# Phase 14: Telink native SWire, real-hardware results

Scope: read-only validation of the native Go Telink backend on one real board.
Nothing was erased, programmed or written.

## Real target

| Item | Value |
|---|---|
| Adapter | CH340 USB-UART, VID:PID 1a86:7523, `/dev/ttyUSB0`, 230400 baud, DTR/RTS off |
| Programmer | pvvx-style SWire programmer firmware, version 0x0008, programmer chip id 0x5562, swdiv 5 |
| Target chip id / revision | 0x5562 / 0x02 (TLSR825x family) |
| Variant | **undetermined**: 0x5562 does not tell 8250/8251/8253/8258 apart |
| Flash | JEDEC 0xc86013 (GigaDevice), 512 KiB; 8-byte unique id read but not recorded |
| SRAM | at least 64 KiB at 0x840000 (live data at 0x84fff0, zeros at 0x850000) |

The user stated the board is not a TLSR8258. The observations (chip id 0x5562,
64 KiB SRAM, 512 KiB flash, firmware strings "TLSR8258-Router") are consistent
with an 8258-class part but do not prove it. This is reported as unresolved.

## Compatibility class

**A**: same SWire transport and same target protocol as the TLSR825x reference.
Framing, CRC16, version, chip id, JEDEC, flash read and SRAM/peripheral reads
all worked unchanged.

## Observations

- Flash is only readable with the CPU stalled; with the CPU running the JEDEC
  read returns garbage. hwlab halts, reads, then resumes (cpu_control 0x08).
- Full 512 KiB read took about 33 s. 0x76000..0x80000 is entirely 0xFF: there is
  no factory EUI or calibration block at the TB-04 locations 0x76000/0x77000.
  The TB-04 map does not describe this board.
- Data at 0x0-0x2000, 0x8000-0x50000, 0x5b000-0x62000 and 0x73000-0x76000.
  Image header magic KNLT.
- The PC register (0x6bc) always reads constant 0x775f0055: unreliable.

## Safety consequence

The old default layout was always `tb04-512k`; the JEDEC size matched, so
erase/program would have been allowed on an unverified board. Now a layout is
active only if selected explicitly, the flash size matches, and its evidence
region is non-blank. On this board erase/program are refused
(PermissionDenied; `allowed_on_current_target: false`). Protected regions
discovered for this board: none verified.

## Validated on hardware (native Go, no Python/probe-rs)

- VID/PID discovery (nothing written to unprobed ports)
- programmer info, target presence, chip id/revision, JEDEC id, flash UID
- peripheral and SRAM reads, flash reads (head, 0x76000, full flash)
- 5 repeated flash reads with identical sha256
- 3 reconnects with identical identity
- halt and resume
- refusal of program/erase (default layout and `layout=tb04-512k`)

## Not validated

Flash erase/program/verify-after-write, memory write, single-step, breakpoints,
soft reset, `attach` (RST pin), flash unlock, PC semantics. Simulator-only.

## Real frames (replayed in `plugins/telink/sws/frames_test.go`)

- Version: TX `000000000024`, RX `00000d00` + `080062550105035a0006020005d7cb`
- Chip id: TX `077d000003006d56`, RX `07000300` + `026255615f`
- JEDEC: TX `0500000000e8`, RX `05000300` + `c86013e233`

## Fixtures

- `testdata/real-telink/…`: **real**, read-only run of `examples/telink-real.yaml`
  (flash UID redacted, no SRAM or flash contents).
- `testdata/synthetic-telink/…`: **synthetic**, kept separate.

## Tests

- `sws`: frames (real + damaged), malformed answers, cancellation, FlashUID
- `target`: fail-closed protection table
- `telink`: modify refusal, candidate discovery, cancellation, sim gating
- `//go:build hardware` read-only test (`HWLAB_TELINK_HW_PORT`)
- `internal/app`: real and synthetic replay

## Reference tools

`TlsrPgm.py` (pvvx, MIT-style notice) was fetched as a protocol reference only;
no code was copied. A local `TLSRPGM` checkout, `tlsr_debug.py` and a tc32 probe-rs
fork were not present locally. No external fallback is wired.
