# esprom: Thread Border Router board (ESP32-H2 behind the S3 bridge, S3 USB-Serial/JTAG)

Date: 2026-11-24. Run by the lab's main agent with `bin/hwlab` from branch
`esprom-plugin` (first PR revision). Board: ESP Thread BR, S3 `7CDFA1FE67F0`
running zigbee-rs-hub `tbr-hub`, H2 MAC `74:4d:bd:64:1e:ce`. The hub was put
into bridge mode (`hub rom`, `lines on`) for the run and restored afterwards:
the H2 flash equals the original except the NCP's `zbnv` area, the hub is
back in its owned mode.

| Target / call | Result |
|---|---|
| `h2_bridge` `bsl identify` (`esprom/bridge-7CDFA1FE67F0`) | 1.3 s; ESP32-H2 by GET_SECURITY_INFO chip id 16; EUI-64 equal to the one the NCP reports |
| `h2_bridge` `flash info` | partition table, app descriptors and otadata decoded correctly |
| `h2_bridge` `flash backup` | 4 MiB, 210 s at 460800 Bd, two reads equal; bytes below `zbnv` identical to an older esptool backup |
| `h2_bridge` `flash program` 744,288 B app into the inactive `ota_1` | 182 sectors changed, 25 s, verified by chip MD5 |
| `h2_bridge` `flash program` 1,966,080 B into `ota_1` (runs 0x1F0000+0xB6000) | once: `block 14 of 46 at 0x00224000: FLASH_DATA: no answer` after 17 s, reported by the core as "plugin process crashed" (the plugin had returned gRPC `Unavailable`; it did not die); the same command then succeeded in 32.6 s |
| `h2_bridge` `flash restore` from the backup | 64 sectors, 15 s, whole-flash MD5 equal to the backup |
| `h2_bridge` `flash restore --arg dry_run=true` | correct `regions_changed` and `would_need` (it still needed `--yes` then) |
| `h2_bridge` `bsl reset` mode=run with console | console captured |
| `usj` (S3 after the bridge's `s3 usj`) `bsl identify` | 0.6 s via the USB-Serial/JTAG entry; ESP32-S3, 8 MB PSRAM, 4 MB Zbit flash, MAC correct |
| `usj` `bsl reset` mode=run | the CDC port re-enumerated, the plugin reopened it, the hub came back |

Follow-ups made in the PR from this run (simulator-tested only): a lost or
corrupted FLASH_DATA restarts the download at that block (resync + new
FLASH_BEGIN, bounded, `write_retries` reported and journaled); no error maps
to `Unavailable` any more (a loader timeout is `DeadlineExceeded`); restore
of one partition or address range from a verified backup; restore needs
`allow_flash` only. Dry runs keep needing `--yes`: they reset the chip into
the loader.

Not run: `usj` mode=bootloader on its own, recovery of an open `espusbjtag`
session after a USJ reset, erase operations.
