# ADR 0036: Bouffalo BL702 boot-ROM ISP (native Go, vendor RAM helper)

Status: accepted

## Context

The BL702 (XT-ZB1 DevKit, zigbee-rs `bl702-sensor`) has no SWD and its
on-board CH340 is a log UART. Its boot ROM has an ISP that, with BOOT held at
power-up, enumerates on the chip's own USB as a CDC port (`ffff:ffff`). The
vendor `bflb-mcu-tool` fails over that port on macOS: its 0x55 sync burst is
longer than the ROM reads, and the leftover bytes corrupt the next command.

## Decision

* A `bflb` plugin with a native Go ISP client (`plugins/bflb/isp`): framing,
  ROM handshake, boot info, segment loading, helper commands (flash id, read,
  erase, write, SHA-256, reset). No cgo; serial through the existing UART
  layer.
* The ROM only loads code; flash access needs the vendor `eflash_loader` RAM
  helper. It is not redistributed: `HWLAB_BFLB_EFLASH_LOADER` points at the
  file from `bflb-mcu-tool`. hwlab parses its boot header and loads it
  through the ROM; it never runs the Python tool.
* USB-specific behaviour is measured, not guessed (see the
  [results](../notes/bflb-bl702-usb-isp-results.md)):
  * short sync (one USB packet) on USB, the full 0x55 burst on UART;
  * connect probes a resident helper first, so a reopened port in either
    the ROM or the helper works without a replug;
  * writes go in 55-byte frames on USB: the helper loses OUT frames of 64
    bytes or more (one full USB packet); reads of 2 KiB are fine.
* Safety follows the other flash plugins: `backup` reads the whole flash
  twice and checks the chip-side SHA-256; `erase`, `program` and `restore`
  need `--yes` and a backup that matches this chip's MAC; every write ends
  with a chip-side SHA-256 compare.

## Consequences

* Flashing a BL702 needs only a USB cable and the helper binary.
* Writes over USB run at ~110 KiB/s (small frames); a 200 KB image takes ~5 s.
* The helper reset reboots into the firmware, but macOS may keep the ISP
  device listed until the cable is replugged.
* BL702 only; UART ISP and restore have not run on hardware.
