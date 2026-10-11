# ADR 0007: MCP tool design

Status: accepted

## Context

An AI client must be able to drive the lab without drowning in data or causing
unsafe actions.

## Decision

- Official `modelcontextprotocol/go-sdk`, **stdio transport only** in M1.
  Logs go to stderr; stdout is protocol-only.
- Tool names use `snake_case`; each maps to one `app` method, shared with the
  CLI.
- Read-only tools are annotated read-only.
- **No raw samples over MCP.** Results are summaries, envelopes, events or
  plots; PNG plots are returned inline as image content. Event queries have a
  default limit (100) and a hard maximum (1000).
- `capture_start` requires a `duration`, so a capture cannot run forever
  unattended; `capture_stop` ends it early.
- Dangerous operations need `confirm: true` (ADR 0004).
- Session references accept id, unique prefix, path or `latest`.

## Consequences

- HTTP/SSE transport and authentication are future work; stdio inherits the
  user's local permissions.
- Underscore names differ from dotted names used in prose (`lab.status`); the
  README lists the real names.
