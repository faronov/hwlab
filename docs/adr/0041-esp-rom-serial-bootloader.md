# ADR 0041: Espressif ROM serial bootloader (`esprom`)

## Context

hwlab reaches ESP chips through their USB-Serial/JTAG JTAG interface
(`espusbjtag`, ADR 0029/0030/0038). That does not cover:

* classic ESP32 boards behind a USB-UART bridge (no USB-Serial/JTAG at all),
  e.g. the SONOFF Dongle Max (ESP32-D0WDR2-V3, 16 MB flash, CP2102N);
* the ESP32-H2 of the Thread Border Router board, which is reachable only
  through the S3's UART bridge (zigbee-rs-hub `tbr-bridge`/`tbr-hub` bridge
  mode, CDC 0 of USB `303a:4002`);
* resetting a chip whose firmware hung or which was left in ROM download mode,
  without buttons, through its own USB-Serial/JTAG CDC port.

All three speak, or are reset through, the Espressif serial bootloader
protocol and DTR/RTS line sequences. esptool implements them but is
GPL-2.0-or-later and Python; hwlab is native Go and must not copy GPL code.

## Decision

A new plugin `esprom` with a pure protocol package `plugins/esprom/rom`.

**Protocol.** Written from Espressif's public *Serial Protocol* document
(esptool docs, advanced-topics/serial-protocol, per-chip pages), the chips'
TRMs and ESP-IDF v5.5.1 headers/eFuse tables (Apache-2.0) and the
esp-flasher-stub sources (Apache-2.0 OR MIT). SLIP framing; ROM commands SYNC,
READ_REG/WRITE_REG, SPI_ATTACH (8-byte ROM form / 4-byte stub form),
SPI_SET_PARAMS, MEM_BEGIN/DATA/END (stub upload), SPI_FLASH_MD5 (ROM: 32 hex
characters, stub: 16 raw bytes), CHANGE_BAUDRATE (old rate 0 for the ROM, the
current one for the stub), GET_SECURITY_INFO; stub-only READ_FLASH (packets
acknowledged with the running byte count, then the stub's MD5),
FLASH_BEGIN/DATA (16 KiB blocks), ERASE_REGION, ERASE_FLASH. The status length
(4 bytes ROM, 2 bytes stub) is learned from the SYNC answer; the ROM's SYNC
value is 0x20120707, the stub's 0, which also detects a stub left running.
Stub write errors are deferred to the next answer (the stub replies before it
writes), so every write run is followed by SPI_FLASH_MD5, whose answer carries
the error and checks the bytes. A READ_FLASH that lost a packet is ended by a
non-4-byte acknowledgement (the stub then leaves its loop), the link is
resynchronized and the 256 KiB segment re-read (3 attempts).

**Chip detection.** The ROM word at 0x40001000 first: 0x00F01D83 is the ESP32
(which has no GET_SECURITY_INFO), 0x000007C6 the ESP32-S2; otherwise the
GET_SECURITY_INFO chip id (S3 9, C6 13, H2 16, C3 5, C2 12, C5 23, P4 18).
Identity from eFuses only by READ_REG: ESP32 BLK0 (MAC, package incl. the 4th
package bit, revision from CHIP_VER_REV1/REV2 and APB_CTRL_DATE bit 31, minor,
features, FLASH_CRYPT_CNT, secure boot v1/v2, download/JTAG disables); S3/C6/H2
BLK1 (MAC, MAC_EXT → EUI-64, wafer version, package) and the
GET_SECURITY_INFO flags. Flash JEDEC id via the SPI1 user command registers
(ESP32 SPI at 0x3FF42000 with W0 at +0x80; SPI_MEM at 0x60002000 (S3) /
0x60003000 (C6/H2) with W0 at +0x58), registers saved and put back.

**Stub.** Espressif's `esp-flasher-stub` v2.0.0 (Apache-2.0 OR MIT, the
default stub of esptool since v5.3; not the GPL legacy stub) is vendored
unchanged for esp32, esp32s3, esp32c6 and esp32h2 under
`third_party/esp-flasher-stub/` with both licenses and a provenance record
(tag, commit, SHA-256 equal to GitHub's asset digests, checked by a test). Only
the base `text`/`data` segments are uploaded; the optional NAND/SPI/SDIO
plugins embedded in the S3/C6 JSON are never loaded. Chips without a vendored
stub are identify-only. Note: this stub's RUN_USER_CODE and FLASH_END reboot
are no-ops, so leaving the loader is always a line reset.

**Transports (target kinds).**

| kind | port | entry | default rate after the stub |
|---|---|---|---|
| `esp32_uart` | CP210x/CH34x/FTDI bridge | classic: (DTR=0,RTS=1) EN low 100 ms → (1,0) EN released + strap low, held 100 ms (550 ms on a second try) → (0,0) | 921600 |
| `h2_bridge` | CDC 0 of the S3 bridge (303a:4002 "... bridge") | classic (the bridge maps EN low = RTS∧¬DTR, BOOT low = DTR∧¬RTS) | 460800 (the bridge follows the line coding) |
| `usj` (s3_rom) | the chip's own 303a:1001 CDC | USB-Serial/JTAG (ESP32-S3 TRM v1.8 tables 33.3-2/33.4-3/33.4-4) | none (USB) |

Every line state is set with one TIOCMSET (both lines at once), so the classic
circuit never passes through (0,0)/(1,1) between reset and strap. Discovery is
passive (IORegistry/sysfs); ids follow tibsl's scheme for UART bridges
(`cp210x-<serial>`, `cp210x-loc<path>`), `bridge-<S3 serial>`,
`usj-<mac>`. Aliases of one interface (macOS `cu.usbserial-*` and the SiLabs
VCP's `cu.SLAB_USBtoUART*`) are one device. The bridge's CDC 1 (console) is
never offered, and a bridge (PID ≠ 0x1001) is never taken for the S3's own
USB-Serial/JTAG.

**Where `usj_reset` lives: in `esprom`, as `FlashProgramming.reset`.** A USJ
chip reset is a DTR/RTS sequence on the CDC tty, exactly the mechanism this
plugin owns for every transport; `espusbjtag` claims only the vendor JTAG
interface and by design never opens the CDC tty (it stays with
`cdc_acm`/the `uart` plugin), so adding tty line control there would mix two
transports in one session and fight the JTAG session that the reset kills
anyway. In `esprom` the reset is line-only: it needs no ROM, no SYNC and no
working firmware, so it recovers a hung application or a chip left in download
mode (`mode=run`: TRM 33.4-4 clear flag → reset → release), or puts a chip into
download mode (`mode=bootloader`, checked by SYNC). A usj device lists
`espusbjtag_device` (the matching `espusbjtag/303a:1001:<serial>` id) to link
the two views of one chip; `espusbjtag` keeps its `companion_cdc_tty`. A port
that re-enumerates after the reset is reopened (bounded wait, same id).

**Session.** OpenDevice only opens the port with both lines released; the ROM
is entered lazily by the first operation that needs it (identify, info, read,
verify, backup, erase, program, restore), so `bsl-reset` through `hwlab
target` never enters the ROM first. After a run reset the next ROM operation
enters again by itself.

**Safety policy** (same tokens and permission names as `espusbjtag`):

* identify/info/read/verify/backup are SAFE in the danger model (they reset the
  chip into download mode, like tibsl, but never write); erase/program/restore/
  reset are CONFIRM.
* `allow_flash` (program, restore), `allow_erase` (erase, restore),
  `allow_chip_erase` (mode=chip), `allow_reset` (reset, `reset=run`) are
  dangerous configuration keys.
* tokens: `program-flash`, `restore-flash`, `erase-flash-region`,
  `erase-app-partition`, `erase-entire-chip`; `confirm_protected=
  modify-protected-region` for protected ranges.
* protected ranges: the bootloader slot [0, 0x8000) (ESP32 bootloader at
  0x1000), the partition-table sector, and **every partition that is not an
  application** (nvs, phy_init, otadata, coredump, nvs_keys, custom data such
  as `hubdata`) — stricter than espusbjtag, which leaves otadata unprotected.
  Without a valid table at 0x8000 the whole flash is protected.
* a verified backup of this chip is required (two identical full reads, the
  chip's whole-flash MD5 equal to the host MD5; manifest bound to chip, MAC,
  JEDEC id and size), and the bytes about to be replaced must still match it
  (chip MD5 of the sector span).
* ESP images (0xE9) are parsed (segments, checksum), must carry the connected
  chip's image chip id and lie inside one region; anything else needs
  `raw=true`.
* program/restore change only 4 KiB sectors whose MD5 differs (64 KiB blocks
  first), keep edge bytes (edge sectors read and merged; the stub erases whole
  sectors), verify every run and the result by the chip's MD5.
* restore gates `confirm_protected` on the sectors that actually differ, using
  the backup's own partition table.
* flash with encryption (odd FLASH_CRYPT_CNT / SPI_BOOT_CRYPT_CNT) or secure
  boot enabled is read-only; eFuses are never written.
* a JSONL journal (`artifacts/esprom-flash/journal.jsonl`) records every
  erase/program/restore/reset before and after; the call is refused when the
  journal cannot be written.
* a port where `tibsl` identified a TI chip is refused unless
  `allow_other_chip=true` (the ESP sequence would only reset it).

## Consequences

* ESP32 boards on USB-UART bridges, the H2 behind the S3 bridge and USJ chips
  share one implementation and one safety policy.
* Tests run against an in-process simulator of the wiring (classic circuit
  with a strap-hold model, USJ flag/reset table, re-enumeration), the ROM
  (4 status bytes, hex MD5, ROM errors) and the stub (2 status bytes, deferred
  errors, READ_FLASH acks/digest, abort), with fault injection.
* Hardware evidence is recorded per capability in the support matrix; only
  what ran on a device is REAL_HW_VERIFIED.

## Not covered / unverified

* Compressed writes (FLASH_DEFL_*), encrypted writes, secure download mode,
  ESP32-S2/C2/C3/C5/P4 flash (no stub vendored), USB-OTG download (S2/S3
  TinyUSB ROM), eFuse writes (never).
* The USJ and h2_bridge paths and all write paths were not run on hardware by
  this ADR's author; see docs/support-matrix.md.
