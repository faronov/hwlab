# Phase 24: native TI XDS110 (LP-XDS110ET)

Hardware: standalone **LP-XDS110ET** (not a LaunchPad with a CC1352R1; corrected
by the user). Probe `0451:bef3`, firmware 3.0.0.22, hardware id 0x23. No target
attached (P7 empty), so target-side results are UNSUPPORTED_BY_ATTACHED_HW.

## Verified on hardware

- Discovery by USB serial; 7 interfaces mapped; only interface 2 is claimed.
- CDC ACM (ttyACM1/2) and HID (interface 5) stay bound; the serial ports open
  while hwlab holds the debug interface; the cmsisdap plugin used the HID face at
  the same time.
- XDS110 framing, version, hardware id; XDS_VERSION round trip min 75 / median 80 /
  p95 92 / max 181 us (500 calls).
- SET_TCK accepted from 100 kHz to 14 MHz; precision not claimed.
- Empty-chain JTAG scan: all zeros, `stuck-low`, no device. SWD attach: explicit
  `-615` error; the probe stays usable.
- Second open -> BUSY; 25 open/close rounds; one physical unplug/replug (user
  performed): DEVICE_GONE, no hang, no stale success, same serial back, firmware
  read after reopen. Only one cycle was run, so "repeat twice" was not met.

## Not verified

Every target operation, SRST polarity, target supply (`SET_SUPPLY`), `-617` ->
no-ACK mapping, JTAG with a real chain.

## EnergyTrace

Superseded: see [xds110.md](../xds110.md#energytrace) and
[xds110-energytrace-protocol.md](../xds110-energytrace-protocol.md). EnergyTrace is now
PROTOCOL_PARTIAL (range 0, native decoder, live verified).

## Bugs found and fixed

- Old-firmware slow-clock plan rounded the period down and could exceed the
  requested clock; now rounds up.
- The board was assumed to be a LaunchPad with CC1352R1; hardware id 0x23 is the
  standalone LP-XDS110ET and is now supply-capable by id.
- The Sim modelled no pin reset; it now maps an SRST edge to a core reset.

## Gaps

Target-side validation (connect a target to P7); JTAG-DP/ICEPick; EnergyTrace
(needs a usbmon capture of a TI tool session); two unplug cycles.
