# ADR 0037: Silicon Labs Wireless Starter Kit (native Go, AEM and PTI)

Status: accepted

## Context

The Silicon Labs Wireless Starter Kit mainboard (BRD4001A) carries a J-Link
OB, an Advanced Energy Monitor (AEM) measuring the radio board's supply, and
a Packet Trace Interface (PTI) receiver for the EFR32 radio. Simplicity
Commander and `silink` reach these functions over the J-Link USB interface or,
when the kit is on Ethernet, over TCP ports 4900–4905. hwlab already has a
native J-Link USB transport (`plugins/jlinknative`, [ADR 0034](0034-native-jlink.md)).
The vendor tools are proprietary and should not be needed at runtime.

## Decision

* A `wstk` plugin in native Go with a transport-independent protocol package
  (`plugins/wstk/protocol`). No Commander, `silink` or SEGGER library at runtime.
* **USB** runs on J-Link **EMUCOM**, a J-Link command (`0xEE`) that reads and
  writes numbered channels of the probe firmware:
  * read `ee 00 <channel u32> <max u32>` → u32 length in its own transfer,
    then the data; write `ee 01 <channel> <length>` + data → u32 accepted;
  * channel 0x10000 is kit control (firmware version, board records:
    BRD number, revision, serial, date), 0x10002 the AEM stream;
  * the data of a read can end on a whole USB packet and is then followed
    by a zero-length packet. It must be consumed (read the remainder with a
    buffer one byte larger), or the probe's next OUT transfer stalls. This
    needed a new `ExchangeUpTo` in `jlinknative/usb`. The simulated kit
    serves remainders in the same chunks and fails if the ZLP would be left.
* **AEM over USB**: a start request makes the kit stream 256-sample buffers at
  10 kSa/s (f32 mA, one voltage per buffer, µs start time). Polling faster
  than a buffer fills returns the newest buffer again; buffers are identified
  by start time, the 32-bit µs wrap is resolved with the message's ms queue
  time. There is no stop command; closing the channel ends the stream.
  Output is `PowerBatch` events in the `ppk2` format.
* **Clock**: kit time (µs buffer start, ms queue time) is mapped to the
  session clock with the windowed minimum-offset estimator of the
  `nrf802154` sniffer, moved unchanged to `internal/clockmap` so both plugins
  share it (`nrf802154/protocol` keeps aliases). Alignment is late by up to
  1 ms plus the unknown minimum USB transit; the kit's sample spacing is used
  as is.
* **Ethernet** uses the same parsers: the admin console (TCP 4902, prompt
  `WSTK> `) and the Debug Channel (TCP 4905, `[` u16 length message `]`,
  DCH v2/v3). PTI messages (EFR TX/RX) are decoded to 802.15.4 frames with
  RSSI, channel and status and written to pcapng with the `nrf802154` link
  type. AEM over Ethernet polls `aem current`/`aem voltage` (≤ 50 Sa/s).
* **Safety**: all methods are `DANGER_SAFE` and read-only. Admin commands go
  through an allowlist (`sys ver`, `boardid list`, `dbg info`, `aem
  current|voltage|info`, `pti info`, `dch info`, `time info`). hwlab never
  sends `target …`, `sys reset`, `dbg mode`, `aem calibrate`, or the kit
  control command `11 03`. During development an admin `target status`
  (issued through the vendor tunnel) made the board controller re-enumerate,
  which is why the target command family is excluded wholesale.
* Device listing identifies the boards once (J-Link held ~80 ms, 1 s timeout,
  retried at most once a minute, skipped while busy, `HWLAB_WSTK_LIST_IDENTIFY=0`
  disables it). A USB capture holds the J-Link for its whole duration.

## Not done

* **PTI and admin over USB.** `silink` reaches them through a socket tunnel on
  EMUCOM channel 0x10003, initialised with a kit command that also touches the
  VCOM. Reproducing it safely needs more traffic analysis and a PTI-emitting
  target; estimate 1–2 days. Until then PTI needs the kit on Ethernet.
* The Ethernet path has not run on hardware (no kit on the LAN). Whether DCH
  4905 streams without a request, the PTI channel/RSSI mapping and the admin
  reply formats of `aem current` and `boardid list` are unverified.
* AEM accuracy and range switching are not checked against a reference load.

## Consequences

* hwlab measures WSTK energy with a USB cable only, in the same artifacts as
  the PPK2; the kit's J-Link is unavailable to other tools during a capture.
* PTI capture is implemented and tested against synthetic DCH streams only.
* Protocol provenance: [licenses](../licenses.md); results:
  [wstk-native-results](../notes/wstk-native-results.md).
