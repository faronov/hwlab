# ADR 0022: Modbus as a protocol layer over generic transports

Status: accepted

## Context

Modbus is a request/response protocol over two very different carriers: RTU on
a serial line (often RS-485) and TCP (MBAP framing). The generic `uart`
plugin ([ADR 0017](0017-generic-uart.md)) already owns serial ports. No Modbus
device, RS-485 adapter or loopback pair was available, so nothing here has met a
real slave.

## Decision

- **Protocol layer, not a second UART.** `internal/modbus` has no I/O of its
  own: PDU/ADU encoding, CRC-16, an RTU frame extractor, a `Client`, a
  `Slave` simulator and an offline `Analyze`. The RTU client runs on a
  byte-stream interface satisfied by the uart package's port (`uart.OpenPort`
  plus `modbus.NewRTU`); TCP uses `modbus.DialTCP`. The plugin only wires these
  to the plugin protocol.
- **Devices are listed, not discovered.** `HWLAB_MODBUS_HOSTS` names slaves:
  `name=tcp://host[:502]` or `name=rtu:/dev/ttyUSB0[@baud[:8E1]]` (RTU default
  19200 8E1). Probing a bus for slaves would transmit at every address, which is
  not a safe discovery operation. `HWLAB_MODBUS_FAKE=1` adds the synthetic
  `fake-modbus`.
- **Capabilities.** `Modbus` (the master operations), `ProtocolDecode`,
  `PacketCapture` and `TimedCapture`. Reads (functions 1-4) are SAFE; every
  write (5, 6, 15, 16, 22, 23) and `raw` are CONFIRM.
- **Writes are never retried.** A retry after a timeout may apply a write
  twice; reads retry on timeout or CRC error up to `retries` (default 0).
- **Capture is read-only polling.** A capture runs configured read polls and
  emits, per transaction, a `request` and a `response` packet (protocol
  `modbus-rtu` or `modbus-tcp`, data = the ADU) and a decoded `modbus` event
  with the interpreted request/response, status
  (`ok|exception|timeout|crc_error|error`) and the round trip as its duration.
  Write polls are refused at configuration time.
- **RTU framing rules** (`ExtractRTU`): frame length comes from the function
  code; CRC is verified; a frame of known length with a bad CRC is consumed (and
  reported) only when what follows is empty or starts a valid frame, otherwise
  the first byte is junk and the extractor resynchronises; an unmodelled function
  code is accepted as the whole buffer only with a valid CRC.
- **Offline analysis.** `app.DecodeModbus` (CLI `hwlab modbus decode`, MCP
  `modbus_decode`) reads the raw serial packets of any session, live or
  replayed, cuts them into frames, pairs requests with responses and reports
  statistics and a limited transaction list. `mode` says what the capturing host
  was: `master`, `slave` or `sniff`. A bad-CRC response is reported as its own
  transaction and leaves its request unanswered; it is never repaired.
- **Latency honesty.** Analysis latency is the difference of timeline chunk
  stamps (tx stamped before the write call, rx after the read returned), not
  wire time. The plugin's own RTT is a host-clock measurement. Both say so.
- **Simulator.** `modbus.Slave` plus the plugin's in-process fake serve fixed
  tables (including a float32 across two registers and a counter input) over an
  in-memory link. It embodies the author's reading of the specification and is
  **not evidence about real devices**; its fixture is labelled synthetic.

## Consequences

- No real device has run: framing timing (3.5-character silence), RS-485
  turnaround, vendor quirks (word order, off-by-one addressing, nonstandard
  exceptions) are untested. `word_order` and `as` are provided but unproven on
  real data.
- Register values are not yet plottable as analog tracks; they live in decoded
  event fields only. Mapping polls to analog streams is a gap.
- TCP is MBAP only (no RTU-over-TCP, no ASCII).
- Passive RTU sniffing relies on the uart plugin's timestamps; one with a
  coarse read size will smear inter-frame gaps, and the analysis gap is
  configurable (`--gap`).
