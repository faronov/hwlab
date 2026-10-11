# Phase 16 — FTDI / MPSSE / JTAG (results)

Scope: the generic `ftdi` plugin ([ADR 0028](../adr/0028-native-ftdi-mpsse-jtag.md)). The attached
XT-ZB1 (BL702 with Sipeed RV-Debugger-BL702 firmware, emulating an FT2232D) is one profile
(`sipeed-bl702`, `implementation: compatible-emulator`), not the architecture.

## Status

| Area | Status |
|---|---|
| FTDI USB transport | **REAL_HW_VERIFIED** |
| MPSSE transport | **REAL_HW_VERIFIED / PARTIAL** (sync and clocked shifts verified; loopback and clock divisor are not supported by this firmware) |
| JTAG target scan | **IMPLEMENTED_UNTESTED** (no target attached; empty chain observed; target path verified against a SYNTHETIC TAP only) |

## Real device (REAL)

| | |
|---|---|
| USB | `0403:6010`, manufacturer `SIPEED`, product `JTAG Debugger`, serial `FactoryAIOT Pro` (factory string), bcdDevice 0x0500, 12 Mbps, configuration 1, 2 interfaces |
| Interface 0 (channel A) | MPSSE/JTAG; bulk IN 0x81, OUT 0x02, 64 B |
| Interface 1 (channel B) | UART; bulk IN 0x83, OUT 0x04, 64 B; stays with `ftdi_sio` |
| `ftdi_sio` on interface 0 | bound at start, **detached** by the plugin (`usbfs` while open), **re-bound** after close, tty returned |
| Interface 1 during JTAG session | stayed `ftdi_sio`; the tty opened at 115200 and read 0 bytes with no error (nothing written) |
| Selftest | sync `0xAA` -> `FA AA`; loopback **not supported** (pattern came back all ones); clock fixed at 6 MHz; latency 2 ms |
| TAP reset / scan | OK; `no-device` (TDO all ones), identical over 5 scans and 3 recorded scans |
| Close/re-open | 4 cycles OK |
| Concurrent open | refused (EBUSY); the first session is unharmed |
| Unplug/replug | **not tested** on real hardware (simulator only). The udev reload re-enumerated the device |
| OpenOCD | not installed; no cross-check; not in the execution path |

Bug found by the hardware test: the first version detached "whatever driver is bound", so a second
open took the claimed interface away from the first session. Only `ftdi_sio` is detached now
(`TestOnlyFtdiSioIsEverDetached`, hardware `SecondOpenIsRefused`). Also fixed: the MPSSE divisor
check rejected the profile's 6 MHz nominal clock; it is skipped for fixed-clock profiles.

## Fixtures

* REAL: `testdata/real-ftdi-sipeed/2026-10-08T200624Z-ftdi-jtag-scan/` (session) and
  `testdata/real-ftdi-sipeed-evidence/evidence.json` (USB identity, endpoints, driver states
  before/during/after, init log, selftest, scans, UART check, result labels).
* SYNTHETIC: `testdata/synthetic-ftdi/2026-10-08T200639Z-ftdi-jtag-sim/` (one TAP,
  IDCODE 0x1BA01477). Replay tests: `internal/app/ftdi_replay_test.go`.

## Provenance

See `plugins/ftdi/NOTICE` and [licenses](../licenses.md): FTDI AN_108/AN_135 and IEEE 1149.1 for
the protocol; OpenOCD (GPL-2.0) config values only as hardware facts; Sipeed firmware (Apache-2.0)
only as behavioural evidence. No code was copied.

## Remaining gaps

* No JTAG target scanned: no IDCODE, chain or IR length from real hardware; IR/DR shifts untested
  on a real TAP.
* nSRST/nTRST are not driven (board wiring unknown).
* Loopback and clock divisor are ignored by this firmware; GPIO opcodes unconfirmed.
* Unplug/replug on real hardware, real FT2232 silicon and other adapters untested.
* Transport only: no RISC-V DM / ARM DP layer, so no memory/flash/halt/registers.
* Linux only. The `uart` plugin still lists the companion tty as a separate device
  (`usb_device`/`usb_interface` link it to the `ftdi` device), and the interface-0 tty disappears
  while a JTAG session is open.
