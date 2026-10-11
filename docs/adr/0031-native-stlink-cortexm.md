# ADR 0031: native ST-Link V2 backend with a generic Cortex-M layer

Status: accepted

## Context

After J-Link (external tool), FTDI/JTAG and the Espressif USB-JTAG (native), the
platform needs an ST-Link backend that, like those, runs without OpenOCD,
STM32CubeProgrammer, st-flash, libusb, Python or CGO. Hardware available: one
ST-LINK/V2 (`0483:3748`, firmware V2J39S7) wired to an **nRF52840 DK**, not an STM32.

## Layering

    internal/usb (Linux usbfs, stall/ClearHalt/reset recovery)
        -> plugins/stlink/stlink   (ST-Link V2 command protocol, SWD/JTAG, AP/DP access)
        -> internal/cortexm        (generic Cortex-M: DHCSR/DCRSR, SCB, FPB, DWT, memory)
        -> plugins/stlink          (plugin: sessions, policy, reconnect, TargetControl/Memory/Flash)
        -> plugins/stlink/stm32    (STM32-only flash layer; nothing generic depends on it)

`internal/cortexm` holds only ARM architecture facts (ARMv7-M DHCSR, DCRSR,
AIRCR, DEMCR, FPB v1, DWT). No STM32 or nRF knowledge lives there. The nRF52840
FICR word is reported as a vendor *hint*; generic code never concludes a chip
from it.

## Decisions

* **Protocol**: written from the stlink-org documentation/sources as a protocol
  reference (BSD-3) and corrected against the real probe; nothing copied
  from OpenOCD (GPL), which was used for behaviour only. Only the V2 protocol
  (`0483:3748`) exists; V1, V3 and V2-1 mass-storage variants are NOT_IMPLEMENTED.
* **Probe state**: the probe powers up in DFU (`F3 07` leaves it); a stall needs
  ClearHalt on IN 0x81 and OUT 0x02; a stale unread reply wedges it, so the IN pipe
  is drained before a command after any error.
* **DEBUG_EXIT (`F2 21`) has no reply.** Waiting for one cost a full 2 s timeout
  on every close (a reopen test took 25 s, the hardware suite 58 s). Close now
  sends it with rx=0: close ~0.15 ms, open ~110 ms, suite 8.8 s.
* **Identity**: the canonical USB serial is read from the raw string descriptor
  (UTF-16 units <= 0xFF; sysfs shows them UTF-8 encoded). Selection is by serial; with
  several probes and no serial the call fails and lists the candidates. Bus/device
  numbers are informational only.
* **Sessions persist the exact probe serial and target identity** (DP IDCODE,
  CPUID, vendor word); a reconnect after unplug must match all of them or fails.
* **Memory writes** are SRAM-only (`0x20000000-0x21FFFFFF`), default deny
  (`allow_memory_write`), read back, and restored on mismatch.
* **Breakpoints** use FPB, **watchpoints** DWT; the original comparator state
  and DEMCR are restored on clear/close.
* **Reset**: SYSRESETREQ and core reset with observation (reset_seen, PC at the
  reset vector). NRST pin reset needs `confirm="reset-pin"` and, because NRST is
  not wired here, it is NOT claimed verified.
* **Concurrency**: the USB interface claim gives cross-process exclusion
  (`FailedPrecondition: usb: interface is busy`); reopening the same id in one
  plugin replaces its session.

## Flash

The STM32 flash layer (`plugins/stlink/stm32`) drives the on-chip flash
controller through the debug port (no loader, no external tool). Scope:
STM32F4 single bank: sector erase and 32-bit programming; STM32F1: identification,
geometry and reads only (halfword programming is not possible through the V2
debug port, so it is refused). Option bytes, RDP, OTP and dual-bank are never
written. **It has only run against a simulated controller**: the attached target
is not an STM32 (DBGMCU reads 0), so every flash capability is
`UNSUPPORTED_BY_ATTACHED_HW` and the plugin refuses flash operations on an
unrecognised target. Policy: `allow_flash`, `allow_erase`, `allow_chip_erase`,
tokens (`program-flash`, `restore-flash`, `erase-flash-region`, `erase-entire-chip`),
`--yes`, and a verified full backup that equals the *current* flash for the
affected sectors (a stale backup is refused).

## Consequences

* Debug is REAL_HW_VERIFIED on a Cortex-M4 behind a real ST-Link; STM32 flash is not.
* Linux only (usbfs). SWO, JTAG transport on hardware, multi-probe on hardware and
  NRST pin reset remain unverified.
