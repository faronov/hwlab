# Fixture ownership and privacy

These are intentional, bounded regression fixtures, not a directory for new
runtime sessions, full-flash backups or raw high-rate captures. Binary and JSON
extensions are not globally ignored: format-specific fixtures belong here only
after a content and privacy audit.

Bench device identifiers in retained fixtures, corresponding examples and test
expectations are fictional aliases. Numeric USB VID/PID, manufacturer defaults,
CPUID/JTAG IDs, register addresses and model names remain protocol facts.
Fictional J-Link serials in examples must be replaced with the user's selected
probe; they are not instructions to address the original bench.

## Retained evidence and transformations

* `real/nrf802154-ch15-10s.pcapng` and the radio artifact in `real-session/`
  retain recorded packet times, lengths, channel/RSSI/LQI and capture-clock
  metadata, but **every MAC frame is synthetic**. Real PANs, addresses,
  encrypted payloads and interface serials are not retained. Radio packet and
  decoded timeline events use the same synthetic frames and sanitized labels.
  These fixtures guard container/replay behaviour, not real Zigbee
  cryptographic integrity or original protocol decoding.
* `real-session/` retains a PPK2 open-circuit baseline decimated by taking
  every tenth sample per block (100 kSa/s to 10 kSa/s). Chunk starts and
  recorded clock bounds remain; sample count, stored period and metadata are
  updated. It is a bounded replay fixture, not an accuracy/calibration test.
  `real/ppk2-ampere-1s.hwcap` remains the bounded one-second 100 kSa/s format
  fixture.
* `real-saleae/` retains exported CSVs, digital edges and decoded noise events
  from floating/hand-touched lines. The raw proprietary `capture.sal` archive,
  including physical-device metadata, is local-only and ignored; it is not in
  the committed artifact index.
* Sanitized J-Link/CC-Debugger/Telink/FTDI/ESP USB-JTAG/ST-Link/CMSIS-DAP/XDS110,
  CC2340 and EnergyTrace recordings remain bounded protocol evidence. Small
  acceptance firmware images are generated test programs, not private
  full-flash dumps. Synthetic fixtures remain explicitly synthetic.

Historical hardware-result notes describe the original bench observations.
Their fictional identifiers and transformed fixtures are not original capture
identities. Raw originals, external hardware backups, TI/CCS installations,
oracle dumps and analysis data must stay outside Git.

The project has no finalized root license. Third-party notices and provenance
are tracked in `docs/licenses.md`; private backup publication is not a public
release or a final license/provenance clearance.
