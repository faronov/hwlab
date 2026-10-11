# Phase 3D — CC-Debugger results

Host: Linux, `cc-tool` 0.26. Hardware: TI CC-Debugger (0451:16a2, serial
90000003, firmware 0x05CC rev 0x0044) wired to a CC2530 (rev 0x24, 256 KB flash,
8 KB RAM).

## Real-hardware checks

| Check | Result |
|---|---|
| USB discovery (sysfs, no tools) | PASS |
| Debugger identification | PASS |
| Target identification (CC2530) | PASS |
| MAC read | PASS |
| Info page read (2048 B) | PASS |
| Full flash read (256 KB, fast) | PASS, 26.7 s |
| `hwlab run examples/ccdebugger-info.yaml` | PASS, 2 `invoke` events (60 ms, 78 ms) |
| Replay with no plugin and no debugger | PASS (`TestRealCCDebuggerSessionReplaysWithoutHardware`) |
| Policy refuses `reset` without `--yes` | PASS |

## Not run on hardware

`reset`, `erase`, `program`: destructive, target belongs to a user project.
Covered by argv unit tests only.

## Limits

No memory read/write, registers, halt/resume or RTT (cc-tool does not offer
them). Slow mode takes minutes for a flash read. macOS not verified.

## Findings

Command/response probes could not join a run and their results were not
recorded; fixed with steps and control devices ([ADR 0014](../adr/0014-steps-and-control-devices.md)).

Fixture: `testdata/real-ccdebugger` (target EUI-64 redacted; no firmware dump).
