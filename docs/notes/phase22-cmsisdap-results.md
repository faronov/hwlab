# Phase 22 — CMSIS-DAP on a real BL702-DAPLink (no target)

Hardware: an XT-ZB1 / BL702 board running CherryUSB BL702-DAPLink. **No SWD/JTAG
target was attached**, so everything below is probe-side. Target operations were
not run on hardware and are listed as such.

## Probe

| | |
|---|---|
| VID:PID | `d6e7:3507`, "Bouffalo" / "RV CMSIS-DAP", bcdUSB 2.00, bcdDevice 1.00, full speed |
| Serial | `012345ABCDEF`: the firmware's placeholder, **not unique per board** |
| Class | EF/02/01 composite, 1 configuration, bus powered 500 mA |
| IF0 | vendor 0xFF "RV CMSIS-DAP", bulk OUT 0x01 + IN 0x82, 64 B, no kernel driver: the DAP interface |
| IF1/IF2 | CDC ACM (interrupt IN 0x83; bulk OUT 0x04 / IN 0x85), driver `cdc_acm`, `/dev/ttyACM0` |
| Transport | CMSIS-DAP v2 bulk. No HID interface, no hidraw node |
| DAP_Info | 0x04 protocol "2.1.0"; every string (0x01-0x03, 0x05-0x09) empty, so no firmware version is reported |
| Capabilities 0xF0 | 0x33: SWD, JTAG, atomic commands, test-domain timer (1 MHz). No SWO, no UART/COM port |
| Packets | size 64, count 8 |

## Results

| Item | Result |
|---|---|
| discovery by sysfs interface string, id from USB serial | REAL_HW_VERIFIED |
| v2 bulk: DAP_Info (2000 round trips), Connect/Disconnect (SWD, JTAG), TransferConfigure, SWD_Configure, SWJ_Sequence, SWJ_Pins read, ResetTarget (executed=false) | REAL_HW_VERIFIED |
| packet limits: oversize refused host-side; undefined command times out and recovers | REAL_HW_VERIFIED |
| DAP_Transfer with no target | decoded as no-ACK and reported as failure, never success |
| clock: 100k, 400k, 1M, 2M, 4M, 10M all return status OK | REAL_HW_VERIFIED as "accepted"; the reply carries no rate |
| JTAG scan, nothing wired | 320 bits all ones, status no-device (no TAP seen) |
| CDC coexistence: IF1/IF2 stay `cdc_acm`, ttyACM0 opens while hwlab holds IF0 | REAL_HW_VERIFIED |
| second concurrent open | BUSY; REAL_HW_VERIFIED |
| 25 open/close rounds, invalid serial | REAL_HW_VERIFIED |
| two physical unplug/replug cycles | DEVICE_GONE, no hang, no stale success, same serial back after 5.3 s and 5.8 s, DAP_Info after reopen; REAL_HW_VERIFIED |
| target identify, halt/resume, registers, memory read/write, step, breakpoint, watchpoint, SYSRESETREQ, nRESET | UNSUPPORTED_BY_ATTACHED_HW on hardware; SIMULATED_ONLY (100 steps, FPB, DWT, reset kinds against `dap.Sim`) |
| v1 HID | IMPLEMENTED_UNTESTED |
| JTAG core debug, SWO, UART/COM port | NOT_IMPLEMENTED |
| flash | NOT_IMPLEMENTED by design (target-specific layers) |

Clock timing (256-bit SWJ sequence, host-measured): 100 kHz about 78 kHz effective,
400 kHz about 275 kHz; 1 MHz and up are shorter than the USB round trip
(about 95 us) and cannot be resolved. No precision is claimed. DAP_Info round
trip averaged 110-126 us over 2000 samples. Target memory throughput was not
measured: no target.

`DAP_SWJ_Pins` read without driving: usually 0x8c, 0x88 once after a JTAG scan,
and a single non-reproduced 0x00 in the first session run. It says nothing about
wiring.

## Bugs found on the real probe and fixed

1. Reads used a 1024-byte buffer and an exact-64-byte reply could hang.
2. A stale response after a timeout was read as the next answer: now drained, with ClearHalt on stall.
3. ENODEV was not mapped to DEVICE_GONE.
4. USB and Cortex-M code was duplicated; now `internal/usb`, `internal/cortexm`, `internal/adiv5`, shared with ST-Link.
5. DAP_Info 0x04 was labelled firmware version; it is the protocol version.
6. Re-opening a probe would have reported BUSY against itself.
7. The firmware leaves stale replies for the next opener; a fresh handle now drains them and any out-of-sync reply triggers a drain.
8. **`DAP_Delay(0)` wedges the firmware**: every later command times out and neither ClearHalt nor USBDEVFS_RESET recovers it, only a physical replug. It is clamped to 1 us.
9. `clock_probe` used `Delay(0)` as its baseline.

## Gaps

- Everything that needs a target; wiring below.
- Several BL702-DAPLink boards cannot be told apart by serial.
- The firmware ignores undefined commands, so one late stray reply may show up as out of sync before recovery.
- v1 HID, SWO, UART/COM, JTAG core debug, nRESET and a probe that implements DAP_ResetTarget are unvalidated.

## Wiring a target (3.3 V logic only, common GND)

- SWD: SWDIO = BL702 GPIO2, SWCLK = GPIO15, nRESET = GPIO24.
- JTAG: TDI = GPIO0, TDO = GPIO1, TMS = GPIO2, TCK = GPIO15.

Then run `examples/cmsisdap-bl702-probe.yaml`, and the hardware tests with
`HWLAB_CMSISDAP_HW=1`.

## Evidence

`testdata/real-cmsisdap-bl702/` (recorded session, replays without hardware),
`testdata/real-cmsisdap-evidence/evidence.json` (descriptors and measurements,
metadata only), `examples/cmsisdap-bl702-probe.yaml`.
