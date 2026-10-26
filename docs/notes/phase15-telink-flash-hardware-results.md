# Phase 15: Telink flash modification on real hardware

Board: TB-04-style TLSR825x (SWire chip id 0x5562, rev 0x02), GD 512 KiB flash
(JEDEC 0xc86013), flash unique id `0011223344556677`. Programmer: pvvx SWire
firmware 0x0008 on a CH340 (`/dev/cu.usbserial-EXAMPLE`, 230400 baud). Host:
macOS. The board runs a zigbee-rs TLSR8258 router image; both factory sectors
(0x76000-0x78000) and 0x78000-0x80000 are blank, so the layout is
`tb04-512k-nofactory`.

The backup itself is a local artifact (complete firmware + board identity) and
is not committed.

## Results

| Step | Command (abridged; `--device telink/pgm --yes --pre DebugProbe.halt --post DebugProbe.resume --config layout=tb04-512k-nofactory`) | Result |
|---|---|---|
| identify | `target identify` | 0x5562 TLSR825x, JEDEC 0xc86013, uid 0011223344556677 |
| layout | `target flash info` | `tb04-512k-nofactory` verified: size matches, all 8 KiB factory bytes 0xFF |
| backup | `target flash backup --arg dir=…` | two identical reads, sha256 `local-backup-hash…`, 67 s; equals an earlier independent backup taken with `flash read` |
| no backup | `flash program … --arg address=0x10000` | refused by the method schema (`backup` required) |
| CPU running | `flash program` without `--pre DebugProbe.halt` | FailedPrecondition: the CPU is not halted |
| factory sector | program at 0x76000 | PermissionDenied (factory-eui64) |
| undescribed | program at 0x78000 | PermissionDenied (not described by the layout) |
| journal | program at 0x74000 without `allow_journals` | PermissionDenied (security-journal) |
| layout auto | program with `layout=auto` | PermissionDenied (map not verified) |
| layout tb04-512k | program with the factory layout | PermissionDenied (factory-eui64 reads blank) |
| identical re-program | program sector 0x10000 with its own content from the backup | 16 pages written, CRC + 4096 B readback OK; full flash still equals the backup |
| boot | `target reset` (hard), `attach mode=soft`, `resume` | PC moves through 0x4xxxx: the firmware runs |
| modify + restore | program a pattern into blank sector 0x6f000, then `flash restore --path BACKUP --arg skip_journals=true` | 3 sectors differed: 0x6f000 restored, 2 security-journal sectors kept (the router had advanced its frame-counter reservation after the reset); 0x00000-0x70000 and 0x78000-0x80000 equal the backup |
| restore without journal choice | `flash restore --path BACKUP` | PermissionDenied: sector 0x74000 differs, nothing written |
| standalone erase | `flash erase --arg address=0x6f000 --arg length=4096 --arg backup=…` | 1 sector erased; app and tail regions still equal the backup |
| stress | 150 × `target halt --post DebugProbe.resume` | 150/150, no link recoveries needed |

Flash block protection was set on the board and was cleared by the first
program (`flash_protection_cleared: true`, persistent).

## Transport finding

Before the transport changes, 1 of 300 halt sessions failed on the first
programmer answer after opening the port (`bad header: ff`). The cause is not
proven (likely a stray byte from the CH340 right after open). Mitigations: 50 ms
settle and input flush after open, header resync over at most 64 stray bytes,
and bounded retries of idempotent reads after draining the line. After them:
150/150.

## Not validated

Programming a whole application image, restore with `allow_journals`, memory
write, single-step and breakpoints.
