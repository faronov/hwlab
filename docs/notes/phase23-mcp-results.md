# Phase 23 — MCP frontend acceptance

Goal: prove MCP client → `hwlab mcp --stdio` process → shared `app` service →
plugin → real hardware, with MCP and CLI as two frontends over one service.
No second MCP implementation, no hardware logic in handlers, no CLI shell-out.

Hardware: ST-Link V2 (0483:3748) + STM32F1 (dev_id 0x410, rev 0x2003, 128 KB
flash, 20 KB SRAM); BL702-DAPLink (d6e7:3507, no target); TI CC-Debugger
(0451:16a2). Not attached: J-Link, ESP, PPK2, nRF, Saleae, FTDI.

## Shared-layer changes (benefit CLI and MCP)

* `internal/apperr` coded errors; `app.Classify` maps gRPC statuses/messages to
  codes (incl. new `PRECONDITION_FAILED`; usb busy/disconnected under
  FailedPrecondition → `BUSY`/`DEVICE_GONE`).
* `app.ValidateArgs` (64-bit addresses; Go int/uint types accepted),
  `confineFiles`/`CheckExperimentPath` (paths under `<home>/artifacts`),
  bounded artifacts, `app.Registry`.
* MCP: structured-error wrapper, 4 new tools (27 total), `--replay SESSION`.

## Bugs found and fixed

1. Address checker rejected Go int/uint (CLI/YAML) and hex uint64.
2. Missing flash backup file reported as `INTERNAL` → `PRECONDITION_FAILED`.
3. Concurrent open (usb busy under FailedPrecondition) not classified `BUSY`.
4. Test-only: relative path double `mcp/` prefix.

## Real-hardware MCP results (`HWLAB_MCP_HW=1`)

| Capability | Status |
|---|---|
| Discovery + identity (serial, CPUID 0x411fc231, DP 0x1ba01477, 0x410/0x2003, 3.2 V, SWD 1800 kHz, FPB 6+2, DWT 4) | MCP_REAL_HW_VERIFIED |
| Halt/resume ×5, registers, SRAM 8/16/32/block write + exact restore | MCP_REAL_HW_VERIFIED |
| Invalid addresses / flash write via memory-write refused | MCP_REAL_HW_VERIFIED |
| 100 single-steps, FPB breakpoint hit, DWT watchpoint hit | MCP_REAL_HW_VERIFIED |
| Flash read → artifact, SHA == original backup | MCP_REAL_HW_VERIFIED |
| Erase/program/restore refusals (confirm, allow_*, token, chip erase, missing backup, path escape) | MCP_REAL_HW_VERIFIED |
| Flash erase/program/restore *success* via MCP | MCP_IMPLEMENTED_UNTESTED (backend REAL_HW_VERIFIED in phase 21; MCP tests deliberately never write flash) |
| CLI-vs-MCP parity (identify fields, memory bytes) | MCP_REAL_HW_VERIFIED |
| 20 open/close rounds; concurrent open → BUSY; client disconnect releases probe; cancel | MCP_REAL_HW_VERIFIED |
| Two probes (ST-Link + CMSIS-DAP) from two clients in parallel | MCP_REAL_HW_VERIFIED |
| Physical unplug → `NOT_FOUND` in ~5 ms, server alive; replug → same serial, identify, memory read | MCP_REAL_HW_VERIFIED |
| In-flight operation during unplug → `DEVICE_GONE` | BACKEND_REAL_HW_VERIFIED only (not driven via MCP) |
| CMSIS-DAP `target_op info` (v2.1.0, 64×8, SWD+JTAG) | MCP_REAL_HW_VERIFIED; target ops UNSUPPORTED_BY_ATTACHED_HW (no target); `identify` → `UNSUPPORTED` |
| CC-Debugger `info` | MCP_REAL_HW_VERIFIED (read-only) |
| Replay workflow without hardware | MCP_REPLAY_VERIFIED |
| J-Link, ESP USB-JTAG, PPK2, nRF, Saleae, FTDI over MCP | UNSUPPORTED_BY_ATTACHED_HW |
| Replay-mismatch error code | NOT_IMPLEMENTED (replay is read-only playback) |

The unplug test failed twice before passing only because of operator timing
(the test waits 5 min per phase); the third, stepwise run passed. After all
tests the STM32 flash SHA-256 was re-read: `acba307b…8fe6a16`, unchanged.

## Performance (MCP `target_op`, cold open per call)

info 56 ms; 16 B read 55 ms; 4 KiB read 98 ms; 128 KiB flash-read 1328 ms;
`lab_status` 6.5 ms; `capability_registry` 41 ms.

## Parity matrix

| Capability | CLI | MCP | Replay |
|---|---|---|---|
| device list / info / identify | ✔ | ✔ | recorded events only |
| halt/resume/reset/registers/memory/step | ✔ (`hwlab target`) | ✔ (`target_op`) | – |
| breakpoint/watchpoint | ✔ | ✔ | – |
| flash info/read/backup/erase/program/restore/verify | ✔ | ✔ | – |
| timeline/session/analysis | ✔ | ✔ | ✔ |
| artifacts list/info/read | ✘ (MCP-only) | ✔ | ✔ |
| capability registry | via docs | ✔ | ✔ |

## Remaining gaps

* No `hwlab artifact` CLI command.
* Go SDK closes the stdio session on malformed JSON instead of `-32700`.
* Breakpoints/watchpoints are per-session (removed on close).
* Stdio only (no HTTP/SSE/auth).
* Backends not attached were not exercised over MCP.
