# Roadmap

Legend: ✅ done · 🟡 partial · ⬜ not started

## Hardware validation status

Honest summary; a plugin is only called validated if it ran on real hardware.

| Status | Plugins |
|---|---|
| **Real hardware, partially validated** (full matrix: [docs/support-matrix.md](docs/support-matrix.md)) | `ppk2` (current, source mode, digital D0 real edges, two-device calibration; current calibration against a reference load unverified); `nrf802154` (sniffer); `saleae` (digital, shared-signal calibration, UART/I2C analyzer pipeline on noise; analog unsupported by the connected original Logic; decode correctness unverified); `jlink` (non-destructive; two real probes selected by serial; RTT verified with firmware on an EFR32MG1P); `ftdi` (USB transport and MPSSE on a Sipeed FT2232D emulator; no JTAG target scanned); `espusbjtag` (ESP32-C6 native USB Serial/JTAG: full debug and flash cycle); `stlink` (ST-Link V2 + nRF52840: native SWD debug, unplug/replug; STM32 flash simulated only); `cmsisdap` (BL702-DAPLink v2 bulk, no target: probe side verified; target operations simulator-only); `xds110` (LP-XDS110ET, no target: probe side verified; target operations simulator-only; EnergyTrace not implemented); `ccdebugger` (native read-only; native flash erase/program not implemented; cc-tool fallback synthetic only); `uart` (TX and RX on a real port, 23 B from the DK UARTE) |
| **Simulator / synthetic + replay only** | `can`, `scpi` (Rigol, Siglent), `modbus`, `picoscope`, `usbcap` (synthetic and usbmon text files; live usbmon never run) |
| **Multi-device correlation on real hardware** | **PPK2 digital input + Saleae on one real signal, one session, measured calibration**: 59970 shared edges over 300 s, PPK2 clock 92.6 ppm off the Saleae clock, residual p95 5 µs, uncertainty ±48 µs ([results](docs/notes/phase12-multi-device-results.md), [ADR 0027](docs/adr/0027-measured-affine-clock-mapping.md)). Other pairings (nRF802154, three-way) not run |

## Milestone 1 — foundation (this release)

- ✅ Capability-based plugin contract (gRPC over unix sockets), SDK, process manager with crash recovery
- ✅ Unified timeline, binary session storage, correlation, power analysis, SVG/PNG plots
- ✅ Experiment engine (YAML), policy/confirmation layer
- ✅ Replay (read-only and `--rerun` through the pipeline)
- ✅ CLI and MCP server (stdio); MCP validated end-to-end on real ST-Link/STM32F1 ([docs](docs/mcp.md), [results](docs/notes/phase23-mcp-results.md))
- ✅ `fake` plugin and the end-to-end integration test (power + packets → timeline → correlation → plot → replay)
- 🟡 `ppk2` — validated on a real PPK2 (ampere/source, 10 min stress, reconnect); calibration against a known load and digital inputs with real signals still open
- ✅ `nrf802154` — channel scan, pcapng (802.15.4 TAP) capture with clock metadata, tshark decode; validated on an nRF52840 DK ([ADR 0010](docs/adr/0010-nrf802154-sniffer.md))
- 🟢 `saleae` — Logic 2 automation capture ([ADR 0015](docs/adr/0015-saleae-logic2.md)); digital + UART/I2C analyzers validated on a real original Saleae Logic, analog unverified

## Open items from Milestone 1

- ⬜ PPK2: check calibration against a known load (resistor), digital inputs with real signals, long-capture drift against an external reference; decide on the reference's spike filter
- ⬜ `saleae`: validate analog capture (needs a Logic Pro/analog model) and decoded SPI/I2C/UART on a real bus
- ⬜ Artifact transfer over the wire (today: shared filesystem only)
- ⬜ MCP over HTTP/SSE with authentication (today: stdio only)
- ✅ Cross-device clock calibration against a real shared signal: `hwlab calibrate-clocks` measures offset, drift and uncertainty ([ADR 0027](docs/adr/0027-measured-affine-clock-mapping.md), [results](docs/notes/phase12-multi-device-results.md)). ⬜ Not yet: applying the measured rate at ingest, pairings with the sniffer, temperature dependence
- ⬜ `jlink`: writes/flash unverified; one Commander session per probe at a time when RTT runs on two probes
- ⬜ `uart`: validate RX on real hardware (loopback jumper P0.06↔P0.08 on the nRF52840 DK, or a USB-UART adapter), parity/data bits on a real adapter
- ⬜ `can`: validate real SocketCAN (vcan on a host with the kernel modules, then a physical adapter): own echo, kernel timestamps, kernel filters, interface-down handling
- ⬜ `scpi`: run the Rigol and Siglent profiles on real instruments (channel naming, waveform scaling, screenshots), USBTMC and serial transports; memory-depth waveforms; LAN discovery; PSU/DMM semantics
- ⬜ `cmsisdap`: wire a real SWD target to the BL702-DAPLink and run the target checks (identify, halt/step/breakpoint/watchpoint, memory, resets incl. nRESET GPIO24, throughput); USB v1 HID on a real probe; JTAG core debug, SWO, RTT; macOS v1 HID and a hardware run of v2 on macOS ([ADR 0035](docs/adr/0035-macos-iokit-usb.md)); Windows transport (flash stays in target-specific layers)
- ⬜ `xds110`: connect a target to P7 and run the target checks (SWD DPIDR/AP, Cortex-M debug, JTAG with ICEPick/JTAG-DP for TI parts, SRST polarity, target supply on a real load); EnergyTrace needs a protocol oracle (usbmon capture of a TI tool session)
- ⬜ `modbus`: run against a real RTU slave (RS-485 timing, word order, vendor quirks) and a TCP slave; plot polled register values as analog tracks
- ⬜ CC-Debugger: run the sacrificial-target acceptance procedure ([phase 15 results](docs/notes/phase15-ccdebugger-results.md)) to validate native chip/page erase, program and config write; further chip families (CC2531/CC254x/CC111x), SmartRF04/05 programmers, macOS transport
- ⬜ Real DUT correlation (spike ↔ DATA ↔ ACK) with `hwlab explain` on a PPK2 + sniffer session
- ⬜ Session retention / compaction tooling; optional chunk compression
- ⬜ Windows validation of the unix-socket launch path
- ⬜ Better idle/spike baselines for duty cycles above 50 %

## Wishlist

Grouped by kind; nothing below is implemented unless stated in the milestones above.

### Power
- ⬜ **PPK2** — validation and polish (see above)

### Radio / wireless
- ✅ **Nordic IEEE 802.15.4** sniffer — capture (Phase 1)
- ⬜ **Zigbee** — decode, network-key handling, device-level timelines
- ⬜ **Thread** — decode, commissioning flows

### Logic and analog
- 🟢 **Saleae** — digital + UART/I2C analyzers validated on real hardware; analog pending
- 🟡 **PicoScope** — `picoscope` plugin ([ADR 0024](docs/adr/0024-picoscope.md)): block-mode contract, vendor-neutral backend seam, synthetic scope, offline waveform analysis (`hwlab waveform measure`); **no real driver and no hardware**. Streaming, rapid-block, FFT, real PicoSDK backend: not implemented
- 🟡 **Rigol** — DS1000Z profile in the `scpi` plugin; **unvalidated, no instrument** ([ADR 0020](docs/adr/0020-generic-scpi.md)). PSU semantics not implemented
- 🟡 **Siglent** — SDS1000X-E profile in the `scpi` plugin; **unvalidated, no instrument**. PSU semantics not implemented
- 🟡 **Generic SCPI** — `scpi` plugin: TCP / serial / USBTMC transports, raw query/binary query/command (CONFIRM), oscilloscope semantics, acquire-and-export capture; tested against a synthetic fake only

### Buses and protocols
- 🟡 **UART** — `uart` plugin ([ADR 0017](docs/adr/0017-generic-uart.md)); real open + TX validated on an nRF52840 DK VCOM, RX/flow control/parity validated over a pty and fakes only
- 🟡 **CAN / CAN-FD / SocketCAN** — `can` plugin ([ADR 0019](docs/adr/0019-generic-can.md)); frame model, capture, pcapng and replay tested with a synthetic virtual bus; **real SocketCAN/vcan/adapters unvalidated**
- ⬜ **ISO-TP**
- ⬜ **UDS**
- 🟡 **Modbus RTU** — `modbus` plugin (master over the generic UART) plus offline `hwlab modbus decode` for recorded buses ([ADR 0022](docs/adr/0022-modbus.md)); simulator and replay tested only, **no real device validated**
- 🟡 **Modbus TCP** — `modbus` plugin (MBAP master); loopback and simulator tested only, **no real device validated**
- 🟡 **USB analyzer** — `usbcap` plugin ([ADR 0025](docs/adr/0025-usb-packet-capture.md)): URB-level model (Linux usbmon), pcapng, offline control/HID/CDC/DFU/MSC analysis (`hwlab usb analyze`); **capture level: URB / host-controller observation (not a physical bus-level USB analyzer); live usbmon: unverified; synthetic and text-file tested; live usbmon never run, no bus-level analyzer backend**
- ⬜ **SPI / I2C / UART protocol analyzers** (decoders over digital captures)

### Debug
- 🟢 **J-Link (`jlink-segger`)** — `jlink` plugin via JLinkExe ([ADR 0012](docs/adr/0012-jlink-debugprobe.md)); non-destructive operations validated on two real probes (J-Link OB, SAM-ICE)
- 🟢 **J-Link native** — `jlinknative` plugin (`jlink-native`, [ADR 0034](docs/adr/0034-native-jlink.md)): pure-Go USB + SWD + generic adiv5/cortexm, no SEGGER software; EFR32MG1P full debug and RTT verified on a real SAM-ICE class probe. Open: nRF52840, multi-probe, real JTAG chain, reset pin, SWO, step speed. macOS ([ADR 0035](docs/adr/0035-macos-iokit-usb.md)): read-only run on a J-Link OB; timeout recovery with the fixed transport not yet re-run
- 🟡 **TI CC-Debugger** — `ccdebugger` plugin: native pure-Go USB backend ([ADR 0018](docs/adr/0018-native-ccdebugger.md)) with cc-tool as optional fallback ([ADR 0013](docs/adr/0013-ccdebugger-backend.md)); hardware-validated read/debug (native identify, registers, SFR, XDATA scratch write, halt/resume, breakpoint, step, flash read, CRC verify, info/MAC); native chip/page erase, program and config write implemented with a preserve-device-config default but simulator-tested only; generic `hwlab target` commands; macOS native backend not available
- 🟡 **FTDI / MPSSE / JTAG** — `ftdi` plugin ([ADR 0028](docs/adr/0028-native-ftdi-mpsse-jtag.md)): native Go usbfs FTDI protocol, MPSSE engine and generic JTAG TAP/IR/DR/IDCODE scan, Sipeed BL702 (FT2232D emulation) profile; USB transport and MPSSE validated on a real device, **JTAG target scan untested (no target attached)**; transport only (no RISC-V DM / ARM DP layer yet), no nSRST/nTRST, Linux only
- 🟡 **Espressif USB Serial/JTAG + RISC-V DM** — `espusbjtag` plugin ([ADR 0029](docs/adr/0029-espressif-usb-jtag-riscv-dm.md), [ADR 0030](docs/adr/0030-esp32c6-flash-and-debug.md), [phase 19](docs/notes/phase19-esp-flash-debug-results.md)): native Go transport, shared JTAG layer, generic RISC-V Debug Module and an ESP-specific flash layer; validated on a real ESP32-C6 (step, hardware breakpoints, SRAM write, resets incl. SoC re-enumeration, physical unplug/replug, flash backup/erase/program/verify/restore with boot); open: hart 1 (unavailable on this board), app/chip erase and unplug-during-write on hardware, eFuse access (deliberately absent), other Espressif chips
- 🟡 **ST-Link V2 + generic Cortex-M** — `stlink` plugin ([ADR 0031](docs/adr/0031-native-stlink-cortexm.md), [phase 20](docs/notes/phase20-stlink-results.md), [phase 21](docs/notes/phase21-stlink-stm32f1-results.md)): native Go usbfs transport, ST-Link V2 protocol, SWD/JTAG, generic Cortex-M layer (`internal/cortexm`) and STM32 F1 (real silicon) and F4 (simulator) flash layers; debug validated on a real ST-Link with an nRF52840 and an STM32F1 (halt/resume, registers, SRAM write, step, FPB breakpoints, DWT watchpoint, resets, physical unplug/replug); native STM32F1 flash verified on real silicon (backup, erase, program, verify, boot, restore); open: F4 and other STM32 families on silicon, full-chip erase/MER, option bytes (deliberately untouched), JTAG and multi-probe on hardware, NRST pin reset, SWO, V1/V3 probes
- 🟡 **CMSIS-DAP** — `cmsisdap` plugin ([ADR 0021](docs/adr/0021-cmsis-dap.md), [phase 22](docs/notes/phase22-cmsisdap-results.md)): probe side validated on a real BL702-DAPLink (v2 bulk, no target); target operations (SWD, halt/resume, registers read/write, SRAM write, 100 steps, 4 hardware breakpoints, reset) validated on a real CC2340 (Cortex-M0+) through the LP-XDS110ET HID interface ([phase 25](docs/notes/phase25-cc2340-m0plus-acceptance.md)); CC23xx SACI reset and flash validated there too ([phase 26](docs/notes/phase26-cc2340-saci-flash.md)); SWO/JTAG debug not implemented
- 🟡 **TI XDS110** — `xds110` plugin ([docs/xds110.md](docs/xds110.md), [phase 24](docs/notes/phase24-xds110-results.md)): native Go usbfs XDS110 protocol, shared ADIv5/Cortex-M/JTAG layers; probe side validated on a real standalone LP-XDS110ET; native SWD debug validated on a real CC2340 (Cortex-M0+, [phase 25](docs/notes/phase25-cc2340-m0plus-acceptance.md)), plus the CC23xx SACI reset/CCFG/flash layer ([phase 26](docs/notes/phase26-cc2340-saci-flash.md), [ADR 0032](docs/adr/0032-cc23xx-target-layer.md)); target power switch UNSUPPORTED_BY_ATTACHED_HW on the LP-XDS110ET (SET_SUPPLY is a no-op; [phase 27](docs/notes/phase27-lp-xds110et-target-power.md), PowerSwitch proposed in [ADR 0033](docs/adr/0033-power-switch.md)); **EnergyTrace range 0 PROTOCOL_PARTIAL** ([protocol](docs/xds110-energytrace-protocol.md)); accuracy vs a calibrated load, low ranges, voltage/energy open
- 🟡 **Telink / SWire (TLSR8258)** — `telink` plugin ([ADR 0023](docs/adr/0023-telink-swire.md)): native Go SWS/SWire transport (flash erase/program/verify, memory, attach, halt/run/step, breakpoints) over the generic `DebugProbe`/`MemoryAccess`/`FlashProgramming` capabilities; factory EUI/config/calibration flash is never erased or overwritten; read-only operations validated on a real board (CH340 programmer, TLSR825x-family target), erase/program fail closed until a protection map is verified, **flash modification never run on hardware**. Still open: a real board run, WAIT_RESP, SWS_PRINTF, OTP chips; TlsrPgm / tc32 probe-rs fallbacks were not needed
- 🟡 **RTT** — log events on the timeline (via J-Link; data verified on an EFR32MG1P running RTT firmware)

### Platform features
- 🟡 **Experiment engine** — YAML runs work; conditionals, loops, parametric sweeps and triggers between devices are missing
- 🟡 **Unified timeline** — in place; richer event schemas per protocol
- 🟡 **Correlation** — nearest/around/pairs; statistical latency reports and rule-based assertions missing
- 🟡 **Plotting** — power/digital/event tracks; analog, protocol lanes and interactive HTML missing
- ✅ **Replay** — recorded sessions through the full pipeline
- ⬜ **Capture regression** — compare a new session against a baseline (current, energy, latency budgets) and fail CI
