# ADR 0005: Artifacts are exchanged through a shared filesystem

Status: accepted (limitation documented)

## Context

Plugins produce files (pcap, vendor captures). Streaming file bytes over gRPC
adds protocol surface and is unnecessary for local plugins.

## Decision

`StartCaptureRequest.artifact_dir` is a directory the plugin may write to;
`ArtifactRef.Path` points to a file there. The core copies/registers the file
into the session (`artifacts/` + `index.json`).

## Consequences

- Simple and fast for local plugins.
- Does **not** work for plugins on another host or in a container without a
  shared mount. Remote plugins need an artifact-streaming RPC (roadmap).
