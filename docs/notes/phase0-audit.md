# Phase 0 audit of Milestone 1 (before real-hardware bring-up)

Checks run: `go vet ./...`, `go test ./...`, `go test -race ./...`, `gofmt -l .` — all clean.
`golangci-lint` is not installed on the dev host; `.golangci.yml` and the CI job are in place.

## Already good

- Core is vendor-free: devices are selected by capability, plugin config is opaque.
- Plugin isolation (subprocess, gRPC over unix socket, crash detection and relaunch) is tested.
- Sample storage is binary and chunked (`HWCAP001`), readers stream chunks, so analysis/plots do not load a capture into memory.
- Plots use min/max/mean envelopes, so short spikes survive decimation.
- Replay re-enters through the normal ingest path; the integration test covers it.
- Policy metadata (`dangerous_config_keys`, `MethodSpec.danger`) already distinguishes safe and state-changing operations.

## What must change for real hardware

1. **nRF 802.15.4 capture** is missing. Needs the serial line protocol, timestamp mapping, pcapng output and optional tshark decode.
2. **PPK2 digital storage**: power chunks store `uint32` digital words per sample next to a 4-byte current. At 100 kSa/s that is ~800 KB/s (~480 MB per 10 min). Measure on the real device; compress or narrow the digital word if growth is a problem.
3. **Per-source clock metadata** (host receive time, device timestamp, offset, uncertainty) is not recorded yet. Required for Phase 3.
4. **Packet events carry the full hex payload in the in-memory timeline.** Fine for thousands of packets; for long sniffer captures it should be measured.
5. **Hardware tests** need a build tag so CI never needs a device.
6. **CGO**: `go.bug.st/serial/enumerator` needs cgo only on darwin (Linux reads sysfs). Phase 8 audits whether a pure-Go darwin enumeration is practical.

## Questionable abstractions

- `Packet.link_type` + `protocol` string is adequate; richer 802.15.4 metadata (channel, RSSI, LQI) already has fields.
- The plugin SDK `Clock` anchors at request receipt only. Devices with their own timebase (the sniffer reports µs since boot) need an offset estimator inside the plugin; that stays plugin-side, and the core records the result.
- `ClockOffsetNs` exists per target but is never calibrated. Phase 3 adds metadata and hooks, not a pretended sync.

## High-rate data path

Batched (`PowerBatch` of 1000 samples), bounded sink (64 batches), `TryEmit` drops and counts for live hardware, gRPC flow control for blocking emitters, drops surface in `EventBatch.dropped_batches` and capture stats. The design is sound; real throughput is unmeasured until the PPK2 stress test.

## Replay format

Adequate for the current needs: stored chunks and `timeline.jsonl` are enough to rebuild everything, including the pcap artifact. Open question: JSON round-tripping loses integer typing in event attributes (documented in ADR 0003).

## Multi-device correlation

The session timeline is a single monotonic int64 ns axis, so correlation queries work. The accuracy claim must stay honest: cross-device alignment is bounded by anchor latency and device clock drift, which Phase 3 will measure and store as an uncertainty rather than assume.
