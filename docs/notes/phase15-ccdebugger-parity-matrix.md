# CC-Debugger capability parity matrix

Phase 15. Native Go backend (`hwlab` → Linux usbfs → TI CC Debugger `0451:16a2` → CC2530)
against `cc-tool` 0.26 and the BSD-3-Clause `cc2530-zigbee/tools` scripts. Statuses are
deliberately strict: a method name existing is not parity.

Legend for the *hwlab native* and *real HW* columns:

- **HW** – ran on the connected CC2530 and the result was checked (against cc-tool where marked).
- **sim** – implemented and tested against the in-process adapter/target simulator only. Never
  counted as hardware-verified.
- **no** – not implemented.
- **n/a** – not applicable to that tool or chip.

The connected CC2530 holds private firmware and an IEEE address. Erase, program and
lock/config writes were therefore **not** run; those rows stay **sim**.

## Adapter

| Capability | cc-tool | cc2530-zigbee/tools | hwlab native | Real HW |
|---|---|---|---|---|
| Enumerate debugger (VID/PID `0451:16a2`) | yes (also SmartRF04/05) | yes | yes | HW |
| Adapter info (target id, state) | partial (printed by `-t`) | yes | yes | HW |
| Adapter firmware version/revision | partial | yes | yes (`fw 0x5CC`, `rev 0x44`) | HW |
| Debug interface speed | yes (`--fast`) | no | no (adapter default; not configurable) | n/a |

## Target

| Capability | cc-tool | cc2530-zigbee/tools | hwlab native | Real HW |
|---|---|---|---|---|
| Chip id / identify from hardware | yes (`-t`) | adapter target id only | yes: CHIPID/CHVER/CHIPINFO registers, `GET_CHIP_ID` cross-check; a requested name is never trusted | HW |
| Revision | yes | no | yes (`0x24`) | HW |
| Flash size | yes (`-s` override) | fixed 256 KB | yes, decoded from CHIPINFO0 (262144); no override by design | HW |
| RAM size | yes | no | yes (8192) | HW |
| Page size, bank count, lock location | internal | no | yes (2048, 8, `0x3FFF0`, page 127) | HW |
| Lock state | yes | no | yes (16-byte structure decoded: debug lock, locked pages) | HW |
| USB capability (CC2531/2540…) | internal | no | yes (flag from CHIPINFO1) | HW (CC2530: no USB) |

## Debug

| Capability | cc-tool | cc2530-zigbee/tools | hwlab native | Real HW |
|---|---|---|---|---|
| Attach / reset-attach | internal | yes | yes (`attach`, explicit policy) | HW |
| Debug status | internal | yes | yes | HW |
| Debug config (DMA_PAUSE, timers…) | internal | yes | yes, plus one-shot `enable_dma` | HW |
| PC | no | yes | yes | HW |
| Register snapshot (A, B, SP, PSW, DPTRs, R0–R7, bank) | no | yes (double read, compared) | yes (same, CPU state preserved) | HW |
| Halt | no | yes | yes (`allow_cpu_control`) | HW (running target) |
| Resume | no | yes | yes; refused until reset after RAM/flash staging | HW |
| Single step | no | yes | yes | HW |
| Hardware breakpoints (4) | no | yes | yes (`allow_breakpoints`) | HW |
| SFR read (reviewed core SFRs only) | no | yes | yes, allow-list; peripheral SFRs refused | HW |
| XDATA read (RAM `0..0x1FFF`) | no | yes | yes | HW |
| XDATA write | no | yes (below `0x1E00`) | yes (same limit, verified write) | HW (scratch, restored) |
| CODE read | no | yes | yes | HW |
| Physical banked flash read | partial | yes | yes (any bank, FMAP restored and checked) | HW |

## Programmer

| Capability | cc-tool | cc2530-zigbee/tools | hwlab native | Real HW |
|---|---|---|---|---|
| Full flash read | yes (`-r`) | yes (256 KB) | yes (`FlashProgramming.read`, range or whole chip, atomic file write) | HW, byte-identical to cc-tool |
| Info page read | yes (`-i`) | no | yes (`read_info_page`) | HW, byte-identical to cc-tool |
| Primary/secondary IEEE read | yes (`-a`) | no | yes (`read_ieee`, source of each reported) | HW (primary identical to cc-tool; secondary erased on this device) |
| Chip erase | yes (`-e`) | **no** | yes (`erase`, `allow_chip_erase` + `erase-entire-chip`) | **sim**, unverified |
| Page erase | internal | no | yes (`erase_pages`, `allow_flash_erase` + `erase-flash-pages`; lock page refused; blank check) | **sim**, unverified |
| Flash program | yes (`-w`) | **no** | yes (`program`, ranges or whole image, all-0xFF skipped, bank/page aware) | **sim**, unverified |
| Verify by readback | yes (`-v r`) | no | yes | HW (read-only compare of a real dump) |
| Verify by CRC | yes (`-v c`) | no | yes, CC2530 hardware CRC-16 via DMA (poly 0x8005, init 0xFFFF) | HW (256 KB in 3 s, corrupted block detected) |
| Blank check | internal | no | yes (after page erase) | **sim** |
| Program + verify | yes | no | yes; default `readback`, mode reported; success is never reported without verify unless `verify=none` | **sim** |
| Image bounds validation | yes | no | yes (flash size, page and bank, preflight fails closed) | HW-sized, plan logic **sim** |
| Programming data path (burst → DMA → RAM) | internal | no | yes | HW (RAM-only, 1–1024 bytes) |
| Flash-controller write path (FCTL/FWDATA DMA) | internal | no | yes | **sim**, unverified |

## Configuration

| Capability | cc-tool | cc2530-zigbee/tools | hwlab native | Real HW |
|---|---|---|---|---|
| IEEE preserved across programming | yes (`-p`) | no | yes: default `preserve-device-config` never touches the lock page; `preserve-mac` and `full-chip-image` are explicit | **sim** (policy), HW (location read) |
| Lock data read | partial | no | yes | HW |
| Lock data interpretation | partial | no | yes (page n lock bits, DBGLOCK = bit 127) | HW |
| Lock / config write | yes (`-l`, `-b`) | no | yes, separate tier: backup file (O_EXCL) → modify → write → verify; `write-device-config`, plus `lock-debug-interface` when it would debug-lock | **sim**, unverified |
| Flash size detect/override | yes (`-s`) | no | detect from hardware only | HW (detect) |
| Target reset to run without debug | yes (`--reset`) | yes | via `attach` + `resume`; direct `reset` is cc-tool only | HW (attach/resume) |

## Architecture

```
plugins/ccdebugger/native/
  usbfs_linux.go     transport: Linux usbfs ioctls, no libusb
  protocol.go        adapter and debug-interface commands, status decoding
  debugger.go        session policy, CPU control, memory, registers
  stream.go          pipelined instruction packets, burst write
  ident.go           identification, lock, IEEE, flash and info page reads
  flash.go           erase, program, verify, CRC, config write
  flashproto.go      shared constants, options, errors, verify modes
  target/            CC253x/CC254x geometry decode (pure, no I/O)
  flashplan/         image → erase/program plan, preservation modes (pure)
```

Other TI families (CC251x/CC111x, CC243x) and the SmartRF04/05 programmers would be added as
further `target` decoders and USB descriptors; nothing in the transport or plan is CC2530
specific. They are not implemented.
