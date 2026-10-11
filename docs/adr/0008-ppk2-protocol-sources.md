# ADR 0008: PPK2 protocol sources and what is verified

Status: accepted — **hardware validation outstanding**

## Context

Nordic does not publish a PPK2 serial protocol specification. The `ppk2`
plugin is written in pure Go (no Python runtime) so it must re-implement the
protocol.

## Decision

The implementation in `plugins/ppk2/protocol` follows the open-source
reference **ppk2-api-python** (IRNAS, `master` at the time of writing), read
directly from its source.

Taken from the reference:

- Opcodes: AVERAGE_START `0x06`, AVERAGE_STOP `0x07`, DEVICE_RUNNING_SET
  `0x0C`, REGULATOR_SET `0x0D`, SET_POWER_MODE `0x11` (ampere = 1,
  source = 2), GET_META_DATA `0x19`, RESET `0x20`.
- Regulator encoding: `diff = mV - 800 + 32`; bytes `0x0D, 3 + diff/256,
  diff%256`. Range clamped to 800–5000 mV.
- Metadata: `key: value` lines terminated by `END`; keys `R`, `GS`, `GI`, `O`,
  `S`, `I`, `UG` for ranges 0–4.
- Sample word (little-endian uint32): ADC bits 0–13 (×4), range bits 14–16,
  logic bits 24–31.
- Calibration: `rwg = (adc − O)·(1.8/163840/R)`;
  `amps = UG·(rwg·(GS·rwg + GI) + (S·vdd/1000 + I))`.
- Sampling rate: 100 kSa/s (10 µs per sample).

Not verified / deliberately different:

- **No spike filter / rolling average** as in the reference; raw samples are
  stored. Behaviour on range switches may differ.
- Ampere mode uses `vdd_mv` (default 3300) only in the calibration
  `S`-term; the plugin never drives the supply in ampere mode.
- The PPK2 exposes two CDC ports; the plugin resolves the control port by
  probing `GET_META_DATA` instead of relying on port ordering.
- `AVERAGE_STOP` plus a drain at open is an addition for robustness against a
  device left streaming.
- Counter bits 18–23 are not used.
- Discovery matches USB VID `1915` / PID `c00a` (the reference matches by
  product name and port location).
- The baud rate is 115200 (irrelevant for USB CDC; the reference uses 9600).
- Timestamps are synthesized (see ADR 0002).

## Consequences

- Unit tests cover encoding, metadata parsing, partial-sample decoding and a
  fuzz test, and the plugin is tested against a simulated port — **none of this
  proves compatibility with a physical PPK2.**
- Before relying on measurements: validate against a known load, compare with
  nRF Connect Power Profiler, and decide on the spike filter. Tracked in the
  roadmap.
