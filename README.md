# hwlab

`hwlab` is a hardware-experimentation platform, not a collection of vendor
wrappers. Any source of physical signal, debug event or network packet — a
power profiler, a logic analyzer, an 802.15.4 sniffer, a debug probe — can be
placed on **one shared timeline**, correlated, plotted, replayed without
hardware and queried by an AI assistant through **MCP**.

```
 power ──┐                       ┌── CLI  (hwlab run / plot / replay …)
 digital ─┤  plugins (gRPC,      │
 packets ─┼─ out-of-process) ──► core ──► session ──► MCP server (stdio)
 debug  ──┘  unix socket         │  unified timeline  └── plots (SVG/PNG)
                                 └── replay (no hardware)
```

Typical question it answers: *"Which packet was on the air when the DUT's
current jumped to 16 mA, and how long did the burst last?"*

Implemented in Go with a native-first design; CLI and MCP share the same
service/core. Linux is the primary platform. Hardware-support claims below
refer to recorded evidence, not blanket validation of every adapter or target.

## Status — Milestone 1

| Area | State |
|---|---|
| Core: timeline, storage, correlation, analysis, plots, replay, policy, experiment engine | working, tested |
| CLI and MCP server (stdio) | working, tested |
| `fake` plugin (power, logic, 802.15.4 radio, debug) | working — drives CI and the integration test |
| `ppk2` plugin (Nordic PPK2, pure Go) | **validated on real hardware**: ampere and source modes, 10 min at 100 kSa/s with no drops, reconnect — see [docs/notes/phase2-ppk2-results.md](docs/notes/phase2-ppk2-results.md). Calibration against a known load is not yet checked |
| `nrf802154` plugin (Nordic 802.15.4 sniffer) | **validated on real hardware** (nRF52840 DK): channel scan, pcapng/TAP capture, optional tshark decode — see [docs/hardware.md](docs/hardware.md) |
| `jlink` plugin (`jlink-segger`: SEGGER J-Link via `JLinkExe`, RTT) | implemented via `JLinkExe`; **validated on two real probes (a J-Link OB and a SAM-ICE) with an nRF52840 and an EFR32MG1P: enumeration, selection by serial, connect, memory read, halt/resume/reset, registers, RTT data; writes and flashing unverified** — see [ADR 0012](docs/adr/0012-jlink-debugprobe.md) |
| `jlinknative` plugin (`jlink-native`, pure Go J-Link USB) | **validated on a real SAM-ICE class probe with an EFR32MG1P, no SEGGER process or library**: SWD, full Cortex-M debug (halt/resume/registers/SRAM/100 steps/breakpoints/watchpoint hit/resets), reconnect, unplug/replug, target-memory RTT identical to `jlink-segger`; JTAG implemented but untested, reset pin unsupported on the attached board, nRF52840 and multi-probe not yet run — see [ADR 0034](docs/adr/0034-native-jlink.md), [results](docs/notes/phase-native-jlink-results.md) |
| `saleae` plugin (Logic 2 automation API) | digital/analog capture, SPI/I2C/UART analyzers, `.sal` + CSV artifacts, replay; **digital capture + UART/I2C analyzers validated on a real original Saleae Logic; analog and real-bus protocol decoding unverified** — see [ADR 0015](docs/adr/0015-saleae-logic2.md) |
| `ccdebugger` plugin (TI CC-Debugger, native Go USB backend + optional cc-tool) | **read/debug validated on real hardware** (CC2530): native identify, lock/IEEE/info page, registers, SFR, XDATA scratch write, halt/resume, breakpoint, step, 256 KB flash read and CRC verify (matching cc-tool); native chip/page erase, program and config write are implemented but **hardware-unverified** (simulator only); generic `hwlab target ...` commands — see [ADR 0018](docs/adr/0018-native-ccdebugger.md) |
| `ftdi` plugin (generic FTDI/MPSSE/JTAG probe, native Go usbfs; Sipeed BL702 profile) | **USB transport and MPSSE validated on a real SIPEED JTAG Debugger (0403:6010, FT2232D emulation); JTAG target scan implemented but untested (no target attached)**; capabilities JTAGTransport, JTAGScan, TargetIdentify only; no OpenOCD, Python or CGO — see [ADR 0028](docs/adr/0028-native-ftdi-mpsse-jtag.md) |
| `espusbjtag` plugin (Espressif native USB Serial/JTAG, native Go usbfs + generic JTAG + RISC-V Debug Module + native SPI1 flash driver) | **validated on a real ESP32-C6 (303a:1001)**: scan, core identification, halt/resume/step, registers, SRAM write+restore, hardware breakpoints, hart/system/SoC reset with re-enumeration, physical unplug/replug, flash geometry/read/backup/erase/program/verify/restore with a boot + debug + restore cycle; hart 1 reports unavailable; app/chip erase simulated only; no eFuse access; destructive operations need `--yes`, plugin tokens, `allow_*` settings and a verified backup; CDC serial stays with `cdc_acm`; no OpenOCD, esptool, ESP-IDF, Python or CGO — see [ADR 0029](docs/adr/0029-espressif-usb-jtag-riscv-dm.md), [ADR 0030](docs/adr/0030-esp32c6-flash-and-debug.md) |
| `stlink` plugin (ST-LINK/V2 `0483:3748`, native Go usbfs + SWD + generic Cortex-M layer + STM32 F1 flash (real) + F4 flash (simulator) layers) | **validated on a real ST-Link V2 with an nRF52840 and an STM32F1 (Cortex-M3) target**: discovery by USB serial, identify, halt/resume, registers, SRAM write+restore, single-step, FPB breakpoints, DWT watchpoint, system/core reset, reconnect, two physical unplug/replug cycles; **native STM32F1 flash is REAL_HW_VERIFIED (two identical backups, page erase, program, verify, boot, restore, hash equal); STM32F4 flash is simulator-only; full-chip erase/MER and option bytes not exercised**; NRST pin reset not wired/observed; JTAG, SWO and V1/V3 not verified; destructive flash operations need `--yes`, tokens, `allow_*` and a fresh verified backup; no OpenOCD, STM32CubeProgrammer, st-flash, libusb, Python or CGO — see [ADR 0031](docs/adr/0031-native-stlink-cortexm.md), [phase 20](docs/notes/phase20-stlink-results.md), [phase 21](docs/notes/phase21-stlink-stm32f1-results.md) |
| `uart` plugin (generic serial transport) | open, configure and **TX validated on a real port** (nRF52840 DK VCOM); RX, flow control and stop bits validated over a pty/fakes only; no real RX yet — see [ADR 0017](docs/adr/0017-generic-uart.md) |
| `can` plugin (CAN / CAN-FD, SocketCAN + virtual bus) | frame model, capture, pcapng (link type 227, decoded by tshark) and replay **tested with a synthetic virtual bus only; real SocketCAN/vcan/adapters unvalidated (no hardware)** — see [ADR 0019](docs/adr/0019-generic-can.md) |
| `scpi` plugin (SCPI over TCP/serial/USBTMC; oscilloscope semantics; Rigol and Siglent profiles) | raw SCPI, scope control, waveform capture/export, plot and replay **tested with a synthetic fake only; real instruments and both vendor profiles unvalidated (no hardware)** — see [ADR 0020](docs/adr/0020-generic-scpi.md) |
| `xds110` plugin (TI XDS110, native Go usbfs, shared ADIv5 + Cortex-M layers) | DebugProbe + MemoryAccess. **Probe side real-hardware verified on a standalone LP-XDS110ET with no target attached** (USB identity, firmware, clocks, empty-chain JTAG scan, CDC/HID coexistence, BUSY, unplug/replug); **native SWD debug verified on a user-owned CC2340 (Cortex-M0+): halt/resume, register and SRAM read/write, 100 steps, 4 hardware breakpoints, reset and reset-and-halt through the SACI boot ROM without a power cycle, flash backup / chip erase / program / verify / restore (CC23xx layer, [phase 26](docs/notes/phase26-cc2340-saci-flash.md), SWD up to 10 MHz); no DWT on that core (UNSUPPORTED_BY_ATTACHED_HW); target power switch UNSUPPORTED_BY_ATTACHED_HW on the LP-XDS110ET (3V3 pin is the probe rail via jumper P9, [phase 27](docs/notes/phase27-lp-xds110et-target-power.md), [ADR 0033](docs/adr/0033-power-switch.md)); dormant SW-DP after power-on is woken automatically on both `xds110` (OCD_PATHMOVE) and `cmsisdap`, [note](docs/notes/cc2340-dormant-swd-wake.md); EnergyTrace range 0 PROTOCOL_PARTIAL (native clean-room decoder, 256 kSPS current only, gated by `allow_target_power`, accuracy not verified against a load)**; target-memory RTT (`rtt_info`, RTT capture with `stream: rtt`) **verified on hardware on the CC2340** (`rtt_write` simulator-tested only) — see [docs/xds110.md](docs/xds110.md), [phase 24](docs/notes/phase24-xds110-results.md), [phase 25](docs/notes/phase25-cc2340-m0plus-acceptance.md), [ADR 0032](docs/adr/0032-cc23xx-target-layer.md) |
| `cmsisdap` plugin (CMSIS-DAP probes, native Go v2 bulk/v1 HID, shared ADIv5 + Cortex-M layers) | DebugProbe + MemoryAccess. **Probe side real-hardware verified on a BL702-DAPLink with no target attached** (DAP_Info, SWD/JTAG connect, clocks, CDC coexistence, BUSY, 2 unplug/replug cycles); **target operations (SWD, identify, halt/resume, registers read/write, memory read/write, 100 steps, hardware breakpoints, reset, CC23xx SACI flash backup/erase/program/verify/restore) and v1 HID verified on a CC2340 through the LP-XDS110ET HID interface; the BL702-DAPLink itself has not had a target; no SWO/JTAG debug**; target-memory RTT (`rtt_info`, `rtt_write`, RTT capture) implemented and simulator-tested, **not yet verified on hardware** — see [ADR 0021](docs/adr/0021-cmsis-dap.md), [phase 22](docs/notes/phase22-cmsisdap-results.md) |
| `telink` plugin (Telink SWire programmer, native Go SWS stack, TLSR825x family) | DebugProbe + MemoryAccess + FlashProgramming (attach, halt/resume/step/reset, breakpoint, registers, memory, flash read/verify/erase/program with factory-data protection) **validated on one real board: identify, memory/flash read, halt/resume, reset/attach, full-flash backup, sector program with readback verify, restore from backup, single-step, 3 hardware breakpoints with hit detection, whole-image program + restore with journals, SRAM write on a stalled core, `MemoryAccess.watch` sleep/wake tracking of a deep-retention sleeper with prime-on-wake; flash modification needs a verified protection map, a halted CPU and a board-matched backup; PC write unsupported by the hardware** — see [ADR 0023](docs/adr/0023-telink-swire.md), [debug results](docs/notes/telink-debug-hardware-results.md), [watch results](docs/notes/telink-watch-retention-hardware-results.md) |
| `bflb` plugin (Bouffalo BL702 boot-ROM ISP over the chip's own USB, native Go + vendor `eflash_loader` RAM helper) | TargetIdentify + FlashProgramming (identify, backup, program, verify, restore, reset). **Validated on one XT-ZB1 DevKit over USB: ROM identify, helper load, full-flash backup, erase / program / chip-side SHA verify of a zigbee-rs image (the firmware booted), reset; restore and the UART path not run on hardware; the vendor helper loses USB frames of 64 bytes or more, so writes go in 55-byte frames** — see [ADR 0036](docs/adr/0036-bflb-native-isp.md), [results](docs/notes/bflb-bl702-usb-isp-results.md) |
| `picoscope` plugin (PicoScope oscilloscopes, block mode; backend seam) + `hwlab waveform measure` | Oscilloscope + AnalogCapture: channels, ranges, timebase, trigger, block acquire, measurements, bounded waveform envelope, CSV artifact, replay **tested with a SYNTHETIC scope only; no PicoSDK driver is built in and no PicoScope was ever connected** — see [ADR 0024](docs/adr/0024-picoscope.md) |
| `usbcap` plugin (USB packet capture at URB level; Linux usbmon) + `hwlab usb analyze` | USBPacketCapture: usbmon-layout packets, pcapng (link type 220), offline control/HID/CDC/DFU/MSC decoding, **capture level: URB / host-controller observation (not a physical bus-level USB analyzer); live usbmon: unverified**; replay **tested with synthetic data and usbmon text files only; the live usbmon backend was never run (no root/debugfs on the dev host) and there is no bus-level analyzer backend** — see [ADR 0025](docs/adr/0025-usb-packet-capture.md) |
| `modbus` plugin (Modbus RTU over the generic UART, Modbus TCP) | master reads/writes, polling capture, offline `hwlab modbus decode` of recorded buses **tested against a synthetic slave and replay only; no real device** — see [ADR 0022](docs/adr/0022-modbus.md) |
| Recorded experiment steps / control devices (command-response probes in a run) | working, tested — see [ADR 0014](docs/adr/0014-steps-and-control-devices.md) |
| Everything else | see [ROADMAP.md](ROADMAP.md) |

## Build

Requires Go (see `go.mod`). `protoc` is only needed to regenerate the API
(`make proto`); generated code is committed.

```sh
make build          # bin/hwlab + bin/hwlab-plugin-{fake,ppk2,nrf802154,saleae,jlink,jlinknative,ccdebugger,ftdi,espusbjtag,stlink,xds110,uart,can,scpi,cmsisdap,modbus,telink,picoscope,usbcap,bflb}
make test           # go test ./...
make race           # go test -race ./...
```

`make build` records its exact outputs and SHA-256 digests in
`bin/.hwlab-build-ownership` (requires `sha256sum`). `make clean` removes only
those registered, unchanged files; modified outputs, neighboring files and
directories without an ownership manifest are retained.

Plugins are separate executables named `hwlab-plugin-<id>`. They are found in
`--plugin-path`, `$HWLAB_PLUGIN_PATH`, `~/.hwlab/plugins`,
`/usr/local/lib/hwlab/plugins` and next to the `hwlab` binary, so running from
`bin/` just works.

> cgo: the core and every plugin build with `CGO_ENABLED=0` on linux and
> windows. On darwin only `ppk2` and `nrf802154` need cgo, and only to identify
> the device by USB VID/PID (`go.bug.st/serial/enumerator` calls IOKit); serial
> I/O itself is pure Go. `uart`, `modbus`, `scpi` and `telink` used to inherit
> that through a shared helper and no longer do (without cgo, darwin port
> listing is a `/dev/cu.*` glob without VID/PID). Modbus TCP and SCPI TCP never
> needed it. `make check-cgo` cross-compiles all of this. Details:
> [ADR 0026](docs/adr/0026-cgo-audit.md).
>
> Native USB probes on macOS: `internal/usb` reaches IOKit through purego
> (no cgo, no libusb), so `jlinknative`, `xds110`, `cmsisdap` (v2 bulk only),
> `stlink`, `espusbjtag` and `ftdi` build and run there. Only `jlinknative` has
> been run on macOS, on one J-Link OB (discovery, probe info, SWD identify,
> memory reads, halt/resume); the others are untested on macOS. CMSIS-DAP v1
> (HID) and the native CC-Debugger stay Linux only. Details:
> [ADR 0035](docs/adr/0035-macos-iokit-usb.md).

## Workflow

Examples below use the built-in `fake` plugin, so they run anywhere. Set
`HWLAB_HOME` to keep sessions out of `~/.hwlab` while trying things:
`export HWLAB_HOME=/tmp/hwlab-home`.

### 1. Discovery

```sh
bin/hwlab plugins     # discovered plugins, versions, capabilities
bin/hwlab devices     # devices reported by every plugin
bin/hwlab status      # plugins, devices, running captures, latest session
```

### 2. Capture

Ad-hoc, synchronized capture on several devices (`ALIAS=PLUGIN[/DEVICE]`,
`--set ALIAS.KEY=VALUE` passes plugin configuration):

```sh
bin/hwlab lab capture --device pwr=fake/power0 --device rf=fake/radio0 \
    --set pwr.realtime=false --duration 1s
```

With real hardware, vendor shortcuts wrap the same engine:

```sh
bin/hwlab ppk2 capture --duration 30s --mode source --voltage 3300 --yes
bin/hwlab sniffer scan --dwell 1s                          # which channels are active?
bin/hwlab sniffer capture --channel 15 --duration 60s      # pcapng + timeline packets
```

A supply voltage is a *dangerous* setting and needs `--yes` (see
[Safety](#safety)). In `ampere` mode the PPK2 never drives the DUT supply. In
`source` mode a capture that switched the DUT supply on switches it off again
when it ends; set `keep_dut_power: true` in the device config to keep it on.

### 3. Experiment

An experiment is a YAML file: devices, duration, analysis and plots
([`examples/fake-experiment.yaml`](examples/fake-experiment.yaml)).

```sh
bin/hwlab run examples/fake-experiment.yaml
```

This creates a session containing the unified timeline, raw sample chunks,
a `.pcap` artifact, derived events (power rises/falls, digital edges) and the
plots listed in the file. Device keys other than `plugin`, `device` and
`requires` are passed to the plugin as its configuration.

### 4. Inspect, analyze, plot

```sh
bin/hwlab session list
bin/hwlab session info latest
bin/hwlab timeline latest --type packet,analysis.power_rise --from 10ms --to 20ms
bin/hwlab correlate latest --help           # packets around power rises
bin/hwlab analyze latest --nominal-voltage 3.3
bin/hwlab plot latest --from 10ms --to 18ms --log --format png -o first-tx.png
bin/hwlab explain latest --spike 3 --nominal-voltage 3.3   # packets, energy, timing interval
```

Plots draw min/max/mean envelopes per pixel column, so a 30 µs spike is never
averaged away, even on a long capture. A session reference is a path, an id, a
unique id prefix or `latest`. Add `--json` to any command for machine-readable
output. `explain` reports cross-device timing as an interval derived from each
device's clock bounds ([ADR 0011](docs/adr/0011-clock-uncertainty-and-explain.md)),
never as a nanosecond-accurate delta.

### 5. Replay without hardware

```sh
bin/hwlab replay latest                     # open read-only, print summary
bin/hwlab replay latest --plot out.svg
bin/hwlab replay latest --rerun             # push the recording through the
                                            # normal pipeline into a new session
bin/hwlab run my-experiment.yaml --replay SESSION   # `plugin: replay` devices
```

`--rerun` exercises ingest, analysis and plotting exactly like a live run, so
regressions in the analysis code can be tested against recorded captures.

### 6. MCP

```sh
bin/hwlab mcp --stdio
```

Client configuration (Claude Desktop, VS Code, Copilot CLI, …):

```json
{
  "mcpServers": {
    "hwlab": {
      "command": "/path/to/bin/hwlab",
      "args": ["mcp", "--stdio"],
      "env": { "HWLAB_HOME": "/path/to/hwlab-home" }
    }
  }
}
```

27 tools (`lab_status`, `device_list`, `target_op`, `capture_*`, `experiment_run`,
`timeline_*`, `artifact_*`, `capability_registry`, …) with structured error codes,
confined file paths, 64-bit address validation and a `--replay SESSION` mode.
See [docs/mcp.md](docs/mcp.md).

Design rules: raw samples are **never** returned over MCP (only summaries,
envelopes, events and plots; PNG plots are returned inline), result sizes are
capped, `capture_start` requires a duration, and dangerous operations require
`confirm: true`, which the assistant must only set after the user approved it.

Example prompt once connected: *"Run a 5 s capture on the fake power0 and
radio0 devices, then tell me what packet is closest to the first current rise
and plot 10–18 ms."*

## Safety

Plugins declare which configuration keys and methods are dangerous (for
example the PPK2 supply voltage or `PowerSource.set_voltage`). The policy layer
refuses them unless the caller confirmed: `--yes` on the CLI,
`confirm: true` on MCP. Plugins must also bound their own parameters.
Undeclared plugin methods are refused. Details:
[ADR 0004](docs/adr/0004-policy-and-confirmation.md).

## Writing a plugin

A plugin is any executable that prints `HWLAB-PLUGIN|1|unix|<socket>` on stdout
and serves the gRPC service in
[`proto/hwlab/plugin/v1/plugin.proto`](proto/hwlab/plugin/v1/plugin.proto).
In Go, use `pkg/pluginsdk`; [`plugins/fake`](plugins/fake) is the smallest
complete example. See [ARCHITECTURE.md](ARCHITECTURE.md) and
[CONTRIBUTING.md](CONTRIBUTING.md).

## Documentation

- [ARCHITECTURE.md](ARCHITECTURE.md) — design, protocol, storage, clock strategy, lifecycle, risks
- [ROADMAP.md](ROADMAP.md) — milestones and the device/protocol wishlist
- [CONTRIBUTING.md](CONTRIBUTING.md)
- [docs/adr/](docs/adr/) — architecture decision records

## License

No license file has been chosen yet; add one before publishing.
