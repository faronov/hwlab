# Phase 8 — CMSIS-DAP results

Hardware: **none available** (`lsusb` shows no CMSIS-DAP probe). This phase is
simulator- and replay-validated only; the USB v1/v2 transports and the SWD
behaviour against real probes and targets are **unvalidated**.

| Item | Result |
|---|---|
| DAP command layer: Info, Connect, Configure, SWJ sequence/clock/pins, Transfer, TransferBlock, ABORT, Reset | unit tests against `dap.Sim` (race detector on) |
| SWD attach: line reset, JTAG-to-SWD, DPIDR, ABORT, power-up, reinit | tests |
| DP/AP/memory access: unaligned edges, 1 KiB TAR wrap, WAIT retry, FAULT + sticky-error recovery, no target | tests |
| Cortex-M: identify, halt, resume, step, core registers, sysreset/hardware reset, DP drop on reset | tests |
| sysfs discovery (v1/v2, preference, non-CMSIS interfaces ignored) | tests on a fake sysfs tree |
| USB v2 (usbfs bulk) and v1 (hidraw) transports | compile and `GOOS=darwin go vet` only: **not run** |
| plugin: listing, danger levels, capabilities (no Flash/RTT), transient halt persistence, memory and register methods, validation, options | tests |
| replay disconnected | pass: `testdata/synthetic-cmsisdap` (labelled synthetic), `TestSyntheticCMSISDAPSessionReplaysWithoutHardware` |
| CLI end to end | `HWLAB_CMSISDAP_SIM=1 hwlab run --yes examples/cmsisdap-sim.yaml`: 8 invoke events; `halt` without `--yes` is refused |
| real CMSIS-DAP probe / target | **not run**: unvalidated |
| flash, JTAG, RTT, SWO | **not implemented** |

## Issues found while building

- Line reset/DPIDR/power-up logic was duplicated between attach and reset
  recovery; it is now one `Target.Reinit()` used by both.
- No real-hardware bugs exist to report: no hardware was involved.

## Known gaps

- Flash programming, JTAG, RTT, SWO.
- Transport errors do not drop the device; a stale USB response needs a reopen.
- Linux only; macOS/Windows unsupported.
- The simulator was written from the same reading of the spec it tests.

## To finish validation

1. Attach any CMSIS-DAP probe (DAPLink, Raspberry Pi Debug Probe, ...) to a
   Cortex-M target; add a udev rule for non-root access.
2. `hwlab devices`, then `DebugProbe.info`, `connect`, `MemoryAccess.read` of the
   vector table.
3. Try halt/resume/step/reset on a disposable target; record a fixture with
   `hwlab run examples/cmsisdap-sim.yaml` with `device:` set to the real probe.
4. Test v1 (HID) and v2 (bulk) probes, a WAIT-heavy slow target and `clock_hz`
   changes.
