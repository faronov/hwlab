# Phase 3B — Saleae real-hardware results

Device: original Saleae Logic (USB 0925:3881, 8 digital channels, no analog),
Logic 2 2.4.14 (AppImage, run headless under Xvfb), automation API 1.0.0.

| Acceptance item | Result |
|---|---|
| real Saleae discovered | pass (`saleae/AABBCCDDEEFF0001`) |
| real capture succeeds | pass (2 s, 3 s, 15 s, 90 s) |
| digital capture | pass; edges from hand-touched GND on CH1 reach the timeline |
| analog capture | **unsupported by connected hardware** (re-checked in the hardware-validation milestone: Logic 2 rejects the request, "Requested analog channels are unavailable"; the original Logic has 8 digital channels only). Not a failure of hwlab; analog export/plot/replay unverified |
| shared real signal | pass: CH0 saw the same 100 Hz wave as a PPK2 digital input; see phase12 note and ADR 0027. SPI analyzer: no suitable signal, not run |
| analyzer path | pipeline only: UART (4 frames) and I2C (2 starts) were "decoded" from noise edges into `decoded` events; SPI accepted, no activity. **This shows the analyzer plumbing runs, not that decoding is correct on a real bus (unverified).** |
| artifacts persist | `.sal`, `digital.csv`, analyzer CSVs |
| replay disconnected | pass: `testdata/real-saleae`, `TestRealSaleaeSessionReplaysWithoutHardware` (timeline, decoded events, plot) |
| CI without Logic 2/hardware | pass (fake Logic 2 server) |

## Bugs found on real hardware

1. An idle capture has only two `digital.csv` rows; the reader dropped the
   initial-state row's time so the session had no extent and the run failed at
   the plot step. Fixed (first batch anchored at row 0; regression test
   `TestReadDigitalCSVFlatCaptureKeepsExtent`).
2. A plot with nothing to draw failed the whole run. It is now a warning
   (`plot.ErrNothingToPlot`).

## Caveats

- Device IDs in this note and retained CSV/timeline fixtures are fictional.
  Raw `.sal` is local-only; see `testdata/README.md`.
- Protocol decoding was not checked against a real bus: the analyzers saw
  floating/hand-touched lines, so decoded frames are noise.
- Logic 2 needs a display; headless use needs Xvfb. Logic 2 and hwlab must share
  a filesystem. USB access needs a udev rule (`0925:3881`).
- StartCapture latency ~0.7 s; timing is reported as unbounded.
