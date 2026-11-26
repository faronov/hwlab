# Silicon Labs WSTK (native): hardware results

Device identifiers below are fictional (`<serial>`); board numbers and
revisions are model facts.

Kit: BRD4001A Rev A02 mainboard with a BRD4183A Rev A00 radio board
(EFR32MG22C224F512IM40), USB to macOS, kit firmware 1.4.6 build 1171. The
target ran unrelated firmware owned by another user; hwlab did not program,
reset, halt or power-cycle it. The J-Link was shared with that user, so runs
were short and started only when no other J-Link client was active.

## USB (EMUCOM)

| Check | Result |
|---|---|
| Discovery | `wstk/wstk:<serial>`, named "Silicon Labs Wireless Starter Kit (BRD4001A + BRD4183A)", with board serials/revisions/dates, firmware, VCOM path and links to `jlink`, `jlinknative` and `uart` devices |
| `PowerMeasurement.info` | firmware 1.4.6 b1171, both board records |
| `read_current` | 256 samples, mean 2.18 mA, 3.336 V |
| AEM 1 s | 10,000 samples |
| AEM 5 s (×2, hardware test) | 50,000 samples each, no gaps or step jumps; one run's samples spanned 5.025 s of session time (cause not established, probably a clock-offset step at start) |
| `hwlab run` 5 s + `hwlab analyze` | 50,000 samples; mean 2.182 mA, min 2.095 mA, max 2.479 mA; charge 10,911 µC, energy 36,403 µJ; plot rendered |
| Clock | jitter of the per-buffer mapping 1.1–5.6 ms; kit vs host drift about −50 ppm (few-second runs, rough) |

The first capture attempt stalled after the first full-packet read: the
trailing zero-length packet was not consumed. Fixed in the EMUCOM reader;
all later runs were clean.

## Not run on hardware

* Ethernet (admin 4902, DCH 4905, AEM polling, PTI → pcapng): no kit on the
  LAN; synthetic tests only.
* PTI: no Ethernet, and the target firmware was not known to enable PTI.
* AEM accuracy against a reference load.

## Incident

While mapping the protocol with the vendor tools (before the plugin existed),
an admin-console `target status` sent through `silink` made the kit's board
controller re-enumerate on USB. It recovered by itself; the effect on the
other user's debug session is unknown. hwlab's admin allowlist excludes all
`target` commands.

## Target reset during an AEM capture (BRD4187A)

Kit BRD4001A (fw 1.4.6 b1171) with a BRD4187A radio board (EFR32MG24) running
a Zigbee router (another user's firmware; the user agreed to resets). The
first three runs were `hwlab run --yes` of a 3 s AEM capture with one
experiment step `DebugProbe.reset`
([ADR 0042](../adr/0042-wstk-reset-during-aem-capture.md)).

| hold | before | in reset | drop stamped after the assert | recovery to half the level | samples | dropped buffers |
|---|---|---|---|---|---|---|
| 20 ms | 11.26 mA | ~0.48 mA (low for ~24 ms) | 32.9 ms | (first build: not computed) | 30,000 | 0 |
| 50 ms | 10.16 mA | 0.49 mA | 32.7 ms | 53.5 ms | 30,000 | 0 |
| 10 ms | 10.16 mA | 0.51 mA | 24.4 ms | 126.9 ms (boot below 5 mA) | 30,000 | 0 |
| 20 ms (MCP) | 10.15 mA | 0.49 mA | 18.8 ms | 23.6 ms | 40,000 | 0 |

The last row ran over MCP: `experiment_run` of a 4 s AEM experiment with
`wait: 1s` returned `state: running`, `device_invoke` `DebugProbe.reset`
(`confirm: true`) reset the target through the running capture, and
`capture_status` with the session returned the finished result (40,000
samples, both markers). A `hwlab invoke ... DebugProbe.reset` from a second
process during a CLI capture was refused (`PROBE_BUSY: interface 2 is held by
pid ..., hwlab-plugin-wstk`) and the capture had no gap.

* nRESET of the kit J-Link reaches the BRD4187A: the current falls from the
  router's 10-11 mA to about 0.48 mA for the hold and the boot follows.
* EMUCOM reads and the reset commands share the one USB connection without a
  transport error or a gap in the AEM stream.
* The drop appears 19-33 ms after the host time of the assert command, so the
  AEM samples are stamped that much late on the session timeline. ADR 0037's
  "late by up to 1 ms plus USB transit" does not hold on this kit; the cause
  is open. A raw dump of the AEM messages showed each buffer first returned
  about 123-125 ms after its microsecond start stamp (by the millisecond
  message time) and then repeated until the next one, 25.6 ms later.

## Kit without AEM (xG24 Explorer Kit)

An EK2703A (J-Link OB, kit firmware 2.0.0 build 402) is found by the same USB
discovery and identified read-only (board list "BRD2703A Rev. A02", mainboard
field BRD2703). It has no AEM; an AEM capture attempt failed with a protocol
error. The plugin now lists it as "Silicon Labs kit without AEM (BRD2703A)"
with no capabilities and `aem: no`; `read_current` and `hwlab run` are refused
(`UNSUPPORTED_BY_ATTACHED_HW`, and "cannot capture" from the core) without an
AEM request. The kit runs another user's firmware: it was not reset, flashed
or captured from.
