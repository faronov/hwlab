# ADR 0038: ESP32-S3 (Xtensa LX7) debug over the native USB Serial/JTAG transport

Status: accepted (debug and flash implemented and validated on a real ESP32-S3 in ROM download mode; core reset, app/chip erase and booting a programmed image are simulator-only)
Extends [ADR 0029](0029-espressif-usb-jtag-riscv-dm.md) and [ADR 0030](0030-esp32c6-flash-and-debug.md).
Hardware evidence: [phase 28 notes](../notes/phase28-esp32s3-xtensa-debug-results.md).

## Context

The `espusbjtag` plugin drives the Espressif USB Serial/JTAG unit (`303a:1001`,
interface 2) natively and, until now, assumed a RISC-V target: one TAP, a
RISC-V DTM/DM (ESP32-C6, ESP32-H2). The ESP32-S3 has the same USB unit but two
Xtensa LX7 cores. Each core has its own Xtensa TAP (IR 5 bits) with the
Xtensa On-Chip-Debug (OCD) module behind it. There is no system bus access:
memory is reached only by making a halted core execute loads and stores.

## Decision

### Generic package `internal/xtensa`, no vendor knowledge

`internal/xtensa` is analogous to `internal/riscv`.

**JTAG transport (`dm.go`)**
- TAP instructions: `NARSEL` (0x1C), `PWRCTL` (0x08), `PWRSTAT` (0x09).
- Register access is an 8-bit NAR scan (`reg<<1 | write`) followed by a 32-bit NDR scan.
- Registers used: OCDID 0x40, DCRCLR 0x42, DCRSET 0x43, DSR 0x44, DDR 0x45, DDREXEC 0x46, DIR0EXEC 0x47, DIR0 0x48.
- **Multi-TAP chains:** a `DM` is bound to one chain position. Every other TAP is loaded with BYPASS (all-ones IR), and the selected TAP's capture bits are picked out of the shifted DR by position. Chain order is the order of `jtag.Chain` (index 0 is nearest TDO).
- **Batching:** a `Queue` of reads and writes is turned into one `jtag.Batch`. The IR is re-selected only when it changes, and long queues are split at `MaxOps`. This is the same idea as the RISC-V DM batching: a halt, a 29-register dump, 11 memory reads and a resume take 15 ms on real hardware.

**Core (`core.go`)**
- **Examine:** PWRCTL wake-up, then `JTAGDEBUGUSE` (written 0→1), clear the PWRSTAT was-reset bits, `DCRSET EnableOCD`, then read OCDID/DSR/DCR. An OCDID of 0 or all-ones becomes `ErrNoDebugModule`; that is how a core held in reset reports.
- **Halt:** `DCRSET DebugInterrupt`, wait for `DSR.Stopped`, then save the context: a3 (the scratch register), PC and PS from `EPC<dl>`/`EPS<dl>`, DEBUGCAUSE, and the exception SRs that debug loads can change (EXCCAUSE, EXCVADDR, EPC1, DEPC).
- **Resume:** restore exactly those registers, then execute `RFDO`. The debug interrupt level is configuration, not a constant: the ESP32-S3 uses 6, verified on hardware.
- **Release:** restores the scratch state but leaves the core halted, as RISC-V Release does. Session close uses it.
- **Reset:** `PWRCTL.CoreReset`, optionally halting at the reset vector.

**Registers (`regs.go`)**
- Special registers are read with `RSR a3, sr` and `WSR a3, DDR`, then a DDR read; writes use the reverse path.
- a0–a15 of the current window are read through DDR. Executing through DIR0EXEC needs no RAM and no instruction cache.
- Unimplemented special registers raise an exec exception and are reported per register instead of failing the whole dump.

**Memory (`mem.go`)**
- Reads are 32-bit: `a3 = addr`, then `LDDR32.P a3` once, followed by repeated DDREXEC reads (each re-executes the load and post-increments). Writes use `SDDR32.P` through DDREXEC.
- Unaligned and 8/16-bit writes are read-modify-write of whole words.
- `MemBatch` runs a list of word reads/writes (the shape of `riscv.BusOp`) as one queue with a single exception check at the end; a fault fails the whole batch. Within a batch the scratch pointer is tracked: an access within -128..127 bytes of where the previous one left `a3` moves it with one `ADDI a3, a3, d` instead of a DDR load + `RSR` (about 12 % fewer JTAG accesses for the SPI1 driver; encoding from the ISA manual, verified on hardware by identical flash reads).
- After every block, `DSR.ExecException/ExecOverrun/ExecBusy` is checked. The sticky bits are cleared and an `*ExecError` (address, DSR, EXCCAUSE) is returned. The exception SRs are restored on resume (verified on hardware).

**Step and breakpoints (`step.go`)**
- Step: `ICOUNTLEVEL = debug level`, `ICOUNT = -2`, then resume and wait for the halt. Both registers are restored afterwards. When stepping off an enabled IBREAK, IBREAKENABLE is cleared for the step.
- The number of IBREAKA slots is **probed** (RSR of IBREAKA*n* until an exec exception), not assumed. The ESP32-S3 has 2.
- A slot that is enabled but was not installed by this session is never reused.

**Simulator (`xtsim`)**
- `xtsim` is a pure-Go TAP, debug module and core. It models PWRCTL/PWRSTAT, NAR/NDR, DIR0EXEC/DDREXEC execution of the instructions above, a windowless register file, memory regions (read-only ROM, faults outside), ICOUNT, IBREAK, core reset, a core "held in reset", and multi-TAP chains.
- Every operation has hardware-free tests (CONTRIBUTING rule 6). The plugin's USB simulator (`sim.Config.Xtensa`) clocks an `xtsim.Chain` instead of the RISC-V target.

### ESP32-S3 chip profile (`plugins/espusbjtag/esp`)

The `esp` package used to take `*riscv.DM`. It now takes a small `Bus` interface: `ReadMem` and `BusBatch` (with the existing `riscv.BusOp` shape), which `*riscv.DM` implements unchanged. This keeps the C6/H2 behaviour byte-for-byte identical, and every existing test passes unmodified. `esp.XtensaBus` adapts a halted `xtensa.Core` to the same interface.

**Identity**
- `ChipS3` is selected only when the JTAG IDCODE is `0x120034e5` **and** the ROM word at `0x40001000` is `0x9`.
- The ROM word was observed on hardware. It equals esptool ≤ v4.8's `ESP32S3ROM.CHIP_DETECT_MAGIC_VALUE = [0x9]`; current esptool no longer uses a magic value for the S3.
- The image chip id is `0x0009` (esptool `ESP32S3ROM.IMAGE_CHIP_ID = 9`).

**Memory map**
- `MemoryMap.Windows` is a list of address windows with class and executability. The source is the ESP32-S3 TRM "System and Memory" chapter, cross-checked with esptool's `MEMORY_MAP`.
- Windows: ROM 0x40000000–0x4005FFFF (instruction bus) and 0x3FF00000–0x3FF1FFFF (data bus); SRAM 0x40370000–0x403DFFFF (instruction bus) and 0x3FC88000–0x3FCFFFFF (data bus, SRAM1 + SRAM2); RTC FAST 0x600FE000–0x600FFFFF; RTC SLOW 0x50000000–0x50001FFF; flash/PSRAM-mapped 0x3C000000–0x3DFFFFFF and 0x42000000–0x43FFFFFF; peripherals 0x60000000–0x600D0FFF.
- Writes are allowed only inside one SRAM-class window. Breakpoints are allowed only in executable windows. Holes between windows read as 0 without a bus exception on hardware, so the map — not a fault — is the guard.
- C6/H2 have no `Windows`, so their behaviour is unchanged.

**Watchdogs**
- RTC_CNTL base 0x60008000: WDTCONFIG0 +0x98, WDTCONFIG1 +0x9C, FEED +0xAC, WDTWPROTECT +0xB0, SWD_CONF +0xB4, SWD_WPROTECT +0xB8. The SWD auto-feed bit is **31** on the S3 (bit 18 on C6/H2, now a per-chip field).
- TIMG0 is at 0x6001F000 and TIMG1 at 0x60020000.
- Sources: ESP-IDF `soc/esp32s3/include/soc/rtc_cntl_reg.h` and esptool `esp32s3.py`. The values were confirmed by reading them on hardware.
- **OpenOCD's `esp32s3.c` uses `SWD_CONF` offset 0xB0.** That disagrees with esptool, ESP-IDF and the hardware: 0xB0 reads the WDT write-protect key (0x50d83aa1) and 0xB4 reads a plausible SWD_CONF. hwlab uses 0xB4.

**Other profile values (flash phase)**
- SPI1: `0x60002000` (ESP-IDF `reg_base.h` `DR_REG_SPI1_BASE`, esptool `SPI_REG_BASE`), same register offsets as the C6 (`spi_mem_reg.h`). The driver base is now a profile field; C6/H2 keep `0x60003000` and their exact access sequence.
- eFuse: `EFUSE_RD_REPEAT_DATA0` at `0x60007030` (`efuse_reg.h`). SPI_BOOT_CRYPT_CNT, SECURE_BOOT_EN, DIS_PAD_JTAG, SOFT_DIS_JTAG, DIS_DOWNLOAD_MODE and ENABLE_SECURITY_DOWNLOAD sit at the same word/bit positions as on the C6/H2 (`esp_efuse_table.csv`); DIS_USB_JTAG is at word 0 bit 22 on the S3 (bit 9 on the C6/H2), a per-chip field (`EfuseDisUSBJTAG`). Read only; nothing in hwlab writes an eFuse.
- RTC super-watchdog unlock key `0x8F1D312A` (`rtc_cntl_reg.h` `RTC_CNTL_SWD_WKEY_VALUE`; read back from SWD_WPROTECT on hardware), a per-chip field (`WDTMap.SWDKey`).
- Flash pins: IO_MUX `0x60009070..0x60009084` (SPIHD/SPIWP/SPICS0/SPICLK/SPIQ/SPID = GPIO27..32, `io_mux_reg.h`, `spi_pins.h`, SPI function 0).
- MMU table `0x600C5000` (`ext_mem_defs.h`) and EXTMEM `DCACHE_CTRL`/`DCACHE_CTRL1` (`extmem_reg.h`) for the mapped-window check.

### Plugin wiring

**Architecture detection.** The session detects the architecture from the scanned chain (`isXtensaChain`): every TAP has the S3 IDCODE and the total IR equals 5 × the number of TAPs.
- If so, it builds one `xtensa.Core` per TAP and routes the following methods to the Xtensa implementation (`xtensa.go`):
  - DebugProbe: chip_info, halt, resume, read_registers, step, harts, set/clear/list_breakpoints, wait_halt, reset.
  - MemoryAccess: read, write.
- Method names and arguments are the same as on the C6. `hart` selects the core: 0 is PRO_CPU (PRID 0xCDCD on hardware), 1 is APP_CPU.
- FlashProgramming is routed to the Xtensa flash session (below).
- Everything else that needs a RISC-V DM goes through `tapIRLen`, which on an S3 chain returns an explicit `FailedPrecondition`. This covers reset kind=system and reset kind=soc. Nothing hangs and no garbage is returned.

**Differences from the C6, documented in the method descriptions:**
- Memory needs a halted core. `MemoryAccess.read` and `write` halt a running core for the access and resume it; a core that was already halted stays halted.
- `chip_info` never halts, so the chip profile (ROM word) is reported only when the core is already halted.
- `step` requires a core that is already halted.
- reset supports kind=hart only (core reset through the debug module).
- Reconnect identity is the USB serial, IDCODE, number of TAPs and core 0's OCDID. The ROM word is not part of it because reading it would need a halt.

**Session close.** Close removes this session's IBREAKs and releases every core: a3 and the exception SRs are restored, and halted cores stay halted. This works across ad-hoc CLI invocations: halt in one call, read in the next, resume in a third (verified).

### Flash (phase 28b)

**Path: JTAG/SPI1, not the ROM loader.** The existing SPI1 user-command driver (`esp/flash.go`, 64-byte transactions, completion checked in the same pipelined batch) runs unchanged through `esp.XtensaBus`, i.e. through loads/stores executed by the halted core 0. Reasons: everything stays inside this plugin and the halted-core model; no code is loaded into target RAM; the CDC interface stays with the `uart` plugin; the same gating code serves the C6 and the S3.

**Session (`xtflash.go`).** Every FlashProgramming call:
1. attaches core 0 and halts it if it runs; halts core 1 too if it is examinable and running (in download mode it is held in reset and skipped);
2. identifies the chip (IDCODE **and** ROM word), quiesces the watchdogs (`esp.Quiesce` with the S3 map), opens SPI1 at the profile base and **attaches the flash pins**;
3. reads JEDEC/SFDP/status, the eFuse security state and the partition table, and for modifying calls refuses unless the security state is known and writable;
4. on exit restores SPI1 and IO_MUX, restores the watchdogs and resumes the cores it halted — **only for read-only calls**. Erase/program/restore leave the cores halted, as on the C6.

**Flash pin attach.** In ROM download mode the flash pins are left in GPIO mode (IO_MUX MCU_SEL = 1, observed `0x1b00`), so the first JEDEC read returned `ffffff`. esptool sends the ROM `SPI_ATTACH` command for the same reason. hwlab clears MCU_SEL (bits 12–14) only on the pins that are not already at function 0, remembers the old words and writes them back on Close. The pin list is a profile field (`Chip.FlashPins`), empty for the C6/H2, so their sequence is unchanged.

**Reset.** On the S3, program/restore default to `reset=none`; an explicit `reset=run|halt` is refused with FailedPrecondition. A system reset would boot the application, which on the validation board (and on any USB-OTG application) removes USB-Serial/JTAG. The `allow_reset` check still applies after this.

**Mapped window.** `info` reports the cache state instead of comparing blindly: it reads `0x3C000000` only if the data cache is enabled, the bus is not shut for core 0, MMU entry 0 is valid and maps flash (not PSRAM). `0x42000000` is never read. In download mode the cache is off and the check is reported as not comparable.

**Image chip id.** Images must carry `0x0009` on the S3 (esptool `ESP32S3ROM.IMAGE_CHIP_ID = 9`, confirmed by the bootloader header in the board's own flash). The error message lists every supported chip.

**Gating** is the C6's, unchanged: `allow_flash` / `allow_erase` / `allow_chip_erase`, confirm tokens, `confirm_protected` for bootloader/partition table/NVS/PHY ranges, a verified full backup (two identical reads, manifest) that must still match the flash in the touched sectors, and a whole-flash SHA-256 after restore.

## Sources

No GPL code was copied; the implementation is original.

| Source | Licence | Used for |
| --- | --- | --- |
| Cadence *Xtensa Instruction Set Architecture Reference Manual* (public PDF) | documentation | RSR/WSR/XSR, L32I/S32I/L16UI/S16I/L8UI/S8I, ISYNC/RSYNC/DSYNC encodings; special-register numbers (EPC*n* = 176+*n*, EPS*n* = 192+*n*, IBREAKA 128+, IBREAKENABLE 96, ICOUNT 236, ICOUNTLEVEL 237, DEBUGCAUSE 233, …); windowed-register semantics |
| OpenOCD `src/target/xtensa/xtensa_debug_module.{h,c}`, `xtensa.{c,h}`, `xtensa_regs.h`, `espressif/esp32s3.c` | GPL-2.0 | **read as reference only** for interface facts: TAP instruction codes, NAR register numbers, PWRCTL/PWRSTAT/DCR/DSR bit positions, the OCD-only encodings `LDDR32.P` (0x0070E0), `SDDR32.P` (0x0070F0) and `RFDO` (0xF1E000), and the power-up/halt/resume order. Facts are not copyrightable; the code structure, batching, error handling and tests are hwlab's own |
| Espressif *ESP32-S3 Technical Reference Manual* | documentation | memory map, RTC_CNTL/TIMG register addresses, SYSTEM_CORE_1_CONTROL_0 |
| ESP-IDF v5.3 `components/soc/esp32s3/include/soc/`: `reg_base.h`, `spi_mem_reg.h`, `rtc_cntl_reg.h`, `timer_group_reg.h`, `efuse_reg.h`, `io_mux_reg.h`, `spi_pins.h`, `extmem_reg.h`, `ext_mem_defs.h`; `components/efuse/esp32s3/esp_efuse_table.csv` | Apache-2.0 | register bases and offsets (SPI1, RTC_CNTL, TIMG, eFuse, IO_MUX, EXTMEM, MMU), eFuse field positions, flash pin numbers and function; facts only, no code copied |
| esptool `esptool/targets/esp32s3.py`, `loader.py` | GPL-2.0+ | read for facts only: IMAGE_CHIP_ID 9, old CHIP_DETECT_MAGIC_VALUE [0x9], SPI_REG_BASE 0x60002000, RTC_CNTL WDT/SWD offsets and keys, MEMORY_MAP; ROM loader command support |
| esptool documentation, *Serial Protocol* | documentation | which ROM-loader commands exist without a stub (READ_FLASH_SLOW yes, READ_FLASH stub-only), the role of `SPI_ATTACH` |

## Verified vs not verified (summary; details in the phase note)

**Verified on a real ESP32-S3 in ROM download mode.** The board stayed enumerated and in download mode throughout.
- Chain: 2 × IDCODE 0x120034e5, IR 5+5.
- Core 0: OCDID 0x0b339fd2, PRID 0xCDCD, debug level 6.
- Core 1: held in reset (OCDID 0).
- Every TAP instruction, NAR register and DSR/PWRCTL bit used.
- Halt, resume, registers, and exception-SR restore.
- Memory reads, including map edges and aliases.
- SRAM 32/8-bit write with restore.
- Step and IBREAK (2 slots).
- ROM word 9, watchdog addresses, plugin methods through the CLI.
- Flash (phase 28b): eFuse security read, flash pin attach and restore, SPI1 at `0x60002000`, JEDEC/SFDP/partition table, read/verify, full backup (two identical reads), erase+program+verify of one unused sector, full restore with whole-flash SHA-256 equal to the backup, every gate error, ADDI encoding.

**Simulated only**
- Core reset (`reset kind=hart`), deliberately not run on hardware.
- Writing IBREAK/ICOUNT on a core whose firmware also uses them.
- Core 1 debugging (the core is held in reset in download mode).
- Long halts while the RTC super-watchdog is enabled: download mode leaves it disabled, but an application may not.
- Flash: programming an ESP image (only a raw pattern was written on hardware), erase mode=app/chip, refusal on an encrypted/secure-boot chip, halting a running core 1, the mapped-window comparison with the cache enabled.

**Not implemented**
- SoC/system reset (also after programming), data breakpoints (DBREAK), window-overflow-aware access to the registers of the callers' frames (only the current window), and the TRAX/PERF registers.

## Consequences

- C6/H2 code paths are unchanged; all earlier tests pass unmodified.
- One plugin serves both architectures. The architecture comes from the chain, not from configuration.
- A future Xtensa chip (ESP32, ESP32-S2) needs a chip profile and its IDCODE added to `isXtensaChain`. The ESP32 needs a different chain layout check, because its two cores share one chain with different IDCODEs.
- **Flash.** The JTAG/SPI1 path was chosen (see above). It reads at about 43–46 KB/s and programs at about 9 KB/s, bound by two DR scans per debug access over USB full speed. A RAM stub would be faster but was deliberately not added. The ROM loader over CDC (`READ_FLASH_SLOW` without a stub) remains an alternative for a future `uart`-side tool.
- After a flash call the watchdog write-protect registers are left locked, whereas the download-mode ROM leaves them unlocked; the watchdog configuration itself is restored.
