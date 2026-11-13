# ADR 0039: STM32WB flash and the coprocessor Firmware Upgrade Service over SWD

Status: accepted. Flash and every FUS operation were run on a real STM32WB5MM-DK (STM32WB55, ST-LINK/V2-1): FUS 1.1.1 → 1.2.0, BLE stack 1.10.0 deleted, 802.15.4 MAC 1.24.0 installed and started. Anti-rollback, Safeboot updates, FUS 2.x and STM32WB1x/WB10/WB15 are not implemented.
Extends [ADR 0031](0031-native-stlink-cortexm.md).
Hardware evidence: [phase 29 notes](../notes/phase29-stm32wb-fus-results.md).

## Context

The STM32WB has two cores. CPU1 (Cortex-M4) runs the application; CPU2 (Cortex-M0+) runs ST's encrypted wireless stack, which is installed by ST's Firmware Upgrade Service (FUS, AN5185). The top of the flash, from `SFSA × 4 KiB`, and the top of SRAM2a/SRAM2b are secure. The debugger cannot read, write or erase them, and the secure boundary moves whenever the FUS installs or deletes something.

Before this ADR the `stlink` plugin knew only the STM32F1 and F4 flash controllers. Changing the coprocessor needed STM32CubeProgrammer, which hwlab does not use.

## Decision

### Flash driver (`stm32/wb.go`, profile dev 0x495)

**Geometry**
- 4 KiB pages; 64-bit double-word programming (`PG`) after the `FLASH_KEYR` unlock.
- The flash size word at `0x1FFF75E0` gives the size.
- Only `0x08000000 .. SFSA×4K` is the CPU1 user area. Erase, program and backup refuse anything at or above it, and the error names the user area.

**Before each operation**
- Wait for `FLASH_SR.BSY`, `CFGBSY` and `PESD` to clear, and clear the error flags.
- Refuse to erase or program while `PWR_CR4.C2BOOT` is set. CPU2 could then access flash concurrently, and the driver deliberately does not arbitrate through HSEM (RM0434 §3.3.12). Every flash change therefore happens with CPU2 stopped and CPU1 halted, right after a system reset, and the FUS methods stage images in that state.

**Option bytes**
- Read only: RDP, WRP and PCROP are reported, never written.

**Backups**
- A backup covers the user area only. Its name and manifest record the user-area size.
- A move of SFSA makes earlier backups refuse to gate destructive operations, with a hint to take a new backup.

### FUS: the debugger plays CPU1 (`stm32/fus.go`)

hwlab runs no helper firmware. The debugger keeps CPU1 halted and performs CPU1's side of ST's transport layer (AN5289) by memory access:
1. Write the reference table, the system table and the memory-manager table into the non-secure part of SRAM2a.
2. Enable the IPCC clock and set `PWR_CR4.C2BOOT`.
3. Exchange system commands and events on IPCC channel 2, polling `C1TOC2SR`/`C2TOC1SR`, and return event buffers on channel 4.

**FUS commands sent**
- `FUS_GET_STATE`, `FUS_FW_UPGRADE`, `FUS_FW_DELETE`, `FUS_START_WS`.
- Never sent: anti-rollback, key, `FUS_LOCK_USER` or option-byte commands.

**Entering the FUS from a running wireless stack**
- Send two `FUS_GET_STATE`, as AN5185 §4 describes. The second one reboots CPU2 into the FUS.

**SRAM-loaded helper on CPU1: rejected**
- It would add a toolchain dependency.
- It would still need the debugger to survive the same resets.
- The transport is small enough to drive with a few dozen memory accesses.

### Resets that reset the debug logic: park stub plus re-attach

**What happens on hardware**
- The resets raised by the wireless stack and the FUS (the reboot into the FUS, and each step of an upgrade, delete or install) are not ordinary system resets.
- `RCC_CSR` reports `OBL` and `PIN`. The SWD link answers ST-LINK status `0x18` until `ENTER_SWD` is sent again.
- `DEMCR` is cleared, so `VC_CORERESET` cannot hold CPU1.
- CPU1 then runs whatever its flash holds. On the first attempt this was ST's demo application, which booted CPU2 itself and raced hwlab.

**Decision**
1. Every FUS operation requires CPU1's boot page to hold the hwlab **park stub**, `stm32.ParkStub`:
   - initial SP `0x20001000`;
   - every handler is `b .` at `0x08000040`.

   Parking is a separate destructive method, `fus_park_cpu1`. It needs the token `park-cpu1`, `allow_flash`/`allow_erase` and a verified backup of page 0. It is idempotent.
2. On a lost link, the engine re-attaches (`ENTER_SWD`, DP IDCODE compared with the original).
   - After every reset it halts CPU1, checks that the stub is still in page 0, re-arms `VC_CORERESET` and logs the reset cause from `RCC_CSR`.
   - If CPU1 runs anything but the stub, the operation aborts with `ErrFUSUnexpected` and CPU1 is halted.
   - At most 20 re-attaches are allowed per operation.
3. Every reset clears `C2BOOT`, so CPU2 waits until the debugger has rebuilt the tables. The debugger always owns the timing.

The stub replaces the CPU1 application. Programming a new application, or restoring the backup, is the user's next step, and the method result says so.

### Methods (`FlashProgramming`)

| Method | Danger | Gate |
|---|---|---|
| `fus_info` | read | none: the device info table and the security registers only |
| `fus_start` | dangerous | `allow_reset`, `allow_memory_write`; enters the FUS and reports versions |
| `fus_park_cpu1` | destructive | token `park-cpu1`, verified backup of page 0 |
| `fus_upgrade` | destructive | token `upgrade-fus`, `expect_fus_version`, `expect_new_fus_version`, `image_sha256`, `address`, verified backup of `[address, SFSA)` |
| `fus_delete_stack` | destructive | token `delete-wireless-stack`, `expect_stack_version` |
| `fus_install_stack` | destructive | token `install-wireless-stack`, `expect_new_stack_version`, `image_sha256`, `address`, verified backup; refused while a stack is installed |
| `fus_start_stack` | dangerous | token `start-wireless-stack`, `expect_stack_version` |

**Checks on every destructive method**
1. Pre-check the running versions against the `expect_*` arguments. A mismatch sends no command and changes nothing.
2. Check the image:
   - the ST footer magic `0xD3A12C5E`;
   - the sha256;
   - page alignment;
   - the end of the image at or below the secure start.
3. Stage the image with read-back verification.
4. Post-verify the new versions and the new SFSA, and report the boundary before and after.

**FUS upgrades**
- Only to FUS ≥ 1.2.0.
- A v1.x FUS below 1.2.0 must go to exactly 1.2.0 first (release notes STEP 6).

**Install addresses**
- The caller supplies the address, taken from ST's release notes. The release notes assume a part with only the FUS installed (SFSA 0xF4).
- With a stack still installed, a FUS image goes directly below the current secure start: `SFSA×4K − 32 KiB`.
- hwlab refuses an image that would cross the secure start. If the address is wrong, the FUS answers `IMG_NOT_FOUND`, which is non-destructive.

### Simulator

`SimWB` models the following:
- the WB flash controller, SFSA and SRRVR;
- the IPCC, the mailbox tables and the device info table (both layouts);
- the FUS command and state machine, including the multi-reset upgrade, delete and install;
- the debug-logic-resetting resets: link loss until `Reattach`, `DEMCR` cleared, CPU1 running from reset.

The engine and plugin tests cover the following:
- gates and tokens;
- the "not parked" refusal (no command sent);
- pre-checks that change nothing;
- bad images and addresses;
- the full park → start → upgrade → delete → install → start sequence;
- runs with and without the ability to re-attach.

## Consequences

**Gained**
- hwlab can back up, erase, program and verify STM32WB55 user flash.
- It can upgrade the FUS and replace the wireless stack without STM32CubeProgrammer.

**Requirements and side effects**
- FUS work needs CPU1 parked, which erases the CPU1 application. The backup taken before parking restores it.
- Backups are tied to the SFSA at the time they were taken. Every stack change needs a new backup before the next flash modification.
- The FUS's own device-ID field is not `DBGMCU_IDCODE`: the tested STM32WB55 reports `0x0496` there, while DBGMCU reads `0x495`. hwlab reports it as `fus_device_id` and identifies the chip by DBGMCU.

**Not done**
- Safeboot updates, FUS 2.x, anti-rollback and user keys: deliberately absent.
- STM32WB1x/WB10/WB15 profiles.
- The MCP server must be restarted to expose the new operations.
