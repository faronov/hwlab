# ADR 0011: Clock uncertainty across devices, `explain` and `calibrate`

Status: accepted. Software verified by tests and by a real PPK2 + nRF52840
session without a DUT; real spike ↔ DATA ↔ ACK correlation is **not yet
verified on hardware** (no DUT was available).

## Context

A session merges streams from devices with independent clocks. Users want to
ask "which packet belongs to this current spike?". Presenting a single
timestamp difference as the answer would imply an accuracy the devices cannot
deliver, so every capture reports how far its time base may be from true time.

## Model

Each capture reports `ClockInfo` (proto `StopCaptureResponse.clock`, stored as
`CaptureMeta.Clock`): `stamped − true` lies in `[ErrorMin, ErrorMax]`, either
side flagged *bounded* or not, plus resolution, jitter, drift and a note. An
unbounded side holds only an observed floor, never a claim.

- **PPK2** — both sides bounded. Early side: sample *j* arrived at host time
  `a_j`, so `t0 ≥ a_j − j·T`; the minimum over the first 2 s is used. Late
  side: `t0` cannot precede the host time of the `AVERAGE_START` write.
  Assumes no device-clock drift over that 2 s window. First real measurement
  (no DUT): `[-4.16 ms, +0.03 ms]`.
- **nRF802154 sniffer** — `[0, +∞)`. Timestamps come from the device timer and
  are mapped to host time by a minimum-offset estimator; they are never early
  but late by an unknown USB transit. Observed transit is reported in the note
  as a statistic, not a bound.

Combining two captures A and B: `true Δ = measured Δ − (eB − eA)`. The lower
end needs `B.max`, `A.min`; the upper end `B.min`, `A.max`. With the sniffer
as B the lower end is unbounded: a packet shown just after a spike may really
have preceded it. `explain` states that (`order undetermined`) instead of
guessing. Nanosecond-level cross-device accuracy is not claimed.

## Commands

- `hwlab explain SESSION --spike N | --packet N | --at T [--window D]
  [--plot FILE]` (MCP `timeline_explain`): the anchor, packets before/after,
  first DATA and the ACK after it, energy and charge, clock summaries and the
  timing interval. Packet kind comes from the 802.15.4 frame-control bits; no
  payload is returned. Plots number spikes with the same `#N`.
- `hwlab calibrate --a-… --b-… --window D`: pairs events that are truly
  simultaneous (a shared trigger) and suggests an offset (`−median`) with
  min/max/MAD. It is **not** valid for causal delays.
- `clock_offset` (experiment device key / MCP `clock_offset`): a signed
  duration added to that device's timestamps at ingest; the stored bounds are
  shifted by the same amount (`OffsetAppliedNs`).
- Replay returns the stored clock, so `explain` gives the same answer on a
  replayed session.

## Not verified

Calibration against a known load; digital inputs with real signals; drift
against an external reference; the sniffer's USB latency distribution; PPK2
start bound under heavy host load; a real DUT correlation.
