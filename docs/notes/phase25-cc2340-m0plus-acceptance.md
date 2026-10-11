# Phase 25: CC2340 / Cortex-M0+ debug acceptance

One user-owned CC2340 module on the LP-XDS110ET 20-pin connector (pin 1 GND,
5 3V3, 11 nRST, 13 TMS = SWDIO, 15 TCK = SWCLK), powered through
`DebugProbe.target_power` 3300 mV. The same probe and target were driven through
both backends in one test run: the native XDS110 plugin (USB interface 2) and the
CMSIS-DAP HID plugin (interface 5). No flash was written and the SACI / SEC-AP
interfaces were never touched.

## What changed in `internal/cortexm`

- No capability is decided from the architecture name any more. The FPB is
  discovered from `FP_CTRL` (comparator count, literal slots, revision) and the DWT
  from `DWT_CTRL.NUMCOMP`; a unit that is not there returns `ErrAbsent`, which the
  plugins map to `FailedPrecondition` with the prefix `UNSUPPORTED_BY_ATTACHED_HW`.
- `AIRCR.VECTRESET` is only offered when the core has it (`Identity.HasVectReset`).
- `Reset` reports `ErrDebugLost` (and `ResetResult.DebugLost`) when the debug port
  goes away with the request or never answers with C_DEBUGEN set afterwards.
  The reset is then not claimed as confirmed; the plugin returns `debug_lost: true`.
- `DebugProbe.disconnect` releases the SWD attachment (target power stays on) so
  the other backend can take the probe.
- Shared harness `internal/targetaccept`: identical phases on any backend,
  `Compare` for the cross-backend consistency check. Sim tests in
  `plugins/cmsisdap/targetaccept_test.go`; the hardware test is
  `TestCC2340BothBackends` (`-tags=hardware`, `HWLAB_CC2340=1`).

## Capability discovery (real target)

| Item | Value |
|---|---|
| DPIDR / MEM-AP 0 IDR | 0x6BA02477 / 0x84770001 (AP1 0x002E0001 and AP2 0x002E0000 are TI-proprietary, not written) |
| CPUID | 0x410CC601, Cortex-M0+ r0p1, ARMv6-M |
| ROM table | SCS 0xE000E000, BPU 0xE0002000. No DWT |
| Breakpoints | FP_CTRL 0x40: 4 code comparators, 0 literal, FPB revision 0, Code region only (< 0x20000000) |
| Watchpoints | DWT_CTRL reads 0 (NUMCOMP 0): UNSUPPORTED_BY_ATTACHED_HW |
| VECTRESET | absent (baseline AIRCR): UNSUPPORTED_BY_ATTACHED_HW |

## Results (both backends: 128 PASS, 0 FAIL, 2 UNSUPPORTED_BY_ATTACHED_HW, 18 INFO)

- SRAM write/restore: scratch window 0x20004780..0x20004880; 8/16/32-bit, aligned
  boundary, zero/ones patterns, unaligned and oversize accesses refused,
  bus faults recovered, original bytes restored exactly.
- Registers: r0, r4, r8, r12, lr written, read back and restored; PC/SP/xPSR
  untouched; writes while running refused.
- 100 steps: every step halt-confirmed, PC deltas +2 x79-80, +4 x2, branches x18-19.
- Breakpoints: 5/5 rounds hit at the requested PC, all 4 comparators usable, a 5th
  and an out-of-Code-region address refused, FP_CTRL/FP_COMP0-3 restored bit-exact
  (the reset values of FP_COMPn are not zero, so they are saved and restored).
- Reset: see below.

## Reset: the CC2340 re-gates its MEM-AP (superseded by phase 26)

> **Superseded.** Phase 26 ([notes](phase26-cc2340-saci-flash.md)) found that every
> reset passes through the SACI boot ROM; the hwlab CC23xx layer now re-opens
> the AHB-AP through SACI, so reset and reset-and-halt work without a power cycle
> (50 loops per probe, 0 failures). The text below is the phase-25 observation as
> it was recorded.

SYSRESETREQ (running, halted, with halt) and nRESET both reset the part, but the
SWD link drops at the request and MEM-AP 0 reads IDR 0 afterwards; DPIDR still
answers. For nRESET the core registers read 0 / differently, which is the
evidence. The reset is therefore reported as `debug_lost` or
`core_registers_changed`, and "reset and halt" cannot halt at the reset vector.
A reset-handler breakpoint does not survive it. Debug access returned only
after a **target power cycle with the supply off for about 10 s** (3 s and 5 s
did not recover the part, whether or not the probe was closed in between).
The hardware test power-cycles through `target_power` for that. A SACI-based
reset (per SEGGER, the CC2340's real reset path) is not implemented.

## Performance (medians of 15, 400 kHz)

| Operation | XDS110 | CMSIS-DAP HID |
|---|---|---|
| register block read | 17.8 ms | 61.1 ms |
| 16 B SRAM read | 0.60 ms | 1.64 ms |
| 2 KiB SRAM read | 28.4 ms | 44.1 ms |
| 256 B SRAM write, verified | 8.7 ms | 15.0 ms |
| 256 B SRAM write, unverified | 4.4 ms | 7.5 ms |
| 100 steps (one call) | 398 ms | 1299 ms |

## Fixtures

`internal/targetaccept/testdata/cc2340/{xds110,cmsisdap}.json` (sanitized: no
serial). `fixture_test.go` loads both without hardware, requires 0 failures, the
expected facts and no cross-backend difference.
