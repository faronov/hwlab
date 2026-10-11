# Phase 19: ESP32-C6 full debug + flash over native USB Serial/JTAG

Board: ESP32-C6 on its built-in USB Serial/JTAG (`303a:1001`), USB serial
sanitized as `AA:BB:CC:DD:EE:FF`. The chip was known to be an ESP32-C6; generic
code only reports the JTAG IDCODE (`0x0000dc25`) as evidence plus the ROM
chip-detect word, never as proof. Design: [ADR 0030](../adr/0030-esp32c6-flash-and-debug.md).

No OpenOCD, esptool, ESP-IDF, Python or CGO is needed or was installed (so no
OpenOCD comparison was possible).

## Status per capability

| Capability | Status | Evidence |
| --- | --- | --- |
| USB discovery / selection by serial / transport | REAL_HW_VERIFIED | phase 18 + repeated runs |
| JTAG scan, RISC-V DM 0.13 | REAL_HW_VERIFIED | 1 TAP (IR 5), 2 harts |
| Halt / resume / registers / memory read | REAL_HW_VERIFIED | |
| SRAM write 8/16/32-bit + restore, invalid access refused | REAL_HW_VERIFIED | `TestHWDebugSRAMWriteRestore` |
| Single step (100x, `dcsr.step` restored) | REAL_HW_VERIFIED | `TestHWDebugSingleStep100` |
| Hardware execution breakpoints (4 mcontrol triggers) | REAL_HW_VERIFIED | `TestHWDebugTriggersAndBreakpoints`; 5/5 hits at the programmed `hwlab_tick` |
| Reset: hart, system, SoC (+ USB re-enumeration, also from a halted hart) | REAL_HW_VERIFIED | |
| Physical unplug/replug (2 rounds, user pulled the cable) | REAL_HW_VERIFIED | DEVICE_GONE, rediscovery by serial |
| Flash info/geometry, physical read, backup | REAL_HW_VERIFIED | JEDEC 5e4016 (Zbit), 4 MiB, SFDP 1.6 |
| Flash sector erase / program / verify / restore / boot | REAL_HW_VERIFIED | full cycle, see below |
| Hart 1 debugging | IMPLEMENTED_UNTESTED | hart 1 reports *unavailable* (dmstatus 0x30a2); not labeled as any core type |
| App-partition erase, full-chip erase | SIMULATED_ONLY | deliberately not run on the only board |
| Unplug during a write | NOT VERIFIED | not attempted (could brick the board) |
| eFuse access | NOT_IMPLEMENTED | read-only security flags only; no write path exists |
| OpenOCD cross-check | NOT_IMPLEMENTED | OpenOCD not installed |

## Debug results

* Hart 0: RV32 (`misa 0x40903105`), SBA v1 32-bit only (8/16-bit writes are read-modify-write), 16 progbuf words.
* Hart 1: `unavailable`; a halt request times out, cleanly reported. Hart selection switches repeatedly without touching the other hart.
* Triggers: 4 slots reported by the hart; slot 0 is used by the running firmware and is saved/restored.
* Reset: system ≈20-30 ms (halt at reset vector 0x40000000); SoC ≈0.6-1.1 s gone-to-back.
* A SoC reset requested while the hart is halted in ROM failed at first ("never disappeared"): boot code run after the resume disables the RTC WDT again. Fix: re-arm via SBA until the device disappears (`TestHWDebugSoCResetFromHalted`).
* Performance (this host, USB full speed): register block 121 ms, 16 B read 0.31 ms (was ≈27 ms before batching), 2 KiB 16 ms, 64 KiB 526 ms, flash read ≈65 KB/s (4 KiB 112 ms, 64 KiB ≈1 s), single-sector program ≈12.5 KB/s, 4 MiB backup read 117 s.

## Flash acceptance (`TestHWFlashAcceptance`, 306 s, passed)

1. Fresh full backup, read twice, identical SHA-256, manifest with JEDEC/geometry.
2. 19 error paths rejected (invalid address, outside flash, zero length, overlap, truncated image, bad token, missing backup/permission, multi-region, raw without flag ...), flash unchanged.
3. Corrupted-image verify reported "differ".
4. Programmed a 448-byte acceptance image (`accept-fw`: counter in SRAM, UART banner, `hwlab_tick`) at 0x0 and verified; one sector erased.
5. Boot: counter advanced 3670016 → 4740875; CDC banner `HWLAB-ESP32C6-ACCEPT-1 counter=004c0000`.
6. Debugged the image: halt, PC in image, 64 B SRAM write/restore, 100 steps, 5 breakpoint hits, resume.
7. Restore from the backup; the whole-flash SHA-256 equals the backup; after a SoC reset 12 of 12 PC samples are in flash-mapped code (original firmware runs).

Local artifacts (never committed): `~/hwlab-backups/esp32c6/`. Sanitized
metadata: `testdata/real-espusbjtag-evidence/flash-and-unplug.json`.

## Incident (worth knowing)

The first re-run of the acceptance test, after writes started working, included
an error-path case "spans partitions" that really wrote zeros over the
bootloader, partition table and otadata, because the region check ignored the
bootloader slot/partition table and non-image data was accepted. The flash was
restored byte-for-byte from the fresh backup. Hardening: `regionsTouched`
(program may touch exactly one region), `raw=true` required for data without
the 0xE9 image magic, regression test `TestProgramRefusesRawAndMultiRegion`;
the hardware case now uses a harmless all-FF area and no permission tokens.

The first physical unplug run also found that Linux can report an unplug as
EPROTO before ENODEV; `internal/usb` now maps it to `ErrDisconnected` when the
device node is gone. Both real rounds returned ENODEV, so that mapping is
code-only (unit tested, not hardware-exercised).

## Fixtures

* `testdata/real-espusbjtag-debug/` — REAL session from `examples/espusbjtag-c6-debug.yaml` (scan, identify, system resets, 4 steps, breakpoint hit, SRAM write+restore, flash geometry, SoC reset with re-enumeration). Replay: `TestRealESPUSBJTAGDebugSessionReplaysWithoutHardware`.
* `testdata/real-espusbjtag-evidence/flash-and-unplug.json` — flash cycle and unplug evidence (hashes/metadata only; `TestESPEvidenceIsSanitized`).

## Remaining gaps

Hart 1 on this board, app/chip erase on hardware, unplug during a write, EPROTO
path on hardware, OpenOCD comparison, other Espressif chips, macOS/Windows USB.
