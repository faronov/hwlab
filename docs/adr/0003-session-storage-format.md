# ADR 0003: Directory-based session format with binary sample chunks

Status: accepted

## Context

Captures mix tens of thousands of samples per second with sparse events, must
be crash-tolerant, diffable in part by humans, and queryable by time range
without loading everything.

## Decision

A session is a directory: `session.json` (metadata), `timeline.jsonl`
(append-only low-rate events, one JSON object per line), `captures/<id>/*.hwcap`
(binary sample chunks), `artifacts/`, `plots/`, `experiment.yaml`.

`.hwcap` files start with the magic `HWCAP001` and hold chunks of kind power,
analog or digital with start time, period, count and little-endian arrays.
Chunk headers carry the time range, so range reads skip chunks. A truncated or
corrupt tail is ignored (crash tolerance). Chunks are capped at 256 MiB to
refuse garbage. `FormatVersion` = 1.

Power chunks carry a flags byte: `1` voltage array present, `2` digital words
present, `4` digital stored as one byte per sample (all words ≤ 0xFF), `8`
digital block constant (a single word). The writer picks the narrowest
encoding per chunk (a PPK2 with idle inputs costs 4 B/sample instead of 8; with
active inputs 5 B/sample). Files written before these flags (digital always
4 bytes per sample, flag `2` only) read unchanged.

## Consequences

- Plain files: easy to inspect, copy, archive, replay.
- No database dependency; no cross-session queries (listing scans directories).
- Arrays are not compressed in v1; compression and retention tooling are
  roadmap items.
- Events that are JSON round-tripped lose integer typing (numbers become
  float64); consumers must not rely on integer attribute types.
