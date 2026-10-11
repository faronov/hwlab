# Phase 17 — J-Link multi-probe validation (real hardware)

No new plugin: the existing `jlink` plugin was checked against two real probes
and fixed where it assumed a single one. SEGGER J-Link software V9.82. Nothing
was flashed, erased or written.

## Probe serial -> target mapping

Serials below are fictional aliases; the hardware results describe the
original bench. See `testdata/README.md` for fixture sanitization.

| Probe serial | Model (ShowEmuList) | Firmware / hardware | Target | Result |
|---|---|---|---|---|
| `90000001` | SAM-ICE (nickname `jlinkv9`, USB 1366:0105) | `J-Link V9 compiled May 7 2021 16:26:12`, V9.70, VTref ~3.32 V | `EFR32MG1PxxxF256` (Cortex-M4 r0p1, SW-DP 0x2BA01477) | full non-destructive sequence, identical to J-Link Commander |
| `900000002` | J-Link OB-SAM3U128-V2-NordicSem (USB 1366:1025) | `J-Link OB-SAM3U128-V2-NordicSemi compiled Jul 8 2025 10:14:41`, V1.00, VTref 3.3 V | its own `NRF52840_XXAA` (nRF52840 DK) | full non-destructive sequence |

Cross-wiring was tested: the OB probe cannot connect to the EFR32 ("Could not
connect to the target device"); only the SAM-ICE reaches it. Both targets are
Cortex-M4 r0p1 with the same DP id and CPUID (0x410FC241), so the core alone
does not tell them apart; the device-specific registers do: EFR32 DEVINFO
MSIZE `0x0FE081F8` = `00012000` (256 kB flash, 32 kB RAM) and PART
`0x0FE081FC` = `8400109e` (family 0x10, EFR32MG1P); nRF52840 FICR
INFO.PART `0x10000100` = `0x00052840`, INFO.FLASH `0x10000110` = 1024 kB. The
unique-ID registers were deliberately not read or recorded.

## Verified through the plugin and compared with `JLinkExe`

| Item | Result |
|---|---|
| enumerate all probes (serial, model, nickname, firmware, hardware) | both listed; model from `ShowEmuList` (the USB product string of the SAM-ICE is `J-Link_jlinkv9`, i.e. the nickname) |
| select by serial | pass; no serial with two probes is refused by `device.Select` and by the commander |
| connect, core identify | pass, same core/DP/CPUID/ROM table as Commander |
| flash/RAM/DEVINFO read | pass, bytes identical to Commander (`mem8`) |
| halt | pass; confirmed with `IsHalted` in the same session (PC 0x0001634A EFR32, 0x000191DC nRF) |
| registers | pass (core, FPU, special registers) |
| resume, reset (`r`,`g`), reconnect | pass; running state confirmed |
| alternating A/B and concurrent A+B calls | pass; each answer carries the asked serial (`TestHardwareTwoProbesSwitch`) |
| session persistence | `session.json` stores `device_id` = serial and `config.target`; events carry `device` = serial and results `probe_serial`/`target` |
| RTT data from firmware | the EFR32 sensor firmware writes RTT lines; received through the plugin |

Fixture: `testdata/real-jlink-multi/2026-10-08T210527Z-jlink-multiprobe`
(REAL). Two firmware RTT lines that carry the target's EUI and network ids were
removed; the three Commander banner lines remain.
`TestRealTwoJLinkSessionReplaysWithoutHardware` asserts every event against its
serial and target, directly and through replay.

## Bugs found and fixed

- Single-probe assumptions: the sysfs-only probe list (no model, no
  commander-only probes), `-USB` taken from a path-like id, hardware test using
  `devs[0]`, a fixed RTT telnet port (two probes collided).
- `halt`/`resume` reported `ok` for an unreachable or unknown probe
  (`Connecting to J-Link ...FAILED` was not an error).
- A commander answering from another probe is now refused (`S/N:` check).
- `halt`/`resume`/`reset` now report and verify the CPU state with `IsHalted`.
- `parseRegs` returned a bogus `VTREF` register.
- `device.Select` silently picked the first of several probes sharing a name.

## Limits and ambiguities

- **One shared probe per held session.** With two long-running Commander
  sessions (RTT on both probes) a third Commander process can reach only the
  probe of the session that started first; the other fails with "Cannot connect
  to the probe/programmer". Reproduced with plain `JLinkExe`, independent of
  hwlab (SEGGER DLL behavior). Run RTT on one probe and set `rtt: false` on the
  others; the plugin explains this when it sees the failure.
- A standalone `halt` does not survive the call: Commander resumes the target
  when it exits.
- The SAM-ICE reports `J-Link V9` firmware and hardware V9.70 with the licence
  list of a standard V9; authenticity cannot be proven from here.
- USB bus/device numbers (and `usb_device`) change on replug; the serial is the
  only stable key. The probe needs a udev rule for 1366 (device node was
  root-only for the SAM-ICE until one was added).
- Not run: memory write, flash, erase, RTT on the DK (no RTT block in its
  firmware).
