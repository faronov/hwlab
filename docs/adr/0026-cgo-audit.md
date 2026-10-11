# ADR 0026: cgo audit

Status: accepted (supersedes the cgo paragraph of ADR 0009)

## Context

The release report said that on macOS `ppk2`, `nrf802154`, `uart`, `modbus`,
`scpi` and `telink` need cgo. That was checked package by package instead of
being assumed.

## Findings

The only cgo in the dependency graph is `go.bug.st/serial/enumerator`, which on
darwin calls IOKit (`nativeGetDetailedPortsList`). `serial.Open` (the actual
serial I/O) is pure Go on linux, windows and darwin. On linux and windows the
enumerator is pure Go, so nothing needs cgo there.

| Plugin | Why it needed cgo on darwin | Classification | Now |
|---|---|---|---|
| ppk2 | `internal/usbserial.List` finds the PPK2 by USB VID/PID 1915:c00a through the enumerator | genuine: IOKit USB property lookup | still needs cgo on darwin |
| nrf802154 | same, sniffer interface VID/PID 1915:154b | genuine: IOKit USB property lookup | still needs cgo on darwin |
| uart | `ListPorts` called the enumerator to show VID/PID/product | implementation convenience | split by build tag |
| modbus | imports `plugins/uart` for the serial transport; TCP itself is `net` | accidental transitive dependency | cgo-free |
| scpi | imports `plugins/uart`; TCP itself is `net` | accidental transitive dependency | cgo-free |
| telink | imports `plugins/uart` for the SWire UART | accidental transitive dependency | cgo-free |

Everything else (core, fake, saleae, jlink, ccdebugger, can, cmsisdap,
picoscope, usbcap) never used cgo. The CC-Debugger native backend uses Linux
usbfs ioctls directly (no libusb); the CAN plugin uses raw SocketCAN sockets.

## Decision

- `plugins/uart/enum.go` (`//go:build !darwin || cgo`) keeps the enumerator
  listing with VID/PID. `plugins/uart/enum_darwin_nocgo.go`
  (`//go:build darwin && !cgo`) lists `/dev/cu.usbmodem*`, `cu.usbserial*`,
  `cu.SLAB*` and `cu.wchusbserial*` without VID/PID. `port.go` no longer
  imports the enumerator, so `modbus`, `scpi` and `telink` stop inheriting cgo.
- `ppk2` and `nrf802154` are left as they are. Their device identity is the USB
  VID/PID, and a pure-Go darwin replacement (parsing `ioreg`/`system_profiler`
  output) could not be tested here, so it would be a guess presented as support.
  This is documented, not hidden.
- `make check-cgo` builds everything with `CGO_ENABLED=0` for linux, windows and
  darwin (excluding the two plugins above on darwin).

## Consequences

- Modbus TCP, SCPI TCP and the SWire UART transport build without cgo on every
  target. They were never logically dependent on it.
- On darwin without cgo, serial-port listing lacks VID/PID; opening a port by
  name works.
- Nothing was run on macOS or Windows; these are cross-compilation results.
