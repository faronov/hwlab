# ADR 0029: native Espressif USB Serial/JTAG transport and a generic RISC-V Debug Module

Status: accepted. Extended by [ADR 0030](0030-esp32c6-flash-and-debug.md): reset, memory write, step, breakpoints and flash are now implemented; the "Reset is not implemented" and read-only statements below describe the first phase only.

## Context

An ESP32-C6 exposes a built-in USB Serial/JTAG device (`303a:1001`, "USB
JTAG/serial debug unit", serial = chip MAC). It has three USB interfaces: a
CDC-ACM pair (0 control, 1 data) and a vendor-class JTAG interface 2 (bulk OUT
0x02, bulk IN 0x83, 64 B). It is not CMSIS-DAP, not FTDI and not a generic UART.
The target is RISC-V, so the CPU debug protocol (RISC-V Debug Module over JTAG)
must be independent of this USB bridge.

## Decision

1. Layering, each layer testable alone:
   `internal/usb` (Linux usbfs, shared with ccdebugger/ftdi) ->
   `plugins/espusbjtag/espusb` (Espressif transport, a `jtag.Port`) ->
   `internal/jtag` (TAP state machine, IR/DR, IDCODE, chain; shared with ftdi) ->
   `internal/riscv` (DTM + Debug Module 0.13: halt, resume, registers, system bus
   access) -> plugin capabilities.
   `internal/riscv` depends only on `jtag.Driver`, so another JTAG transport
   (for example the FTDI MPSSE port) can reuse it. The generic `usb`, `jtag` and
   `Port` code was moved out of `plugins/ftdi` for this; ftdi behaviour is
   unchanged and its tests stay green.
2. Native Go, no CGO, no libusb, no OpenOCD, no ESP-IDF, no Python at run time.
3. Only interface 2 is claimed and no kernel driver is detached. `cdc_acm`
   keeps interfaces 0/1, so `/dev/ttyACMx` stays usable through the `uart`
   plugin; no serial stack is duplicated. Interfaces 0/1 are refused by the
   plugin even if requested. The device lists `companion_cdc_*` properties.
4. The wire protocol (capability descriptor, vendor control requests, 4-bit
   command stream with run-length repeat and flush) was implemented from a
   protocol description treated as documentation only, then verified on real
   hardware. The driver never issues the SETIO or SET_CHIPID requests and never
   changes the USB configuration. Because the IN FIFO is tiny, at most 480
   captured bits are outstanding before a flush and read.
5. Capabilities: `JTAGTransport` (`info`, `tap_reset`, `shift_ir`, `shift_dr`),
   `JTAGScan`, `TargetIdentify`, `DebugProbe` (`info`, `chip_info`, `halt`,
   `resume`, `read_registers`) and `MemoryAccess.read`. Not declared: reset,
   write, flash, attach, breakpoints. `shift_ir`, `shift_dr`, `halt`, `resume` and
   `read_registers` are CONFIRM; `read` and `chip_info` are SAFE.
6. Memory reads use system bus access (32-bit). They are capped at 4096 B per
   call and refuse the peripheral window `0x60000000-0x6FFFFFFF` unless
   `allow_peripheral=true`, because a read there can have side effects.
7. `read_registers` halts a running hart to read it and restores it, unless
   `keep_halted`. The DM is deactivated on close only if this session activated
   it and the hart is running; a halted hart or a pre-active DM is left alone.
8. Chip naming is a hint from the IDCODE only (`0x0000dc25` -> ESP32-C6,
   labeled "inferred"); the USB descriptors do not name the chip.
9. Reset is not implemented: `ndmreset` may reset the USB peripheral and drop
   the session, which cannot be handled safely yet.
10. `HWLAB_ESPJTAG_SIM=1` adds a SYNTHETIC device (`internal/riscv/rvsim`, a
    bit-level TAP + DTM + DM model) for CI; everything it returns is marked
    `synthetic` and `target_verified: false`.

## Clean implementation / licenses

Written from the RISC-V Debug Specification 0.13, IEEE 1149.1 and the
protocol description above. The OpenOCD driver (GPL-2.0) was read as
documentation only; no code was copied or translated. See
`plugins/espusbjtag/NOTICE` and `docs/licenses.md`.

## Consequences

* Read-only CPU debug of an ESP32-C6 works without any external tool.
* The RISC-V layer is reusable but only validated over this transport.
* Reads are slow (about 2.5 ms per DMI op, so a few seconds for 4 KiB).
* Hart 1 (probably the LP core), reset, writes and flash are future work.
