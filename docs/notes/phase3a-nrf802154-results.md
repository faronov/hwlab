# Phase 3A — nRF52840 802.15.4 results

Hardware: nRF52840 DK with the Nordic 802.15.4 sniffer firmware (1915:154b).
Re-verified this session against the user's live network (channel 15).

The retained fixtures no longer contain that network's frame bytes or device
identities: they preserve timing/radio metadata with synthetic MAC frames.
The PPK2 session baseline is decimated 10:1; see `testdata/README.md`.

| Acceptance item | Result |
|---|---|
| Real nRF52840 capture | PASS (62 frames in 4.5 s, channel 15) |
| Channel scan 11-26 | PASS (ch15 active, 6 pkts at -74 dBm mean; ch20 1 pkt) |
| RSSI / LQI | PASS (reported per channel and per packet) |
| Valid pcapng | PASS (`capinfos`/`tshark` read all 62 frames) |
| Raw capture without tshark | PASS (`--no-decode`; tshark is optional) |
| Decode | PASS: 802.15.4 and ZigBee layers; ACK frames present |
| Clean cancellation | PASS (SIGINT at 4 s of a 60 s capture: complete session, no stray plugin process) |
| Replay with device disconnected | PASS (`TestRealSessionReplaysWithoutHardware`) |
| Real short fixture | `testdata/real-session` (sniffer + PPK2) |

## Gaps (not claimed)

- Decoded events carry the tshark layer stack and info text. There is no
  hwlab-level classification of joins/rejoins or retransmissions.
- Thread (6LoWPAN/MLE/CoAP) decode goes through the same tshark path but was
  **not exercised**: no Thread traffic was observed on this host.
- Zigbee payloads are decoded only if tshark has the network key; the live
  network is encrypted, so APS/ZCL content is not visible.
