# ADR 0012 — J-Link debug probe and RTT via JLinkExe

Status: accepted. Validated on a real J-Link OB (non-destructive operations
only); target RTT data, writes and flashing are unverified (see the end).

## Context

SEGGER's probe protocol and the J-Link DLL are proprietary. The core must stay
free of SEGGER concepts: it only knows the generic `DebugProbe`,
`MemoryAccess`, `FlashProgramming` and `RTT` capabilities and the `Log` /
`DebugEvent` records.

## Decision

All SEGGER knowledge lives in `plugins/jlink`, which drives the vendor's own
`JLinkExe` (J-Link Commander) as a subprocess.

- **No protocol reimplementation, no cgo.** The plugin needs only sysfs and
  `exec`, which also helps the CGO audit (ADR 0009).
- **Discovery.** Linux: scan `/sys/bus/usb/devices` for VID 1366. Elsewhere:
  `ShowEmuList`. Serial numbers are normalised (leading zeros stripped) so the
  two sources merge.
- **Capability gating.** Without `JLinkExe` a probe is reported with only
  `DebugProbe`, `operations=discovery-only` and `missing=JLinkExe`; the plugin
  also reports a `JLinkExe` runtime requirement with an install hint.
  `JLinkExe` is located via `HWLAB_JLINK_EXE`, `PATH`, then the standard
  install directories.
- **Operations** run as one Commander script (`-CommandFile`, `ExitOnError 1`,
  timeout) under a per-probe mutex. Safe: `DebugProbe.info`, `connect`,
  `MemoryAccess.read`. Confirm-required (`--yes` / MCP `confirm`):
  `halt`, `resume`, `reset`, `read_registers`, `MemoryAccess.write` (max 4096
  bytes), `FlashProgramming.program`.
- **RTT.** `StartCapture` starts a long-lived `JLinkExe` with
  `-RTTTelnetPort` and connects to the telnet port (default 19021, retrying).
  With `rtt_launch: false` it only attaches to an existing server. Lines are
  reassembled across reads and become `Log` events (`source=jlink-rtt`); the
  J-Link banner lines are `source=jlink`, level debug. ANSI is stripped and
  `<err>`/`<wrn>`/`<dbg>` map to levels. `jlink` also advertises
  `LiveRawStream`, which is the capability the core needs to run a capture.
- **Clock.** Lines are stamped on host receive. Probe/target time is unknown,
  so the bound is `[0, +inf)` (late by an unknown latency, never early),
  resolution 1 ms. Cross-device order against RTT is therefore usually
  "undetermined" ([ADR 0011](0011-clock-uncertainty-and-explain.md)).

Config keys: `target`, `interface` (default SWD), `speed_khz`, `rtt_host`,
`rtt_port`, `rtt_launch`, `rtt_connect_timeout_ms`.

## Verified

Against fakes: discovery (temp sysfs tree, fake `ShowEmuList`), capability
reporting, danger classification, script generation, failure detection,
RTT parsing/levels/attach/unreachable port, and a core end-to-end run with a
stand-in `JLinkExe`.

On real hardware (nRF52840 DK on-board J-Link OB, fictional serial alias 900000002, J-Link
software V9.82, target `NRF52840_XXAA`): USB discovery, `info`, `connect`
(Cortex-M4, SW-DP), `MemoryAccess.read` (flash vector table: SP 0x2000CD80, reset vector 0x7D49), `halt`, `resume`, `reset`, `read_registers`, and the
RTT telnet attach with banner lines in the timeline. Replay without hardware:
`testdata/real-jlink`. Real-hardware fixes: `info` and `connect` need an
explicit `f` / `connect` command (Commander connects lazily, so an empty script
printed nothing), and the banner line `SEGGER J-Link (unknown) ...` is
classified as banner, not target output.

## Not verified on real hardware

- target RTT output (the DK firmware has no RTT control block, so only the
  banner arrived; the RTT channel number is not yet carried in the event);
- memory write, flash programming and erase (deliberately not run);
- JTAG, and probes other than the J-Link OB.

## Addendum: several probes (phase 17)

The device id is the probe serial; every Commander call passes `-USB <serial>`
and the answer's `S/N:` must match or the call fails. Probe discovery merges
sysfs with `ShowEmuList USB` (model, nickname). Results carry `probe_serial`
and `target`. RTT telnet ports are allocated per capture. SEGGER's DLL shares
only one long-running session's probe with other processes, so RTT on two
probes at once breaks calls to one of them; use `rtt: false` on the others.
Results: `docs/notes/phase17-jlink-multiprobe-results.md`.
