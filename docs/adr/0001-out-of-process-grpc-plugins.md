# ADR 0001: Plugins are out-of-process gRPC services

Status: accepted

## Context

Hardware drivers are crash-prone (USB, vendor libraries, cgo), may need
different runtimes (Python vendor SDKs, C libraries) and should be addable
without recompiling the core.

## Decision

Each plugin is a separate executable named `hwlab-plugin-<id>`. The core
launches it, the plugin serves the gRPC service from
`proto/hwlab/plugin/v1/plugin.proto` on a unix socket and announces it with one
stdout line `HWLAB-PLUGIN|1|unix|<path>`. The handshake carries
`ProtocolVersion`; mismatches are refused. High-rate data uses server-streaming
`StreamEvents` with batched payloads; gRPC flow control provides backpressure.
Plugins are discovered from `--plugin-path`, `$HWLAB_PLUGIN_PATH`,
`~/.hwlab/plugins`, `/usr/local/lib/hwlab/plugins` and the directory of the
`hwlab` binary.

Compatibility rules: only additive changes inside v1 (new fields, enum values,
messages, capability ids, event types). Capability ids, event types and device
config keys are strings, so new hardware never needs a protocol change.
Breaking changes require `hwlab.plugin.v2` and a new protocol version.

## Consequences

- A crashing plugin cannot take the core down; the core detects it
  (`ErrCrashed`) and relaunches on next use.
- Plugins can be written in any language; Go plugins use `pkg/pluginsdk`.
- Extra latency/serialization cost versus in-process calls, mitigated by
  batching.
- Unix sockets limit the first release to local plugins (Windows launch path is
  unvalidated, see the roadmap).
- The Go plugin framework (`hashicorp/go-plugin`) was not used to keep the
  dependency surface small; the launch protocol is simple enough to
  re-implement in any language.
