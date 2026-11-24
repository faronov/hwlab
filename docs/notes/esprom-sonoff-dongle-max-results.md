# esprom: SONOFF Dongle Max (ESP32) read-only run

Date: 2026-11-24. Host: macOS (x86_64), `bin/hwlab` with the `esprom` plugin
from branch `esprom-plugin`. Board: SONOFF Dongle Max (Dongle-M), CP2102N
`10c4:ea60` "SONOFF Dongle Max MG24" at `/dev/cu.usbserial-14730`. Only
read-only commands and line resets were sent: no FLASH_BEGIN/DATA,
ERASE_REGION, ERASE_FLASH or WRITE_REG outside the SPI1 user registers used
for the JEDEC id (saved and put back). The full artifact README with the
decoded layout is local (`~/.hwlab/artifacts/sonoff-dongle-max/README.md`);
the backup holds credentials and is not committed.

| Step | Result |
|---|---|
| console before any entry (port opened with DTR=RTS=0) | 0 bytes in 5 s and in another 20 s: the stock firmware is silent; opening the port did not reset the chip |
| `hwlab devices` | one device `esprom/cp210x-ba653de3e67bf11194bff77a9507fdd7`, node `cu.usbserial-14730`, `alt_ports` `cu.SLAB_USBtoUART17`; nothing opened |
| `bsl identify` (2.0 s) | classic entry (TIOCMSET atomic), ROM `ets Jul 29 2019`, `boot:0x2 (DOWNLOAD_BOOT…)`, SYNC after 3 attempts; ROM word 0x00F01D83 → ESP32; eFuses: ESP32-D0WDR2-V3, v3.1, MAC 70:4b:ca:f7:46:f4, dual core, BT, 240 MHz, embedded PSRAM, no flash encryption/secure boot; stub esp-flasher-stub v2.0.0 (OHAI), 921600 Bd; JEDEC 85 20 18 (Puya) → 16 MiB |
| `flash backup` (6 min 51 s) | 16 MiB read twice with READ_FLASH (stub MD5 per 256 KiB), SHA-256 `6024d1b420a5113ead942b14d8c3f77ef68bd4df513ab2a3a9458de4caaf3240` both times, chip SPI_FLASH_MD5 `1d3bb6eb8f59743a78ade22bc030835e` equal to the host MD5; 409 s for both reads |
| independent check of the bytes | the bootloader (0x1000) and application (ota_0) images each carry an appended SHA-256 that matches the bytes read |
| `flash info` | partition table: nvs 0x1C000/256 KiB, otadata 0x5C000, phy_init 0x5E000, ota_0 0x60000/6 MiB, ota_1 0x660000/6 MiB (empty), files (littlefs) 0xC60000/0x3A0000; protected: bootloader slot, table, nvs, otadata, phy_init, files; app `Dongle-M` version `1`, ESP-IDF v5.2.1-dirty, Feb 12 2026; otadata selects ota_0 |
| `flash verify` of the 32 KiB bootloader slot against the backup | SPI_FLASH_MD5 match |
| `bsl reset --arg mode=bootloader` | DOWNLOAD_BOOT, SYNC after 4 attempts, chip detected |
| `bsl reset --arg console_ms=8000` | line-only run reset (DTR=0 RTS=1 100 ms); ROM `boot:0x12 (SPI_FAST_FLASH_BOOT)`, `configsip: 153911750` (SPI pins remapped by eFuse; SPI_ATTACH(0) honoured it), bootloader entry 0x4008066c; the bootloader and app print nothing else |

Not run on hardware: the ESP32-H2 behind the S3 bridge, USB-Serial/JTAG entry
and reset, ESP32-S3/C6/H2 identity decode, erase/program/restore, other rates
and bridges (simulator only).
