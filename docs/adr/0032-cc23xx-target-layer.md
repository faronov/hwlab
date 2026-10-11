# ADR 0032: CC23xx target layer (SACI, CCFG, flash) shared by both probes

Status: accepted

## Context

The CC2340 debug port is a gated SWD interface. After any reset the boot ROM runs
first and the AHB-AP is closed until the boot sequence reaches the application (or
SACI is left). Flash is not memory-mapped for writing: the only supported writer is
the boot ROM's SACI mailbox, reached through CFG-AP. Both the native XDS110 and the
CMSIS-DAP plugins reach the same target, so SACI cannot live in either plugin.

## Layering

    internal/adiv5              raw AP register access (rawap.go), DP/AP
        -> internal/cortexm     generic Cortex-M (unchanged, no TI knowledge)
        -> internal/ti/cc23xx   family layer, probe independent
             saci/              mailbox protocol
             ccfg/              CCFG parser (no secrets returned)
             ccsim/             simulator used by tests
             session, reset, identify, flash
             service/           methods, gating, backup/erase/program/restore, specs
        -> plugins/xds110, plugins/cmsisdap   call the service; supply only a transport,
                                              a clock hook (`CapClock`) and permissions

`internal/cortexm` stays free of TI specifics. The plugins contain no SACI logic.

## Decisions

* **Every CC23xx reset ends in SACI.** nRESET and SYSRESETREQ are both followed by
  entering SACI, then a SACI exit (run, halt or reset-and-halt) re-opens the AHB-AP.
  Reset therefore recovers debug access without a power cycle.
* **Clock cap.** SYSRESETREQ at SWD clocks of 600 kHz and above leaves the mailbox
  stuck, so the clock is capped to 400 kHz through the plugin's `CapClock` hook until
  the first SACI exchange is done. Empirical; cause unknown.
* **Flash.** Only ERASE_CHIP exists, so program is always whole-chip erase, MAIN
  program, CRC verify, CCFG program, verify, finish. A blank device merges the image
  over 0xFF; otherwise it merges over a verified backup. The pipelined program
  command is NOT_IMPLEMENTED.
* **Safety.** Destructive methods need `--yes`, a method-specific token, device
  permissions (`allow_flash`, `allow_erase`, `allow_chip_erase`, `allow_ccfg_write`)
  and a verified backup. Debug security is never weakened; die ID, user record,
  password and hash are never logged or returned. Backups are written mode 0600 with
  a manifest `hwlab-cc23xx-flash-backup/1`.
* **Identification** uses documented registers only. A raw PARTID is reported as is;
  no marketing name is inferred.
* **No vendor tools or runtimes**: no UniFlash, CCS, OpenOCD, Python, cgo.
* **Truthful status**: absent hardware (DWT, external JTAG) is
  UNSUPPORTED_BY_ATTACHED_HW; unexercised paths (CCFG write, power-cycle recovery)
  are IMPLEMENTED_UNTESTED.

## ROM serial bootloader boundary

The ROM serial (UART/SPI) bootloader is a separate boot path outside the debug
transport. It is not implemented here; see
[docs/cc23xx-rom-bootloader-boundary.md](../cc23xx-rom-bootloader-boundary.md).

## Consequences

Adding another CC23xx part means a device table and CCFG offsets, not a new
plugin. A new probe only has to provide raw SWD AP access and a clock hook.
