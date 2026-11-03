# Phase 28: ESP32-S3 (Xtensa LX7) debug over native USB Serial/JTAG

Board: an ESP32-S3 on its built-in USB Serial/JTAG (`303a:1001`). The USB serial is sanitized as `AA:BB:CC:DD:EE:FF`; the flash backup hash is anonymized.

The board was held in **ROM download mode** (BOOT strap) for the whole phase:
- Its flash holds another application, which enumerates as a USB OTG device. While that application runs, the USB Serial/JTAG unit is gone.
- Nothing in this phase reset the SoC or touched eFuses (they were only read). The debug part wrote no flash; the flash part (phase 28b below) wrote exactly one unused 4 KiB sector and put it back.
- After every run, `lsusb` still showed `303a:1001`.

Design and sources: [ADR 0038](../adr/0038-xtensa-esp32s3.md). No OpenOCD, esptool or ESP-IDF was installed or run; hwlab's own Go code was the only tool on the wire.

## Status per capability

| Capability | Status | Evidence |
| --- | --- | --- |
| JTAG scan, 2 Xtensa TAPs, architecture detection | REAL_HW_VERIFIED | `TestHWS3Probe`, CLI `JTAGScan.scan` |
| Chip identity (IDCODE + ROM word 9) | REAL_HW_VERIFIED | `TestHWS3PluginReadOnly`, `chip_info` |
| Power-up / OCD enable / OCDID | REAL_HW_VERIFIED | core 0 |
| Halt / resume (debug interrupt, RFDO) | REAL_HW_VERIFIED | `TestHWS3HaltResume` |
| a0–a15, pc, ps, special registers | REAL_HW_VERIFIED | `TestHWS3CoreOps`, `read_registers` |
| Memory read (ROM, SRAM, RTC, aliases), fault detection + exception-SR restore | REAL_HW_VERIFIED | `TestHWS3CoreOps`, `TestHWS3PluginDebug` |
| SRAM write 32/8-bit (RMW), previous content, read-back, restore; ROM write refused | REAL_HW_VERIFIED | `TestHWS3PluginSRAMWrite` |
| Single step (ICOUNT) | REAL_HW_VERIFIED | ROM poll loop |
| IBREAK breakpoints (slot count probed: 2), wait_halt | REAL_HW_VERIFIED | breakpoint in the ROM loop |
| Halt state across separate CLI invocations (Release + re-attach) | REAL_HW_VERIFIED | CLI halt → read → resume |
| RISC-V-only methods refuse cleanly | REAL_HW_VERIFIED | CLI `reset kind=soc` (FlashProgramming was refused in phase 28a; it is implemented since 28b) |
| Core reset (`reset kind=hart`) | SIMULATED_ONLY | deliberately not run: unknown effect on the download-mode ROM |
| Core 1 (APP_CPU) debugging | IMPLEMENTED_UNTESTED | core 1 is held in reset in download mode (OCDID 0); clearly reported |
| Session close + reconnect identity | SIMULATED_ONLY | `TestS3CloseRestoresAndReconnect`; on hardware, only through CLI session ends |
| Long halts with the RTC super-watchdog enabled | NOT VERIFIED | download mode leaves every watchdog disabled |
| Flash (SPI1 over the halted core) and eFuse security decoding | see phase 28b below | |
| SoC/system reset, DBREAK | NOT_IMPLEMENTED | refused with explicit errors |

## Observed values

### Chain and power-up

The chain has 2 TAPs. Both report IDCODE `0x120034e5` (manufacturer 0x272, part 0x2003, version 1), with IR 5 + 5 at TCK 4000 kHz. TAP index 0 (nearest TDO) is core 0 (PRO_CPU): `PRID = 0x0000cdcd`.

Core 0 (TAP 0) power-up:
- PWRCTL previous value `0x80`.
- PWRSTAT `0x5f` on first read (the was-reset bits are set), then `0x0f` after clearing.
- OCDID `0x0b339fd2`.
- DSR while running: `0x80000000` (DbgModPowerOn). It reads `0x8000cc01` later, when the sticky DDR flags are set.
- DCR after EnableOCD: `0x00600001` (EnableOCD, RunStallInEn, DebugModeOutEn).

Core 1 (TAP 1):
- OCDID `0`, DSR `0`, PWRSTAT `0x5f`.
- `SYSTEM_CORE_1_CONTROL_0` (`0x600C0000`) = `0x00000004`, which means core 1 is held in reset by the ROM.

### Halt, registers and identity

**Halt**
- After `DCRSET DebugInterrupt`, DSR is `0x80200010` (PowerOn, DebugIntHost, Stopped) within 0.36 ms. DebugInterrupt clears itself in DCR.
- DSR after the context save: `0x80008411`. A later halt read `0x8000cc11`.
- DEBUGCAUSE `0x20` (debug interrupt).

**Debug level 6**
- `EPC6` (SR 182) = PC `0x40041a76`.
- `EPS6` (SR 198) = PS `0x00060520`. PS while in debug mode is `0x00060536`.

**Configuration registers**

| Register | Value |
| --- | --- |
| configid0 | `0xc2f0fffe` |
| configid1 | `0x23090f1f` |
| atomctl | `0x15` |
| vecbase | `0x40000000` |
| cpenable | `0xff` |
| windowbase | 7 |
| windowstart | `0xa8` |

**Identity**
- ROM word `[0x40001000]` = `0x00000009`. This equals `IMAGE_CHIP_ID` and esptool ≤ v4.8's `CHIP_DETECT_MAGIC_VALUE`.
- ROM `[0x40000000..]` = `1049c500 e52049d5 49f53049 00003400`.

### Step and breakpoints

- **ROM poll loop:** `0x40041a76 → 0x40041a79 → 0x40041a7c → 0x40041a76`. Each step takes about 3 ms, with cause ICOUNT.
- **IBREAK:** 2 slots (probed).
  - A breakpoint at `0x40041a7c` was hit 3 times (cause IBREAK), about 5 ms each, including stepping over the breakpoint on resume.
  - The plugin's `run_wait_ms` hit it as well.
  - After the breakpoints were cleared, `ibreakenable = 0` and `ibreaka0/1 = 0`.

### Memory

**Faulting debug load (`0x0`, `0x10000000`)**
- DSR `0x8000cc13` (ExecDone, ExecException, Stopped).
- EXCCAUSE `0x1c` (LoadProhibited); EXCVADDR is the faulting address; DEPC `0x40000280`; EPC1 unchanged.
- The plugin reports the error with the address and the class of the region.
- After resume and a second halt, EXCCAUSE, EXCVADDR and DEPC are back to their original values.

**Holes do not fault.** Reads at `0x40060000` (just past ROM) and `0x3FD00000` (just past DRAM) return `00000000` with no exception. **The memory map in the chip profile, not a bus fault, is the write guard.**

**Readable addresses**
- ROM: `0x4005FFFC`.
- DRAM: `0x3FC88000`, `0x3FCFFFFC`.
- IRAM: `0x40370000`, `0x403DFFFC`.
- RTC FAST: `0x600FE000` (needs `allow_peripheral`, as on the C6).
- RTC SLOW: `0x50000000`.

**Aliases**
- `0x3FF00000` (ROM1 data bus) has the same content as `0x40040000` (ROM1 instruction bus).
- `0x3FC88000` (SRAM1 data bus) has the same content as `0x40378000` (SRAM1 instruction bus).

**SRAM write at `0x3FC90000`**
- Original content: `4c 40 83 2a 15 2e 56 bd`.
- The word write and the byte RMW at +1 both read back correctly, and `previous_hex` matched the original.
- The original was restored and verified.
- A write to ROM was refused with InvalidArgument before any JTAG traffic.

### Watchdogs in download mode (read only)

| Register | Address | Value |
| --- | --- | --- |
| RTC_CNTL WDTCONFIG0 | `0x60008098` | `0x00013214` (EN, bit 31, clear) |
| WDTCONFIG1 | `0x6000809C` | `0x00030d40` |
| WDTWPROTECT | `0x600080B0` | `0x50d83aa1` |
| SWD_CONF | `0x600080B4` | `0x04b00000` |
| SWD_WPROTECT | `0x600080B8` | `0x8f1d312a` |
| TIMG0 CONFIG0 | `0x6001F048` | `0x0004c000` (EN clear) |
| TIMG0 WPROTECT | `0x6001F064` | `0x50d83aa1` |
| TIMG1 CONFIG0 | `0x60020048` | `0x0004c000` |
| TIMG1 WPROTECT | `0x60020064` | `0x50d83aa1` |

The values confirm that SWD_CONF is at `+0xB4`, as esptool and ESP-IDF say. OpenOCD's `esp32s3.c` uses `+0xB0`, which is the RTC WDT write-protect register.

### Timing (host USB full speed)

| Operation | Time |
| --- | --- |
| Halt | < 0.4 ms |
| Resume | ≈ 1 ms |
| Step | ≈ 3 ms |
| Breakpoint round trip | ≈ 5 ms |
| Halt + 29 registers + 11 memory reads + resume | 15–16 ms |

## Tests

Hardware tests, gated by `-tags hardware`, `HWLAB_ESPJTAG_HW=1` and `HWLAB_ESPJTAG_CHIP=s3`:

```
HWLAB_ESPJTAG_HW=1 HWLAB_ESPJTAG_CHIP=s3 go test -tags hardware ./plugins/espusbjtag/ -run TestHWS3 -v -count=1
```

- `TestHWS3Probe`
- `TestHWS3HaltResume`
- `TestHWS3CoreOps`
- `TestHWS3PluginReadOnly`
- `TestHWS3PluginDebug`
- `TestHWS3PluginSRAMWrite`

All of them pass, and the board is still in download mode afterwards.

Simulator tests (always run):

| File | Tests |
| --- | --- |
| `internal/xtensa/xtensa_test.go` | TestChainScan, TestNewDMErrors, TestExamine, TestExamineClearsWasReset, TestHaltResumeRestoresScratch, TestRegisters, TestRegisterDumpIsBatched, TestMemory, TestMemoryFaultRestoresExceptionState, TestReleaseAndReattach, TestStep, TestBreakpoints, TestReset, TestOtherChainPositions, TestEncodings |
| `plugins/espusbjtag/esp/chip_s3_test.go` | TestDetectS3, TestChipS3Map, TestQuiesceRestoreS3, TestReadSecurityS3 (28a refused; 28b decodes the S3 layout) |
| `plugins/espusbjtag/s3_test.go` | TestS3ScanAndIdentify, TestS3ChipInfo, TestS3HaltRegistersResume, TestS3MemoryRead, TestS3MemoryWrite, TestS3WriteRefusesWrongROMWord, TestS3Step, TestS3Breakpoints, TestS3Harts, TestS3RISCVOnlyMethodsRefused, TestS3ResetHart, TestS3CloseRestoresAndReconnect |

## Notes written before the flash phase (kept; outcome in phase 28b)

**SPI1**
- SPI1 is at `0x60002000` on the S3 (the C6 uses `0x60003000`), with the same register offsets (esptool `SPI_USR_OFFS 0x18` etc.).
- The existing driver can be reused once its base comes from the chip profile.
- Every access is a debug load or store by a halted core, so throughput will be below the C6's system-bus path. Batch through `esp.XtensaBus`.

**Watchdogs**
- All watchdogs are disabled in download mode (see the table above).
- The S3 Quiesce path uses RTC_CNTL `0x60008000` (+0x98/+0x9C/+0xAC/+0xB0), SWD `+0xB4`/`+0xB8` (auto-feed bit 31), and TIMG `0x6001F000`/`0x60020000`.

**ROM loader alternative over the USB CDC interface**
- The S3 ROM supports `READ_FLASH_SLOW` (0x0E, 64 bytes per command) without a stub. The fast `READ_FLASH` (0xD2) is stub-only, according to the esptool serial-protocol documentation.
- `FLASH_BEGIN`/`FLASH_DATA`, `SPI_FLASH_MD5`, `READ_REG`/`WRITE_REG` are ROM commands.
- esptool notes an S3 ROM bug: with secure boot enabled, the stub cannot be executed.
- This path needs the CDC interface, which belongs to the uart plugin. JTAG/SPI1 keeps everything inside this plugin and works while the core is halted.

## Phase 28b: flash over JTAG/SPI1

Path chosen: the existing SPI1 user-command driver, run through the halted core 0 (`esp.XtensaBus`, every register access is a debug load or store). Nothing goes over the CDC interface; no stub, no ROM loader command. Design: [ADR 0038](../adr/0038-xtensa-esp32s3.md#flash-phase-28b).

### Status per capability

| Capability | Status | Evidence |
| --- | --- | --- |
| eFuse security state (read-only) | REAL_HW_VERIFIED | `FlashProgramming.info` |
| Flash pin attach + restore (IO_MUX) | REAL_HW_VERIFIED | `TestHWS3FlashProbe` (registers re-read after Close) |
| JEDEC, SFDP size, status register, partition table | REAL_HW_VERIFIED | `TestHWS3FlashProbe`, `TestHWS3PluginFlashReadOnly` |
| read (inline, to file), verify (match, mismatch) | REAL_HW_VERIFIED | `TestHWS3PluginFlashReadOnly` |
| Full backup (two identical reads, manifest) | REAL_HW_VERIFIED | `TestHWS3PluginFlashBackup` |
| erase region, program (raw), verify, read-back on one unused sector | REAL_HW_VERIFIED | `TestHWS3PluginFlashSectorCycle` |
| Full restore (only differing sectors written, whole-flash SHA-256) | REAL_HW_VERIFIED | `TestHWS3PluginFlashSectorCycle` |
| Gate errors (token, backup, stale backup, wrong-chip image, range, alignment, reset=run, chip erase permission) | REAL_HW_VERIFIED | both plugin tests |
| Programming an ESP image (header/segments/chip id 0x0009 accepted) | SIMULATED_ONLY | `TestS3FlashProgramVerifyRestore`; on hardware only a raw pattern was written |
| erase mode=app / mode=chip | SIMULATED_ONLY | deliberately not run: the only app partition holds the user's firmware |
| Mapped-window cross-check | REAL_HW_VERIFIED (negative) | data cache off in download mode: reported as not comparable, nothing read from the window |
| Halting core 1 when it runs | SIMULATED_ONLY | `TestS3FlashHaltsOtherCore`; core 1 is held in reset in download mode |
| Encrypted / secure-boot chip refused | SIMULATED_ONLY | `TestS3FlashRefusedWhenEncrypted` (this board has neither) |
| Reset after program/restore | NOT_IMPLEMENTED (by design) | reset=none is the S3 default; reset=run/halt refused (it would boot the application, which switches off USB-Serial/JTAG on this board) |

### Observed values

**eFuse (`EFUSE_RD_REPEAT_DATA0..3` at `0x60007030`, read only):** `0x00000000 0x0000d1f5 0x00000000 0x00000000` → SPI_BOOT_CRYPT_CNT 0, SECURE_BOOT_EN 0, DIS_USB_JTAG 0 (word 0 bit 22 on the S3), DIS_PAD_JTAG 0, SOFT_DIS_JTAG 0, DIS_DOWNLOAD_MODE 0, ENABLE_SECURITY_DOWNLOAD 0 → flash writable. VDD_SPI_XPD/TIEH/FORCE = 1 (bits in word 1).

**The first JEDEC read returned `ffffff`.** In ROM download mode the flash pins are not routed to SPI1: IO_MUX `0x6000906C..0x60009084` (GPIO26..32) all read `0x00001b00`, i.e. MCU_SEL = 1 (GPIO). esptool solves this with the ROM's `SPI_ATTACH` command. hwlab instead sets MCU_SEL = 0 (SPI function, `spi_pins.h` `SPI_FUNC_NUM 0`) on SPIHD/SPIWP/SPICS0/SPICLK/SPIQ/SPID (GPIO27..32, `+0x70..+0x84`) for the duration of the call and writes the original words back on Close. SPICS1 (GPIO26, PSRAM) is not touched. After Close all six read `0x00001b00` again.

**SPI1 (`0x60002000`) at entry** (restored on Close, re-read equal): CTRL `0x002ca000`, CTRL1 `0x00000ffc`, CLOCK `0x00030103`, USER `0x80000000`, USER1 `0x5c000007`, USER2 `0x70000000`, MISC `0x00000002`.

**Flash:** JEDEC `20 40 16` (XMC), SFDP 1.6, 4 MiB (SFDP density and JEDEC capacity code agree), status register 0. Bootloader header `e9 03 02 2f`, chip id bytes `09 00`. Partition table: nvs `0x9000`/`0x6000`, phy_init `0xf000`/`0x1000`, factory app `0x10000`/`0x100000` (application data ends at `0x6b000`).

**Mapped window:** EXTMEM DCACHE_CTRL `0`, DCACHE_CTRL1 `3`, ICACHE_CTRL `0`, MMU entry 0 `0x00008b08`: the cache is off in download mode, so the window at `0x3C000000` is not compared (and `0x42000000` is never read).

**Backup** (`TestHWS3PluginFlashBackup`): 4 194 304 bytes, both reads SHA-256 `local-backup-hash…`, 190.2 s for the two reads. Kept as a local artifact (mode 0600), never committed.

**Sector cycle at `0x3ff000`** (all `0xFF` in the backup, outside every partition and protected range; the highest of 917 such sectors):
- erase region: 130 ms, verified erased;
- program 4 KiB raw pattern: 426 ms (programmed without erase, the sector was blank), verify + read-back equal;
- a further program against the now stale backup: refused (FailedPrecondition, "the backup no longer matches the flash");
- full restore: 94.5 s, 1023 sectors identical, 1 sector erased (`0x3ff000`), `flash_sha256_after` = backup SHA-256 `local-backup-hash…`.

**Afterwards:** the USB device number was unchanged (no re-enumeration), core 0 running, DSR `0x8000cc01`, still in download mode.

**Error paths seen on hardware:** verify mismatch → DataLoss (first differing address reported); read outside the 4 MiB → OutOfRange; wrong/missing confirm token and missing backup → PermissionDenied; `reset=run` → FailedPrecondition; an ESP32-C6 image (chip id `0x000d`) → FailedPrecondition before any erase; unaligned erase → InvalidArgument; backup without manifest → FailedPrecondition; `erase mode=chip` without `allow_chip_erase` → PermissionDenied.

### Throughput (USB full speed, TCK 4 MHz)

| Operation | Speed |
| --- | --- |
| read, first version (DDR reload per access group) | 37.6–39.4 KB/s |
| read, with ADDI pointer tracking in `MemBatch` | 43.4 KB/s (4 KiB), 46.2 KB/s (64 KiB) |
| full backup (2 × 4 MiB) | 43 KB/s (190 s) |
| program 4 KiB (incl. per-page status polls) | 9.4 KB/s (426 ms) |
| erase 4 KiB sector | 130 ms |
| full restore, nothing to write (compare only) | ≈ 44 KB/s (94.5 s) |

Every SPI1 register access is two DR scans (8-bit NAR + 32-bit NDR) and reads are bound by USB bandwidth (about 52 TCK per access): a 64-byte flash read is ~35 debug accesses. Faster reads would need code on the target (a RAM stub), which this phase deliberately avoided.

### Tests

Hardware (`-tags hardware`, `HWLAB_ESPJTAG_HW=1 HWLAB_ESPJTAG_CHIP=s3`):
- `TestHWS3FlashProbe` (driver level, read-only, checks the IO_MUX/SPI1 restore);
- `TestHWS3PluginFlashReadOnly` (info, read, verify, all refusal paths; writes nothing);
- `TestHWS3PluginFlashBackup` (needs `HWLAB_ESPJTAG_S3_FLASH_BACKUP=<dir>`);
- `TestHWS3PluginFlashSectorCycle` (needs `HWLAB_ESPJTAG_S3_FLASH_WRITE=1`, `HWLAB_ESPJTAG_S3_BACKUP=<backup .bin>` and, to put the backup back in the same run, `HWLAB_ESPJTAG_S3_FLASH_RESTORE=1`).

Simulator: `plugins/espusbjtag/s3flash_test.go` (TestS3FlashInfoReadBackup, TestS3FlashMappedWindow, TestS3FlashProgramVerifyRestore, TestS3FlashRefusedWhenEncrypted, TestS3FlashHaltsOtherCore), `internal/xtensa` TestMemBatch and the ADDI encodings in TestEncodings.

### Open points

- After a flash call the RTC/TIMG watchdog write-protect registers are left locked (0); the download-mode ROM had left them holding the unlock key. The watchdog configuration itself is restored. Harmless for the ROM, recorded as a state difference.
- A modifying call (erase/program/restore) leaves core 0 halted, as on the C6; `DebugProbe.resume` puts the ROM back into its download loop.
- Programming a real bootable S3 image and then booting it was not done (it would boot the application and the board would leave USB-Serial/JTAG).

