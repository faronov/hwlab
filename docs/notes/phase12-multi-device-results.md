# Phase 12 — multi-device experiments: REAL results

Status: **real calibrated two-device experiment done** (PPK2 digital input +
Saleae Logic, one experiment, one session). Supersedes the earlier "not run"
note. Other combinations (Saleae + J-Link as sources, nRF802154 + anything,
PPK2 + nRF802154, three-way) are still not run.

## Setup

* Signal source: nRF52840 DK, **flash untouched**. J-Link memory writes
  configure TIMER4 (1 MHz, CC0 = 5000, shortcut compare->clear), a GPIOTE toggle
  task on P0.03 and P0.04, and one PPI channel with a FORK so ONE compare event
  toggles both pins (~60 ns apart). 100 Hz square wave, 200 edges/s. A DK reset
  clears it.
* Wiring: DK P0.03 -> PPK2 **D0 (logic port)**; DK P0.04 -> Saleae CH0; DK VDD
  -> PPK2 **logic-port VCC**; common GND. The PPK2 logic-port VCC pin must be
  tied to the target I/O voltage: without it D0 reads 0 in every mode (the
  power header GND/VOUT/VIN/GND is a different connector).
* Experiment: `examples/ppk2-saleae-shared-signal.yaml` (PPK2 source mode 3.3 V,
  Saleae 4 MHz digital CH0, J-Link steps). Two J-Link writes to TIMER4.CC0
  stretch the half-period 5 -> 8 ms -> 5 ms (a **marker** both devices see);
  restoring a shorter CC0 also needs a TIMER4 CLEAR, otherwise the counter has
  already passed the compare value and the wave stalls (found on the first
  300 s attempt, which stopped at 5.2 s).
* Single-channel Saleae at 24 MHz produced no events; 4 MHz works.

## Results

`hwlab calibrate-clocks` (ADR 0027), reference = Saleae, device = PPK2:

| | 10 s | 12 s | 300 s |
|---|---|---|---|
| matched edges | 1723 | 1982 | 59970 |
| initial offset (PPK2 -> Saleae session time) | 77.8 ms | 51.0 ms | 44.2 ms |
| residual median / p95 / max | 2.5 / 5.2 / 76 us | 2.5 / 5.0 / 68 us | 2.5 / 5.3 / 43 us |
| drift (PPK2 clock vs Saleae clock) | +93.95 ppm | +92.75 ppm | +92.61 ppm |
| fixed offset only: p95 / max residual | 0.47 / 0.58 ms | 0.52 / 0.63 ms | 13.1 / 13.9 ms |
| resulting uncertainty | +/- 81 us | +/- 73 us | +/- 48 us |

* Drift **is measurable** and reproducible (92.6-94 ppm); a fixed offset is not
  enough (13.9 ms after 300 s).
* The initial offsets differ per run: they are the start-request latencies of
  the two devices, not a clock property.
* Residual structure over 300 s stays within +/-1 us per third: an affine
  mapping suffices.
* Uncertainty is bounded by the PPK2's 10 us timestamp resolution and a few
  outlier edges (PPK2 intervals of 4.6 ms instead of 5.0 ms appear a few times
  per run; cause not analysed).
* The 60 ns pin skew is below the PPK2 resolution and not observable; common
  delays cannot be seen by this method.

## Offline acceptance (devices unreachable)

Sessions were copied to a fresh home and `hwlab` run with no plugin binaries on
PATH and no device environment (the devices were still physically attached but
nothing could reach them). Verified from stored data only: `session info`
(shows the stored calibration), `timeline` window queries, `calibrate-clocks`
recomputation (identical numbers), `plot` of a selected window, MCP
`clock_calibrate` and `timeline_nearest`. The CI-safe version is
`TestRealPPK2SaleaeSessionReplaysOffline` on `testdata/real-ppk2-saleae`
(12 s session, timeline only; the 300 s calibration summary is
`long-300s-calibration.json`; device serials replaced by placeholders).
The raw `.hwcap` and Logic 2 artifacts are not committed.

## Synthetic vs real

`TestCalibrateRecoversKnownOffsetBetweenTwoCaptureDevices` and
`internal/correlation/affine_test.go` are synthetic. They test the plumbing;
the numbers above are the only measured cross-device timing.

## Not covered

PPK2 + nRF802154, Saleae + nRF802154, three-way sessions; temperature
dependence of the drift; applying the rate at ingest (see ADR 0027).
