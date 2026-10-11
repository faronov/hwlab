# Hardware validation + release hardening: results

Distinguishes *code implemented*, *replay/synthetic tested* and *real-hardware
tested*. The machine-readable matrix is `internal/status/registry.go`, shown by
`hwlab plugins --details`, `hwlab doctor`, MCP `plugin_list`/`doctor`, and
rendered in [support-matrix.md](../support-matrix.md).

## Item results

| # | Item | Result |
|---|---|---|
| 1 | Status model | done: implementation (complete/partial/skeleton/missing), validation (synthetic/replay/real-hardware-partial/real-hardware/untested), dependency (native/vendor-sdk/external-tool/optional-external-tool), verified/unverified/gaps; `hwlab plugins`, `hwlab doctor`, MCP, docs |
| 2 | CGO audit | done, [ADR 0026](../adr/0026-cgo-audit.md): only `go.bug.st/serial/enumerator` on darwin (IOKit) needs cgo; ppk2 and nrf802154 for VID/PID lookup. uart, modbus, scpi, telink no longer need it (`make check-cgo`) |
| 3 | Saleae completion | analog: **unsupported by connected hardware** (original Logic, 8 digital channels; "Requested analog channels are unavailable"). SPI: no suitable signal, not run. Shared real signal capture: done (item 7) |
| 4 | J-Link RTT | transport verified with a **host-planted RTT control block** (`rtt_address` config); no RTT-capable firmware was available and no valuable target was flashed → **firmware RTT: hardware-unverified**. Fixture `testdata/real-jlink-rtt`, test `jlink_rtt_replay_test.go` |
| 5 | CC-Debugger native | read-only validation on the private CC2530: USB discovery, adapter info, target ID, debug status, PC, CODE/XDATA reads. Flash erase/program native: **implemented: no, validation: no**. cc-tool optional fallback, never run on a target |
| 6 | UART RX | real: DK UARTE0 TX (P0.06) → J-Link VCOM; 23 bytes, timestamped, fixture + replay test (`testdata/real-uart-rx`) |
| 7 | PPK2 + Saleae shared signal | done: [phase 12](phase12-multi-device-results.md) |
| 8 | Multi-device session | done: one session, offline verification with devices unreachable; fixture `testdata/real-ppk2-saleae` |
| 9 | Timing model | affine `session = a*device + b` stored as evidence, [ADR 0027](../adr/0027-measured-affine-clock-mapping.md) |
| 10 | USB capture status | explicit everywhere: URB / host-controller observation, not a physical bus analyzer, live usbmon unverified |
| 11 | License | [licenses.md](../licenses.md): Apache-2.0 recommended and safe based on the current source composition; no LICENSE file added |

## How the DK test signal is produced (no flash change)

All through `hwlab invoke jlink/... MemoryAccess.write`, with data quoted so YAML
keeps it a string (`-a 'data="04000000"'`). nRF52840 registers:

| Register | Address | Value (LE bytes) |
|---|---|---|
| TIMER4 MODE, BITMODE | 0x4001B504 | `0000000003000000` (timer, 32-bit) |
| TIMER4 PRESCALER | 0x4001B510 | `04000000` (1 MHz) |
| TIMER4 CC0 | 0x4001B540 | `88130000` (5000 µs) |
| TIMER4 SHORTS | 0x4001B200 | `01000000` (COMPARE0→CLEAR) |
| GPIOTE CONFIG0/1 | 0x40006510 | `0303030003040300` (toggle tasks P0.03, P0.04) |
| PPI CH0 EEP/TEP | 0x4001F510 | `40b1014000600040` (TIMER4 COMPARE0 → GPIOTE OUT0) |
| PPI FORK TEP0 | 0x4001F910 | `04600040` (→ GPIOTE OUT1) |
| PPI CHENSET | 0x4001F504 | `01000000` |
| TIMER4 START | 0x4001B000 | `01000000` |
| TIMER4 CLEAR | 0x4001B00C | `01000000` (needed after shortening CC0) |

A DK reset clears the setup.

## Findings worth keeping

* PPK2 logic-port VCC must be wired to the target I/O voltage; D0 was always 0
  until it was.
* The J-Link OB occasionally re-enumerates as 1366:0101 (root only under the
  1366:1025 udev rule); a DK USB replug restores it.
* After a USB-level Logic 2 wedge (`GetDevices` hangs) only a physical replug of
  the Saleae helped.
* Saleae single-channel capture at 24 MHz yielded no events; 4 MHz works.
* PPK2 timestamps drift 92.6-94 ppm against the Saleae and show occasional
  re-timed samples; fixed offsets are insufficient.
* Phase 3b's UART/I2C "decoded" frames came from floating lines: pipeline proof
  only.
