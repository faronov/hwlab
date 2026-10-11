# ADR 0034: native J-Link backend (`jlink-native`)

Status: accepted

## Context

The J-Link backend (ADR 0012) drives SEGGER's `JLinkExe`. It is real-hardware
verified but depends on proprietary software, spawns a process per call and
cannot be used where that software is not installed. A second, independent
backend was wanted that runs the existing generic debugger on a J-Link with
no SEGGER process or library.

Two backends now exist and are chosen **explicitly**; neither falls back to the
other:

| id | what it is |
|---|---|
| `jlink-segger` (plugin `jlink`) | drives `JLinkExe` + RTT telnet; unchanged |
| `jlink-native` (plugin `jlinknative`) | pure Go: usbfs -> J-Link command set -> SWD -> adiv5 -> cortexm |

## Layering

    Linux usbfs -> internal/usb
      -> plugins/jlinknative/usb       framing, exact-length replies, error classes
      -> plugins/jlinknative/protocol  probe commands (version, caps, VTref, speed, interface, sequence, nRESET)
      -> plugins/jlinknative/swd       SWD bit sequences; implements adiv5.Link
         plugins/jlinknative/jtagport  TMS/TDI/TDO vectors; implements jtag.Port
      -> internal/adiv5 (DP/AP) -> internal/cortexm (halt, registers, step, FPB, DWT, reset)
      -> internal/rtt                  target-memory RTT (probe independent)
      -> plugins/jlinknative           plugin: sessions, policy, reconnect, DebugProbe/MemoryAccess

No halt, register, step, breakpoint, watchpoint or reset logic is in the
J-Link packages; they provide the wire only. Flash programming is deliberately
**not** offered: it belongs to target-family layers, and SEGGER's device
database and flash loaders are not recreated.

## Clean-implementation boundary (licensing)

The J-Link low-level protocol is not a published standard. Other public
implementations of it (libjaylink, OpenOCD's J-Link driver) are GPL.

* No GPL source was incorporated, copied or mechanically translated, and none
  was used as the basis of this code. Command ids, reply layouts and the SWD
  bit-sequence encoding in `protocol/` and `swd/` were established from
  observation of the real probe's USB traffic and replies, from the ARM
  Debug Interface (ADIv5) and ARMv7-M architecture specifications, and from
  the facts that are public interoperability knowledge (a command id and its
  reply size are facts, not expression).
* References consulted: ARM IHI0031 (ADIv5), ARM DDI0403 (ARMv7-M); SEGGER's
  `JLinkExe` used only as a **behavioural oracle** (its console output, the
  values it reads from the same target) and never as a runtime dependency of
  `jlink-native`; the hwlab XDS110, CMSIS-DAP and ST-Link backends for the
  shared adiv5/cortexm integration pattern.
* Commands whose behaviour was not observed are not implemented and are not
  sent (for example 0xC1 and 0xD4 were never established as safe). Unknown
  commands, status bytes and capability bits are kept in diagnostics instead of
  being guessed. Capability bit names are not claimed.
* The SEGGER tools are not redistributed and are not a runtime dependency; no
  libusb, no CGO, no OpenOCD, no Python.

## Decisions

* **Identity** is the USB serial (`jlink-native:<serial>`, leading zeros of the
  descriptor trimmed). Bus and device numbers are never persisted; the opened
  device's serial is compared with the requested one.
* **Exact replies.** A probe that is sent a command while a reply is pending,
  or whose reply is read short, can fall into its bootloader (VID:PID
  1366:0101) and then needs a physical replug. Every exchange therefore reads
  the complete reply; a short or garbled reply is an error and the transport
  drains before the next command.
* **SWD** runs through the probe's bit-sequence command: each DP/AP
  transaction (and runs of them) is built host-side with parity, ACK and data
  checks. WAIT is retried, FAULT is cleared, and a non-OK ACK inside a batch
  triggers a line reset + resync with the tracked DP SELECT/CTRL restored.
  Because later transactions of a batch were already clocked after a failed
  one, the batch result after a non-OK ACK is discarded, never reported.
* **Clock**: the programmed rate is the probe's configuration, not a
  measurement. Above about 2 MHz full-speed USB hides the clock. 12 MHz
  (the probe's reported maximum) is refused by the target on the wired EFR32
  and reported as `PROTOCOL_ERROR: invalid ACK`.
* **Reset**: Cortex-M resets (SYSRESETREQ, VECTRESET, reset-and-halt via
  DFSR.VCATCH) are `internal/cortexm`. The probe's nRESET output is driven,
  but a pin reset is only claimed when the target is seen to reset; on the
  attached board nRESET is not wired and the capability is
  `UNSUPPORTED_BY_ATTACHED_HW`.
* **RTT** is target-memory based (`internal/rtt`): search, validated control
  block, wrap-aware up-buffer reads that advance RdOff only after a complete
  read, a down-buffer write that never overwrites unread data, bounds checks,
  periodic revalidation of the block and back-off polling. Nothing RTT-specific
  is in the probe protocol.
* **JTAG**: `jtagport` only moves bits; the TAP state machine and chain scan
  are `internal/jtag`. `IMPLEMENTED_UNTESTED`: no downstream JTAG target.
* **Errors** keep their class: `PROBE_GONE` (DEVICE_GONE), `PROBE_BUSY`,
  `USB_TIMEOUT`, `USB_STALL`, `PROTOCOL_ERROR`, `TARGET_POWER_OFF`,
  `SWD_NO_ACK`, `SWD_WAIT`, `SWD_FAULT`, `TARGET_UNAVAILABLE`,
  `UNSUPPORTED_PROBE_COMMAND`. A short reply, bad status, missing ACK, wrong
  serial, failed halt or failed write verification is never success.
* **Debug-state ownership** is shared `internal/cortexm` policy, not transport
  logic. DWT installation is verified and rolled back on failure; unresolved
  rollback stays in a pending cleanup ledger, never the successful-watchpoint
  cache. Clear verifies hardware disabled before removing ownership. Reset,
  operational errors, cancellation and session close clean only owned DWT/FPB
  state with a fresh bounded context. Foreign comparators and unrelated DEMCR
  bits are preserved. Primary operation errors retain their classification
  when cleanup also fails; physically disconnected hardware cannot be
  guaranteed cleaned.

## Evidence

See `docs/notes/phase-native-jlink-results.md` and `docs/support-matrix.md`.
The sanitized wire recording `testdata/real-jlink-native/wire.json` replays the
whole native stack with no probe attached; it is a real recording, not a model.

## Not verified

nRF52840 and a second probe (deferred; the other probe serves a sniffer),
multi-probe isolation, JTAG on a real chain, the reset pin, `rtt_write` on
hardware, trace/SWO, other probe generations.
