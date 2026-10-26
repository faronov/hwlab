# Telink MemoryAccess.watch and retention-sleep hardware results

Same board and programmer as `telink-debug-hardware-results.md` (TB-04-style
TLSR825x, chip id 0x5562 rev 0x02, pvvx SWire firmware on a CH340). All calls
went through the hwlab MCP server (`target_op` / `device_invoke`), layout
`tb04-512k-nofactory`.

## Why watch exists

A deep-retention sleeper answers SWire only while awake. Each CLI invocation
opens the port (~1.2 s), so CLI polling never caught a wake. `MemoryAccess.watch`
samples one window back-to-back inside a single invocation and reports
awake/unreachable segments; a sleeping core gives programmer error 3 (SWire
read timeout), which is classified as "unreachable" and costs ~3-5 ms.

`prime` writes up to four SRAM blocks on the first awake sample and reads them
back. With `halt_on_wake` the core is stalled first (which keeps it awake), the
writes are made and verified, and the core is always resumed. Only the first
wake is disturbed.

## Sequences run

Firmware: zigbee-rs TLSR8258 retention sensor (unjoined), two images differing
only in the joined slow-poll interval.

| Sequence | Result |
|---|---|
| `target_op flash-erase` journals + `flash-program` whole image at 0x0 (twice, two images) | 782 pages programmed and readback-verified each time; images booted |
| watch 256 B, no prime, 120 s | ~20 wakes seen at ~5.03 s; each wake one sample long |
| watch 256 B, prime + `halt_on_wake` | prime never ran: a 256 B read (~20 ms) outlasts the wake |
| watch 3 B, `interval_ms 0`, prime + `halt_on_wake`, 60 s (each image) | prime OK (core stalled at the wake PC, 3 blocks written + verified, resumed within ~40 ms); then 11-12 wakes, 4-5 samples (10-17 ms) each |
| watch on primed blocks below `_retention_limit_` | values kept across every observed retention wake |
| watch on a primed block above `_retention_limit_` (0x84c000) | lost after each sleep: random content with differing bit flips (unpowered SRAM) |
| `target_op flash-restore` with `allow_journals` | 46 sectors rewritten, flash hash equals the backup; router image boots and stays reachable |

## Limits

- Wake detection resolution is the sample time (~3 ms at 3 B, ~20 ms at 256 B);
  short wakes can be missed with large windows or non-zero intervals (seen as
  10 s / 15 s gaps).
- Telink invocations must not run in parallel: two concurrent invocations share
  the serial port and corrupt each other's frames.
- SRAM writes were checked only on a stalled core.
