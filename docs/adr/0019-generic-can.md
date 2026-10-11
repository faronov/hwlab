# ADR 0019: Generic CAN / CAN-FD as a transport plugin

Status: accepted

## Context

CAN is the bus under ISO-TP, UDS, J1939 and CANopen, and USB-CAN adapters will
arrive later. Each of those must see one frame model, not one per backend. The
core already had the `CANTransport` capability id.

## Decision

- **`can` is a transport, not a protocol.** Analyzers consume `can.Frame`
  (package `internal/can`) and never open an interface themselves.
- **One frame model:** `Frame{Interface, ID, Extended, RTR, Error, FD, BRS, ESI,
  DLC, Data, Own}` covers classic CAN and CAN-FD. `Validate` enforces the real
  limits (11/29-bit ids, 0-8 classic bytes, the legal FD lengths 12/16/20/24/32/48/64,
  RTR is classic-only and has no payload, BRS/ESI need FD).
- **Event shape:** no new proto message. A frame is a `Packet{protocol:"can",
  link_type:227}` whose `data` is the SocketCAN pcap layout (network-order id word,
  length, flags), plus attributes `interface, dir, kind, id, extended, dlc,
  payload` (`brs`, `esi` for FD). The same bytes go to the pcapng artifact, so
  Wireshark dissects it with no help (verified with tshark: CAN, extended id,
  RTR and CAN-FD frames). Direction is **not** part of the pcap layout, so it
  travels in `attr.dir`; use `can.FromPacket(data, attrs)` to rebuild a frame.
- **Backends behind `Bus`** (`Recv(timeout)`, `Send`, `Close`): Linux SocketCAN
  (`AF_CAN`/`CAN_RAW` through `golang.org/x/sys/unix`, pure Go, no cgo) and an
  in-process virtual bus. A USB-CAN adapter becomes a third `Bus` with no change
  to the capture, MCP or analyzers. SocketCAN is Linux-only; other platforms
  list no interfaces.
- **SocketCAN specifics:** `CAN_RAW_FD_FRAMES` only when requested and the MTU is
  72; `CAN_RAW_RECV_OWN_MSGS` so a capture records what the host sent (the echo
  is flagged `MSG_CONFIRM` and becomes `dir=tx`); optional kernel `CAN_RAW_FILTER`
  from candump-style filters; `SO_TIMESTAMPNS` for kernel receive times. Reads
  poll on the socket plus an eventfd, so `Close` and cancellation interrupt a
  blocked read and the capture ends cleanly.
- **hwlab never configures the interface.** Bit rate, FD, termination and
  up/down are `ip link`'s job; a down interface is refused with the command to
  run. This keeps the plugin from silently changing a bus other nodes depend on.
- **Safety:** `status` and `recv` are SAFE; `send` is **CONFIRM** (a frame can
  actuate every node on the bus), validated before any write. `inject` exists
  only on the virtual bus and is SAFE since nothing leaves the process. A
  capture on an interface without echo (`own: false`) records sends itself,
  stamped before the write, so a transmit never appears after its reply.
- **Drops are not hidden:** live events use `TryEmit` (a CAN bus cannot be
  paused); every frame still goes to the pcapng, so the file is the complete
  record and `events_dropped_batches` states the difference.
- **Clock honesty:** a kernel timestamp is late only by the unmeasured driver
  interrupt latency and the frame began earlier by its bus time; a host stamp
  adds wakeup latency. Nothing was measured against a bus reference, so no
  accuracy is claimed. The virtual bus states that its stamps only order frames.
- **Virtual bus:** enabled by `HWLAB_CAN_VIRTUAL=1`, listed as `virtual-can0`,
  and everywhere labelled synthetic (device transport `virtual`, stream and
  artifact attribute `synthetic=true`, clock note). It exists for CI, demos and
  fixtures, never as evidence about a bus.

## Validation status

| Part | Status |
| --- | --- |
| Frame model, both encodings, filters | unit tests |
| Virtual bus, plugin, capture, pcapng, tshark decode, replay | tests + synthetic fixture (`testdata/synthetic-can`) |
| SocketCAN socket layer | tested over a datagram socketpair (framing, FD, RTR, timeouts, close unblocking, send encoding) and a sysfs fixture; **not run against a real or vcan interface** |
| `MSG_CONFIRM` own echo, kernel timestamps, kernel filters, ENETDOWN handling | **unvalidated** (needs a kernel with `can`/`vcan` modules or a CAN adapter) |
| Physical bus | **unvalidated**, no hardware |

## Consequences

- ISO-TP, UDS, J1939 and CANopen are decoders over `can` packet events or the
  pcapng; none exist yet.
- Adding a USB-CAN backend means implementing `Bus`, `Lister` and `Opener`.
- Error frames are decoded only as raw id/data until an analyzer interprets the
  kernel error classes.
