# Phase 9 — Modbus results

Hardware: **none available** (no Modbus slave, RS-485 adapter or serial
loopback on the host). This phase is simulator- and replay-validated only; the
RTU timing and every real device behaviour are **unvalidated**.

| Item | Result |
|---|---|
| PDU encode/parse/validate for functions 1-6, 15, 16, 22, 23 and exceptions | unit tests (race detector on) |
| CRC-16, RTU frame extractor (resync, bad-CRC boundary rule, unmodelled functions) | tests |
| MBAP/TCP framing and the TCP master | tests over a loopback listener |
| Client: retries on reads only, writes never retried, exception handling, timeouts | tests against `modbus.Slave` |
| value interpretation (`u16/i16/u32/i32/f32`, word order) | tests |
| plugin: listing, danger levels, reads, writes, error mapping, lease exclusivity | tests |
| polling capture: request/response packets, decoded transactions, validation, cancel | tests |
| offline analysis: pairing, exception, no response, CRC error, limits | `internal/modbus` and `internal/app` tests on synthetic uart packets |
| replay disconnected | pass: `testdata/synthetic-modbus` (labelled synthetic), `TestSyntheticModbusSessionReplaysWithoutHardware` |
| CLI/MCP | `hwlab modbus decode`, MCP `modbus_decode`; `hwlab run --yes examples/modbus-fake.yaml` produced 18 decoded transactions, 36 packets, 3 invokes (the third a deliberate exception) |
| real Modbus slave (RTU or TCP) | **not run**: unvalidated |
| RS-485 timing, vendor quirks | **not run** |

## Issues found while building

- The first RTU extractor mis-cut a good frame that followed noise and let a
  trailing stray byte turn a good frame into a CRC error. Fixed with the
  boundary rule and the "unmodelled function" rule; regression tests added.
- A transient `Invoke` does not open the device, so exclusivity tests must call
  `OpenDevice` first (as `hwlab run` does).
- A synthetic fixture quirk: the counter input grows per request, so its
  recorded values are deterministic only in order, not in time.

## Known gaps

- No analog plotting of register values.
- No RTU-over-TCP, ASCII, or bus scanning (deliberate).
- Latencies are host-timestamp differences.

## To finish validation

1. Connect any Modbus device: TCP (`HWLAB_MODBUS_HOSTS="meter=tcp://ip:502"`)
   or an RS-485 adapter (`rtu:/dev/ttyUSB0@9600:8N1`).
2. `hwlab invoke modbus/meter Modbus.read_holding_registers '{"address":0,"quantity":2}'`;
   try `as: f32` and both `word_order` values on a known register.
3. Record `examples/modbus-fake.yaml` with `device:` set to the real device.
4. For passive analysis, record both directions with the `uart` plugin and run
   `hwlab modbus decode --mode sniff`.
