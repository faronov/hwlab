# Phase 21: ST-Link on a real STM32F1, native flash

Probe: ST-LINK/V2 `0483:3748`, firmware V2J39S7, serial sanitized as `38FFAABBCCDDEEFF00112233`,
target voltage ~3.25 V, SWD 100 kHz -> 100, 400 -> 480, 1000 -> 950 kHz (the probe's clock table).
Design: [ADR 0031](../adr/0031-native-stlink-cortexm.md). No OpenOCD, STM32CubeProgrammer, st-flash, libusb,
Python or CGO.

## Target identification (from hardware only)

* DP IDCODE `0x1BA01477`, CPUID `0x411FC231` (Cortex-M3 r1p1), FPB 6+2 comparators, DWT 4.
* `DBGMCU_IDCODE` `0x20036410`: device `0x410`, revision `0x2003` -> **STM32F1 medium-density** (family confidence high).
* The flash-size register reads `0xFFFF` (blank), so 128 KB / 128 pages of 1 KB is a probed readable range,
  not a datasheet value. SRAM 20 KB. **The exact part number is not determined** and the part is not
  called genuine or counterfeit.
* Protection: RDP 0, WRPR `0xffffffff`; the option-byte area has a non-standard layout and was never written.
* The original firmware relocates VTOR, so the reset vector is read after the reset (bug found and fixed).

## Per capability

| Capability | Status | Evidence |
| --- | --- | --- |
| Identify, halt/resume, registers (r0-r15, xPSR, MSP, PSP, CONTROL) | REAL_HW_VERIFIED | repeated cycles |
| SRAM R/W (8/16/32, bulk) + restore, invalid-address refusal | REAL_HW_VERIFIED | byte-exact restore |
| Single-step | REAL_HW_VERIFIED | 100 steps followed program order incl. 32-bit BL; SRAM counter equals executed stores |
| FPB breakpoint | REAL_HW_VERIFIED | 5 hits at `work()` of the hwlab image |
| DWT write watchpoint | REAL_HW_VERIFIED | fired on the counter word |
| System / core reset | REAL_HW_VERIFIED | reset observed, reconnect OK |
| NRST pin reset | IMPLEMENTED_UNTESTED | not observed through the connector; not claimed |
| Flash info, protection read, flash read | REAL_HW_VERIFIED | 64 KiB in 629 ms (~102 KB/s) |
| Backup (two identical full reads) | REAL_HW_VERIFIED | SHA-256 `acba307bbb3fafa322220b35c9add1e5fc31ab4ba54ba9ba58a88255a8fe6a16` |
| Page erase | REAL_HW_VERIFIED | page 0 blank on read-back, neighbours unchanged |
| Program + verify | REAL_HW_VERIFIED | 128-byte hwlab image (`examples/stlink-stm32f1-accept.bin`) via SRAM halfword loader |
| Boot of the programmed image | REAL_HW_VERIFIED | reset PC `0x08000040`, SRAM counter advancing |
| Restore of the original | REAL_HW_VERIFIED | whole-flash SHA equals the original; original reset vector and execution back |
| Full-chip erase | IMPLEMENTED_UNTESTED | per-page behind `allow_chip_erase` + `erase-entire-chip`; never run on the user's board |
| Mass erase (MER), option bytes | NOT_IMPLEMENTED | deliberately absent |
| Open/close x20, concurrent open BUSY, invalid serial | REAL_HW_VERIFIED | |
| Physical unplug/replug x2 | REAL_HW_VERIFIED | DEVICE_GONE, same serial, identity and memory OK; the first attempt timed out (no replug) and was rerun |
| STM32F4 flash | SIMULATED_ONLY | no F4 silicon |

## Throughput

Register block 8 ms; 16 B read 0.7 ms; 2 KiB read 19.6 ms; 64 KiB flash read 629 ms (~102 KB/s);
4 KiB SRAM write 123 ms (~32 KB/s); flash program 8 KiB 1126 ms (~7.3 KB/s, with page erase);
restore of 9 pages 717 ms. A 64 KiB SRAM transfer is impossible on a 20 KB SRAM part.

## Fixtures

* `testdata/real-stlink-stm32-flash/2026-10-09T160528Z-stlink-stm32f1/` - sanitized 44-invoke recording of the whole cycle (replays without hardware).
* `testdata/real-stlink-stm32-evidence/evidence.json` - metadata and hashes only; no firmware bytes, UID or real serial.

## Bugs found and fixed

* reset vector read before the reset (VTOR relocation) -> read after the reset;
* backup gate rejected programming into a page already erased -> blank sectors are accepted;
* protection refusal reported as DataLoss -> PermissionDenied before anything is erased;
* `reset` reported `reset_vector 0x0` when the core was not halted -> fields now only when halted.

## Remaining gaps

NRST unobserved; MER and option bytes not implemented; exact part number undetermined; F4 and other families
simulator-only; JTAG and multi-probe not exercised.
