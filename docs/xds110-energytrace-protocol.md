# LP-XDS110ET EnergyTrace protocol (clean-room notes)

Status: **PROTOCOL_PARTIAL** for range 0; other ranges, voltage and energy are
not decoded. Hardware: LP-XDS110ET (USB `0451:bef3`), firmware 3.0.0.32,
hardware id `0x23`.

## Method and provenance

* Wire behaviour was observed from real USB traffic of the official TI tool
  (`stune` from CCS 12.8.1) used **only as a behavioural oracle**, with an
  LD_PRELOAD shim that logs usbfs URBs (usbmon cannot copy the payload of the
  high-rate bulk endpoint). No TI or GPL code was read, decompiled or copied.
* Every statement below carries a confidence label:
  **CONFIRMED** (reproduced by hwlab against the oracle CSV or live hardware),
  **STRONG_EVIDENCE**, **HYPOTHESIS**, **UNKNOWN**.
* No command was guessed, scanned or brute-forced. hwlab sends exactly the
  sequence below and nothing else.
* The raw oracle captures are not committed. The sanitized replay fixture is
  `plugins/xds110/energytrace/testdata/real-lpxds110et/fixture.json` (no serial,
  no host data).

## USB layout (CONFIRMED)

| if | class | endpoints | driver | use |
|----|-------|-----------|--------|-----|
| 0/1 | CDC ACM | | cdc_acm | UART |
| 2 | vendor | OUT 0x02 / IN 0x83 | none (hwlab) | command channel, same framing as the debug protocol |
| 3/4 | CDC ACM | | cdc_acm | UART |
| 5 | HID | | usbhid | CMSIS-DAP v1 |
| 6 | vendor | OUT 0x05 / IN 0x87 | none (hwlab) | **EnergyTrace data stream** |

hwlab claims only interfaces 2 and 6; the CDC ports stay usable during a
capture (verified live).

## Command channel (CONFIRMED)

Frame: `'*'`, u16 LE length, command id, arguments. Reply payload: i32 LE
status followed by data. Status `-644` on stop/release means "already stopped"
and is tolerated; any other non-zero status aborts the start.

Start sequence (all replies status 0):

| # | cmd | args | reply data | meaning |
|---|-----|------|-----------|---------|
| 1 | 0x28 | | | UNKNOWN |
| 2 | 0x03 | | `32000003 2300` | version / hardware id (0x23 required; CONFIRMED as identity) |
| 3 | 0x04 | `02000000` | | same command id as debug `SetTCK`; sent unchanged (STRONG_EVIDENCE) |
| 4 | 0x46 | | `0200000007` | UNKNOWN |
| 5 | 0x03 | | | repeat of 2 |
| 6 | 0x30 | `00` | | UNKNOWN |
| 7 | 0x31 | `01` | | UNKNOWN |
| 8 | 0x1d | `010080bb000000` | | stream configuration; `80bb` = 0xbb80 = 48000 also appears in the stream prelude, meaning UNKNOWN |
| 9 | 0x1e | `0000` / `0400` / `0100` (u16 range) | `u32 sum, u32 count` | calibration for ranges 0, 4, 1 (CONFIRMED format, meaning of ranges 4 and 1 UNKNOWN) |
| 10 | 0x1f | | | start streaming |

Stop: `0x20` (stop), drain, `0x21` (release), `0x02` (disconnect). The TI tool
repeats 0x20/0x21 which then return -644. TI never sends `0x01` (connect);
hwlab reconnects the command channel afterwards so debug works again.

## Stream on IN 0x87 (CONFIRMED against the oracle CSV)

* First read: 3-byte prelude `35 80 bb` (marker 0x35, u16 LE 48000; meaning of
  the value UNKNOWN).
* Data packet, 1030 bytes: `0x68`, sequence u8 (wraps ff→00), u16 LE payload
  length (1024), 2 zero bytes, then 256 × u32 big-endian samples.
* Sample code `v = w >> 8` (24 bit). The low byte (high nibble varies) is
  ignored: HYPOTHESIS that it is noise/flags.
* Rate: 256 kSPS (period 3906.25 ns), 256 samples per packet, i.e. 1000 packets/s
  (CONFIRMED by wall clock and by sample counts of 10 s captures).
* The first data packet is dropped (the TI CSV row i corresponds to stream sample
  i+256; CONFIRMED).

## Current formula (CONFIRMED for range 0 around 7 mA)

```
current_nA = 15.135849 * (v - sum/count)
```

`sum/count` is the range-0 calibration reply of command 0x1e. Residual against
the oracle CSV: rms 0.29 nA, max 0.51 nA (rounding of the CSV). The gain
15.135849 nA/count is derived from the oracle, **not** from an independent
calibrated load.

## What is not decoded (UNKNOWN)

* Behaviour of ranges 4 and 1 (calibrations exist, stream format at other
  ranges unobserved; needs a load or a range switch).
* Voltage and energy: the TI CSV computes them host-side; the stream carries
  neither. hwlab reports "voltage not measured".
* Meaning of commands 0x28, 0x46, 0x30, 0x31 and the arguments of 0x1d.
* Starting the stream does **not** switch the 3V3 pin (corrected in phase 27): with
  jumper P9 on XDS the pin is 3.3 V with USB plugged and no capture running, and
  with P9 on EXT it stays 0.1 V during a capture. hwlab keeps the capture gated
  behind `allow_target_power` and confirmation as a conservative choice because
  the unknown start commands are not understood. `XDS_SET_SUPPLY` also does not
  change the pin ([phase 27](notes/phase27-lp-xds110et-target-power.md)).
* The ~6.95 mA
  reading is the user's CC2340 board on the 20-pin debug connector: pulling its 3V3
  wire gave +3.4 uA (zero offset, noise) and reconnecting caused a 115 mA inrush.
* Absolute accuracy: unverified against an independent reference.

## hwlab implementation

* Package `plugins/xds110/energytrace`: `proto.go` (parser, calibration,
  conversion), `session.go` (command sequence), `reader.go` (stream reader with
  stall detection and sequence-gap accounting).
* Plugin `xds110`: `PowerMeasurement.info`, capture via `capture_start`; samples
  stream into artifact chunks. Lost packets are reported in `DroppedBatches`.
* Experiments: `examples/xds110-energytrace.yaml`. A debug-only experiment on
  the same probe sets `control: true` so it never starts the stream.
* Hardware tests: `//go:build hardware`, `HWLAB_XDS110_HW=1
  HWLAB_XDS110_ENERGYTRACE_HW=1`.
