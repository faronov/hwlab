# ADR 0027: measured affine clock mapping between capture devices

Status: accepted

## Context

Two capture devices on one wire (PPK2 digital input D0 and a Saleae channel)
have independent clocks. The earlier model could only express a fixed offset
(`clock_offset`, estimated by `hwlab calibrate` from median event deltas) plus
per-capture error bounds (`ClockMeta`). Nothing had been measured on real
hardware.

## Measurement (real hardware, ONE experiment, ONE session)

One 100 Hz square wave from an nRF52840 DK (flash untouched; TIMER4 + PPI
fork + GPIOTE configured through J-Link memory writes, so both pins switch from
one compare event, ~60 ns apart) went to PPK2 D0 and Saleae CH0 (4 MHz).

| Run | Matched edges | Initial offset | Residual median / p95 / max (affine) | Drift PPK2 vs Saleae | Fixed offset leaves (p95 / max) | Uncertainty |
|---|---|---|---|---|---|---|
| 10 s (stretch marker) | 1723 | 77.8 ms | 2.5 / 5.2 / 76 us | +93.95 ppm (+/-0.04) | 0.47 / 0.58 ms | +/- 81 us |
| 12 s | 1982 | 51.0 ms | 2.5 / 5.0 / 68 us | +92.75 ppm (+/-0.03) | 0.52 / 0.63 ms | +/- 73 us |
| 300 s | 59970 | 44.2 ms | 2.5 / 5.3 / 43 us | +92.61 ppm (+/-0.0001) | 13.1 / 13.9 ms | +/- 48 us |

* The initial offset is not a clock property: it is the start-request latency
  of each device (the Saleae start takes 0.4-0.8 s through Logic 2) and changes
  per run.
* The drift is real and reproducible (92.6-94 ppm). It is the rate of the PPK2
  sample-count clock (nominal 10 us per sample) against the Saleae crystal. A
  fixed offset is therefore insufficient: over 300 s it leaves up to 13.9 ms.
* The affine fit is adequate. Residual means per third of the 300 s run stay
  within +/-1 us of zero (slight curvature, plausibly thermal); no richer clock
  model is warranted.
* The PPK2 sometimes shows 4.6 ms instead of 5.0 ms intervals (a handful per
  run): lost or re-timed samples. They are rejected as outliers by the fit; the
  rate does not absorb them. This is a PPK2-side observation, not explained.

## Decision

1. Timeline timestamps stay as recorded (device time mapped to session time
   by the plugin's anchor, plus the optional fixed `clock_offset` of ADR 0011).
   Nothing rewrites stored events.
2. A measured mapping is stored as evidence in `session.json`
   (`clock_calibrations`):
   `reference_session = ref_session_ns + rate * (t - ref_device_ns)`, i.e.
   `session_time = a*device_time + b` with `a = rate`. The reference point sits
   mid-data so integer nanoseconds keep full precision. Fields: pairs,
   rejected outliers, span, initial offset, drift ppm (+ standard error),
   residual median/p95/max, uncertainty, and the p95 a fixed offset would leave.
3. `correlation.MatchEdges` pairs the edges of one signal by interval sequence,
   not by time, because a periodic wave is ambiguous. It requires an
   irregularity (marker) matched at exactly one alignment and refuses otherwise
   ("ambiguous"). Edges are never paired by nearest time.
4. `correlation.FitAffine` is ordinary least squares with one robust pass
   (reject beyond 5 MAD, minimum bound given by the clock resolution).
5. Entry points: `hwlab calibrate-clocks SESSION --ref A --dev B
   [--ref-channel --dev-channel] [--save]`, MCP `clock_calibrate` (read-only),
   `App.CalibrateClocks`. `hwlab session info` prints stored calibrations.
6. The claimed uncertainty is `worst residual + resolution/2` of both clocks.
   It excludes common-mode delay and the skew between the two probe points
   (here ~60 ns of GPIO skew, below the 10 us PPK2 resolution, so not
   resolvable by this method), and says nothing about absolute time. No
   nanosecond claims are made: the PPK2 resolves 10 us and the Saleae 250 ns at
   4 MHz.

## Not done (deliberately)

* No distributed clock-sync system, no continuous disciplining.
* The rate is not applied to the stored timeline automatically. A user can feed
  the fixed offset to `clock_offset`; applying `a` at ingest is possible later
  (`correlation.Affine.Apply`) once a second device pair shows the drift is
  stable between runs for the same hardware. The three runs above agree within
  1.4 ppm; temperature dependence is untested.
* Only digital edges are supported as the shared event.

## Distinguishing real from synthetic

`TestCalibrateRecoversKnownOffsetBetweenTwoCaptureDevices` (fake devices) and
`internal/correlation/affine_test.go` are SYNTHETIC. The real result is
`testdata/real-ppk2-saleae` replayed by `TestRealPPK2SaleaeSessionReplaysOffline`.
