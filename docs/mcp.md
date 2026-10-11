# MCP frontend

`hwlab mcp --stdio` is a thin MCP frontend over the same service layer
(`internal/app`) the CLI uses. Handlers in `internal/mcp` only decode
arguments, call `app`, and encode results; there is no hardware logic and no
CLI shell-out. Probe/target work goes through plugins (out-of-process gRPC).

```sh
bin/hwlab mcp --stdio                       # live: all discovered plugins
bin/hwlab mcp --stdio --replay SESSION_DIR  # replay only, no hardware
```

```json
{ "mcpServers": { "hwlab": {
  "command": "/path/to/bin/hwlab", "args": ["mcp", "--stdio"],
  "env": { "HWLAB_HOME": "/path/to/hwlab-home",
           "HWLAB_PLUGIN_PATH": "/path/to/bin" } } } }
```

On Linux the user needs access to the USB device nodes (udev rules / group).

## Tools (27)

| Group | Tools |
|---|---|
| Lab | `lab_status`, `plugin_list`, `doctor`, `capability_registry` |
| Devices | `device_list`, `device_invoke`, `target_op` |
| Capture | `capture_start`, `capture_stop`, `capture_status`, `capture_marker`, `experiment_run` |
| Sessions | `session_list`, `session_info`, `timeline_query`, `timeline_nearest`, `timeline_correlate`, `timeline_explain` |
| Analysis | `clock_calibrate`, `waveform_measure`, `usb_analyze`, `modbus_decode`, `power_analyze`, `plot_render` |
| Artifacts | `artifact_list`, `artifact_info`, `artifact_read` |

`capability_registry` returns the supported `target_op` operations, loaded
plugins/capabilities and the error-code table, so a client can discover
behaviour instead of guessing.

## Errors

Failures are tool results with `isError: true` and structured content:

```json
{"ok": false, "error": {"code": "PERMISSION_DENIED", "message": "...", "retryable": false, "hint": "..."}}
```

Protocol-level problems (unknown method, malformed arguments) are JSON-RPC
errors. Codes: `INVALID_ARGUMENT`, `BAD_ADDRESS`, `NOT_FOUND`,
`AMBIGUOUS_DEVICE`, `BUSY`, `DEVICE_GONE`, `TIMEOUT`, `CANCELLED`,
`CONFIRMATION_REQUIRED`, `PERMISSION_DENIED`, `PATH_DENIED`,
`PROTECTED_TARGET`, `UNSUPPORTED`, `UNSUPPORTED_BY_ATTACHED_HW`,
`SWD_NO_ACK`, `SWD_WAIT`, `SWD_FAULT`, `SWD_PROTOCOL`, `TARGET_NOT_HALTED`,
`VERIFY_FAILED`, `PRECONDITION_FAILED`, `INTERNAL`. Classification lives in
`app.Classify`, shared by CLI and MCP.

## Safety

* Dangerous operations need `confirm: true` (`CONFIRMATION_REQUIRED`
  otherwise) and the plugin's own `allow_*` configuration
  (`PERMISSION_DENIED`). Flash erase/program additionally need the plugin
  confirmation token; chip erase needs `allow_chip_erase`.
* Addresses are validated as 64-bit values (decimal or `0x` hex strings, or
  numbers); out-of-range input is `INVALID_ARGUMENT`/`BAD_ADDRESS`.
* File-path arguments (`path`, `dir`, …) are confined under
  `<HWLAB_HOME>/artifacts`; relative paths land in `artifacts/mcp/`.
  Escapes (`..`, absolute outside the root, symlinks) → `PATH_DENIED`.
* Raw captures and flash images are never returned inline. Use `artifact_*`:
  `artifact_read` is bounded (a 1 MiB request is refused) and confined.
* Flash programming through generic memory-write is refused.
* Request size is bounded; a 3 MiB argument is rejected cleanly.

## Session model

`target_op` is one ephemeral device session: open → `pre` calls → op → `post`
calls → close. Halt state persists on the target between calls, but hardware
breakpoints/watchpoints installed by a session are removed when it closes.
For flows that need them armed, use `pre`/`post` (for example
`pre=[set_watchpoint, resume]`, `op=wait-halt`, `post=[clear_watchpoint]`) or
`breakpoint-set` with `run_wait_ms`. A second concurrent open of the same
probe returns `BUSY`; a vanished client releases the probe.

## Replay

`--replay SESSION_DIR` serves a recorded session read-only: timeline and
analysis tools work, `experiment_run` with `plugin: replay` re-emits recorded
`invoke` events, and `target_op` fails (no hardware). Fixtures:
`testdata/real-stlink-stm32-flash/`.

## Known behaviour

* The Go MCP SDK ends the stdio session on a non-JSON line instead of
  replying `-32700`; the process exits cleanly and a new session works.
* There is no `hwlab artifact` CLI command; artifact tools are MCP-only.

## Tests

* `go test ./internal/mcp` — real-process protocol tests (spawns the built
  `hwlab mcp --stdio` over stdio): schema, structured errors, malformed input,
  restarts, concurrent clients, replay.
* `go test ./internal/app` — validation, classification, confinement.
* Hardware (`-tags hardware`, opt-in `HWLAB_MCP_HW=1`; unplug test also
  `HWLAB_MCP_UNPLUG=1`): see `internal/mcp/hardware_test.go`. Results:
  [phase23](notes/phase23-mcp-results.md).
