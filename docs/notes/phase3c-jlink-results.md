# Phase 3C — J-Link real-hardware results

Probe: J-Link OB-SAM3U128-V2-NordicSemi on an nRF52840 DK (USB 1366:1025,
fictional serial alias 900000002), SEGGER J-Link software V9.82, target `NRF52840_XXAA`
(Cortex-M4, SW-DP 0x2BA01477). The DK was not reflashed; only non-destructive
operations were run.

| Acceptance item | Result |
|---|---|
| J-Link discovery | pass |
| target connection | pass (`DebugProbe.connect`) |
| target/device information | pass (core, DP id, firmware, hardware, VTref 3.3 V) |
| memory read | pass (flash 0x0, 16 bytes) |
| reset / halt / resume | pass (CONFIRM-gated, run with `--yes`) |
| registers | pass (`read_registers`) |
| RTT | attach pass (telnet banner lines on the timeline); **target RTT data unverified**, the DK firmware has no RTT block |
| replay disconnected | pass: `testdata/real-jlink`, `TestRealJLinkSessionReplaysWithoutHardware` |

## Bugs found on real hardware

- `info` and `connect` ran an empty script; Commander connects lazily so nothing
  was printed. They now send `f` / `connect`.
- The RTT banner line `SEGGER J-Link (unknown) V1.0, SN=...` was reported as
  target output.

## Not done

Memory write, flash and erase (destructive; no reason to run on the DK), JTAG,
RTT channel number in the normalized event.
