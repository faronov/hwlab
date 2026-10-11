# Phase 13 — Telink SWire results

Hardware: **none available** (no SWire programmer, no TLSR825x board). This
phase is simulator- and replay-validated only; the protocol, the debug
registers and every behaviour on real silicon are **unvalidated**.

| Item | Result |
|---|---|
| framing: CRC-16/MODBUS (check value 0x4b37), timeouts, bad CRC, dropped replies, error responses | tests against `sws.Sim` (race detector on) |
| version/configure, activate, chip id, no target, sleeping target | tests |
| SWire block read/write | tests |
| debug: stall/stop, step, registers, breakpoint, go, soft reboot + re-activate | tests |
| flash: read, JEDEC, unlock, erase, 256-byte page program, read-back verify | tests |
| flash safety: factory refusal, journal opt-in, undescribed tail, sector alignment, capacity mismatch, unlock failure; flash unchanged after every refusal | tests |
| plugin: listing, capabilities, danger levels, options, leasing, memory rules, new-file-only flash read | tests |
| replay disconnected | pass: `testdata/synthetic-telink` (labelled synthetic), `TestSyntheticTelinkSessionReplaysWithoutHardware` |
| CLI end to end | `HWLAB_TELINK_SIM=1 hwlab run --yes examples/telink-sim.yaml`: 12 invoke events, the factory erase is refused and recorded as a warning |
| real programmer / TLSR8258 | **not run**: unvalidated |
| WAIT_RESP, SWS_PRINTF, FIFO, analog registers, OTP, baud switching | **not implemented** |

## Issues found while building

- Response `count` is not always a payload length (write acks, flash CRC reply);
  parsing is per command.
- No real-hardware bugs exist to report: no hardware was involved.

## Known gaps

- Halted detection after a breakpoint hit is a heuristic.
- No native fallback for chips other than the TLSR825x family.
- Real SWire divider tuning and activation timing need a real board.
