# XDS110 two-pin cJTAG to a CC2652P (SONOFF ZBDongle-P) — results

Setup: stand-alone XDS110 `0451:bef3`, serial LS420BFZ, firmware 3.0.0.32
("XDS110 (03.00.00.32) Embed with CMSIS-DAP"), on macOS (IOKit). Target: a
SONOFF ZBDongle-P V1.3 powered from its own USB connector, wired through its
5-pin header 3V3/GND/TMS/TCK/RESET to the XDS110 10-pin Cortex header (1 VTref,
2 TMS, 4 TCK, 10 nRESET, 3/5/9 GND). Everything below was read-only: no flash
write, no erase, no reset of this dongle.

## Why the first `jtag_scan` read "TDO stuck low"

`jtag_scan` connects with `CJTAG_CONNECT` format 1, which switches a 1149.7
target to four-pin JTAG and then samples TDO. The CC2652P's TDO (DIO16) is not
on the dongle's header, so TDO reads all zeros. Nothing was wrong with the
wiring; the board needs a two-pin format (`interface=cjtag`).

## Two-pin activation (XDS110 firmware behaviour)

- `XDS_SET_PROPERTY` (0x49) property 4: status -122 on firmware 3.0.0.32.
  `CJTAG_CONNECT` format 7 (OScan1) activates the two-pin format anyway.
- A second activation while the target is still in OScan1 reads all ones. The
  nTRST pulse with about 50 ms of TCK on either side (as in the pending OpenOCD
  two-pin patch) returns the target to its power-up state first; nTRST itself
  is not wired, the TCK run does it. Every attach does this.
- Scans go through `OCD_SCAN_REQUEST`; no Test-Logic-Reset is clocked.

## ICEPick and DAP

| Item | Value |
|---|---|
| ICEPick IR capture | 0b000001 |
| ICEPick IDCODE | 0x3BB4102F (version 3, part 0xBB41, TI 0x017) |
| ICEPICKCODE | 0x41611CC0 |
| control register (raw read) | 0x01002006 |
| debug TAP 0 router register before routing (raw read) | 0x20280027 |
| chain IR capture after routing | 0x011 (DAP 0b0001, ICEPick 0b000001) |
| CPU DAP IDCODE | 0x4BA00477 |
| JTAG-DP | DPv0: DP address 0 reads 0; CTRL/STAT power-up acknowledged (0xF0000000) |
| AHB-AP IDR | 0x24770011 |
| CPUID | 0x410FC241 (Cortex-M4 r0p1) |

## Chip (FCFG1)

| Item | Value |
|---|---|
| ICEPICK_DEVICE_ID | 0x3BB4102F: wafer 0xBB41 (CC13x2/CC26x2), PG 3 |
| USER_ID | 0x3202F000: CC26 (not CC13), PA (P variant), 7x7 mm, BLE + RF4CE + 15.4 + proprietary |
| part | CC2652P |
| FLASH_SIZE | 44 sectors x 8 KiB = 352 KiB |
| factory IEEE (MAC_15_4) | 00:12:4B:00:24:C3:18:6D |
| ROM_API_TABLE[0] / flash table | 0x0000CB41 / 0x100002C4 (FlashSectorErase 0x10002997, FlashProgram 0x10002885) |
| HAPI table | Crc32 0x10005085, sector erase 0x10005325, program 0x10005345 |

## CCFG (0x57FA8)

| Word | Value | Meaning |
|---|---|---|
| BL_CONFIG | 0xC5FE0DC5 | bootloader enabled, **backdoor on DIO13, active low** |
| ERASE_CONF | 0xFFFFFFFF | chip and bank erase enabled |
| CCFG_TI_OPTIONS | 0xFFFFFF00 | TI failure analysis disabled |
| CCFG_TAP_DAP_0 | 0xFFC5C500 | CPU DAP enabled, PWRPROF TAP enabled, TEST TAP disabled |
| CCFG_TAP_DAP_1 | 0xFF000000 | PBIST2, PBIST1 and AON TAPs disabled |
| IMAGE_VALID_CONF | 0x00000000 | image valid |
| CCFG_PROT_* | 0xFFFFFFFF x4 | no sector write protection |
| IEEE_MAC_0/1 | 0xFFFFFFFF | no IEEE override |

Debug is **not** locked. The ROM bootloader is reachable only through DIO13
held low at reset; the ZBDongle-P's DTR/RTS circuit drives DIO15, which is why
the tibsl entry (`dtr-rts`) got no answer.

## Firmware

A TI Z-Stack router image (strings `ZigBeeAlliance09`, `TexasInstruments`,
`ti.router`, `20210128`); sectors 0x0..0x2DFFF hold code. A router build has no
MT/ZNP interface on the UART, which explains the silent serial port.

## Backups and throughput

| Clock | Read rate | Full backup (2 reads) | SHA-256 |
|---|---|---|---|
| 1 MHz | 20 KiB/s | 34.1 s | fb007cc8218f52d6cd808cb5b428e10014c0dc43b4ca0107b3cd1746ae4a0ef2 |
| 8.5 MHz | 90 KiB/s | 7.9 s | identical |

All four full reads agree; the core kept running. Stored locally under
`$HWLAB_HOME/artifacts/cc26xx-flash/` (never committed).

## Core control

`status`, `halt` (PC 0x00027E2A), `read_registers`, `resume`, `status`
through `interface=cjtag` at 4 MHz.

## Not run on hardware

Erase, program, restore, reset over cJTAG and the ICEPick chip erase are
exercised against the SYNTHETIC model only (`xds.NewSimCC26x2`).
