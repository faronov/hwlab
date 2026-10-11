# ADR 0017: Generic UART as a transport plugin

Status: accepted

## Context

Several planned features need a serial line: Modbus RTU, AT-command and NMEA
analyzers, bootloader tools, vendor protocols. If each opened ports itself we
would get several divergent serial stacks. The core already has a
`SerialTransport` capability.

## Decision

- **`uart` is a transport, not a protocol.** Protocol layers (Modbus RTU, …) are
  built on `SerialTransport` and must never open serial ports themselves.
- **Capabilities:** `SerialTransport` (configure, read, write, flush, status,
  set_lines), `TimedCapture`, `LiveRawStream`. No new proto message: received and
  sent bytes are `Packet{protocol:"uart"}` events on streams `rx` / `tx`, with
  attributes `dir`, `port`, `mode` (e.g. `8N1`), `baud`. With `lines: true` the
  plugin also emits `Log` events (source `uart`, stream `lines`) per text line.
- **Chunking:** rx bytes are grouped by idle gap (default 10 ms), maximum size
  (1024 B) and maximum age (100 ms); larger chunks are split. Chunk slices are
  never reused because the sink retains them.
- **Safety:** `read`, `status`, `configure`, `flush` are SAFE; `write` and
  `set_lines` are CONFIRM. `dtr` and `rts` config keys are dangerous, since
  toggling them resets many boards or enters bootloaders.
- **Device ids** are the port base name (`ttyACM0`). Ports the OS does not
  enumerate are added via `HWLAB_UART_EXTRA_PORTS` or the `path` config key,
  which also enables pty-based end-to-end tests.
- **Library:** `go.bug.st/serial` (pure Go on Linux; enumeration needs cgo on
  macOS, as for the other USB plugins, see ADR 0009). It has no flow control, so
  `flow_control` is applied to the termios of the already-open fd, obtained from
  the library's unexported handle by reflection (`flow_unix.go`). The pty test
  asserts the resulting CRTSCTS/IXON/IXOFF/CSTOPB bits and will fail loudly if a
  library upgrade breaks the hack. Windows: flow control is reported unsupported.
- **Timing claims:** rx is stamped when the host read returns, i.e. late by an
  unmeasured USB/driver latency; tx is stamped before the write call. The clock
  info therefore declares rx as never early with an unbounded upper error and
  drops the lower bound when tx was used. No precision is claimed.

## Validation status

- Real hardware: open, configure and TX on the nRF52840 DK's J-Link VCOM
  (`testdata/real-uart`, TX-only replay fixture). **Real RX was not validated**:
  the DK firmware is silent and a loopback needs a jumper
  (`HWLAB_UART_LOOPBACK=1` hardware test is provided).
- pty: RX, framing, flow control, stop bits, and the full pipeline
  (run → timeline → replay) in `plugins/uart/e2e_linux_test.go`. A pty forces 8N,
  so data bits and parity are covered by fakes only.

## Consequences

Modbus RTU (phase 9) gets a ready transport with replayable events. A future
raw-bytes event type would only be needed if `Packet` proves too awkward for
non-packet streams; real use by Modbus should decide that.
