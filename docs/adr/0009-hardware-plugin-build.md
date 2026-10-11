# ADR 0009: Hardware plugins and cgo (macOS)

Status: accepted

## Context

USB serial enumeration with `go.bug.st/serial/enumerator` uses cgo (IOKit) on
darwin. The core and the fake plugin should cross-compile easily.

## Decision

- The core (`hwlab`) and the `fake` plugin are **cgo-free** and are
  cross-compiled in CI with `CGO_ENABLED=0` (linux/amd64, linux/arm64,
  darwin/amd64, darwin/arm64).
- Hardware plugins that use `internal/usbserial` (`ppk2`, `nrf802154`) are
  built natively per platform; on macOS this needs cgo and Xcode command-line
  tools. CI builds them on a macOS runner.
- Hardware access is isolated behind the `usbserial` package interfaces
  (`Lister`, `Opener`, `Port`) so tests use simulated ports.

## Consequences

- Release artifacts for macOS must come from a macOS builder.
- On Linux and Windows no extra toolchain is needed beyond Go.
