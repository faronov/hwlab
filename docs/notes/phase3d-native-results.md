# Phase 3D (native) — CC-Debugger direct-USB results

Host: Linux, no `cc-tool` involved in these operations, no cgo. Hardware: TI
CC-Debugger (0451:16a2, firmware 0x5CC rev 0x44) wired to a private CC2530.
Policy: read-only, reset-attach and resume only; **nothing was written, erased
or programmed**, and the target was left running.

| Check | Result |
|---|---|
| Native usbfs open (VID/PID check, claim interface 0) | PASS |
| Adapter state (target 0x2530) | PASS |
| Reset-attach (status 0x22, PC 0) | PASS |
| Status / debug-config decode | PASS |
| Register read (stable across two reads) | PASS |
| CODE read | PASS — identical to the earlier cc-tool dump |
| Physical flash read (bank 1, 0x8000) | PASS — identical to the cc-tool dump |
| `enable_dma` + `resume` (status 0x03, target running) | PASS |
| Recorded steps + replay with the probe unplugged | PASS (`testdata/real-ccdebugger-native`) |
| XDATA write, breakpoints, step, halt of a running target | **not run on hardware** (simulator only) |
| Native chip erase / flash program | **not implemented** (cc-tool fallback) |

Run it: `hwlab run --yes examples/ccdebugger-native.yaml`.
See [ADR 0018](../adr/0018-native-ccdebugger.md) for design and the clean-room
licensing statement.
