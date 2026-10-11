# Phase 26: CC2340 native SACI, reset, CCFG and flash

Same target and probe as [phase 25](phase25-cc2340-m0plus-acceptance.md): one
user-owned CC2340 (E104-BT55SP) on an LP-XDS110ET (pin 1 GND, 5 3V3, 11 nRST,
13 SWDIO, 15 SWCLK), driven through the native XDS110 backend and through the
CMSIS-DAP HID backend. Everything runs in Go; no UniFlash, Code Composer Studio,
OpenOCD, Python or cgo. The design is [ADR 0032](../adr/0032-cc23xx-target-layer.md).

## What was built

- `internal/ti/cc23xx/saci`: SACI mailbox protocol (CFG-AP raw access).
- `internal/ti/cc23xx`: session (enter and leave SACI, clock cap), reset (nRESET
  and SYSRESETREQ, both ending in SACI), identify, CCFG parser (`ccfg/`), flash
  (`flash.go`), simulator (`ccsim/`), and the shared `service/` that both plugins call.
- Plugins `xds110` and `cmsisdap`: the `FlashProgramming` capability, `saci_*`
  methods, reset routed through the service, automatic AHB-AP recovery before core
  methods. CLI `hwlab target cc23xx ...`, `hwlab target flash ...`, MCP `target_op`.
- Safety: every destructive call needs `--yes`, a token (`erase-entire-chip`,
  `program-flash`, `restore-flash`, `program-ccfg`, `accept-ccfg-security-change`),
  the device-level permissions `allow_flash`, `allow_erase`, `allow_chip_erase`,
  `allow_ccfg_write`, and a verified backup. Secrets (die ID, user record,
  password, hash) are never logged or returned.

## Reset: obsolete phase-25 conclusion

Phase 25 concluded that the CC2340 re-gates its MEM-AP after a reset and only a
10 s power cycle recovers it. That was incomplete: every reset passes through the
SACI boot ROM, and leaving SACI re-opens the AHB-AP. With the SACI layer:

- reset while running, while halted, reset-and-halt (stops at the application reset
  handler), through nRESET and through SYSRESETREQ: confirmed, DPIDR 0x6BA02477 and
  CPUID 0x410CC601 identical afterwards, no power cycle.
- 50 reset/reconnect loops per probe cycling hardware, sysreset, sysreset+halt and
  hardware+halt: **0 failures** (XDS110 6.7 s, CMSIS-DAP 11.1 s).
- DAP_ResetTarget: UNSUPPORTED_BY_ATTACHED_HW on CMSIS-DAP (the probe executes
  nothing); the XDS110 does not offer it.

### The 400 kHz SYSRESETREQ cap (empirical)

After SYSRESETREQ the first SACI mailbox command leaves `TXCTL.TXD_FULL` stuck at
SWD clocks of 600 kHz and above (BOOTSTA 0xC2 or a mailbox timeout). Clean-start
experiments: 400 kHz and 500 kHz work, 600 kHz, 800 kHz, 1, 2, 4 MHz fail; capping
to 400 kHz before the AIRCR write fixes all of them. nRESET entry works at every
clock up to 4 MHz. **The cause is not understood.** `SysresetSACIClockHz` is
400 kHz, restored after the first SACI exchange. A failed attempt can leave the
mailbox stuck across runs until something un-sticks it; keep that in mind when
experimenting.

### Power cycle: not exercised

`target_power` off for 2 s and 12 s did not remove the target's power on this
board. Phase 27 found why: the LP-XDS110ET has no switchable target supply
(`XDS_SET_SUPPLY` is acknowledged and changes nothing; the 3V3 pin stays 3.3 V), so
`target_power` is now UNSUPPORTED_BY_ATTACHED_HW on it and power-cycle recovery is
NOT_IMPLEMENTED until an external PowerSwitch exists. See
[phase 27](phase27-lp-xds110et-target-power.md) and
[ADR 0033](../adr/0033-power-switch.md). The old note that phase 25 recovered after a
10 s supply-off is superseded: that recovery was not a power cycle.

## Destructive flash acceptance (REAL_HW, 47 s)

`TestCC2340FlashDestructive` (guards below). Flow: XDS110 info, ccfg_info, backup
(512 KiB, verified, identical reads) → XDS110 chip erase (18 ms) → program the
160-byte acceptance firmware (erase 12 ms, program 6 ms, verify 218 ms) → boot,
RunFirmware and the generic phases through XDS110, then verify and the same
phases through CMSIS-DAP → CMSIS-DAP restore of the original (1727 ms, 23 non-blank
sectors, 46080 bytes) → full read-back byte- and SHA-256-identical to the backup,
CCFG unchanged, vector table equals the original.

Results: XDS110 148 pass / 0 fail, CMSIS-DAP 153 pass / 0 fail.

The acceptance firmware (`internal/targetaccept/accfw.go`, hand-assembled Thumb,
marker `HWLAB-CC2340-ACCEPT-1`): a counter at SRAM 0x20008F00 incremented in a loop
that calls `bp_func`; it proves the application runs, a breakpoint on `bp_func`
is hit 5 of 5 times with the counter advancing, the counter is frozen while halted,
reset-and-halt stops at the reset handler, and nRESET and SYSRESETREQ restart it.

Not done on purpose: a real CCFG write (a wrong CCFG can lock the chip); the
CCFG programming path is IMPLEMENTED_UNTESTED (simulator only). The pipelined
program command is NOT_IMPLEMENTED. Program through CMSIS-DAP was exercised only
as the restore.

## Speed ladder (REAL_HW, 76 s)

`TestCC2340SpeedLadder`. The device is reopened per clock. Both probes are stable
(0 failures, flash read equal to the reference) at 100 k, 400 k, 1 M, 2 M, 4 M, 8 M
and **10 MHz, the highest tested**. Neither probe reports the clock it generates;
the numbers are requested clocks.

| Medians (ms) | 100 k | 400 k | 1 M | 2 M | 4 M | 8 M | 10 M |
|---|---|---|---|---|---|---|---|
| XDS110 register block | 105 | 33.6 | 18.8 | 14.0 | 13.2 | 10.6 | 11.9 |
| XDS110 16 B read | 8.2 | 2.5 | 1.28 | 0.93 | 0.66 | 0.54 | 0.59 |
| XDS110 2 KiB read | 252 | 65.7 | 29.1 | 15.9 | 9.9 | 6.8 | 5.1 |
| XDS110 256 B write verified | 73.5 | 19.9 | 9.4 | 5.7 | 3.9 | 2.6 | 2.4 |
| XDS110 100 steps | 2028 | 665 | 372 | 287 | 246 | 245 | 237 |
| CMSIS-DAP register block | 176 | 79 | 62 | 32.7 | 36.6 | 34.4 | 33.5 |
| CMSIS-DAP 16 B read | 9.6 | 4.0 | 2.8 | 2.0 | 2.0 | 2.5 | 2.6 |
| CMSIS-DAP 2 KiB read | 286 | 87.8 | 45.3 | 30.5 | 29.0 | 31.6 | 32.6 |
| CMSIS-DAP 256 B write verified | 84 | 28 | 15.8 | 10.2 | 10.8 | 12.3 | 12.8 |
| CMSIS-DAP 100 steps | 3645 | 1655 | 1300 | 675 | 687 | 724 | 733 |

CMSIS-DAP saturates at about 2 MHz (USB HID latency).

## Hardware suites (all opt-in)

| Suite | Test | Guard |
|---|---|---|
| non-destructive (default hardware run) | `TestCC2340BothBackends` | `HWLAB_CC2340=1` (`_PHASES`, `_SKIPROM`, `_REPORT_DIR`, `_RESET`, `_POWER`, `_SERIAL`) |
| destructive flash | `TestCC2340FlashDestructive` | `HWLAB_CC2340_FLASH=1` and `HWLAB_CC2340_DESTRUCTIVE=erase-and-restore-this-chip` (`_BACKUP_DIR`) |
| speed | `TestCC2340SpeedLadder` | `HWLAB_CC2340_SPEED=1` |
| reset loop and power | `TestCC2340ResetLoopAndPowerCycle` | `HWLAB_CC2340_POWER_RESET=1` (`_RESET_LOOPS`, default 50) |

Run with `go test -tags=hardware -count=1 -v -run <Test> ./internal/targetaccept/`.
Non-destructive result: XDS110 130 pass / 0 fail, CMSIS-DAP 135 pass / 0 fail, the
cross-backend `Compare` clean (3 runs in a row). Facts that are a property of the
transport (`reset.dap_reset*`) are not compared.

## Fixtures

`internal/targetaccept/testdata/cc2340/`: `xds110.json`, `cmsisdap.json` (non-destructive),
`flash-xds110.json`, `flash-cmsisdap.json`, `flash-timing.json`, `speed.json`. They
hold no die ID, user record, password, hash or flash dump (only the first 32 flash
bytes, the acceptance image's vector table). Replay tests in `fixture_test.go` need no hardware.

## CC2340 facts

AP0 AHB (IDR 0x84770001), AP1 CFG-AP (0x002E0001), AP2 SEC-AP (0x002E0000);
DEVICEID 0x1BB8402F, raw PARTID 0x80012DDA (no marketing name is derived); 512 KiB
flash, 256 sectors of 2 KiB, 2 banks, 16-byte program granule; SRAM at 0x20000000;
FPB 4 comparators; no DWT; CCFG valid, debug open. Only whole-chip erase exists in
SACI, so the service always erases the chip, then programs MAIN, verifies by CRC,
programs CCFG, verifies.

## Remaining gaps

- SYSRESETREQ SACI clock cap: cause unknown.
- Power-cycle recovery not exercised (the probe supply does not cut this board).
- CCFG write not exercised on hardware; pipelined program not implemented;
  `flashProt` / `chipEraseRetain` offsets not decoded (retain defaults to 0).
- Probes do not report their real clock.
- Only one CC2340 board; other CC23xx parts untested.
