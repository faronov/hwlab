# Phase 27: LP-XDS110ET target power and EnergyTrace semantics

Hardware: LP-XDS110ET (`0451:bef3`, hardware id 0x23, firmware 3.0.0.x) with a
user-owned CC2340 (E104-BT55SP on a breadboard) on the 20-pin connector (pin 1 GND,
5 3V3, 11 nRST, 13 SWDIO, 15 SWCLK). Voltages were read by the user with a
multimeter; everything else is from hwlab.

## Result in one paragraph

The 3V3 pin of the 20-pin connector is the probe's own always-on rail when jumper P9
(TGT VDD) is on XDS. `XDS_SET_SUPPLY` (0x32) is acknowledged with status success and
changes nothing: the pin stays 3.3 V with the supply "off", "on 3300 mV" and
"on 2500 mV". The LP-XDS110ET has no software-switchable target supply, so
`DebugProbe.target_power` now returns `UNSUPPORTED_BY_ATTACHED_HW` on hardware id
0x23. A hard power cycle needs an external PowerSwitch
([ADR 0033](../adr/0033-power-switch.md)).

## Measurements (real hardware)

3V3 pin (pin 5 vs GND) read by the user. "EnergyTrace" is the mean/median current
from hwlab's own capture.

| # | State | 3V3 pin | EnergyTrace | Notes |
|---|---|---|---|---|
| 1 | P9 = XDS, hwlab idle, USB plugged | 3.3 V | | no hwlab command running |
| 2 | P9 = XDS, `target_power` off, device held open 60 s | **3.3 V** | | |
| 3 | P9 = XDS, SET_SUPPLY on 2500 mV, 50 s | **3.3 V** | | not 2.5 V |
| 4 | P9 = XDS, EnergyTrace stream running (45 s) | 3.3 V | 7.00 mA mean, 0 lost packets | |
| 5 | P9 = XDS, USB pulled | 0 V | | the rail comes from USB |
| 6 | P9 = XDS, USB replugged, no hwlab running | 3.3 V | | the pin is live without any capture |
| 7 | P9 = EXT, USB plugged | 0.1 V | | nothing external attached |
| 8 | P9 = EXT, SET_SUPPLY on 2500 mV | 0.1 V | | the command has no effect here either |
| 9 | P9 = EXT, EnergyTrace stream running (45 s) | 0.1 V | 2.5 uA mean | the stream does not power the pin |
| 10 | P9 = XDS again, CC2340 running | 3.3 V | 6.93 mA median (4 s) | |
| 11 | CC2340 nRESET held low 3 s (probe SET_SRST) | | 35-41 uA, back to 6.97 mA on release | nRESET reaches the MCU, current path is real |
| 12 | 3V3 wire removed (earlier session, 70 s capture) | | about 3 uA offset, 115 mA inrush on reconnect | |

| 13 | After SWD recovered: core halted (DebugProbe halt) | 3.3 V | 5.68 mA median (2 s, 512000 samples) | halting changes the current, so EnergyTrace measures the CC2340 itself |
| 14 | Same, core resumed | 3.3 V | 7.01 mA median | |

Not measured: the P9 pin voltages individually (only the jumper position was
reported), voltages on ET_VDD / LP_VDD / LS_VDD, target VCC on the CC2340 side
separately from the connector pin (the user measured the connector pin).

### Back-powering

With P9 on EXT the pin sat at 0.1 V and the EnergyTrace current at 2.5 uA, while the
probe's SWDIO/SWCLK/nRESET were driven by hwlab and the CC2340 was wired. The target
did not answer SWD (-615). So there is no meaningful back-powering of the CC2340
through SWDIO, SWCLK or nRESET (about 0.1 V on the rail, no debug access). This is a
measurement of one board, not a design guarantee.

## Classification

Case "VCC stays about 3.3 V after target_power off": the previous hwlab `target_power`
did **not** implement power gating on this probe. The earlier "supply off for 12 s"
result was therefore not a failed cut with some other cause; there was nothing to
cut. Earlier notes (phases 24-26) that "SET_SUPPLY powered the CC2340" or that
"starting the EnergyTrace stream powers the 3V3 pin" were wrong: the pin is already
live from USB with P9 on XDS, and both effects were confounded by that. They are
corrected in `docs/xds110.md`, `docs/xds110-energytrace-protocol.md` and the status
registry.

## Rail and pin mapping

Sources: the TI LP-XDS110ET Quick Start (SPRUJ01), the net names in TI's
LP-XDS110ET Allegro board file (E2E, unpacked for net names only), the XDS110 product
page and the user's measurements. The TI user guide and the schematic PDF were not
obtainable, so connectivity between nets was **not traced**; roles marked "inferred"
come from the net names only.

| Physical pin / net | What it is | Source | Destination | Software controllable | Seen by EnergyTrace |
|---|---|---|---|---|---|
| 20-pin pin 5 (3V3) | probe rail offered to the LaunchPad/target | the probe rail, USB powered, selected by P9 = XDS | target VCC (CC2340 board) | **no** (SET_SUPPLY is a no-op) | yes: the current of this pin is what EnergyTrace reports |
| P9 (TGT VDD, 3-pin jumper) | XDS / EXT selection of the TGT VDD rail | manual jumper; user: set to XDS | XDS = probe rail onto the 3V3 pin, EXT = target supplies its own rail | no (mechanical) | with XDS yes; with EXT the pin is unpowered (0.1 V, 2.5 uA) |
| P9 pins 1/2/3 individually | not measured | | | | |
| ET_VDD, ET_3V3, ET_AVDD, ET_DVDD | EnergyTrace measurement circuit rails (inferred from net names) | inferred: probe-side | inferred: EnergyTrace circuit | not tested | n/a |
| LP_VDD, LS_VDD, SW_VDD, XDS_VDD | other board rails (inferred from net names only) | not traced | not traced | not tested | not traced |
| target VCC (CC2340) | the connector pin, via the breadboard wire | pin 5 | CC2340 VDD | no | yes, same path |

Why pulling the 3V3 wire drops the EnergyTrace current: the target's supply current
flows through the probe's measurement circuit before it leaves the 3V3 pin
(USB -> probe -> EnergyTrace measurement -> 3V3 pin -> target, consistent with the
Quick Start's description of a charge-pulse measurement). Remove the wire and the
load is gone (about 3 uA offset); a nRESET hold drops it to 35-41 uA. We did not
trace this on the schematic, so the exact topology of the measurement stage is from
the Quick Start, not from the schematic.

## Protocol audit (what hwlab sends)

Frames: `*`, u16 LE length, command id, args. The reply is an i32 LE status, then data.

| Use | Command | Request args | Reply | Semantics established |
|---|---|---|---|---|
| connect/version | 0x01 / 0x03 | none | status, fw u32, hw u16 | hardware id 0x23 on this probe |
| disconnect | 0x02 | none | status | |
| set TCK | 0x04 | u32 | status | |
| nRESET (SRST) | 0x0e (`SetSRST`) | u8 level (1 = released) | status | measured: level 0 puts the CC2340 in reset (current 35 uA) |
| target supply | **0x32** `XDS_SET_SUPPLY` | u32 LE millivolts + u8 enable | status | OpenOCD sends it only to hardware id 0x21 (stand-alone). Here: status success, **no effect on the pin** |
| EnergyTrace sequence | 0x28, 0x03, 0x04, 0x46, 0x03, 0x30, 0x31, 0x1d, 0x1e x3, 0x1f; stop 0x20, 0x21 | see `docs/xds110-energytrace-protocol.md` | | none of 0x28/0x46/0x30/0x31 or the 0x1d args changes the pin voltage (3.3 V before, during, after) |

There is no separate "target power enable" or "voltage selection" command for the
LP-XDS110ET in the public OpenOCD driver beyond `XDS_SET_SUPPLY`, and TI's tooling
describes "Probe supplied power" + voltage only for the stand-alone XDS110. I did not
find a public TI protocol document; this rests on the OpenOCD driver (read for
behaviour only, nothing copied).

## What changed in hwlab

* `xds.Probe.SetSupply` and `Info.SupplyCapable`: only hardware id 0x21; other
  hardware returns `ErrNoSupply` and the command is **not sent**.
* `DebugProbe.target_power`: on hardware id 0x23 (both enable and disable) returns
  `UNSUPPORTED_BY_ATTACHED_HW`. The stand-alone 0x21 path remains, UNTESTED (no such
  probe attached).
* `DebugProbe.info`, `PowerMeasurement.info` and the method description no longer
  say the stream "may energize" the connector.
* Status registry: "target power supply" (REAL_HW_VERIFIED) is replaced by
  `target_power_source (jumper P9)` REAL_HW_VERIFIED, `target_power_switch`
  UNSUPPORTED_BY_ATTACHED_HW, `target_power_switch (stand-alone XDS110, id 0x21)`
  IMPLEMENTED_UNTESTED and `target_power_measure (EnergyTrace)` REAL_HW_VERIFIED
  (current path, not accuracy). CC2340 "power-cycle recovery" is NOT_IMPLEMENTED
  (needs a PowerSwitch).
* Hardware acceptance tests no longer call `target_power`; the power-cycle half of
  `TestCC2340ResetLoop` is gone and says why.
* New simulator-backed tests: `TestTargetPowerUnsupportedOnLPXDS110ET`,
  `TestSupplyIsGatedByHardware`.

EnergyTrace capture is still gated by `allow_target_power` at open time. That is now
documented as a conservative choice: the effect of the unknown start commands is not
established, but the pin is not switched by them.

## SWD outage after the P9 change (resolved, cause unknown)

After the jumper changes (P9 moved XDS -> EXT -> XDS, USB re-plugged) SWD did not
answer on either backend (XDS110 -615, CMSIS-DAP "no target answered"), including
with nRESET held. Module-side checks (nRST connected, SWD wires pulled from the
probe): nRESET 3.2 V, free SWDIO 3.3 V, free SWCLK 0 V, i.e. the target's default
pull-up/pull-down, so the app had not repurposed the pins. With the wires back in
place SWD answered again on both backends (Cortex-M0+ r0p1, CPUID 0x410CC601, ROM
table SCS + BPU). The cause was not identified; re-seating the SWD/nRST wires is the
only thing that changed, so a marginal contact is the most likely explanation and is
not proven. It is unrelated to the target-power semantics above.

## References

* TI LP-XDS110ET Quick Start (SPRUJ01): TGT VDD selection XDS / EXT, EnergyTrace.
* TI XDS110 product page: target voltage 1.8-3.6 V and "Probe supplied power" for
  the stand-alone probe.
* OpenOCD `src/jtag/drivers/xds110.c`: `XDS_SET_SUPPLY` 0x32, used only for hardware
  id 0x21.
* TI LP-XDS110ET Allegro board file (E2E thread 1665231): net names.
