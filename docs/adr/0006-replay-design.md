# ADR 0006: Replay is a built-in backend over stored sessions

Status: accepted

## Context

Replay must work without hardware, drive CI, and exercise the real pipeline
rather than a parallel code path.

## Decision

`internal/replay` implements the same `plugin.Backend` interface as a plugin
and is registered in-process (`Manager.RegisterBuiltin`). It re-emits stored
sample chunks and timeline events as plugin batches.

- `hwlab replay SESSION` opens read-only and prints a summary.
- `hwlab replay SESSION --rerun` and `hwlab run --replay SESSION` push the
  recording through ingest, analysis and plotting into a **new** session.
- Derived events (`analysis.*`, `digital.edge`) are not replayed; they are
  regenerated, which is what makes replay useful for regression-testing the
  analysis code.

## Consequences

- Replay and live capture share ingest, so they cannot drift apart.
- Re-running a replay records `replay-of-…` names; they nest when replaying a
  replay (cosmetic).
- Edge attributes lose integer typing after the JSON round trip (see ADR 0003).
