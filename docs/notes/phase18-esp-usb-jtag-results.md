> Superseded in scope by [phase 19](phase19-esp-flash-debug-results.md): reset, writes, step, breakpoints, flash and unplug/replug were added and verified later. This note records the read-only phase.

# Phase 18 — Espressif USB Serial/JTAG + RISC-V DM (results)

Scope: the `espusbjtag` plugin ([ADR 0029](../adr/0029-espressif-usb-jtag-riscv-dm.md)) on a real ESP32-C6.
Everything below marked REAL ran on the attached chip; nothing was erased, flashed, written or reset.

## Status

| Area | Status |
|---|---|
| USB discovery, interface claim | **REAL_HW_VERIFIED** |
| Espressif USB-JTAG transport | **REAL_HW_VERIFIED** |
| JTAG scan / IDCODE | **REAL_HW_VERIFIED** (1 TAP, `0x0000dc25`, IR 5, stable) |
| RISC-V DTM / DM identification | **REAL_HW_VERIFIED** |
| halt / resume / registers (hart 0) | **REAL_HW_VERIFIED** |
| memory read (SBA) | **REAL_HW_VERIFIED** (ROM, SRAM, flash-mapped) |
| reset, memory write, flash, hart 1, other chips | **NOT IMPLEMENTED / UNTESTED** |
| OpenOCD comparison | not possible (not installed) |

## Real device (REAL)

| | |
|---|---|
| USB | `303a:1001`, `Espressif`, `USB JTAG/serial debug unit`, serial = chip MAC (sanitized in fixtures), configuration 1, 3 interfaces |
| Interface 0 | CDC-ACM control, class 0x02, `cdc_acm`, interrupt IN 0x82, `/dev/ttyACM1` |
| Interface 1 | CDC data, class 0x0a, `cdc_acm`, bulk OUT 0x01 / IN 0x81 |
| Interface 2 | vendor class 0xff, no driver, bulk OUT 0x02 / IN 0x83, 64 B; **the only interface claimed** |
| Kernel driver detach | none needed; `cdc_acm` stayed on 0/1 throughout; `/dev/ttyACM1` opened read-only after sessions |
| Capability descriptor | `01 0a 01 08 c0 12 01 00 ff 00`: protocol 1, APB 48 MHz, base 24 MHz, divider 1..255 (1000 kHz -> 24) |
| JTAG chain | 1 TAP, IDCODE `0x0000dc25` (version 0, part 0x000d, manufacturer field 0x612), IR length 5; identical over all scans |
| DTM | dtmcs `0x1071`: version 1, abits 7, idle 1 |
| Debug Module | spec 0.13, authenticated, 2 harts, progbuf 16 words, datacount 2, SBA 32-bit, `hasresethaltreq` |
| Core | misa `0x40903105`, mvendorid `0x612`, marchid `0x80000002`, mimpid 2, mhartid 0 |
| Halt / resume | halt about 8 ms, hart 0 halted at a pc in flash-mapped code; resume returned it to running |
| Registers | x0..x31, pc, dcsr (`0xc3`) read while halted |
| Memory (16 B, SBA, CPU running) | `0x40000000`: `6f100068 00…`; `0x40800000`: `6f00801a 6f00c02d 6f008023 6f004024`; `0x42000000`: `e9040220 1ae20042 ee000000 0d000000`; each about 26 ms; 2048 B read OK |
| Repeated open/close | 10 one-shot CLI scans + 10 hardware-test rounds, all identical |
| Concurrent open | the second opener is refused (interface 2 busy); the first session is unharmed |
| Unplug/replug | **not tested** on real hardware (simulator only) |
| Reset | **not run** (not implemented; `ndmreset` may drop the USB peripheral) |
| OpenOCD | not installed; no cross-check; not in the execution path |

## Fixtures

* REAL: `testdata/real-espusbjtag/` (a recorded session) and `testdata/real-espusbjtag-evidence/evidence.json`.
  The MAC serial is replaced by `AA:BB:CC:DD:EE:FF`. Register values are a snapshot of whatever firmware was running.
* SYNTHETIC: `testdata/synthetic-espusbjtag/` (in-process model; marked `synthetic`).
* Replay tests: `internal/app/espusbjtag_replay_test.go`.

## Tests

* Unit/CI (no device): `go test ./plugins/espusbjtag/... ./internal/riscv/... ./internal/usb/... ./internal/jtag/...`.
* Hardware (`//go:build hardware`, opt-in): `HWLAB_ESPJTAG_HW=1 go test -tags hardware -count=1 -v ./plugins/espusbjtag/ -run Hardware`.

## Remaining gaps

reset (ndmreset/hartreset), memory write, flash, breakpoints/step, hart 1 (probably LP core), program-buffer memory
fallback, faster pipelined reads, other Espressif chips, hot-unplug on real hardware, OpenOCD comparison, chip naming
beyond the IDCODE hint.
