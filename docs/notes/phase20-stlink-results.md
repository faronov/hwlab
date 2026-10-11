# Phase 20: native ST-Link V2 debug (SWD, Cortex-M)

Probe: ST-LINK/V2 `0483:3748` (firmware V2J39S7, USB serial sanitized as
`38FFAABBCCDDEEFF00112233`), target an **nRF52840 DK** (Cortex-M4, DP IDCODE
`0x2BA01477`, CPUID `0x410FC241`). Not an STM32. Design: [ADR 0031](../adr/0031-native-stlink-cortexm.md).
No OpenOCD, STM32CubeProgrammer, st-flash, libusb, Python or CGO was used or is needed.

## Per capability

| Capability | Status | Evidence |
| --- | --- | --- |
| USB transport, discovery, selection by serial | REAL_HW_VERIFIED | 0483:3748, IF0 OUT 0x02 / IN 0x81 / IN 0x83 |
| SWD, identify, halt/resume, registers | REAL_HW_VERIFIED | 25 halt/resume cycles |
| Memory read | REAL_HW_VERIFIED | ~102 KB/s |
| SRAM write + restore (8/16/32, unaligned) | REAL_HW_VERIFIED | invalid addresses refused |
| Single-step | REAL_HW_VERIFIED | 100 steps, 77 sequential + 23 loop branches |
| FPB breakpoints | REAL_HW_VERIFIED | 5 real hits at the reset loop |
| DWT watchpoint | REAL_HW_VERIFIED | one real write hit |
| System / core reset | REAL_HW_VERIFIED | reset observed, PC at the reset vector |
| NRST pin reset | IMPLEMENTED_UNTESTED | NRST not wired; nothing was observed |
| Reconnect, 10 reopens, concurrent open refused | REAL_HW_VERIFIED | |
| Physical unplug/replug (2 rounds, user pulled the cable) | REAL_HW_VERIFIED | DEVICE_GONE, slowest call 224 ms, same serial and identity |
| SWD clocks | REAL_HW_VERIFIED | 4000, 1800, 950, 480, 240 kHz |
| JTAG transport, several probes | SIMULATED_ONLY | one probe, SWD-only target |
| STM32 flash info/read/erase/program/verify/backup/restore | UNSUPPORTED_BY_ATTACHED_HW | simulator only |
| Option bytes/RDP, SWO, V1/V3/V2-1 | NOT_IMPLEMENTED | |

## Findings on real hardware

* The probe sends **no reply to DEBUG_EXIT**: waiting for one made each close take 2 s. Fixed (close 2.0 s -> 0.15 ms, hardware suite 58 s -> 8.8 s).
* The USB serial descriptor is UTF-16 with units <= 0xFF; sysfs shows it garbled (`38c3bf...`), so the raw descriptor is read.
* After a stall both pipes need ClearHalt and an unread reply must be drained first.
* NRST is not wired: SYSRESETREQ/core reset were observed, pin reset was not and is not claimed.

## Throughput (SWD 1800 kHz)

| Operation | Time |
| --- | --- |
| register block | 8.2 ms |
| 16 B read | 0.49 ms |
| 2 KiB read | 19.6 ms (~102 KB/s) |
| 64 KiB read | 628 ms (~102 KB/s) |
| 4 KiB SRAM write | 123 ms (~32 KB/s, not optimized) |

## Safety

Nothing was erased, programmed or written outside SRAM scratch (restored). Flash
is refused on a target that is not a recognised STM32. Option bytes, RDP and eFuse-like
settings have no write path. See `testdata/real-stlink-debug` (replayable session)
and `testdata/real-stlink-evidence/evidence.json`.

## Reproduce

    HWLAB_STLINK_HW=1 go test -tags hardware -count=1 -run HW -v ./plugins/stlink/
    # needs a person:
    HWLAB_STLINK_HW=1 HWLAB_STLINK_UNPLUG=1 go test -tags hardware -run HWUnplugReplug -v -timeout 40m ./plugins/stlink/
    HWLAB_HOME=/tmp/x hwlab run --yes examples/stlink-debug.yaml
