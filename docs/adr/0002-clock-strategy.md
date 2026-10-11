# ADR 0002: Session clock is int64 ns on a monotonic timeline

Status: accepted

## Context

Power, digital, packet and debug sources have different clocks (none,
device ticks, host receive time). Correlation requires one ordering that
survives NTP steps and wall-clock changes.

## Decision

- All timestamps are `int64` nanoseconds since the **session epoch**, taken
  from the core's monotonic clock when the capture starts. Wall-clock time is
  stored as metadata only.
- `StartCapture` carries `session_time_ns`; plugins map their clock onto it
  with `pluginsdk.Clock`, anchoring at the actual capture start.
- The core can apply a per-device `ClockOffsetNs` on ingest.
- Fixed-rate sample streams use `start_ns + i·period_ns`; the PPK2 has no
  per-sample timestamps, so sample *i* is at `t0 + i·10 µs`.

## Consequences

- Ordering is robust; cross-device accuracy equals anchor latency (typically
  sub-millisecond), not better. This is documented rather than hidden.
- The PPK2 stream can drift against other devices on long captures.
  Mitigation: markers/shared trigger now, calibration on the roadmap.
- Simulated (`realtime: false`) runs compress time; the session duration is the
  content end.
- `int64` ns covers ~292 years, so no overflow concern.
