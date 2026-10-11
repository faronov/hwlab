# Hardware support matrix and setup

| Device | Plugin | Status | Notes |
|---|---|---|---|
| nRF52840 DK, nRF 802.15.4 Sniffer firmware (1915:154b) | `nrf802154` | **validated** (scan, capture, pcapng/TAP, tshark decode, replay) | see below |
| Nordic PPK2 (1915:c00a) | `ppk2` | **validated** (ampere, source, 10 min stress, replug); calibration vs. known load and digital signals unverified | [results](notes/phase2-ppk2-results.md) |
| SEGGER J-Link / RTT (`jlink-segger`) | `jlink` | **hardware-validated (non-destructive ops on a J-Link OB); target RTT data, write and flash unverified** | see below, [ADR 0012](adr/0012-jlink-debugprobe.md), [results](notes/phase3c-jlink-results.md) |
| SEGGER J-Link, native (`jlink-native`) | `jlinknative` | **real-hardware-validated on an EFR32MG1P** (SWD, Cortex-M debug, RTT, unplug/replug); JTAG untested, nRF52840/multi-probe not run | [J-Link native](#j-link-native-jlink-native), [ADR 0034](adr/0034-native-jlink.md), [results](notes/phase-native-jlink-results.md) |
| TI CC-Debugger (0451:16a2) + CC2530 | `ccdebugger` | **read/debug validated** (native identify, lock/IEEE/info page, registers, SFR, XDATA scratch write, halt/resume, breakpoint, step, flash read and CRC verify matching cc-tool; replay); native chip/page erase, program and config write implemented, **not run on hardware** | see below, [ADR 0018](adr/0018-native-ccdebugger.md), [ADR 0013](adr/0013-ccdebugger-backend.md), [results](notes/phase3d-native-results.md), [phase 15](notes/phase15-ccdebugger-results.md) |
| ESP32-C6 native USB Serial/JTAG (303a:1001) | `espusbjtag` | **real-hardware-validated** (per-capability list in the [support matrix](support-matrix.md)): debug, reset, unplug/replug, flash backup/program/verify/restore; hart 1, app/chip erase not verified | see below, [ADR 0029](adr/0029-espressif-usb-jtag-riscv-dm.md), [ADR 0030](adr/0030-esp32c6-flash-and-debug.md), [phase 18](notes/phase18-esp-usb-jtag-results.md), [phase 19](notes/phase19-esp-flash-debug-results.md) |
| Sipeed RV-Debugger-BL702 / XT-ZB1 (0403:6010, FT2232D emulation) | `ftdi` | **FTDI USB transport and MPSSE real-hardware-partial; JTAG target scan implemented but untested (no target attached)** | see below, [ADR 0028](adr/0028-native-ftdi-mpsse-jtag.md), [results](notes/phase16-ftdi-mpsse-jtag-results.md) |
| Saleae Logic 2 | `saleae` | **hardware-validated (digital + UART/I2C analyzers, original Logic); analog unverified** | see below, [ADR 0015](adr/0015-saleae-logic2.md), [results](notes/phase3b-saleae-results.md) |
| fake devices | `fake` | working | drives CI |
| generic serial (any port) | `uart` | **TX validated on a real port; RX over pty/fakes only** | see below, [ADR 0017](adr/0017-generic-uart.md), [results](notes/phase5-uart-results.md) |
| CAN / CAN-FD (SocketCAN, virtual bus) | `can` | **unvalidated on real hardware** (synthetic virtual bus + socketpair tests, tshark-decoded pcapng, replay) | see below, [ADR 0019](adr/0019-generic-can.md), [results](notes/phase6-can-results.md) |
| SCPI oscilloscopes (Rigol, Siglent, generic) | `scpi` | **unvalidated on real hardware** (synthetic fake instrument, replay) | see below, [ADR 0020](adr/0020-generic-scpi.md), [results](notes/phase7-scpi-results.md) |
| TI XDS110 (LP-XDS110ET, LaunchPad on-board XDS110) | `xds110` | probe side **real-hardware verified** on one standalone LP-XDS110ET; native SWD debug (attach, identify, halt/resume, registers read/write, SRAM write/restore, 100 steps, 4 hardware breakpoints, reset via SACI, flash backup/erase/program/verify/restore) verified on a user-owned CC2340 (Cortex-M0+), with results matching the same probe's HID CMSIS-DAP interface; DWT absent on that core (UNSUPPORTED_BY_ATTACHED_HW); target power switch UNSUPPORTED_BY_ATTACHED_HW (3V3 pin = probe rail, jumper P9; see [phase 27](notes/phase27-lp-xds110et-target-power.md)); EnergyTrace range 0 PROTOCOL_PARTIAL (current only, accuracy unverified) | [docs/xds110.md](xds110.md), [phase 24 results](notes/phase24-xds110-results.md), [phase 25 results](notes/phase25-cc2340-m0plus-acceptance.md), [phase 26 results](notes/phase26-cc2340-saci-flash.md) |
| CMSIS-DAP probes (DAPLink, Pi Debug Probe, BL702-DAPLink, ...) | `cmsisdap` | probe side **real-hardware verified** on one BL702-DAPLink (v2 bulk, no target attached) and, for the target side (SWD, identify, halt/resume, registers read/write, memory read/write, 100 steps, hardware breakpoints, reset, CC23xx flash) and v1 HID, on a CC2340 through the LP-XDS110ET's HID interface; no SWO/JTAG debug | see below, [ADR 0021](adr/0021-cmsis-dap.md), [phase 22 results](notes/phase22-cmsisdap-results.md) |
| Modbus RTU / TCP devices | `modbus` | **unvalidated on real hardware** (synthetic slave, replay) | see below, [ADR 0022](adr/0022-modbus.md), [results](notes/phase9-modbus-results.md) |
| Telink SWire programmer + TLSR8258 | `telink` | **unvalidated on real hardware** (simulator, replay); no chip erase | see below, [ADR 0023](adr/0023-telink-swire.md), [results](notes/phase13-telink-results.md) |
| PicoScope oscilloscopes | `picoscope` | **no real driver, never connected** (synthetic scope, replay); contract only | see below, [ADR 0024](adr/0024-picoscope.md), [results](notes/phase10-picoscope-results.md) |
| USB packet capture (Linux usbmon) | `usbcap` | **live backend never run** (no root/debugfs); synthetic + text-file + replay only | see below, [ADR 0025](adr/0025-usb-packet-capture.md), [results](notes/phase11-usb-results.md) |

## nRF 802.15.4 sniffer on Linux

1. Flash the nRF Sniffer for 802.15.4 firmware (hwlab never flashes devices).
2. Add your user to `dialout` (or the group owning `/dev/ttyACM*`) and re-login.
3. ModemManager may probe CDC ports and steal the first bytes; if capture
   fails to start, stop it (`systemctl stop ModemManager`) or add a udev rule
   `ENV{ID_MM_DEVICE_IGNORE}="1"` for 1915:154b. Do not run Wireshark's
   extcap on the same port at the same time (hwlab reports a busy port).
4. Optional: install `tshark` (any 4.x) for decoded events. Without root,
   unpack the Debian packages into a prefix and put a wrapper that sets
   `LD_LIBRARY_PATH` and `WIRESHARK_DATA_DIR` on `PATH`.

```sh
hwlab devices
hwlab sniffer scan --dwell 1s            # per-channel packets, RSSI, LQI
hwlab sniffer capture --channel 15 --duration 10s [--no-decode]
```

Open `<session>/artifacts/sniffer/nrf802154-chN.pcapng` in Wireshark; the
`clock.*` artifact metadata describes timing uncertainty
([ADR 0010](adr/0010-nrf802154-sniffer.md)).

## Combined PPK2 + sniffer

```sh
hwlab run examples/zigbee-power.yaml          # PPK2 + sniffer in one session
hwlab session info latest                     # per-device clock bounds
hwlab explain latest --spike 3 --nominal-voltage 3.3 --plot spike3.png
hwlab calibrate latest --a-type … --b-type … --window 5ms
```

The DUT must be powered through the PPK2 (source mode, or ampere mode in
series) and transmit on the sniffed channel. Cross-device timing is an
interval, not a point: the sniffer's lower bound is unbounded, so `explain`
can report "order undetermined". See [ADR 0011](adr/0011-clock-uncertainty-and-explain.md).
## PPK2 digital input + Saleae on one signal

DK P0.03 → PPK2 D0, DK P0.04 → Saleae CH0, DK VDD → PPK2 **logic-port VCC**
(the logic port is a separate connector from the GND/VOUT/VIN/GND power header
and reads nothing unless its VCC pin is tied to the target I/O voltage), common
GND. See `examples/ppk2-saleae-shared-signal.yaml` and
`docs/notes/phase12-multi-device-results.md`; then
`hwlab calibrate-clocks latest --ref logic --dev ppk2 --ref-channel SHARED --dev-channel D0 --save`.
The signal must contain a non-periodic marker so edges can be paired.

## Combined PPK2 + sniffer (continued)

Verified on hardware: both devices recording in one session with real clock
bounds (PPK2 `[-4.16, +0.03] ms`) and replay without devices
(`testdata/real-session`, no DUT attached, so noise only). Not verified: a
real spike ↔ DATA ↔ ACK correlation.

## SEGGER J-Link and RTT (`jlink-segger`)

hwlab drives SEGGER's own `JLinkExe`; it never bundles or reimplements it.

1. Install the SEGGER J-Link software pack and the udev rules it ships.
2. Make `JLinkExe` findable: on `PATH`, in `/opt/SEGGER/JLink/`, or via
   `HWLAB_JLINK_EXE=/path/to/JLinkExe`. Without it the probe is listed as
   `discovery-only`.
3. Set `target` to the SEGGER device name (e.g. `NRF52840_XXAA`).

```yaml
devices:
  fw:  { plugin: jlink, requires: [RTT], target: NRF52840_XXAA }   # rtt_port: 19021
  ppk: { plugin: ppk2, mode: ampere }
  air: { plugin: nrf802154, channel: 15 }
```

Other keys: `interface`, `speed_khz`, `rtt_host`, `rtt_port`, `rtt_launch`
(`false` attaches to an RTT server you already run), `rtt_connect_timeout_ms`,
`rtt_address` (RTT control block address, e.g. from the firmware map file; sent
as `exec SetRTTAddr`. The J-Link software's own block search did not find a
block in RAM on the DK, an explicit address did).
Halt, reset, register reads, memory writes and flashing need `--yes`.
RTT lines are stamped on host receive, so their clock bound is `[0, +inf)`.
`rtt: false` makes the device call-only: no long-running Commander session.

**Several probes.** The device id is the probe's serial number
(`hwlab devices` lists every probe with model, nickname and `usb_device`); the
USB bus/device numbers change on replug, the serial does not. With more than
one probe attached a `device:` is required, and every Commander call passes
`-USB <serial>`; the plugin refuses an answer whose `S/N:` is another probe.
Results of `connect`, `halt`, `resume`, `reset`, `read_registers` and `read`
carry `probe_serial` and `target`, and the session stores both. Names are not a
selector when two probes share a model.
SEGGER's DLL lets other processes share only one probe that a long-running
session holds, so with two probes run RTT on one and set `rtt: false` on the
other (`examples/jlink-multiprobe.yaml`). Each Commander call is its own
session and the target runs again when it ends, so `halt` is confirmed with
`IsHalted` inside the same call and register reads are one script with the halt.
udev (used for the SAM-ICE, whose node was root-only; adjust `OWNER`):
`SUBSYSTEM=="usb", ENV{DEVTYPE}=="usb_device", ATTR{idVendor}=="1366", OWNER="alexx", MODE="0600"`,
then replug the probe.

**Status:** validated on a real J-Link OB (nRF52840 DK) and a SAM-ICE
(EFR32MG1P), non-destructive operations only; see the ADR and
`docs/notes/phase17-jlink-multiprobe-results.md`. SEGGER's license forbids redistribution, so
download the pack from segger.com; on Linux install its `99-jlink.rules`.

## J-Link native (`jlink-native`)

Plugin `jlinknative`: pure Go (Linux usbfs, no libusb, no CGO, no SEGGER
software). It is a **separate, explicitly selected** backend; there is no
fallback in either direction between it and `jlink-segger` (plugin `jlink`).

* Device id `jlink-native:<serial>`; the USB serial is the only identity, bus
  and device numbers change on replug.
* The probe needs read/write access to its USB node (VID 1366). Example udev
  rule (adjust `OWNER`): `SUBSYSTEM=="usb", ENV{DEVTYPE}=="usb_device", ATTR{idVendor}=="1366", OWNER="alexx", MODE="0600"`.
  `hwlab doctor` reports node access and warns if the probe is in bootloader
  mode (`1366:0101`, needs a physical replug).
* Provides: probe info, VTref, SWD (clock 100 kHz-8 MHz tested; 12 MHz refused by the tested target),
  generic Cortex-M debug (`internal/cortexm`), target-memory RTT (`internal/rtt`),
  JTAG chain scan (untested on hardware). Flash programming is **not** offered;
  use the target-family layers.
* Differences from `jlink-segger`: no device database, no flash loaders, no
  SWO, single-step is slower (about 6 ms vs under 1 ms); connect and small
  reads are faster (no process spawn).
* Verified: EFR32MG1P through a SAM-ICE class probe (`J-Link V9`). Not run: nRF52840, two probes at once.
  Per-capability status: [support matrix](support-matrix.md).

Hardware tests: `HWLAB_JLINKNATIVE_HW=1 HWLAB_JLINKNATIVE_SERIAL=<serial> go test -tags=hardware ./plugins/jlinknative/`
(skips without a probe; destructive-adjacent steps use a scratch SRAM address, `HWLAB_JLINKNATIVE_SCRATCH`).

## TI CC-Debugger (CC253x/CC254x/CC111x)

The primary backend is a native pure-Go USB implementation (Linux usbfs, no cgo,
no libusb; [ADR 0018](adr/0018-native-ccdebugger.md)). The open-source `cc-tool`
is an optional fallback (and cross-check); only a direct target `reset` still
requires it. `hwlab doctor` reports the native backend and cc-tool separately.
hwlab never bundles it.

1. Add a udev rule (or group access) for `0451:16a2` so the `/dev/bus/usb` node is
   user-accessible. Install `cc-tool` only if you need the fallback operations
   (`HWLAB_CC_TOOL` overrides the location). The native backend needs interface 0
   free; a cc-tool operation drops the native debug session first.
2. Native capabilities: `DebugProbe` (attach, status, reset_halt, halt, resume,
   step, read_registers, read_sfr, set_breakpoint, wait_halted, enable_dma) and
   `MemoryAccess` (xdata/code/flash read, xdata write) and native `FlashProgramming`
   (chip_info, read_lock, read_ieee, read_info_page, verify, erase, erase_pages,
   program, write_config). No RTT.
3. Everything that touches the target is off by default. Choose
   `access: adapter-only | existing-debug-session | reset-debug-session` and grant
   `allow_cpu_control`, `allow_target_reset`, `allow_memory_access`,
   `allow_memory_write`, `allow_breakpoints`, `allow_dma_enable` individually.
4. Record read-only identification, or a reset-attach read session:

```sh
hwlab run examples/ccdebugger-info.yaml            # cc-tool identification
hwlab run --yes examples/ccdebugger-native.yaml    # native reset-attach, reads, resume
```

Generic commands work through the same service (CLI and MCP `target_op`):

```sh
hwlab run --yes examples/ccdebugger-identify.yaml   # read-only identify/lock/IEEE/info page
hwlab target identify --attach --post DebugProbe.enable_dma --post DebugProbe.resume --yes
```

Native erase/program/config write need `--yes`, a confirmation token and the
`allow_flash_erase` / `allow_flash_program` / `allow_chip_erase` /
`allow_config_write` tiers; the default `preserve-device-config` mode never
touches the lock page. They are simulator-tested only. A full native flash read
takes ~3 min (64 B per round trip; cc-tool ~26 s in fast mode); CRC verify of a
full image takes ~3.4 s. Opt-in hardware tests:
`HWLAB_CC_NATIVE_RESET=1 HWLAB_CC_NATIVE_DEBUG=1 go test -tags hardware ./plugins/ccdebugger/native`.
macOS has no native backend yet.

## Saleae Logic 2

1. Install Saleae Logic 2 and start it with the automation server:
   `Logic --automation` (or enable it in Preferences > Automation). Default
   endpoint `127.0.0.1:10430`; override with `HWLAB_SALEAE_ADDR`.
2. Logic 2 and hwlab must see the same filesystem: Logic 2 writes the `.sal`
   and CSV exports into the session's artifact directory.
3. `hwlab devices` lists each attached analyzer; `hwlab invoke saleae
   DigitalCapture.app_info` reports the Logic 2 and API versions.
   `HWLAB_SALEAE_SIMULATION=1` also lists Logic 2's simulation devices.
4. Sample rates must be given explicitly (they depend on the model and on the
   enabled channels). See `examples/saleae-spi.yaml`.
5. Logic 2 delivers data only after the capture ends (no live stream); events
   appear on the timeline when the run stops.

## Generic serial (UART)

The `uart` plugin is a transport (`SerialTransport` + `TimedCapture`), not a
protocol: Modbus RTU, AT, NMEA and bootloader analyzers sit on top of it and
never open ports themselves.

- `hwlab devices` lists every port as `uart/<basename>` (`ttyACM0`, `ttyUSB0`,
  `cu.usbmodem…`). Ports the OS does not enumerate (pty, `socat`, rfcomm) can
  be offered with `HWLAB_UART_EXTRA_PORTS=name=/dev/pts/5`, or by setting the
  `path` config key.
- Config: `baud`, `data_bits` 5-8, `parity` none/even/odd/mark/space,
  `stop_bits` 1/1.5/2, `flow_control` none/rtscts/xonxoff (Linux/macOS),
  `lines: true` for one log event per text line, `idle_gap_ms`,
  `max_chunk_bytes`, `max_chunk_ms`. `dtr`/`rts` initial levels are dangerous
  config keys (they reset many boards).
- Methods: `read`, `status`, `configure`, `flush` are safe; `write` and
  `set_lines` need `--yes`.
- Timestamps are when the host read returned; USB-serial latency (e.g. 16 ms
  FTDI default) is not measured, and no precision is claimed.
- The serial port is exclusive: close other terminals first. On Linux you need
  the `dialout` group.
- Example: `examples/uart-nrf52840-dk.yaml`. For real RX, jumper the DK's
  P0.06 (TX) to P0.08 (RX) and run
  `HWLAB_UART_LOOPBACK=1 go test -tags=hardware ./plugins/uart/ -run Hardware -v`.

## CAN / CAN-FD

The `can` plugin is a transport (`CANTransport` + `PacketCapture` +
`TimedCapture`); ISO-TP, UDS, J1939 and CANopen will be decoders over its frames.
See [ADR 0019](adr/0019-generic-can.md). **No CAN hardware has been tried:** only
the synthetic virtual bus and a socketpair-based test of the socket layer.

- `hwlab devices` lists SocketCAN interfaces as `can/<name>` (`can0`, `vcan0`).
  `HWLAB_CAN_VIRTUAL=1` adds the in-process `virtual-can0`, whose frames are
  synthetic. Linux only.
- hwlab never changes the interface. Bring it up first:
  `sudo ip link set can0 up type can bitrate 500000` (FD: add `dbitrate 2000000 fd on`).
  Set `fd: true` only on an FD-capable interface (MTU 72).
- Config: `fd` (default: the interface's capability), `own` (default true:
  record frames this host sends), `error_frames`, `filters` (candump `id:mask`,
  `id~mask`, or a bare id).
- Methods: `status`, `recv` (max 256 frames), `inject` (virtual bus only) are
  safe; `send` transmits onto the bus and needs `--yes`.
- Captures produce `can` packet events (`attr.id`, `attr.dir`, `attr.kind`,
  `attr.dlc`, `attr.payload`) and `can-<iface>.pcapng` (link type 227) that
  Wireshark opens directly.
- Example: `examples/can-virtual.yaml`. First real test: a `vcan0` on a host with
  the `vcan` module, then `cansend vcan0 123#DEADBEEF`.

## SCPI instruments (oscilloscopes)

The `scpi` plugin offers raw SCPI (`SCPI`) on any instrument and scope
semantics (`Oscilloscope`) for the Rigol DS1000Z and Siglent SDS1000X-E
profiles. See [ADR 0020](adr/0020-generic-scpi.md). **No instrument has been
tried:** the profiles come from public programming guides and are reported
`profile_validated=false`.

- `export HWLAB_SCPI_HOSTS="scope=tcp://192.168.1.50,psu=serial:/dev/ttyUSB1@9600"`
  (TCP defaults to port 5025, serial to 9600 baud); `/dev/usbtmc*` are listed
  automatically. `HWLAB_SCPI_FAKE=rigol|siglent` adds the synthetic
  `fake-<profile>`. `hwlab devices` shows them as `scpi/<name>`.
- Device config: `timeout_ms`, `profile` (`rigol`, `siglent`, `generic`; default
  detected from `*IDN?`), `channel_count`. Capture config: `channels`, `trigger`
  (`none` reads the instrument's current memory, `single` arms and waits),
  `trigger_timeout_s`, `waveform`, `screenshot`.
- `query`, `query_binary` and all reads are safe and refuse anything that is not
  a query. `command`, `configure_channel`, `set_timebase`, `set_trigger` and
  `acquire` change the instrument and need `--yes`.
- A capture writes `waveform-chN.csv`, optional `screenshot.*`, and analog
  events that plot like any other analog stream. The `waveform` method returns
  statistics and an envelope only; full data lives in artifacts.
- Example: `examples/scpi-fake.yaml`.

## CMSIS-DAP probes

The `cmsisdap` plugin drives a CMSIS-DAP probe over SWD with a pure-Go stack
(no libusb, no CGO, no OpenOCD/probe-rs/Python) and exposes `DebugProbe` and
`MemoryAccess`. Layers: `internal/usb` -> `plugins/cmsisdap/dap` ->
`internal/adiv5` -> `internal/cortexm` (shared with ST-Link). See
[ADR 0021](adr/0021-cmsis-dap.md) and [phase 22 results](notes/phase22-cmsisdap-results.md).

**Validated:** one BL702-DAPLink (`d6e7:3507`, CherryUSB, v2 bulk) with **no target
attached**: discovery, DAP_Info, capabilities, packet limits, connect/disconnect
(SWD and JTAG), clock commands, read-only pins, empty-chain JTAG scan, CDC
coexistence, BUSY, 25 open/close rounds, two physical unplug/replug cycles.
**Not validated:** every target operation (identify, halt/resume, registers,
memory, step, breakpoint, watchpoint, reset) and CMSIS-DAP v1 HID.

- Linux only. Probes are found through sysfs by their "CMSIS-DAP" interface
  string; v2 (bulk) is used when present, otherwise v1 (HID, via hidraw). Only
  the DAP interface is claimed; a composite probe's CDC ACM interfaces keep their
  kernel driver and `/dev/ttyACM*` stays usable.
- Device id: `cmsisdap/<vid>:<pid>:<usb serial>`. Never the bus/device number
  (it changed on every replug). The BL702-DAPLink reports the placeholder serial
  `012345ABCDEF`, so two such boards cannot be told apart.
- Non-root access needs a udev rule for the probe, for example:
  `SUBSYSTEM=="usb", ATTRS{idVendor}=="d6e7", ATTRS{idProduct}=="3507", MODE="0660", GROUP="hwlab"`
  and for v1 probes `KERNEL=="hidraw*", ATTRS{idVendor}=="0d28", MODE="0660", TAG+="uaccess"`.
- Probe-level methods: `info` (DAP_Info, capabilities, packet limits), `pins`
  (read-only), `clock_probe` (accepted rates plus host-side timing; the firmware
  approximates, no precision is claimed), `jtag_scan` (TAP enumeration only),
  `ap_scan`, `dap_reset`.
- Firmware quirks seen: an undefined command gets no reply (timeout);
  `DAP_Delay(0)` wedges the BL702-DAPLink until a physical replug (hwlab never
  sends it); a fresh opener may find stale replies, which are drained on open.
- `HWLAB_CMSISDAP_SIM=1` adds `cmsisdap/sim-cmsisdap`, a SYNTHETIC probe and
  Cortex-M4 used by CI and the synthetic fixture.
- Device config: `clock_hz` (default 1 MHz), `ap` (default 0), `timeout_ms`.
- Reads (`info`, `pins`, `connect`, `status`, `MemoryAccess.read`) are safe. Halt,
  resume, step, reset, register access and memory writes need `--yes`.
- Not implemented: flash (belongs to target-specific layers), SWO, UART/COM port,
  JTAG core debugging.
- Wiring a target to the BL702-DAPLink (3.3 V logic only, common GND):
  SWD: SWDIO = GPIO2, SWCLK = GPIO15, nRESET = GPIO24.
  JTAG: TDI = GPIO0, TDO = GPIO1, TMS = GPIO2, TCK = GPIO15.
- Examples: `examples/cmsisdap-bl702-probe.yaml` (real, no target needed),
  `examples/cmsisdap-sim.yaml` (synthetic).
- Hardware tests (opt in): `HWLAB_CMSISDAP_HW=1 go test -tags hardware -count=1 -v ./plugins/cmsisdap/...`;
  unplug (needs a person): `HWLAB_CMSISDAP_UNPLUG=1 -run HWPluginUnplugReplug`.
  Never set `HWLAB_CMSISDAP_WEDGE=1` casually: it deliberately wedges the firmware.

## TI XDS110 probes

The `xds110` plugin drives the XDS110 vendor interface natively (see [xds110.md](xds110.md)): device id `xds110/<vid>:<pid>:<usb serial>`, only the debug interface is claimed, CDC and HID interfaces keep their drivers. udev: `SUBSYSTEM=="usb", ATTRS{idVendor}=="0451", ATTRS{idProduct}=="bef3", MODE="0660", GROUP="hwlab"`. The same probe is also listed by the `cmsisdap` plugin through its HID interface; the two use different interfaces and can be open together. Hardware tests (opt in): `HWLAB_XDS110_HW=1 go test -tags hardware -count=1 -v ./plugins/xds110/`; unplug (needs a person): `HWLAB_XDS110_UNPLUG=1 -run HWUnplugReplug`.

## Telink SWire programmer

The `telink` plugin drives a USB-serial SWire programmer (TlsrPgm-style, 230400
baud) and a TLSR8258 target with a pure-Go stack and exposes `DebugProbe`,
`MemoryAccess` and `FlashProgramming`. See
[ADR 0023](adr/0023-telink-swire.md). **No real programmer or chip has been tried.**

- Ports are never scanned. List the programmer explicitly:
  `HWLAB_TELINK_PORTS=pgm=/dev/ttyUSB0[@baud]` gives `telink/pgm`.
- `HWLAB_TELINK_SIM=1` adds `telink/sim-telink`, a SYNTHETIC programmer and
  target used by CI and the fixture.
- Device config: `baud`, `swdiv`, `timeout_ms`, `layout` (`tb04-512k` only).
- `info`, `connect`, `status`, `read_registers`, memory read, flash read and
  verify are safe. `connect` never touches the target. `attach` pulses RST;
  halt, resume, step, reset, breakpoints, memory write, flash erase and program
  need `--yes`.
- Flash safety: the factory EUI-64 (0x76000) and config/calibration (0x77000)
  sectors can never be erased or overwritten; the journals (0x70000-0x76000)
  need `allow_journals`; chip erase does not exist. Programming verifies by
  read-back.
- Not implemented: WAIT_RESP, SWS_PRINTF, FIFO, analog registers, OTP, baud
  switching.
- Example: `examples/telink-sim.yaml`.

## PicoScope

The `picoscope` plugin is the contract for block-mode PicoScope acquisition
(see [ADR 0024](adr/0024-picoscope.md)). **This build has no PicoSDK driver and
no PicoScope has ever been connected to it.** The only backend is a SYNTHETIC
scope.

- `HWLAB_PICOSCOPE_FAKE=1` adds `picoscope/fake-ps`: channels A-D, 8/12 bit,
  generated signals (A 1 kHz square, B 10 kHz sine, C 1.2 V DC, D 50 Hz
  +-15 V). Nothing else is listed.
- Capture and device settings are flat keys: `channels`, `range_v`,
  `offset_v`, `coupling`, `resolution_bits`, `sample_rate_hz`, `samples`,
  `pre_trigger`, `trigger_source` (a channel or `none`), `trigger_level_v`,
  `trigger_slope`, `trigger_timeout_s`, `trigger_auto`. A requested rate is
  rounded to the instrument's grid and the result reports the achieved rate.
- Block mode only: one acquisition per capture. Streaming and rapid-block are
  not implemented. Waveforms reach the session as analog events plus a
  `waveform.csv` artifact, never through an RPC result.
- Measure a recorded channel, live or replayed:
  `hwlab waveform measure latest --channel A` (MCP `waveform_measure`).
- Example: `examples/picoscope-fake.yaml`.

## USB packet capture

**Capture level: URB / host-controller observation, not a physical bus-level USB analyzer. Live usbmon: unverified.**

The `usbcap` plugin captures USB at URB level (see
[ADR 0025](adr/0025-usb-packet-capture.md)). **The live Linux usbmon backend has
never run on the development host** (it needs root or a readable
`/sys/kernel/debug/usb/usbmon`, plus `modprobe usbmon`). It is not a bus-level
analyzer: no SOF, tokens or handshakes.

- `HWLAB_USBCAP_SYNTH=1` adds `usbcap/synthetic` (a HID mouse, a mass-storage
  stick and a CDC serial port; SYNTHETIC).
- `HWLAB_USBCAP_SOURCES=name=/path,...` registers usbmon text captures or FIFOs
  (`cat /sys/kernel/debug/usb/usbmon/1u > mon.txt`) as offline sources.
- Live buses appear as `usbcap/usbmonN` only if the debugfs file is openable.
  The binary usbmon interface (`usbmonbin`, `/dev/usbmonN`) is read natively; it
  cannot copy the payload of some high-rate bulk endpoints (the XDS110
  EnergyTrace stream), so that payload was captured with a usbfs ioctl shim.
- Options: `speed` (replay factor for offline sources) and `snaplen` (default
  256 bytes of payload, `0` = headers only; payloads can hold secrets).
- Analyse a recorded session, live or replayed:
  `hwlab usb analyze latest [--device 1:5] [--errors]` (MCP `usb_analyze`).
- Example: `examples/usbcap-synth.yaml`.

## Modbus devices

The `modbus` plugin is a Modbus master (RTU over the generic serial layer, and
TCP). See [ADR 0022](adr/0022-modbus.md). **No real Modbus device has been
tried.**

- `export HWLAB_MODBUS_HOSTS="meter=tcp://192.168.1.60:502,rs485=rtu:/dev/ttyUSB0@19200:8E1"`
  lists the slaves (RTU default 19200 8E1); nothing is scanned.
  `HWLAB_MODBUS_FAKE=1` adds the SYNTHETIC `fake-modbus`. `hwlab devices` shows
  them as `modbus/<name>`.
- Options: `unit` (default 1), `timeout_ms` (default 1000), `retries` (reads only).
- Reads are safe; every write and `raw` need `--yes`. A capture polls read-only
  (`polls:` in the device config).
- RS-485 adapters usually switch direction in hardware or the kernel driver;
  hwlab does not toggle RTS.
- To analyse a bus you only listen to, record it with the `uart` plugin and run
  `hwlab modbus decode SESSION --mode sniff`.
- Example: `examples/modbus-fake.yaml`.

## FTDI / MPSSE / JTAG (`ftdi`)

Native Go over Linux usbfs ([ADR 0028](adr/0028-native-ftdi-mpsse-jtag.md)): no
CGO, libusb, OpenOCD or Python. Generic MPSSE JTAG with a `sipeed-bl702`
profile (compatible emulator of FT2232D). Interface 0 (channel A) is JTAG,
interface 1 (channel B) stays a UART for the `uart` plugin.

* Permissions: usbfs needs access to the device node, for example
  `/etc/udev/rules.d/99-ftdi-mpsse.rules`:
  `SUBSYSTEM=="usb", ENV{DEVTYPE}=="usb_device", ATTR{idVendor}=="0403", ATTR{idProduct}=="6010", OWNER="<user>", MODE="0600"`,
  then `sudo udevadm control --reload && sudo udevadm trigger --subsystem-match=usb --action=add`.
  `hwlab doctor` shows whether access works.
* Only interface 0 is claimed; `ftdi_sio` is detached from it alone and re-bound
  on close. The UART on interface 1 stays usable.
* `hwlab invoke ftdi/<id> JTAGTransport.selftest`, `hwlab target jtag scan`,
  `hwlab target jtag identify`; `shift_ir`/`shift_dr` need `--yes`. Examples:
  `examples/ftdi-jtag-scan.yaml` (real), `examples/ftdi-jtag-sim.yaml`
  (SYNTHETIC; `HWLAB_FTDI_SIM=1|chain|empty`).
* Device options: `profile`, `interface`, `clock_hz`, `detach_kernel_driver`,
  `latency_ms`, `timeout_ms`.
* Only the transport is claimed: no memory, flash, halt or register access.
* Hardware test (read-only, no target writes):
  `HWLAB_FTDI_HW=1 go test -tags hardware -count=1 -v ./plugins/ftdi/ -run Hardware`.
* Status: see [phase 16 results](notes/phase16-ftdi-mpsse-jtag-results.md).

## Espressif USB Serial/JTAG (`espusbjtag`)

Native Go over Linux usbfs ([ADR 0029](adr/0029-espressif-usb-jtag-riscv-dm.md)):
no CGO, libusb, OpenOCD, ESP-IDF or Python. The device (`303a:1001`, the built-in USB of the
ESP32-C6; other chips with the same peripheral are untested) exposes CDC-ACM on interfaces 0/1 and a vendor JTAG
interface 2. Only interface 2 is claimed and no kernel driver is detached, so
`/dev/ttyACMx` stays with `cdc_acm` and keeps working through the `uart` plugin.

* Permissions: `/etc/udev/rules.d/99-esp-usb-jtag.rules`:
  `SUBSYSTEM=="usb", ENV{DEVTYPE}=="usb_device", ATTR{idVendor}=="303a", ATTR{idProduct}=="1001", OWNER="<user>", MODE="0600"`,
  then `sudo udevadm control --reload && sudo udevadm trigger --subsystem-match=usb --action=add`.
  `hwlab doctor` shows whether access works.
* Device id: `espusbjtag/303a:1001:<serial>` (the serial is the chip MAC with `:` as `_`).
* `hwlab target jtag scan`, `hwlab target identify`, `hwlab target memory read --arg address=0x40800000 --arg length=16`,
  `hwlab target halt|resume|registers --yes`. Reads of the peripheral window
  `0x60000000-0x6FFFFFFF` are refused unless `allow_peripheral=true` (reads there can have side effects).
  Examples: `examples/espusbjtag-c6.yaml` (real), `examples/espusbjtag-sim.yaml`
  (SYNTHETIC; `HWLAB_ESPJTAG_SIM=1`).
* Device options: `interface`, `clock_khz` (default 1000), `timeout_ms`, `hart`.
* `halt` leaves the hart halted with the debug module active until `resume`.
* Interfaces of `303a:1001`: IF0 CDC control and IF1 CDC data (both stay bound to `cdc_acm`, never
  claimed), IF2 vendor JTAG (bulk OUT 0x02, IN 0x83, claimed by the plugin only). Identity is the USB
  serial (the chip MAC); the bus/device number is never used and may change after a reset or replug.
* Debug (CLI `hwlab target ...`, MCP `target_op`, same service and policy): `step`, `breakpoint set|clear|list`,
  `memory write` (SRAM only, `allow_memory_write=true`), `reset --arg kind=hart|system|soc` (`allow_reset=true`;
  `soc` also needs `confirm=reset-soc` and re-enumerates USB, after which the plugin rediscovers the device by
  serial, rebuilds TAP/DM and re-checks the identity), `harts` (hart 1 reports *unavailable* on the tested
  board; harts are described by what the debug module reports, never by an assumed role).
* Flash (ESP-specific, native SPI1 driver, [ADR 0030](adr/0030-esp32c6-flash-and-debug.md)): `hwlab target flash
  info|read|backup|erase|program|verify|restore`. Large data goes to artifacts (`--path`), never inline. Always
  take `backup` first (two identical reads, SHA-256, manifest). Destructive calls need `--yes`, the plugin token
  (`program-flash`, `restore-flash`, `erase-flash-region`, `erase-app-partition`, `erase-entire-chip`), the device
  options `allow_flash` / `allow_erase` (`allow_chip_erase` additionally for a whole-chip erase) and a verified
  backup; the bootloader slot, partition table and data partitions need `confirm_protected=modify-protected-region`;
  raw data (no 0xE9 image magic) needs `raw=true`. eFuses are never written. Backups are local files and must not
  be committed.
* Unplug: operations on a vanished device fail with `DEVICE_GONE` (bounded, about 8 s); the session recovers when
  the same serial reappears.
* The JTAG IDCODE (`0x0000dc25`) is reported as observed evidence; generic code does not conclude "ESP32-C6" from it.
* Examples: `examples/espusbjtag-c6-debug.yaml` (real, non-destructive; SRAM scratch restored).
* Hardware tests: `HWLAB_ESPJTAG_HW=1 go test -tags hardware -count=1 -v ./plugins/espusbjtag/ -run 'TestHWDebug'`;
  flash (writes the board after a fresh full backup): `HWLAB_ESPJTAG_FLASH=1 HWLAB_ESPJTAG_BACKUP_DIR=<dir>
  HWLAB_ESPJTAG_FW=<accept.bin> -run TestHWFlashAcceptance -timeout 25m`; unplug (needs a person):
  `HWLAB_ESPJTAG_UNPLUG=1 -run TestHWUnplugReplug`.
* Status: see [phase 18](notes/phase18-esp-usb-jtag-results.md) and [phase 19](notes/phase19-esp-flash-debug-results.md) results.

## ST-Link V2 (`stlink`)

Native Go over Linux usbfs ([ADR 0031](adr/0031-native-stlink-cortexm.md)): no CGO, libusb, OpenOCD,
STM32CubeProgrammer, st-flash or Python. Probe `0483:3748` has one vendor interface IF0 (class 0xff, no kernel
driver): bulk OUT 0x02, IN 0x81 (replies), IN 0x83 (SWO, unused). V2 only; V1/V3/V2-1 are not implemented.

* Permissions: `/etc/udev/rules.d/99-stlink.rules`:
  `SUBSYSTEM=="usb", ENV{DEVTYPE}=="usb_device", ATTR{idVendor}=="0483", ATTR{idProduct}=="3748", OWNER="<user>", MODE="0600"`,
  then `sudo udevadm control --reload && sudo udevadm trigger --subsystem-match=usb --action=add`. `hwlab doctor` shows access.
* Device id: `stlink/0483:3748:<serial>`. The serial is the raw USB string descriptor (UTF-16, units <= 0xFF);
  sysfs shows it garbled (`38c3bf...`), the plugin reads the descriptor itself. With several probes a serial is required
  and an unknown or missing one fails listing the candidates; bus/device numbers are never identity.
* Device options: `transport` (`swd` default, `jtag`), `clock_khz` (default 1800; 4000, 1800, 1200, 950, 480, 240, 125,
  100, 50, 25, 15, 5), `timeout_ms`, `reconnect_timeout_ms` (default 8000), `allow_memory_write`, `allow_reset`,
  `allow_flash`, `allow_erase`, `allow_chip_erase`.
* `hwlab target identify|halt|resume|registers|step|reset|set-speed|memory read|write|breakpoint set|clear|list|watchpoint set|clear|list --plugin stlink`.
  Memory writes are SRAM-only and need `allow_memory_write=true`; a pin reset needs `confirm=reset-pin`
  and a wired NRST (not wired on the tested setup, so never observed).
* The probe sends no reply to DEBUG_EXIT; the plugin does not wait for one.
* Only one process can hold the interface: a second open fails with `usb: interface is busy`.
* Unplug: calls fail with `DEVICE_GONE` (no stale success); the session recovers when the same serial returns and the
  DP IDCODE, CPUID and vendor word match.
* STM32 flash (`target flash info|read|backup|erase|program|verify|restore`): STM32F1 (halfword SRAM loader, page
  erase) is verified on a real STM32F1 medium-density part; STM32F4 is simulator-only; other targets (nRF52840) report
  `UNSUPPORTED_BY_ATTACHED_HW` and refuse. Destructive calls need `--yes`, a token
  (`program-flash`, `restore-flash`, `erase-flash-region`, `erase-entire-chip`), the `allow_*` options and a
  `backup=<path>` manifest of two identical reads whose affected sectors still equal the flash (or are blank).
  Option bytes, RDP and write protection are never written; protected parts are refused; MER is not implemented
  (`erase-entire-chip` is per-page and needs `allow_chip_erase`). The F1 size register may read blank; the size is then probed.
* Examples: `examples/stlink-debug.yaml` (real, non-destructive; SRAM scratch restored);
  `examples/stlink-stm32f1.yaml` + `stlink-stm32f1-accept.bin` (DESTRUCTIVE full cycle: needs a prior backup; restores the original).
* Flash hardware test: `HWLAB_STLINK_HW=1 HWLAB_STLINK_FLASH=1 [HWLAB_STLINK_EXPECT_SHA=<sha256>] go test -tags hardware -count=1 -run HWFlashAcceptance -v ./plugins/stlink/`
  (backs up twice, restores the original in a cleanup even on failure).
* Hardware tests: `HWLAB_STLINK_HW=1 [HWLAB_STLINK_SERIAL=<serial>] go test -tags hardware -count=1 -run HW -v ./plugins/stlink/`;
  unplug (needs a person): add `HWLAB_STLINK_UNPLUG=1 -run HWUnplugReplug -timeout 40m` (a unique progress-log path is printed; `HWLAB_STLINK_UNPLUG_LOG` selects an explicit operator-owned log).
* Status: [phase 20](notes/phase20-stlink-results.md), [phase 21](notes/phase21-stlink-stm32f1-results.md).

## Hardware tests

Opt-in and excluded from CI:

```sh
go test -tags=hardware ./plugins/nrf802154/ -run Hardware -v
HWLAB_CC_READ_FLASH=1 go test -tags=hardware ./plugins/ccdebugger/ -run Hardware -v  # read-only; skips without a probe
HWLAB_JLINK_TARGET=NRF52840_XXAA HWLAB_JLINK_RTT=1 \
  go test -tags=hardware ./plugins/jlink/ -run Hardware -v   # skips without a probe
HWLAB_FTDI_HW=1 go test -tags=hardware ./plugins/ftdi/ -run Hardware -v         # skips without 0403:6010
HWLAB_ESPJTAG_HW=1 go test -tags=hardware ./plugins/espusbjtag/ -run Hardware -v   # debug tests; skips without 303a:1001
HWLAB_STLINK_HW=1 go test -tags=hardware ./plugins/stlink/ -run HW -v         # skips without 0483:3748
HWLAB_MCP_HW=1 go test -tags=hardware ./internal/mcp/ -run MCPHW -v   # real MCP client -> server -> ST-Link/BL702/CC-Debugger; add HWLAB_MCP_UNPLUG=1 for the interactive unplug test
go test -tags=hardware ./plugins/saleae/ -run Hardware -v    # skips without Logic 2 + a Saleae
HWLAB_UART_PORT=ttyACM0 go test -tags=hardware ./plugins/uart/ -run Hardware -v   # writes "hwlab" to the port; skips without a port
```
