# Phase 15 — CC-Debugger native programmer and debug (results)

Scope: TI CC-Debugger only. Target: one real CC Debugger (`0451:16a2`, serial `90000003`,
adapter firmware `0x5CC` rev `0x44`) with a private CC2530. The capability-by-capability
comparison is in [phase15-ccdebugger-parity-matrix.md](phase15-ccdebugger-parity-matrix.md).

## Provenance

| Source | Used for | Licence |
|---|---|---|
| TI SWRU191F (CC253x/4x User's Guide) | flash controller (FCTL/FADDR/FWDATA), DMA descriptors and triggers, debug interface commands, CHIPID/CHVER/CHIPINFO, lock-bit layout, RNG/CRC registers | public documentation |
| `cc2530-zigbee/tools/cc_debugger.py`, `cc2530_debug.py`, `check_debug_hardware.py`, `cc_tool_no_run_guard.c` | adapter framing, debug session policy, CPU-state preservation, existing Go port | BSD-3-Clause (see `native/NOTICE`) |
| cc-tool 0.26 (GPL-2.0) | behavioural reference and read-only cross-check of results on real hardware, run as a separate process | no code copied or translated |
| Observations on the real adapter and CC2530 | pipelined framing, burst write, CRC polynomial, `GET_CHIP_ID` | this project |

The BSD references contain **no** chip erase, flash program, chip-id or info-page code; those
were written from TI documentation and checked against the simulator and, where safe, the
real device.

## What was built

Native Go, `GOOS=linux`, no CGO, no libusb, no Python, no cc-tool.

- `native/target` – family geometry decode from CHIPID/CHVER/CHIPINFO0/1: flash size, page
  size, banks, RAM, USB, lock location and page, primary IEEE location. CC253x/CC254x
  decoders; further families are additional tables.
- `native/flashplan` – turns an image (`.hex` or `.bin`+base) into erase groups and 1 KiB
  program blocks: bounds, page and bank splitting, skipping all-0xFF blocks, and the
  preservation modes.
- `native/stream.go` – pipelined instruction packets and burst write.
- `native/ident.go` – identify, lock, IEEE, info page, flash read.
- `native/flash.go`, `flashproto.go` – chip erase, page erase, program, readback and CRC
  verify, config write, error and permission types.
- `plugins/ccdebugger/nativeflash.go` – plugin methods: `chip_info`, `read_lock`, `read_ieee`,
  `read`, `read_info_page`, `verify`, `erase`, `erase_pages`, `program`, `write_config`.
- `internal/app/target.go`, `internal/cli/target.go`, MCP `target_op` – generic
  `hwlab target info|identify|attach|halt|resume|reset|registers|memory read|memory write|flash read|program|verify|erase`
  over the generic capabilities, so J-Link, CMSIS-DAP, Telink and CC-Debugger share one
  service. `hwlab doctor` reports the native usbfs backend and cc-tool independently.

### Flash geometry

Nothing in programming is hard-coded to 256 KB. `ReadChipInfo` decodes the geometry from the
hardware (CC2530F256: 262144 bytes, 128 pages of 2048, 8 banks of 32 KiB, RAM 8192). The plan
and the flash operations take that geometry: addresses convert to the word address
(byte/4) in FADDR, page ranges are checked for alignment and index, blocks never cross a
page or a bank, and the lock page (the last page) is refused for page erase. Before the chip
is identified only the legacy hardware-validated read window (excluding the NV and lock
pages) is allowed.

### Preservation policy

| Mode (default first) | Lock page / secondary IEEE | Notes |
|---|---|---|
| `preserve-device-config` | never written or erased | image bytes inside the lock page are rejected |
| `preserve-mac` | lock-bit page may be rewritten (for example to set lock bits) and then needs `confirm_config`; the secondary IEEE bytes are kept: an image overlapping them is rejected and the saved bytes are re-written | secondary IEEE preserved, lock bits may change |
| `full-chip-image` | image may include it; `chip_erase=true` is allowed only here | needs `confirm_config` when config is touched |

Tiers, each independent and off by default: `allow_flash_erase`, `allow_flash_program`
(needs erase), `allow_chip_erase` (needs erase), `allow_config_write` (needs program). Every
dangerous call also needs the generic `--yes` and a plugin token: `program-flash`,
`erase-flash-pages`, `erase-entire-chip`, `write-device-config`, `lock-debug-interface` (only if
the result would debug-lock the target). Config write additionally requires a backup file
(created exclusively before any erase) and verifies afterwards. A verification failure after
programming faults the session; a read-only `verify` mismatch does not. After any flash or RAM
staging the target must be reset before `resume`; chip erase never resumes.

## Real-hardware results

| Check | Result |
|---|---|
| Doctor: native usbfs backend, cc-tool | both reported, independently |
| Identify | CC2530, CHIPID `0xA5`, rev `0x24`, CHIPINFO0 `0x44`/CHIPINFO1 `0x07`, 256 KB flash, 8 KB RAM, 2 KB pages, 8 banks, no USB, not debug-locked |
| `GET_CHIP_ID` command (`0x68`) | returns `0xA5`/`0x24`, equal to the register read |
| Lock bits | all `0xFF` (nothing locked, debug not locked) |
| Info page | byte-identical to `cc-tool -i` |
| IEEE | primary `00:12:4B:…` identical to `cc-tool -a`; secondary erased (`FF…`) on this device |
| Full flash read (native, pipelined) | 262144 bytes, SHA-256 identical to `cc-tool -r`; ~3 minutes |
| CRC verify (hardware CRC via DMA) | full 256 KB image in ~3.4 s, 213 non-blank blocks match; one flipped bit detected at block `0x1000` |
| Hardware CRC polynomial | **CRC-16 poly `0x8005`, init `0xFFFF`, MSB first** (the model first assumed CCITT `0x1021`, which the real chip disproved; fixed with hardware-derived test vectors) |
| Burst write + DMA staging | 1, 2, 7, 64, 255, 256, 1000 and 1024 bytes landed in scratch RAM correctly (FCTL never touched) |
| SFR read | SP and PSW equal to the register snapshot; peripheral SFR refused |
| XDATA scratch write | save, pattern, readback, restore, readback; registers identical before/after |
| Halt/resume on a running target | halted at a firmware PC, registers read, resumed |
| Breakpoint + step | breakpoint at the reset vector's jump target hit at that PC; one step advanced the PC |
| Target state at end of every run | running (reset + `enable_dma` + `resume`) |

Test commands (opt-in, never run by CI):

```
HWLAB_CC_NATIVE_RESET=1 HWLAB_CC_NATIVE_DEBUG=1 go test -tags hardware ./plugins/ccdebugger/native -v
```

Replay fixture: `testdata/real-ccdebugger-identify` (recorded with
`examples/ccdebugger-identify.yaml`; the unique part of the primary IEEE address is redacted;
no flash dump is stored). The 256 KB dumps used for comparison were deleted after hashing.

## Implemented but NOT hardware-verified

Chip erase, page erase, flash program (the FCTL/FWDATA write path, including DMA channel 2),
blank check after erase, lock-bit and secondary IEEE write, and the preservation logic as
applied to real flash. They are exercised by the in-process simulator (including timeouts,
busy states, disconnects, malformed replies, locked pages, chunking and verify failures) and
are never marked real-hardware-verified in the status registry. Also unverified: the location
`0x3FFE8` for the secondary IEEE (this device's value is erased), debug-locked targets, and any
chip other than the connected CC2530F256.

## Acceptance procedure for a sacrificial CC2530 (not run)

1. `hwlab target flash read` the complete flash; save the info page, lock bits and IEEE; record
   `hwlab target identify` output.
2. Program a known test image with `hwlab target flash program` (default
   `preserve-device-config`, `verify=both`) using `--config allow_flash_erase=true
   allow_flash_program=true` plus `--yes --token program-flash`.
3. `hwlab target flash verify` by readback and by CRC; reset and run the image.
4. Compare lock bits, secondary IEEE and the info page with step 1: they must be unchanged.
5. Restore the original image and verify again.
6. Only then, on that same disposable chip: page erase, chip erase (`full-chip-image`,
   `allow_chip_erase`, token `erase-entire-chip`) and config write, each followed by a read-back.
7. Store the evidence and a sanitised fixture; only then update the registry.

## Remaining differences

Versus cc-tool: no `--fast` interface-speed option, no explicit flash-size override (by
design the hardware reports it), a direct target `reset` still uses cc-tool, only the CC2530
family is decoded (CC2531/33/40/41/43/44/45, CC111x/CC251x, CC243x are not), only the
CC Debugger programmer descriptor exists (SmartRF04/05 are not), and Linux only. Erase,
program and config write are unverified on hardware.

Versus `cc2530-zigbee/tools`: everything there (status, config, PC, registers, SFR allow-list,
XDATA, CODE, flash read, halt/resume/step/breakpoints) is present and now hardware-verified in
Go, except the Python tools' no-run guard and interactive reporting, which are out of scope for
a library. The BSD tools had no programmer functions; the Go backend adds them.
