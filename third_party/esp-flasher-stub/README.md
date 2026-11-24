# esp-flasher-stub (vendored prebuilt images)

Prebuilt RAM stub images of Espressif's **esp-flasher-stub**, loaded by the
`esprom` plugin into an ESP chip's RAM through the ROM serial bootloader
(MEM_BEGIN / MEM_DATA / MEM_END) to get the fast stub-only commands
(READ_FLASH, ERASE_REGION, 16 KiB FLASH_DATA blocks, raw-MD5 SPI_FLASH_MD5).

* Upstream: https://github.com/espressif/esp-flasher-stub
* License: **Apache-2.0 OR MIT** at the user's option (upstream README
  "License" section; both texts are kept here unchanged as `LICENSE-APACHE`
  and `LICENSE-MIT`; source files carry `SPDX-License-Identifier: Apache-2.0 OR MIT`).
  The images also contain code from the `esp-stub-lib` submodule
  (https://github.com/espressif/esp-stub-lib, commit
  `91e85dc3b84abdaf398773170c237e7056ce0906`), released under the same
  Apache-2.0 OR MIT choice.
* This is NOT the legacy esptool flasher stub (`esptool-legacy-flasher-stub`,
  GPL-2.0-or-later). No esptool code or esptool-shipped stub binary is
  included in hwlab.

## Provenance

Release **v2.0.0** (tag object `00dbbfc9ea092db51a482d3f468734ed46a83f0c`,
commit `78732829f86561c86f85a90594f553f209e48f38`, tagged 2026-11-13).
Files downloaded unchanged from the release assets
(`https://github.com/espressif/esp-flasher-stub/releases/download/v2.0.0/<chip>.json`);
the SHA-256 values equal the digests GitHub publishes for those assets.

| File | Size (bytes) | SHA-256 |
|---|---:|---|
| `esp32.json` | 9032 | `a6ff782c69ef16f7459cb12de5c4ea55c97457957c8cc7aee669f91627b062b8` |
| `esp32s3.json` | 19739 | `9a8ad53a62a9db1164342d48fba2810a75bf0d1069d5973d8d521082a5756129` |
| `esp32c6.json` | 10673 | `12f5f0d9aef5bfaafb67f1ddac4fa0263e8a9023e0c8bff181d2167976ce2cf6` |
| `esp32h2.json` | 8336 | `8b1af9dbf666d6f53ebc3a3ef554b7904f892b02537978b36e6724627970fbd4` |

`TestStubDigestsAndUpload` (`plugins/esprom/rom/rom_test.go`) checks these
digests, so an edited file fails the tests.

## Format and how hwlab uses it

Each JSON holds `entry`, `text`/`text_start`, `data`/`data_start` (base64,
absolute load addresses) and `bss_start`. The ESP32-S3 and ESP32-C6 files
also embed optional plugins (`plugins`: NAND, SPI and SDIO transports with
`plugin_start_offset`/`plugin_start_slots`); hwlab uploads only the base
`text` and `data` segments, so `plugin_start[]` stays zero and no plugin
runs. After MEM_END the stub answers with the SLIP frame `OHAI`.

Updating: download the new release assets, replace the files, update this
table and the digests in `plugins/esprom/rom/rom_test.go`, and re-run the
simulator tests and a real read-only backup.
