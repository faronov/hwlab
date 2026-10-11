# Phase 6 — generic CAN results

Hardware: **none available** (no CAN adapter, no `can`/`vcan` kernel modules, no
sudo to load them). This phase is therefore replay/synthetic-validated only,
and nothing here is marked hardware-validated.

| Item | Result |
|---|---|
| frame model (classic, extended, RTR, error, CAN-FD) | unit tests, both encodings (pcap network order, kernel native order) |
| virtual bus: loopback, own echo, FD gating, filters, close | tests |
| plugin: discovery, open/close, status, recv, send (CONFIRM), inject | tests |
| capture: events, tx/rx direction, filters, drops, bus-loss failure | tests (race detector on) |
| pcapng artifact (link type 227) | round-trip with `pcap.ReadNG`; **tshark dissects CAN / extended / RTR / CAN-FD** |
| SocketCAN socket layer | tested over a unix datagram socketpair: classic/FD/RTR decode, timeout, send encoding, FD gating, Close unblocking a blocked Recv; sysfs interface discovery fixture |
| real SocketCAN / vcan / adapter | **not run**: unvalidated |
| `MSG_CONFIRM` echo, kernel timestamps, kernel filters, ENETDOWN | **unvalidated** |
| replay disconnected | pass: `testdata/synthetic-can` (labelled synthetic), `TestSyntheticCANSessionReplaysWithoutHardware` |
| CLI end to end | `HWLAB_CAN_VIRTUAL=1 hwlab run --yes examples/can-virtual.yaml`: 6 packets, tx/rx flagged, pcapng decoded by tshark |

## Notes

- The pcap SocketCAN layout has no direction field; direction is carried in
  `attr.dir` (found by the replay test, now documented in ADR 0019).
- A unix datagram socketpair does not signal a vanished peer the way a CAN
  socket reports `ENETDOWN`, so that path is covered by reading only.

## To finish validation

On a host with `can`, `can_raw` and `vcan` modules (or any SocketCAN adapter):

```sh
sudo ip link add dev vcan0 type vcan && sudo ip link set vcan0 up
hwlab run --yes examples/can-virtual.yaml   # with device: vcan0
cansend vcan0 123#DEADBEEF                  # appears as an rx packet
```

then a real adapter on a live bus for kernel timestamps and own-echo behaviour.
