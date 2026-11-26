# ADR 0042: Target reset during a WSTK AEM capture

Status: accepted

## Context

A USB AEM capture of a Silicon Labs WSTK ([ADR 0037](0037-wstk-native.md))
reads the energy monitor through EMUCOM channels of the kit's on-board J-Link
and keeps that J-Link open for the whole capture. A J-Link USB interface is
claimed by one process at a time (usbfs claim on Linux, exclusive IOKit open on
macOS), so the `jlink` and `jlinknative` plugins, which run in their own
processes, cannot reach the target while the current is measured. The common
measurement "reset the target, then measure its boot" was therefore impossible.

Options considered:

1. **Share the USB device between processes.** SEGGER's DLL does this with
   its own inter-process arbitration; hwlab's native transport has no such
   layer and a plugin cannot hand an open interface to another process.
   Rejected.
2. **Stop the capture, reset, restart.** Loses exactly the interval of
   interest. Rejected.
3. **The kit's own reset.** The admin console's `target reset` needs the kit on
   Ethernet and belongs to the `target` command family that hwlab never sends
   (it re-enumerated the board controller once, ADR 0037). The kit control
   command space over USB is not mapped. Rejected.
4. **Multiplex the one connection inside the wstk plugin.** The J-Link command
   set is strictly request/reply. EMUCOM reads (`0xEE`, several transfers each)
   and the probe's own commands can follow each other on the same connection
   as long as no operation is split by another.

## Decision

Option 4. The wstk plugin offers `DebugProbe.reset` (danger: confirm) for USB
kits:

* During a USB AEM capture of the kit the reset goes through the capture's
  connection. The AEM loop takes a lock around each EMUCOM read, the reset
  around each probe command, so no operation is interleaved inside another;
  the AEM stream keeps running. A capture that ends waits for a reset in
  progress before it releases the connection, so nRESET is never left low by a
  closed connection.
* Without a capture the kit's J-Link is opened for the reset and released.
* Only the **nRESET line** is driven (J-Link commands `0xDC` low, `0xDD` high,
  `hold_ms` 1..1000, default 20), never an SWD attach. A SYSRESETREQ would need
  debug power-up requests and C_DEBUGEN, which change the very current being
  measured and outlive the reset. The release is sent even when the request is
  cancelled and is retried.
* Markers `target_reset` and `target_reset_release` go on the capture's
  timeline at the host times of the commands, and the result reports whether
  the AEM current shows the reset (`reset_observed`: a drop below half of the
  level before the reset that lasts at least half the hold), the currents
  before and during, the AEM stamping lag of the drop and the recovery time.
  A reset outside a capture is reported as not observed.
* A reset is reached from an experiment step on the AEM device
  (`call: DebugProbe.reset`), or with `device_invoke` on the MCP server that
  runs the capture. A second CLI process (`hwlab invoke` while `hwlab run`
  captures) starts its own plugin process and is refused: the probe is held.

"Reset, then capture N ms" is an experiment:

```yaml
name: boot-current
duration: 3s
devices:
  aem: {plugin: wstk, device: wstk:<serial>, requires: [PowerMeasurement]}
steps:
  - {name: reset, at: 500ms, device: aem, call: DebugProbe.reset, args: {hold_ms: 20}}
```

## Evidence (BRD4001A fw 1.4.6 b1171 + BRD4187A EFR32MG24, macOS)

Three 3 s CLI captures with one reset step each (hold 20, 50 and 10 ms) and
one 4 s MCP run (`experiment_run` with `wait: 1s`, then `device_invoke`
reset, then `capture_status`) while the target ran a Zigbee router at
10-11 mA: every reset was seen (current to ~0.48 mA for the hold, then the
boot), no dropped buffer, no transport error. A reset from a second CLI
process was refused with PROBE_BUSY and did not disturb the capture. The drop
was stamped 19-33 ms after the assert command on the session timeline: the
AEM samples are late by that much, not by at most about 1 ms as ADR 0037
assumed. Why is not established (open question; one
hypothesis is that the buffer stamp is not the time of its first sample).

## Consequences

* The current around a reset can be measured on a WSTK with one USB cable.
* The marker times are host command times; read the AEM drop with the
  reported `aem_lag_ms` in mind until the AEM clock mapping is corrected.
* No SWD reset or other debug access exists during a capture; the wstk plugin
  is not a debugger.
