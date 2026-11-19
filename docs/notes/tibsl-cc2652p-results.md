# tibsl: TI ROM serial bootloader on a CC2652P dongle (read-only run)

Date: 2026-11-19. Host: macOS, `bin/hwlab` from branch `ti-bsl`.

Board: CC2652P1F USB dongle (ZBDongle-P style, CP2102N at
`/dev/cu.usbserial-0001`), shared with another agent that develops firmware
on it. Only read-only commands, the DTR/RTS entry and COMMAND_RESET were
sent; no erase, SEND_DATA, MEMORY_WRITE, BANK_ERASE or SET_CCFG.

Configuration: `HWLAB_TIBSL_PORTS=cc2652p=/dev/cu.usbserial-0001@460800:dtr-rts`.

| Step | Result |
|---|---|
| entry (dtr-rts, one TIOCMSET per step: (1,0) 100 ms, (0,1) 100 ms, (1,0) 500 ms) | 0x55 0x55 answered 00 CC on the first try; `lines_atomic=true` |
| `bsl identify` | GET_CHIP_ID 0x3202F000; ICEPICK 0x3BB4102F (wafer 0xBB41, PG 3); USER_ID 0x3202F000 (PA, 7x7 mm, all protocols) → CC2652P; FLASH_SIZE 0x2C × 8 KiB = 352 KiB; IEEE 00:12:4B:00:20:CD:1D:7D; CCFG BL_CONFIG 0xC5FE0FC5 (backdoor DIO15 active low) — all equal to the values measured independently with cc2538-bsl |
| identify with `bsl_entry=manual` while the BSL was still synchronized | `resynced=true` (85-byte packet completed with zeros, NACK, PING ACK) |
| `bsl ccfg-info` | 88 bytes identical to the cc2538-bsl backup's CCFG; CPU DAP enabled, TI FA disabled, image valid, no write-protected sectors |
| `memory read 0x500012F0+8` | 7d1dcd20004b1200 (FCFG1 MAC_15_4) |
| `flash read 0x0+64` | data + ROM CRC32 checked |
| `flash backup` | 352 KiB read twice in 19.5 s at 460800 Bd (MEMORY_READ, 63 words, GET_STATUS after each); both SHA-256 c5c80322…867f; ROM CRC32 0xEAD1ABE0 = host CRC32 |
| comparison with the cc2538-bsl backup (SHA 958b9ba1…496b, taken before the other agent flashed its image) | 38 of 44 sectors byte-identical incl. the CCFG sector; the 6 differing sectors (0x0, 0x2000, 0x4000, 0x50000, 0x52000, 0x54000) are exactly the ones the other agent rewrote/erased |
| `flash verify` of the new backup | ROM CRC32 match |
| `flash program --arg dry_run=true` with the old Z-Stack image | plan by per-sector ROM CRC32: 6 sectors to change, 38 unchanged, CCFG sector unchanged — equal to the host-side diff; image CRC32 0xEB810577 equals the ROM CRC32 cc2538-bsl recorded for that image |
| `bsl reset` | lines released, COMMAND_RESET ACKed; the port was free afterwards |

Not run on hardware: sector/bank erase, program, restore, the CCFG gate on a
real write, `rts-dtr`, other baud rates, CC2538 and other CC13xx/CC26xx parts
(all covered by the in-process simulator only).
