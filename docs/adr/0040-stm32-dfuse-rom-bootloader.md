# ADR 0040: STM32 ROM USB DFU bootloader (DfuSe), native Go

Status: accepted (implementation). Tested only against an in-process simulator;
**not validated on hardware**. The first hardware run is to be read-only (see
"Validation plan").

## Context

A P-NUCLEO-WB55 USB dongle (MB1293C, STM32WB55CG) has no debugger. With
BOOT0 selected it enumerates as the ST system-memory bootloader, "DFU in FS
Mode", `0483:df11`. The ST-LINK plugin ([ADR 0039](0039-stm32wb-flash-and-fus.md))
cannot reach it. The same board family was found running FUS 0.5.3 with BLE
stack 1.0.0 and SFSA 0xCB.

The bootloader speaks USB DFU 1.1 with ST's DfuSe extensions (AN3156; the
behaviour is also documented by dfu-util's `dfuse.c`, which was consulted as a
protocol description only, no code was copied).

## Decision

A new plugin, `stm32dfu`, with a native Go DfuSe client over control
transfers of the existing `internal/usb` layer (Linux usbfs, macOS IOKit
through purego). It uses no cgo, no libusb, no dfu-util and no
STM32CubeProgrammer. `internal/usb` gained `AltSetter` (SET_INTERFACE) for
both backends.

* **Discovery:** every `0483:df11` device. The id is `stm32dfu/<USB serial>`;
  duplicates get `<serial>@<port>`, and a device without a serial gets
  `dfu@<port>`. The serial is read again when the device is opened and must
  match.
* **Protocol** (`plugins/stm32dfu/dfu`):
  * The DFU functional descriptor is parsed: wTransferSize, can-upload/download,
    manifestation-tolerant and will-detach.
  * Each alternate setting's string, e.g. `@Internal Flash  /0x08000000/256*04Kg`,
    becomes a memory map with r/e/w attributes per segment.
  * bcdDevice is reported as the bootloader version.
  * Requests used: DNLOAD, UPLOAD, GETSTATUS (honouring bwPollTimeout, bounded
    by 5 s per poll and 3 s/10 s per command), CLRSTATUS (on dfuERROR), GETSTATE
    and ABORT.
  * The DfuSe commands are set address pointer (0x21) and page erase (0x41 with
    a 4-byte address). Data uses wBlockNum ≥ 2 at pointer + (block−2)×wTransferSize.
  * Every bounded error ends with ABORT/CLRSTATUS back to dfuIDLE where possible.
* **A guard in the client** refuses, before anything is sent:
  * the 1-byte mass erase;
  * read unprotect (0x92);
  * get-commands and any other block-0 command except 0x21 and the 5-byte 0x41;
  * wBlockNum 1;
  * any erase or data download outside the write window or on another
    alternate setting, so the option bytes, OTP and every non-flash alt are
    never written;
  * a zero-length download outside `leave`.

  The write window is opened only when the secure boundary is established. It
  is [0x08000000, SFSA×4K) on the flash alt.
* **Secure boundary (SFSA)**, RM0434: the FLASH_SFR option word is read by
  UPLOAD at 0x1FFF8070 through the option-bytes alt (0x1FFF8000, 128 bytes).
  Each option word is a 64-bit double word with the complement in the high
  half. The SFR value at 0x1FFF8070 equals the FLASH_SFR register at
  0x58004080 that the ST-LINK plugin reads, and SRRVR at 0x1FFF8078 equals
  0x58004084.
  * SFSA = SFR[7:0]; FSD (bit 8) must be 0.
  * The complement at 0x1FFF8074 must match.
  * 0 < SFSA×4K < flash size, where the flash size comes from 0x1FFF75E0, or
    else from the flash alt.
  * The flash alt must end either at SFSA×4K or at the end of flash.

  If any of these fails, the plugin is **read-only**:
  * erase, program, backup and restore are refused;
  * flash reads are refused;
  * the write window stays closed;
  * SRAM, OTP and the option bytes can still be read.

  Flash at or above SFSA×4K is never read, erased or written, and the boot
  alias at 0 is never read.
* **Chip identity:** DBGMCU_IDCODE (0xE0042000, dev 0x495) when the ROM
  allows the read. Otherwise the chip is inferred from the memory map: 4 KiB
  pages at 0x08000000 plus a 128-byte option-bytes alt at 0x1FFF8000. Only
  an STM32WB5x can be modified.
* **Operations** (generic `target_op` names; methods on `FlashProgramming`
  unless noted):

  | op | method | gates |
  |---|---|---|
  | `info` | DebugProbe.info | none (static when not open) |
  | `identify` | DebugProbe.chip_info | none |
  | `memory-read` | MemoryAccess.read | `address`, `length` ≤ 4096 inline or `path` ≤ 16 MiB; peripherals and 0xE0000000+ need `allow_peripheral` |
  | `flash-info` | info | none |
  | `flash-read`, `flash-verify` | read, verify | user area only |
  | `fus-info` | fus_info | none |
  | `flash-backup` | backup | MCP confirm; two identical reads of the whole user area, mode 0600, manifest `hwlab-stm32-flash-backup/1` with SFSA, user size and a "never restore onto a joined Zigbee device" warning |
  | `flash-erase` | erase | `mode=region`, page-aligned, `confirm=erase-flash-region`, `allow_flash`+`allow_erase`, `backup` |
  | `flash-program` | program | `confirm=program-flash`, `allow_flash` (+`allow_erase` when a programmed double word must change), `backup` |
  | `flash-restore` | restore | `confirm=restore-flash`, `allow_flash`+`allow_erase` |
  | `dfu-leave` | leave | `confirm=leave-dfu`, `allow_reset` |

  The backup gate is the ST-LINK plugin's. The backup must:
  * be of this chip;
  * have the same user-area size and secure start;
  * have two identical reads with a matching SHA-256;
  * equal the flash in every affected page, or the page is blank.

  `program` changes only the covering pages. It merges the image over the page
  and erases a page only when the double-word rule needs it. It then reads
  every page back and compares the whole image again. Data is never returned
  inline beyond 4 KiB; artifacts go to `$HWLAB_HOME/artifacts/stm32dfu-flash`.
* **`fus_info`:** reads the reference table pointer at 0x20030000 and the word
  at 0x20030030 (ST documents it as the FUS version:
  `STM32_Programmer_CLI -c port=usb1 -r32 0x20030030 1`). It then decodes the
  device information table with the ST-LINK plugin's decoder (FUS layout with
  key 0xA94656B9, or the wireless-stack layout). The ROM bootloader starts the
  FUS itself; the plugin sends no FUS command.
* **`leave`:** set address 0x08000000, a zero-length DNLOAD, then GETSTATUS.
  The device then leaves USB. With BOOT0 still selected, the next hardware
  reset returns to the bootloader.
* **Option bytes** are never written. The plugin never sends mass erase or
  read unprotect, and never offers FUS commands. FUS upgrades stay with the
  ST-LINK plugin.

## FUS 0.5.3 guard in the ST-LINK plugin

ST's STM32CubeWB release notes (V1.24.0, STEP 5) require
`stm32wb5x_FUS_fw_for_fus_0_5_3.bin` on FUS 0.5.3. That image's SHA-256 is
`3f73a9be…4b30` and it yields FUS 1.2.0.

`fus_upgrade` now enforces this rule:
* on 0.5.3 it accepts only that image, and only with
  `expect_new_fus_version=1.2.0`;
* on 0.5.3 it refuses the plain 1.2.0 and 2.2.0 FUS images;
* it refuses that special image on any other FUS.

## Validation plan (for the lead)

Status: implementation done, hardware validation **not** done.

1. Read only:
   * `info`, `identify`, `flash-info`, `fus-info`;
   * `memory-read` of 0x1FFF8000 (128 bytes) and of 0x20030030;
   * `flash-read` of a few pages.

   Check the alt strings, wTransferSize and bcdDevice. Check that SFR/SRRVR
   at 0x1FFF8070/0x1FFF8078 decode to SFSA 0xCB, and whether the ROM allows
   UPLOAD from SRAM2a, DBGMCU, 0x1FFF75E0 and 0x1FFF7580.
2. `flash-backup` twice and compare.
3. Only then erase or program one user page, and restore.

Points not confirmed on silicon:
* whether every option-byte double word has its complement in the high half
  (assumed; a mismatch makes the plugin read-only);
* whether the flash alt string of the WB ROM reflects SFSA or the whole flash
  (both are accepted);
* whether the ROM serves UPLOAD outside flash, SRAM and the option bytes. The
  plugin falls back to inferring the chip when DBGMCU fails; an SRAM2a failure
  is reported as an error by `fus-info`;
* SET_INTERFACE and string descriptors on macOS IOKit against this device;
* whether `leave` jumps or resets on the WB ROM.

## Consequences

* The dongle can be identified, backed up and reprogrammed without a debugger,
  within the same safety model as the ST-LINK plugin.
* The MCP server must be restarted to load the plugin.
* Not done:
  * FUS and wireless-stack operations through DFU;
  * option bytes;
  * other STM32 families' flash (they get descriptors and non-flash reads only);
  * DFU detach from a runtime-mode application.
