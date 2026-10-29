# Bouffalo BL702 USB ISP: hardware results

Host-local device paths and the chip MAC are anonymized.

Board: XT-ZB1 DevKit (BL702, 1 MiB flash), right micro-USB (chip USB) with
BOOT held at plug-in; macOS, `bflb/cu.usbmodemEXAMPLE`. Helper:
`eflash_loader_32m.bin` from `bflb-mcu-tool` (59,200-byte segment).

## Results

| Step | Result |
|---|---|
| ROM identify (no helper) | ROM 0x07020001, chip id/MAC read and CRC-checked, one transient FL after the handshake retried |
| Helper load + start | ~0.5 s; a resident helper is detected and reused |
| Flash id | JEDEC 0xc22014, 1 MiB, 4 KiB sectors |
| Backup | 1 MiB read twice in 3.56 s, both equal to the chip-side SHA-256 |
| Program | 205,392-byte `bl702-sensor.flash.bin` at 0: erase 1.93 s, write 1.86 s, chip SHA equal |
| Rest of flash | 0x34000..end equal to the backup |
| Verify | match, also after a power cycle |
| Boot | CH340 log: `BL702 boot`, `logger ready`, `executor ready`, `sensor task` |
| Reset | chip reboots into the firmware; macOS keeps the ISP CDC device listed (no answers) until replug |

Not run on hardware: `restore`, the UART path.

## Helper loses USB frames of 64 bytes or more

The first `program` erased 0x0..0x33fff and then got no answer to the first
2048-byte write. Probes with the helper:

* frames of 56..63 bytes (one short USB packet): 3000 writes, ~105 KiB/s,
  readback equal;
* a 64-byte frame (exactly one full packet): failed on the second write;
* multi-packet frames: sometimes worked at first (up to ~368 bytes) and then
  degraded until 65 bytes failed; pacing between packets did not help.

After a loss the helper waits for the rest of the frame until its 2 s receive
timeout and leaves partial data in its ring, so the next frames get FL0103.
The disassembly (`USB_Rework_IRQHandler` → `USB_CMD_RB_Memory_Push`, 3 KiB
ring) did not show the cause. The ROM's own USB loads 4080-byte segments
without a loss, and 2 KiB reads (IN) are fine.

Work-around: `isp.USBWriteChunk = 55` (4-byte header + 4-byte address + 55
bytes = 63). The simulator drops host writes of 64 bytes or more while the
helper runs, so the tests fail if the chunking is removed.

## Other notes

* The ROM in handshake mode ignores the boot-info probe; after the handshake
  the first command may get one FL, which the client retries.
* A reopened port can leave the helper in command or handshake mode; connect
  handles both.
* A 2200-byte burst of zeros blocks the USB write; do not use it to resync.
