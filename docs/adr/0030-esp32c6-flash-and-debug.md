# ADR 0030: native ESP32-C6 flash programming and full debug over USB Serial/JTAG

Status: accepted (extends [ADR 0029](0029-espressif-usb-jtag-riscv-dm.md))

## Context

ADR 0029 delivered a read-only debugger. This phase adds memory write,
single-step, hardware breakpoints, resets, unplug/replug handling, and a complete
flash read / erase / program / verify / backup / restore path, all without
OpenOCD, esptool, ESP-IDF, Python or CGO.

## Layering (unchanged, enforced)

    internal/usb -> plugins/espusbjtag/espusb -> internal/jtag -> internal/riscv
                                                                     ^
                         plugins/espusbjtag/esp (ESP-only) ----------+

`internal/riscv` stays generic: DM, hart selection, abstract registers, SBA with
program-buffer-free 8/16/32-bit access (8/16-bit writes are read-modify-write on
a 32-bit-only SBA), `dcsr.step`, trigger discovery via `tselect/tdata1/tdata2/tinfo`
(the slot count is whatever the hart answers; this C6 has 4 mcontrol triggers) and
batching (`BusBatch`). Nothing ESP-specific lives there. Register addresses, the
memory map, the reset semantics, the flash controller, partition/image handling
and the watchdog handling are in `plugins/espusbjtag/esp`.

## Flash path: why SPI1 driven over JTAG

Priority order from the task: (1) native Go through debug/ROM facilities,
(2) a target-side stub in SRAM, (3) the ROM bootloader protocol over CDC.

Chosen: **(1)**. The Go code halts the hart and drives the SPI1 flash controller
registers directly through System Bus Access: JEDEC (0x9F), SFDP (0x5A), READ,
WREN, status polling, page program and erase. No code runs on the target and no
ROM function is called, so there is nothing to upload, no stub to trust and the
CDC interface (which stays with `cdc_acm`) is not involved. (2) would need a
cross-compiled stub and (3) needs the chip in download mode, which a running
application does not allow and which changes the system state.

Hardware fact (observed): on this C6 the `SPI_MEM_CMD` register bits for page
program and sector erase are ignored. Writes work through the **user-mode
command** (`SPI_MEM_USR` with the PE bit) with opcodes 0x02/0x20/0xD8/0xC7
followed by waiting for host idle and the flash WIP bit. This is what the
implementation uses (`esp/flash.go: issueUser`).

The watchdogs (both timer groups and the RTC WDT) are disabled for the duration
of an operation and restored afterwards (`esp.Quiesce` / `Restore`).

Reads go through the same controller (physical reads, 64 B per transaction,
batched into few USB round trips); the CPU's memory-mapped window is only used as
a cross-check.

## Safety policy

* Every destructive call needs the common hwlab confirmation (`--yes`), the
  plugin's own token, and the device-level permission:
  `program-flash` / `restore-flash` (`allow_flash`), `erase-flash-region`,
  `erase-app-partition` (`allow_erase`), `erase-entire-chip` (`allow_erase` and
  `allow_chip_erase`), `reset-soc` (`allow_reset`).
* A **verified backup** (two reads with identical SHA-256, JEDEC/geometry in a
  manifest, stored as a local artifact) must be supplied for any erase/program.
* Only the sectors a region touches are erased; unrelated sectors are preserved.
  A sector whose content is identical is skipped.
* Bootloader slot, partition table and data partitions are protected and need
  `confirm_protected=modify-protected-region`. A program may touch exactly one
  region. Data that is not an ESP image (no 0xE9 magic) needs `raw=true`.
* eFuses are only read (security flags); no eFuse write exists in the code.
* The generic `target erase` does not erase this device: only the ESP-specific
  methods with the above gates can.

## Reset

Three distinct operations: `hart` (dmcontrol hartreset, where supported),
`system` (`ndmreset`, ~20 ms, always halted at the reset vector, USB stays up),
`soc` (RTC WDT reset-system; the USB peripheral re-enumerates). For `soc` the
old handle is discarded only after the device is seen gone (bounded wait), the
device is then rediscovered **by USB serial**, interface 2 is reopened, the
TAP/DTM/DM are rebuilt and the identity (serial, IDCODE, ROM word) is compared.
If the hart was halted when the reset was requested (typically parked in ROM
after a reset-halt) the boot code that runs after the resume disables the
watchdog again, so the plugin re-arms it through SBA until the reset happens.

## Unplug

The kernel can report an unplug during an in-flight transfer as EPROTO or EIO
before ENODEV. `internal/usb` therefore checks whether the device node is still
there (`USBDEVFS_GET_CAPABILITIES` / `stat`) and maps such errors to
`ErrDisconnected`, which becomes `DEVICE_GONE`. A session then waits (bounded,
about 8 s) for the same serial to return and re-establishes itself.

## Consequences

* Flash read about 65 KB/s, program about 12 KB/s (one sector), 4 MiB backup ~117 s.
* Full REAL_HW cycle program + verify + boot + debug + restore was run; see
  [phase 19](../notes/phase19-esp-flash-debug-results.md).
* Not covered: eFuse programming (deliberately), other Espressif chips, hart 1
  (reports unavailable on this board), app-partition and full-chip erase on real
  hardware, interrupted-write power-loss behaviour.
