# Phase 10 — PicoScope results

Hardware: **none available** (no PicoScope, no PicoSDK driver built in). This
phase is contract-, synthetic- and replay-validated only. Nothing here has met a
real instrument.

| Item | Result |
|---|---|
| backend seam (`Backend`, request validation, rate grid, limits) | tests through the plugin, race detector on |
| synthetic scope: 8/12-bit quantisation, shared memory, rate falls with channel count, clipping/overflow, edge triggers, pre-trigger, timeout, auto | tests |
| known-answer measurements (1 kHz square: frequency, 50 % duty, rise 1.6 µs, fall 3.2 µs; 10 kHz sine rms 0.707; DC 1.2 V within one 8-bit LSB) | tests |
| settings validation, rate rounding reported, setup-dependent limits, no empty channel set, transient-call refusals | tests |
| waveform results bounded (envelope ≤ 1000 buckets, no raw samples through RPC) | test |
| capture: interleaved analog events (≤ 50 000 frames each), `waveform.csv` with time zero at the trigger, stats, modest clock note, cancel while waiting for a trigger, trigger timeout fails the capture | tests |
| offline analysis: `hwlab waveform measure`, MCP `waveform_measure`, `app.MeasureWaveform` (segments, windows, errors) | tests on the fixture and replay |
| replay disconnected | pass: `testdata/synthetic-picoscope` (labelled synthetic), `TestSyntheticPicoScopeSessionReplaysWithoutHardware` |
| CLI end to end | `HWLAB_PICOSCOPE_FAKE=1 hwlab run --yes examples/picoscope-fake.yaml`: 3 invoke events, 8000 frames, `waveform.csv`, plot |
| real PicoScope / PicoSDK backend | **not run, not written**: validation pending |
| streaming mode, rapid-block, FFT, advanced triggers, signal generator | **not implemented** |

## Issues found while building

- `pre_trigger` and `trigger_slope` were only validated when the trigger was
  enabled, so a bad value was accepted silently and failed later. They are now
  validated when parsed.
- A capture counts frames (samples per channel) in its statistics, as the Saleae
  plugin does, so replayed and live sample counts agree.
- An 8-bit sample on a 5 V range has a 39 mV step, so DC measurements are
  quantised; the tests use that tolerance rather than hiding it.
- Reads of settings and of the last block must stay available while a capture
  runs (an experiment step called `info` during a capture and was refused).

## Known gaps

- No real backend. The PicoSDK needs CGO or a helper process (ADR 0024).
- The waveform analyser assumes roughly two-level signals for rise/fall and duty;
  other signals report fewer metrics, with notes.
- Replayed sessions lose the per-channel overflow flag (it is not stored in the
  analog stream).
