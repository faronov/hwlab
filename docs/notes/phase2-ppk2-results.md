# Phase 2 — PPK2 on real hardware

Device: Nordic PPK2 (1915:c00a), two CDC ports (`if01`, `if03`); the plugin
probes both with `GET_META_DATA` and uses the one that answers (`ttyACM1`).
Host: Linux x86-64, Go build of `hwlab` and `hwlab-plugin-ppk2`.

## What was verified

| Item | Result |
|---|---|
| Discovery, connect, metadata (calibration, HW id) | works (`PowerMeasurement.get_metadata`) |
| Ampere mode, 5 s | 499 680 samples ≈ 100 kSa/s, no dropped batches |
| Source mode 3.3 V, nothing on VOUT | works; `--yes` required, refused without it |
| Timed capture, early stop (SIGINT) | works; stop → process exit in ~0.25 s |
| Session persistence, replay without plugins | works |
| Statistics, charge, energy, windowed analysis | works (`analyze --from/--to --nominal-voltage`) |
| PNG/SVG plot (min/max/mean envelope) | works |
| Replug of the PPK2 on a live plugin | works (`TestHardwarePPK2Replug`) |

Open-circuit noise floor observed: mean ≈ 70–270 nA, min ≈ −1.1 µA. This is
not a calibration check.

## 10-minute stress (ampere, 100 kSa/s)

- 61 000 000 samples in 610.4 s (exactly 100 kSa/s), 0 dropped batches.
- RSS flat: plugin ≈ 19.7–20.5 MB, `hwlab` ≈ 23.7–23.9 MB; 19–20 threads
  constant. No growth over 10 min.
- CPU: plugin ≈ 18 % of one core, `hwlab` ≈ 5 %.
- Storage: 468 MB for the run with 8 B/sample (before the narrowing below).
- Analysis of the whole session: 3.5 s, 40 MB RSS; a 1 s window: 0.17 s.
- Replay of the session: 0.16 s.

## Changes made because of the hardware work

- **DUT supply is switched off when a capture that switched it on ends**
  (`keep_dut_power: true` opts out). Previously it stayed on after stop.
  Voltage and power-on remain explicit, confirmed operations.
- **Reconnect**: a failed port marks the device broken; the next call reopens
  it, and `StartCapture` retries once with a fresh connection.
- **Digital storage narrowing** ([ADR 0003](../adr/0003-session-storage-format.md)):
  constant blocks cost one word, 8-input data one byte per sample. Idle
  inputs: 4 B/sample (about 1.4 GB/h instead of 2.8 GB/h at 100 kSa/s);
  active inputs: 5 B/sample.

## Not verified

- Current accuracy against a known load.
- Digital inputs with real signals (only floating inputs were seen).
- Long-term clock drift between the PPK2 sample clock and the host.
- Reading VOUT voltage independently (the PPK2 reports only current).
