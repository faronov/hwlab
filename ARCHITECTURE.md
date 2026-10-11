# Architecture

## 1. Goals and principles

- The core knows **capabilities**, never vendors. "PowerMeasurement" is a core
  concept; "PPK2" exists only inside a plugin.
- Everything lands on **one timeline** (int64 ns on a session-monotonic
  clock), so power, digital, packet and debug data can be correlated.
- Plugins are **separate processes** speaking a **versioned gRPC/protobuf**
  contract over a unix socket: a crashing driver cannot take the core down,
  and plugins can be written in any language.
- **Replay is first-class**: a recorded session can be served back through the
  same pipeline, so CI and AI workflows need no hardware.
- Raw high-rate samples stay on disk; humans and AI see **summaries,
  envelopes, events and plots**.
- Dangerous operations are explicit and confirmed.

## 2. Package graph

```
cmd/hwlab ──► internal/cli ──┐
                             ├──► internal/app ──► internal/capture ──► internal/plugin ──► (plugin processes)
              internal/mcp ──┘        │   │              │                     ▲
                                      │   │              ├──► internal/session │ pkg/api/pluginv1 (generated)
                                      │   │              ├──► internal/timeline│ pkg/pluginsdk (plugin side)
                                      │   │              └──► internal/artifact
                                      │   ├──► internal/experiment (YAML)
                                      │   ├──► internal/policy
                                      │   ├──► internal/replay (builtin backend over a stored session)
                                      │   ├──► internal/analysis   (stats, envelopes, spikes, idle)
                                      │   ├──► internal/correlation (around/nearest/pairs)
                                      │   └──► internal/plot       (SVG + PNG canvases)
plugins/{fake,ppk2,nrf802154,saleae} ──► pkg/pluginsdk, internal/usbserial (hardware ones)
internal/decode (tshark wrapper), internal/pcap (writer), internal/device, internal/config
```

Rules: `timeline`, `analysis`, `correlation`, `pcap`, `policy` have no I/O
dependencies and are unit-tested in isolation. `app` is the only layer the CLI
and MCP both call, so both front-ends behave identically. Plugins depend only
on `pkg/*` (plus the shared `usbserial` helper).

## 3. Plugin contract

Defined in `proto/hwlab/plugin/v1/plugin.proto` (`ProtocolVersion` = 1):

| RPC | Purpose |
|---|---|
| `Handshake` | protocol version, `PluginInfo`: id, version, capabilities, declared `MethodSpec`s with `Danger` level, `dangerous_config_keys` |
| `ListDevices` | discovery; devices carry capabilities and properties |
| `OpenDevice` / `CloseDevice` | acquire/release hardware with configuration (`google.protobuf.Struct`) |
| `StartCapture` / `StopCapture` | begin a capture with `session_time_ns`, optional `duration_ns`, `artifact_dir`; stop returns `StopCaptureResponse` with stats and artifacts |
| `StreamEvents` | server stream of `EventBatch` for one capture; gRPC flow control provides backpressure |
| `Invoke` | declared capability methods (`PowerSource.set_voltage`, …) |
| `Shutdown` | graceful exit |

Event payloads (one `oneof` in `Event`): `PowerBatch`, `AnalogBatch`,
`DigitalBatch` (`DigitalSampled` / `DigitalEdges`) for **batched high-rate
samples** (start timestamp + period + arrays, never one message per sample);
`Packet`, `Log`, `Marker`, `DecodedProtocolEvent`, `Trigger`, `DebugEvent` for
low-rate events; `GenericPayload` for forward compatibility.

Capability identifiers (`pkg/api/pluginv1/caps.go`): PowerMeasurement,
VoltageMeasurement, PowerSource, DigitalCapture, AnalogCapture,
PacketCapture, ProtocolDecode, USBPacketCapture, DebugProbe, MemoryAccess,
FlashProgramming, RTT, TriggerInput/Output, LiveRawStream, TimedCapture,
OfflineCapture, ArtifactExport, SCPI, SerialTransport. A device requires a
capability, never a vendor (`requires: [PowerMeasurement]`).

## 4. Plugin lifecycle

1. **Discovery** — `Manager` scans the search paths for `hwlab-plugin-<id>`
   executables. Earlier paths win.
2. **Launch** — plain `exec.Command`. The plugin starts a gRPC server on a
   unix socket and prints `HWLAB-PLUGIN|1|unix|<path>` to stdout; the core
   dials it with a timeout. stderr is forwarded to the core log.
3. **Handshake** — protocol version must match; `PluginInfo` is cached.
4. **Use** — `ListDevices`, `OpenDevice`, `StartCapture` + `StreamEvents`,
   `StopCapture`, `CloseDevice`. Plugins are launched lazily and shared.
5. **Crash handling** — the process watcher closes `Done()`; any call on a dead
   plugin returns an error wrapping `ErrCrashed`; the next `Manager.Get`
   relaunches it. An in-flight capture keeps the data ingested so far and is
   marked failed.
6. **Shutdown** — `Shutdown` RPC / stdin close, then kill after 3 s.

The `replay` backend is registered in-process (`RegisterBuiltin`) and
implements the same `Backend` interface.

## 5. Clock and timestamp strategy

- The timeline is **int64 nanoseconds since the session epoch**, taken from
  Go's monotonic clock when the capture starts. Wall-clock time is stored as
  metadata only and never used for ordering.
- `StartCapture` carries `session_time_ns`, the session time at send. Plugins
  use `pluginsdk.Clock` to map their own monotonic clock onto it, anchoring
  at the moment the capture really starts.
- Devices with hardware timestamps report them relative to their own epoch;
  the core applies a per-device `ClockOffsetNs` on ingest.
- Devices without timestamps (the PPK2 streams fixed-rate samples) are
  assumed to run at their nominal period from the anchor: sample *i* is at
  `t0 + i·period`. Drift is therefore bounded only by the device oscillator
  and USB scheduling; long captures should be sync-checked with markers or a
  shared trigger. This is a documented limitation.
- Cross-device accuracy is bounded by anchor latency (USB + process
  scheduling): expect sub-millisecond, not microsecond, alignment unless a
  hardware trigger is shared. Roadmap: trigger-based clock calibration.
- Simulated runs (`realtime: false`) generate content at an artificial rate;
  the session duration is taken from the content end.

## 6. Storage — session format

```
<session-id>/
  session.json            metadata: devices, captures, streams, status, experiment
  timeline.jsonl          low-rate events, one JSON object per line, append-only
  experiment.yaml         the experiment as run
  captures/<cap>/*.hwcap  binary sample chunks
  artifacts/              plugin/generated files + index.json (e.g. radio0.pcap)
  plots/                  rendered plots
  devices/                reserved
```

`.hwcap` (magic `HWCAP001`) is a sequence of self-describing chunks (kind,
stream, start ns, period, count, little-endian arrays). Chunks record their
time range, so range queries skip irrelevant chunks without scanning. The
reader tolerates a truncated final chunk/line, so a crashed capture is still
readable. Analysis streams chunks through callbacks — memory use is
independent of capture length.

## 7. Analysis, correlation, plotting

- `analysis`: statistics (mean/min/max, charge, energy), min/max/mean
  **envelopes** with a fixed bucket count, spike and idle detection against a
  median-bucket baseline.
- `correlation`: events around a timestamp, nearest, preceding/following,
  A→B pairs with latency summary.
- `plot`: stacked tracks (current, digital, events) rendered to SVG or PNG from
  envelopes; PNG uses a pure-Go canvas (no cgo, no system fonts).

## 8. Interfaces

- **CLI** (`cobra`): see `hwlab --help`.
- **MCP** (`modelcontextprotocol/go-sdk`, stdio): tool list in the README.
  Tools call the same `app` methods as the CLI.
- **Experiment engine**: YAML → `experiment.Experiment` → `app.RunExperiment`
  (resolve devices by plugin/device/required capabilities, policy check, open,
  synchronized start, ingest, derive events, stop, analyse, plot, finalize).

## 9. MVP scope (Milestone 1)

In: everything in the status table of the README marked *working*, plus the
PPK2 plugin implementation (unvalidated on hardware) and, since the hardware
bring-up, the nRF 802.15.4 sniffer ([ADR 0010](docs/adr/0010-nrf802154-sniffer.md)).
Out: Saleae capture, remote artifact transfer, MCP over HTTP, all roadmap devices.

## 10. Risks

| Risk | Mitigation / status |
|---|---|
| Clock alignment between independent devices | documented anchor model; markers; roadmap: shared-trigger calibration |
| PPK2 behaviour differs from the reference on real hardware (CDC ports, spike filter, drift) | protocol cross-checked against reference source ([ADR 0008](docs/adr/0008-ppk2-protocol-sources.md)); simulated-port tests; hardware validation tracked in the roadmap |
| High-rate streams overwhelm the pipeline | batched samples, bounded queues, gRPC backpressure, drop counters reported, never silent |
| Plugin crash or hang | process isolation, crash detection, relaunch, timeouts, partial sessions remain readable |
| Dangerous hardware actions triggered by an AI | declared danger levels, confirmation required, CLI/MCP asymmetry for `allow_dangerous`, bounds checked in plugins |
| Artifacts exchanged via a shared filesystem | works for local plugins only; remote plugins need artifact streaming ([ADR 0005](docs/adr/0005-artifacts-shared-filesystem.md)) |
| cgo for USB enumeration on macOS | hardware plugins built natively; core stays cgo-free ([ADR 0009](docs/adr/0009-hardware-plugin-build.md)) |
| Protocol evolution | `ProtocolVersion` in handshake; additive protobuf changes; `GenericPayload` escape hatch |
| Large sessions on disk | binary chunks, no raw samples via MCP; retention tooling is on the roadmap |

## 11. Decisions

See [docs/adr/](docs/adr/).
