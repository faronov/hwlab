# hwlab implementation audit

Date: 2026-10-08. Scope: evidence-based audit of what hwlab does today.
No repository files were modified by the audit and no destructive hardware
operation was run. Contradictions with existing docs are listed in section 8
and are **not fixed** yet.

## 0. Build and test verification

| Check | Result |
|---|---|
| `gofmt -l .` | clean |
| `go vet ./...` and `go vet -tags=hardware ./...` | clean |
| `go test -race -count=1 ./...` | all packages ok (`RACE_RC=0`) |
| `CGO_ENABLED=0 go build ./...` | ok |

Hardware-tagged tests run in the audit (safe defaults only):

| Test | Outcome |
|---|---|
| PPK2 ampere capture | PASS |
| ccdebugger native adapter state | PASS (target=0x2530 fw=1484) |
| J-Link probe info | PASS |
| J-Link connect and flash read (NRF52840_XXAA) | PASS |
| UART open and write on ttyACM0 | PASS |
| nRF802154 sniffer start/stop | SKIP (the DK runs the test square wave, no sniffer attached) |
| Saleae digital capture | SKIP (Logic 2 not running) |

Not run: the cc-tool hardware test in `plugins/ccdebugger`, and the
destructive/slow variants (`HWLAB_TEST_PPK2_SOURCE`, `HWLAB_CC_NATIVE_RESET`,
`HWLAB_CC_READ_FLASH`, `HWLAB_TEST_PPK2_REPLUG`).

Other facts:
- `hwlab devices` found ccdebugger, jlink, ppk2 and three uart ports.
- `hwlab doctor` warns that tshark, Logic 2 and usbmon are unavailable.
- Windows builds were not tested in this audit.

## 1. Backend classification, proven from code

Repo-wide search for `os/exec`, `import "C"`, dlopen, tshark, JLink, OpenOCD,
cc-tool, probe-rs, TlsrPgm, PicoSDK: no cgo, dlopen or purego anywhere.
`os/exec` is used only in:
- `internal/plugin/client.go` (spawns plugins)
- `internal/app/doctor.go` (LookPath)
- `internal/decode/tshark.go`
- `plugins/ccdebugger/tool.go` (cc-tool)
- `plugins/jlink/commander.go` (JLinkExe)
- `internal/testutil` (tests)

OpenOCD, probe-rs, TlsrPgm and PicoSDK appear only in comments, in
`internal/status/registry.go`, and in one error-message hint in
`plugins/cmsisdap/dap/usb_linux.go:69`.

| Plugin | Class | Evidence |
|---|---|---|
| PPK2 | NATIVE_OS | Protocol in Go (`plugins/ppk2/protocol/protocol.go`) over CDC serial from `internal/usbserial` (`go.bug.st/serial`). No exec. |
| nRF802154 | NATIVE_OS + OPTIONAL_EXTERNAL | Serial shell protocol `plugins/nrf802154/protocol/device.go`. tshark via `internal/decode/tshark.go` (ErrUnavailable if missing). |
| Saleae | EXTERNAL_API | gRPC to Logic 2 at `HWLAB_SALEAE_ADDR` (default 127.0.0.1:10430) via `plugins/saleae/internal/saleaepb`. Does not launch Logic 2. Registry label "vendor-sdk" is imprecise. |
| J-Link | EXTERNAL_CLI | `plugins/jlink/commander.go` runs JLinkExe with `-CommandFile` scripts. RTT is a telnet client (port 19021) to the JLinkExe-hosted server. Native part: Linux sysfs discovery (`usb.go`). |
| CC-Debugger | NATIVE_GO (Linux) + OPTIONAL_EXTERNAL | `plugins/ccdebugger/native/usbfs_linux.go`, `debugger.go`: direct usbfs ioctls, no libusb. cc-tool optional fallback in `tool.go`. Non-Linux: no native backend. |
| UART | NATIVE_OS | `plugins/uart/port.go` (`go.bug.st/serial`), termios/flow control via x/sys/unix. |
| CAN / SocketCAN | NATIVE_OS (SocketCAN) + SYNTHETIC (virtual bus) | `plugins/can/socketcan_linux.go` (raw AF_CAN socket), `virtual.go`. No USB-CAN backend. |
| SCPI | NATIVE_OS | TCP, serial (via `uart.OpenPort`), Linux `/dev/usbtmc*` (`internal/scpi/transport.go:139`). |
| Rigol / Siglent | NATIVE_OS | Profiles in `internal/scpi/profile.go`; no separate backend. |
| CMSIS-DAP | NATIVE_GO (Linux) | usbfs bulk v2 and hidraw v1 in `plugins/cmsisdap/dap/usb_linux.go`; `discover.go` sysfs scan. Non-Linux: stub. |
| Modbus RTU / TCP | NATIVE_OS | `internal/modbus`; RTU via `uart.OpenPort`, TCP via `net`. |
| Telink | NATIVE_OS (serial) | `plugins/telink/sws/{frame,programmer,debug,flashops,layout}.go` over `uart.Port`. TlsrPgm/probe-rs fallbacks not wired. |
| PicoScope | SYNTHETIC_ONLY | `plugins/picoscope/backend/fake.go` is the only backend. No PicoSDK binding. |
| USB capture | NATIVE_OS (file/usbmon text) + SYNTHETIC | `internal/usbcap` reads usbmon text from file, FIFO or live path; `usbcap.Synthetic` generator. |
| fake | SYNTHETIC_ONLY | CI plugin. |

## 2. Capability audit and scores

Abbreviations: HW = REAL_HW_VERIFIED, HWp = REAL_HW_PARTIAL,
REPLAY = REPLAY_TESTED, SYN = SYNTHETIC_TESTED, UNT = IMPLEMENTED_UNTESTED,
NOT = NOT_IMPLEMENTED.

**PPK2 — 9/9 implemented, 8/9 HW**
- discovery HW
- metadata and calibration read HW
- ampere capture HW (accuracy against a reference load unverified)
- source voltage HW
- DUT power HW
- digital input HWp (only D0 saw real edges; D1–D7 unverified)
- long capture HW (10 min, 0 drops)
- reconnect HW
- replay HW

Evidence: `docs/notes/phase2-ppk2-results.md`, `testdata/real/ppk2-ampere-1s.hwcap`, `testdata/real-ppk2-saleae`, the audit's ampere test.

**nRF802154 — 6/7 implemented, 6/7 HW**
- discovery HW
- channel scan HW (dwell-based)
- pcapng/TAP capture HW
- tshark decode HW (optional)
- sniffer start/stop HW (skipped in the audit, run earlier)
- replay HW
- channel hopping NOT

Evidence: `testdata/real/nrf802154-ch15-10s.pcapng`, `testdata/real-session`.

**Saleae — 7/7 implemented, 3/7 HW**
- discovery HW
- digital capture HW (also shared-signal calibration)
- replay HW
- UART analyzer HWp (pipeline ran on noise only)
- I2C analyzer HWp (same)
- analog UNT (original Logic has no analog: unsupported by hardware, not failed)
- SPI UNT (no signal available)

Evidence: `testdata/real-saleae`, `testdata/real-ppk2-saleae`.

**J-Link — 12/12 implemented, 9/12 HW**
- discovery, info, connect/identify, memory read, halt, resume, reset, registers HW
- replay HW
- memory write HWp (peripheral-register writes drove the DK square wave; RAM/flash writes unverified)
- RTT HWp (transport verified with a host-planted control block; firmware RTT unverified)
- flash program UNT

Evidence: `docs/notes/milestone-hw-validation-results.md`, `testdata/real-jlink`, `testdata/real-jlink-rtt`, `testdata/real-ppk2-saleae/*/experiment.yaml`.

**CC-Debugger — 13/15 implemented, 7/15 HW**
- discovery, adapter info, reset-attach and status, register read, CODE/flash read HW
- cc-tool read-only operations HW (info, MAC, 256 KB flash read)
- replay HW
- halt/resume HWp (resume + enable_dma verified)
- XDATA read UNT
- XDATA write SYN
- step SYN
- breakpoint SYN
- cc-tool erase/program UNT
- native erase NOT
- native program NOT

Evidence: `docs/notes/phase3d-native-results.md`, `docs/notes/phase3d-ccdebugger-results.md`, `testdata/real-ccdebugger*`; synthetic: `native/simulator.go`.

**UART — 9/9 implemented, 6/9 HW**
- discovery, open/configure, TX, RX, timestamped chunk capture, replay HW
- hardware flow control SYN (pty/fakes)
- parity/stop/data bits and baud above 115200 SYN
- set_lines UNT

Evidence: `testdata/real-uart`, `testdata/real-uart-rx` (23 B from the DK UARTE0).

**CAN / SocketCAN — 7/7 implemented, 0/7 HW**
- recv, send, interface status, CAN-FD, pcapng export, capture, replay: all SYN
- Socket layer tested only over a unix socketpair (`plugins/can/socketcan_linux_test.go`); no vcan or adapter ever used. The virtual bus is synthetic.

**SCPI core — 10/10 implemented, 0/10 HW**
- TCP, serial, USBTMC, identify/query/command, binary query, channel/timebase config, trigger, acquire/waveform, measure, replay: all SYN
- Only peer is the in-process `internal/scpi/fake.go`. Serial and USBTMC transports are UNT.

**Rigol — 4/4 implemented, 0/4 HW. Siglent — 4/4 implemented, 0/4 HW**
- Profile constants (channel naming, scaling, trigger, waveform), SYN against the fake.

**CMSIS-DAP — 12/15 implemented, 0/15 HW**
- discovery, USB v2 and v1 transports, connect, halt/resume, step, reset, register read/write, memory read/write, replay: all SYN against `dap/sim.go`; transports UNT
- NOT: flash, JTAG, RTT/SWO

**Modbus RTU — 5/5 implemented, 0/5 HW**
- read, write, polling capture, offline decode, replay: all SYN.

**Modbus TCP — 4/4 implemented, 0/4 HW**
- read, write, polling capture, replay: all SYN (real loopback TCP, synthetic slave).

**Telink — 13/14 implemented, 0/14 HW**
- discovery (`HWLAB_TELINK_PORTS` only), attach, halt/resume, step, reset, registers, breakpoints, memory read/write, flash read/verify, erase, program, factory-region protection, replay: all SYN against `sws/sim.go`
- NOT: SWS printf logging

**PicoScope — 1/9 implemented, 0/9 HW**
- Only offline `waveform measure` is real; all device-facing capabilities are SYN against the fake. PicoSDK backend NOT.

**USB capture — 5/6 implemented, 0/6 HW**
- usbmon text/file ingest, pcapng, offline decode, replay: SYN
- live usbmon UNT (permission denied here)
- physical bus analyzer NOT

## 3. Master table

| Plugin | Backend | Impl | HW | Level | Real HW tested | Replay | CGO | Required external | Major gaps |
|---|---|---|---|---|---|---|---|---|---|
| PPK2 | NATIVE_OS | 9/9 | 8/9 | 4 | yes | REAL | darwin only | none | calibration vs reference load, D1–D7 |
| nRF802154 | NATIVE_OS + OPT | 6/7 | 6/7 | 4 | yes | REAL | darwin only | none (tshark optional) | channel hopping |
| Saleae | EXTERNAL_API | 7/7 | 3/7 | 4 | yes (partial) | REAL | no | Logic 2 | analog, SPI, decode correctness |
| J-Link | EXTERNAL_CLI | 12/12 | 9/12 | 4 | yes | REAL | no | JLinkExe | firmware RTT, RAM/flash write, program |
| CC-Debugger | NATIVE_GO + OPT | 13/15 | 7/15 | 4 | yes (read-only) | REAL | no | none (cc-tool optional) | native erase/program NOT; writes, step, breakpoints unverified |
| UART | NATIVE_OS | 9/9 | 6/9 | 4 | yes | REAL | no | none | flow control, parity, high baud |
| CAN / SocketCAN | NATIVE_OS | 7/7 | 0/7 | 3 | no | SYNTHETIC | no | none | real vcan/adapter, USB-CAN |
| SCPI | NATIVE_OS | 10/10 | 0/10 | 3 | no | SYNTHETIC | no | none | any real instrument |
| Rigol | NATIVE_OS | 4/4 | 0/4 | 2 | no | SYNTHETIC | no | none | profile unchecked on a real device |
| Siglent | NATIVE_OS | 4/4 | 0/4 | 2 | no | SYNTHETIC | no | none | profile unchecked on a real device |
| CMSIS-DAP | NATIVE_GO (Linux) | 12/15 | 0/15 | 3 | no | SYNTHETIC | no | none | real probe; flash, JTAG, RTT NOT |
| Modbus RTU | NATIVE_OS | 5/5 | 0/5 | 3 | no | SYNTHETIC | no | none | real slave |
| Modbus TCP | NATIVE_OS | 4/4 | 0/4 | 3 | no | SYNTHETIC | no | none | real slave |
| Telink | NATIVE_OS | 13/14 | 0/14 | 3 | no | SYNTHETIC | no | none | real board; SWS printf NOT |
| PicoScope | SYNTHETIC_ONLY | 1/9 | 0/9 | 1 | no | SYNTHETIC | no | PicoSDK (not bound) | no real backend |
| USB capture | NATIVE_OS + SYN | 5/6 | 0/6 | 3 | no | SYNTHETIC | no | none | live usbmon; no bus-level analyzer |

Level rationale:
- Level 4: PPK2, nRF802154, Saleae, J-Link, CC-Debugger, UART — important paths ran on real hardware, intended scope not complete.
- Level 3: real backend implemented, synthetic tests only.
- Level 2: Rigol/Siglent — profile values checked only against a fake written by the same author.
- Level 1: PicoScope — no real backend.
- No plugin reaches Level 5.

## 4. Replay evidence

- **REAL_CAPTURE_REPLAY:**
  - ppk2: `testdata/real/ppk2-ampere-1s.hwcap`
  - nrf802154: `testdata/real/nrf802154-ch15-10s.pcapng`, `testdata/real-session`
  - saleae: `testdata/real-saleae`
  - jlink: `testdata/real-jlink`, `testdata/real-jlink-rtt`
  - ccdebugger: `testdata/real-ccdebugger`, `testdata/real-ccdebugger-native`
  - uart: `testdata/real-uart`, `testdata/real-uart-rx`
  - ppk2 + saleae: `testdata/real-ppk2-saleae`
- **SYNTHETIC_REPLAY** (`testdata/synthetic-{can,cmsisdap,modbus,picoscope,scpi,telink,usbcap}`): can, scpi/Rigol/Siglent, cmsisdap, modbus, telink, picoscope, usbcap.
- Replay tests: `internal/app/*_replay_test.go`. Plot and MCP queries from replay were confirmed for the real PPK2+Saleae session; not individually re-verified for every other fixture.

## 5. Dependency table

| Plugin | Core backend | Required | Optional |
|---|---|---|---|
| PPK2 | Go serial | none | none |
| nRF802154 | Go serial | none | tshark |
| Saleae | Logic 2 gRPC | Logic 2 running with automation | none |
| J-Link | JLinkExe | SEGGER J-Link software (`HWLAB_JLINK_EXE` or PATH) | none |
| CC-Debugger | Go usbfs | none (Linux only) | cc-tool |
| UART | Go serial | none | none |
| CAN | SocketCAN | Linux, a `can`/`vcan` interface | none |
| SCPI | TCP, serial, `/dev/usbtmc*` | none | none |
| Modbus | TCP, serial | none | none |
| CMSIS-DAP | usbfs and hidraw | Linux, udev permissions | none |
| Telink | Go serial | none | TlsrPgm and probe-rs (not wired) |
| PicoScope | none | PicoSDK (not bound) | none |
| USB capture | usbmon text | debugfs/usbmon access for live capture | none |

## 6. CGO (measured with `CGO_ENABLED=0` cross-builds)

- Core `hwlab`: builds with CGO=0 on linux and darwin, amd64 and arm64.
- linux amd64 / arm64: no plugin requires CGO.
- darwin amd64 / arm64:
  - **ppk2, nrf802154**: CGO=0 build fails (`go.bug.st/serial/enumerator: undefined: nativeGetDetailedPortsList`). Needed only for VID/PID identification via IOKit; serial I/O itself is pure Go.
  - **uart, modbus, scpi, telink**: build with CGO=0; `plugins/uart/enum_darwin_nocgo.go` provides a `/dev/cu.*` glob without VID/PID. With CGO on, `enumerator` still appears in their deps but is not required.
  - Modbus TCP and SCPI TCP audited separately: no CGO.
  - saleae, jlink, ccdebugger, can, cmsisdap, picoscope, usbcap: no CGO.
- Windows: not tested in this audit.
- The earlier claim (only ppk2 and nrf802154 need CGO, only on darwin) is confirmed.
- Linux-only hardware paths (ccdebugger native, cmsisdap transports, SocketCAN, J-Link sysfs discovery) compile elsewhere but report the hardware path unavailable — a platform limit, not a CGO one.

## 7. Placeholders, stubs and wiring

**Placeholders**
- No TODO/FIXME or "not implemented" panics outside tests.
- `panic` only in `plugins/ccdebugger/native/simulator.go:277`.
- Synthetic paths are gated by env vars: `HWLAB_SCPI_FAKE`, `HWLAB_MODBUS_FAKE`, `HWLAB_TELINK_SIM`, `HWLAB_CMSISDAP_SIM`, `HWLAB_PICOSCOPE_FAKE`, `HWLAB_USBCAP_SYNTH`.
- `CapTriggerInput`, `CapTriggerOutput`, `CapVoltageMeasurement` are used only by the fake plugin or not at all.

**Wiring**
- All plugin methods are reachable through `hwlab invoke`, MCP `device_invoke` and experiment steps. Dangerous methods are gated by `policy.RequireConfirmation` (`internal/app/app.go:204–227`, `run.go:370`).
- Debug, memory and flash operations have no dedicated CLI commands, only the generic invoke route.
- Capture needs TimedCapture, LiveRawStream or OfflineCapture (`internal/app/run.go:295`); telink and cmsisdap have none, so they work only as invoke targets and experiment steps.
- scpi, modbus and telink find devices only through env vars (`HWLAB_SCPI_HOSTS`, `HWLAB_MODBUS_HOSTS`, `HWLAB_TELINK_PORTS`); no scanning.
- Telink TlsrPgm/probe-rs fallbacks are not wired.
- `can` `inject` works on the virtual bus only.
- Saleae uses post-hoc CSV export, no live streaming.
- `hwlab doctor`/`devices` list both PPK2 CDC ports (ttyACM1, ttyACM2) as separate uart devices.

## 8. Contradictions (listed, not fixed)

1. **UART RX is stale.** README:32, ROADMAP (open items and wishlist), `docs/hardware.md:11`, ADR 0017:44 and `phase5-uart-results.md` still say RX was never verified on real hardware. Real RX is verified (`testdata/real-uart-rx`).
2. **cmsisdap transport.** `internal/status/registry.go` and `docs/support-matrix.md` say there is no USB HID/bulk transport. `plugins/cmsisdap/dap/usb_linux.go` and `discover.go` implement both (untested).
3. **USBTMC.** Registry and support matrix say it is not written. `internal/scpi/transport.go:139` implements it; README, ROADMAP and ADR 0020 describe it as existing.
4. **J-Link memory write.** Registry and `phase3c-jlink-results.md` list writes as unverified/not run. Peripheral-register writes did run on real hardware (`milestone-hw-validation-results.md`, `real-ppk2-saleae`). RAM and flash writes remain unverified.
5. **cc-tool.** Registry says never run against a target. `phase3d-ccdebugger-results.md` records real read-only cc-tool runs.
6. **nRF "channel hopping unverified".** The feature does not exist: missing, not unverified.
7. **Dependency labels.** Saleae is tagged "vendor-sdk" but is EXTERNAL_API. ccdebugger is "native" without noting it is Linux-only.
8. **Windows.** README says Windows builds are CGO-free; not rechecked in this audit.

## Summary

**A. Genuinely native**
- PPK2, nRF802154 capture, UART (Go over serial).
- CC-Debugger native (usbfs, Linux).
- SocketCAN, SCPI, Modbus, Telink SWS (OS interfaces + Go; unverified except UART/serial layer).
- CMSIS-DAP USB transport (Linux, unverified).

**B. Vendor SDK / API**
- Saleae Logic 2 (EXTERNAL_API).
- PicoSDK: a dependency that is not bound.

**C. External CLI wrappers**
- J-Link (JLinkExe, including RTT).
- cc-tool (optional).
- tshark (optional).

**D. Synthetic or replay only**
- CAN, SCPI/Rigol/Siglent, CMSIS-DAP, Modbus RTU/TCP, Telink, PicoScope, USB capture.

**E. Real-hardware verified**
- PPK2, nRF802154 sniffer, Saleae digital, J-Link non-destructive operations and register writes, CC-Debugger read-only paths, UART TX/RX.
- PPK2+Saleae shared-signal calibration.

**F. Exists but unverified**
- SocketCAN; SCPI, USBTMC, Rigol, Siglent; CMSIS-DAP transports; Modbus; Telink; live usbmon.
- J-Link firmware RTT, RAM/flash write, program.
- CC-Debugger native writes, step, breakpoints.
- Saleae analog and SPI; UART flow control.

**G. Actually missing**
- PicoSDK backend; native CC2530 erase/program; nRF channel hopping.
- Telink SWS printf, TlsrPgm and probe-rs fallbacks.
- CMSIS-DAP flash, JTAG, RTT.
- USB-CAN backends; physical bus-level USB analyzer; dedicated debug CLI commands.

**H. Top 5 next validation tasks**
1. Real SocketCAN: vcan, then a physical adapter.
2. A real SCPI instrument (Rigol or Siglent) over TCP and USBTMC.
3. A real CMSIS-DAP probe plus target, to verify the USB transport.
4. Firmware RTT with a real RTT control block (J-Link), plus RAM/flash writes on a sacrificial target.
5. A TLSR8258 board with an SWS programmer, and a sacrificial CC2530 for native flash.

## Addendum (Telink phase 14)

The Telink rows above predate phase 14. Current state: read-only operations
(identify, memory/flash read, halt/resume) are validated on one real board and
the replay fixture `testdata/real-telink` is real. See
[phase14-telink-real-hardware-results.md](phase14-telink-real-hardware-results.md).
Erase/program/memory write/step/breakpoints remain unverified on hardware.

## Addendum (CC-Debugger phase 15)

The CC-Debugger rows above predate phase 15. Native identify, lock/IEEE/info
page, SFR, XDATA scratch write, halt/resume, breakpoint, step, flash read and CRC
verify are now validated on the real CC2530. Native chip/page erase, program and
config write are implemented but hardware-unverified. See
[phase15-ccdebugger-results.md](phase15-ccdebugger-results.md) and
[phase15-ccdebugger-parity-matrix.md](phase15-ccdebugger-parity-matrix.md).
