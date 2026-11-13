# Phase 29: STM32WB55 flash and FUS on hardware

Board: ST STM32WB5MM-DK. Probe: on-board ST-LINK/V2-1 `0483:374b:0669FF363730554157120838`, firmware V2J37S27, SWD at 1800 kHz, target at 3.25 V.
Chip: `DBGMCU_IDCODE 0x20016495` (STM32WB55, rev 0x2001), 1 MiB flash, RDP level 0, no WRP or PCROP.
Design: [ADR 0039](../adr/0039-stm32wb-flash-and-fus.md). Every step below used the `hwlab` CLI (`hwlab target stm32wb ...`, `hwlab target flash ...`) and no vendor tool.

## ST images

The images come from STM32CubeWB `Projects/STM32WB_Copro_Wireless_Binaries/STM32WB5x`, release notes V1.24.0. They are kept locally under `~/.hwlab/artifacts/stm32wb-copro/` and are never committed.

| Image | Bytes | sha256 |
|---|---|---|
| `stm32wb5x_FUS_fw_1_2_0.bin` | 24492 | `2712d4d23417c00af9a0fb6337c6f93118d8ffce02c61db7f4e611b74baddbfd` |
| `stm32wb5x_Mac_802_15_4_fw.bin` | 74100 | `e88ca4a1111e38b08d098a8b09cdf0813eca85113668ec6580b79d7536621c4a` |

## Results

| Step | Result |
|---|---|
| `flash info` | WB5x, SFSA 0xCB (secure from `0x080CB000`), 203 user pages |
| Backup (factory state) | 831488 B, two identical reads, sha256 `979c9335bc7cdb6fa3b7359bdfbdc9d05a0c4c804ec2529cbd9ad3acecb07ee7` |
| `fus_info` | ST BLE demo running: FUS 1.1.1, BLE stack 1.10.0 (InfoStack 0x01) |
| First `fus_start` (CPU1 not parked) | **Failed safely.** Details below. Led to the park/re-attach design |
| `fus_park_cpu1` | Page 0 replaced by the 72-byte stub, verified; the backup gate matched |
| `fus_start` | FUS 1.1.1 running, state 0x00 `NO_ERROR` |
| `fus_upgrade` to 1.2.0 at `0x080C3000` | 4 FUS resets, each with link loss, re-attach and the stub halted. Result: **FUS 1.2.0**, BLE 1.10.0 unchanged, SFSA unchanged, SRRVR `0x9e2bd000` → `0xa083d000` (secure SRAM2a from `0x20038000`) |
| `fus_delete_stack` (expect 1.10.0) | 1 FUS reset. Stack 0.0.0; **SFSA 0xCB → 0xF4** |
| Backup (FUS only) | 999424 B, two identical reads, sha256 `4860f57f9ad5111e87502990990e39496bb063d0595b9e67dc1d69f31922be84` |
| `fus_install_stack` MAC at `0x080E1000` | 2 FUS resets, then the stack booted. **MAC 1.24.0** installed; **SFSA 0xF4 → 0xE1**; secure SRAM2a from `0x20034000` |
| `fus_start_stack` | Wireless stack 1.24.0 running. `fus_info`: layout `wireless_stack`, FUS 1.2.0, stack 1.24.0, InfoStack 0x40 |

The first `fus_start` failure, as observed:
1. The ST demo was running the wireless stack.
2. The first `FUS_GET_STATE` returned 0xFF.
3. The reboot into the FUS then made every SWD access fail with ST-LINK status `0x18` for more than 1 s.

Afterwards, `DHCSR` read `0x03010000` (`C_DEBUGEN=0`), `DEMCR` read 0 and `RCC_CSR` read `0x1E004000` (OBL, PIN, BOR, SFT). CPU1 had run the demo again, and the demo had booted CPU2. Nothing was erased; the FUS state was intact.

**Final state**
- CPU1 user flash is `0x08000000 .. 0x080E1000` (900 KiB). Page 0 holds the park stub; the rest is the factory content, which the factory backup holds.
- CPU2 runs the 802.15.4 MAC 1.24.0 under FUS 1.2.0.
- `FLASH_SRRVR` reads `0x80438400` with the stack running.

## Observations

**Reset behaviour**
- Every reset raised by the wireless stack or the FUS reported `RCC_CSR` `OBL PIN` and dropped the SWD link. ST-LINK `ENTER_SWD` restored the link every time, and DP IDCODE was unchanged.
- hwlab's own `AIRCR` resets reported `PIN SFT` and kept the debug state.

**FUS device-ID field**
- The FUS device info table reports device ID `0x0496` while DBGMCU reads `0x495`. hwlab names the field `fus_device_id`.

**Not done**
- Safeboot 2.0.0 update. It is optional: ST says it can be installed over FUS 1.1.x. It performs a factory reset, so it was not run.
- Anti-rollback was not activated.
- FUS 2.2.0 was not installed.
