# Licenses and provenance audit

Audit date: hardware-validation + release-hardening milestone. **No LICENSE file
has been added**; that needs explicit owner approval.

## Conclusion

**Apache-2.0 was recommended for hwlab-owned code in the recorded audit, but
no project license is finalized.** The documented linked dependencies are
permissively licensed, the declared derived-work input (BSD-3-Clause) is
attributed, and cc-tool (GPL) is optionally executed as a separate process.
This is a preliminary provenance sanity check, not an independent proof of
source originality or a public-release license clearance.

## Linked Go dependencies (`go list -deps -test ./...`)

| Module | License |
|---|---|
| modelcontextprotocol/go-sdk, google/jsonschema-go, segmentio/asm, segmentio/encoding | MIT |
| gopkg.in/yaml.v3 | MIT (+ Apache-2.0 for the libyaml port, see its NOTICE) |
| spf13/cobra, google.golang.org/grpc, genproto googleapis/rpc | Apache-2.0 |
| spf13/pflag, yosida95/uritemplate, go.bug.st/serial, golang.org/x/{image,net,oauth2,sync,sys,text,time}, google.golang.org/protobuf | BSD-3-Clause |

Binary redistribution must carry these notices (Apache-2.0/BSD/MIT attribution);
a third-party notices bundle should be generated at release time.
Checked by reading each module's LICENSE file in the module cache; modules that
are only in `go.sum` / the module graph and not linked were not audited.

## Vendored and generated code

* `third_party/saleae/saleae.proto`: Saleae Logic 2 Automation API, **Apache-2.0**
  (LICENSE and NOTICE kept in `third_party/saleae/`); only change: `go_package`.
  Generated `plugins/saleae/internal/saleaepb` carries the upstream header
  comments and is covered by the same license.
* `proto/hwlab/**` and its generated Go: hwlab-owned.

## BSD-derived CC-Debugger code

`plugins/ccdebugger/native` is a Go port of the BSD-3-Clause reference
(`cc2530-zigbee/tools/cc_debugger.py`, `cc2530_debug.py`, copyright "cc2530-zigbee
contributors"). The BSD notice is reproduced in
`plugins/ccdebugger/native/NOTICE`. BSD-3-Clause is compatible with an Apache-2.0
project; the notice must stay with the source and binary distributions. The
reference tree is the same owner's project, so a dual statement is also possible.
Target-side commands, the flash controller, DMA and CRC behaviour follow TI's
public CC253x documentation (SWRU191F). The native erase/program/verify code
(phase 15) was written independently from that documentation, the BSD reference
and real-hardware observations.

## FTDI / MPSSE / JTAG (`plugins/ftdi`)

Written independently from FTDI AN_108/AN_135 and IEEE 1149.1. OpenOCD
(GPL-2.0) config values for the Sipeed RV-Debugger (VID:PID, channel,
`layout_init`, speed) were treated as hardware facts; no OpenOCD code was copied,
linked or executed. The Sipeed RV-Debugger-BL702 firmware (Apache-2.0) was only
read as behavioural evidence. Details: `plugins/ftdi/NOTICE`,
[ADR 0028](adr/0028-native-ftdi-mpsse-jtag.md). OpenOCD is an optional external
cross-check and is never required.

## Espressif USB Serial/JTAG and RISC-V Debug Module (`plugins/espusbjtag`, `internal/riscv`)

Written independently from the RISC-V Debug Specification 0.13 and IEEE 1149.1.
The wire protocol of Espressif's USB-JTAG bridge was read from the protocol
description in the OpenOCD driver (GPL-2.0) as documentation only; no OpenOCD
code was copied, translated, linked or executed, and the result was checked and
corrected against a real ESP32-C6. Details: `plugins/espusbjtag/NOTICE`,
[ADR 0029](adr/0029-espressif-usb-jtag-riscv-dm.md), [ADR 0030](adr/0030-esp32c6-flash-and-debug.md).
The ESP32-C6 register addresses and bit positions used by the flash, watchdog and
security code (`plugins/espusbjtag/esp`) were read from public ESP-IDF headers
(Apache-2.0) as documentation; no code was copied and the behaviour was verified
on real hardware. OpenOCD, esptool and ESP-IDF are optional external references
and are never required.

## ST-Link V2 and Cortex-M debug (`plugins/stlink`, `internal/cortexm`)

Written independently from the ARM ADIv5 / ARMv7-M documentation. The ST-Link V2 USB command
set was read from stlink-org/stlink (BSD-3-Clause) as a protocol reference and corrected against a real
probe; no code was copied. OpenOCD (GPL-2.0) was consulted for behaviour only and is neither copied nor
run. STM32 register and device-id tables come from ST reference manuals; the F1 flash loader is a hwlab-owned Thumb routine. Details: `plugins/stlink/NOTICE`,
[ADR 0031](adr/0031-native-stlink-cortexm.md). No external tool is required.

## cc-tool (GPL-2.0)

* No cc-tool source was copied or mechanically translated. Grep of the tree for
  cc-tool identifiers (`cc_253x`, `CCProgrammer`, `cc_debug_interface`,
  `usb_device.cpp`) finds nothing; the only mentions are statements that nothing
  is derived from it (`native/protocol.go`) and the registry.
* `plugins/ccdebugger/tool.go` runs a user-installed `cc-tool` via `os/exec` as a
  separate process (`exec.LookPath("cc-tool")`); nothing is linked or bundled.
  Optional external tool.
* Limitation of this audit: it relies on the recorded development process (the
  native backend was written from the BSD reference and TI documents) and on the
  absence of upstream identifiers; no automated code-similarity scan against
  cc-tool was run. Do one before a public release if provenance must be proven.

## External vendor software (not relicensed, not redistributed)

* SEGGER J-Link software (`JLinkExe`, proprietary): run as an external tool by `jlink-segger` only. `jlink-native` does not use it at runtime (only as a behavioural oracle in tests) and incorporates no GPL J-Link code (libjaylink/OpenOCD); see [ADR 0034](adr/0034-native-jlink.md).
* Saleae Logic 2 (proprietary application): reached over its Automation gRPC API.
* PicoSDK (proprietary): **not used at all yet**; the picoscope plugin has a
  simulated backend and no PicoSDK binding is vendored.
* nRF Sniffer firmware, tshark, cc-tool: user-installed, external.

## Other

* Retained bench fixtures use fictional device identifiers. Live-network
  packet bytes are replaced with synthetic frames, and the large PPK2
  baseline fixture is decimated. Raw Saleae capture stays local and is not
  committed. Scope and limitations are documented in `testdata/README.md`.
* CherryUSB/BL702 firmware was a device-side behavioural reference, not
  vendored host source. TI XDS110/EnergyTrace work records the TI tools as
  external behavioural oracles, not source inputs; see
  `docs/xds110-energytrace-protocol.md`. Native J-Link provenance is recorded
  in ADR 0034. These recorded development claims need an independent review
  before public release if originality must be established.
* No Python code is part of the runtime.
