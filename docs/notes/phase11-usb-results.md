# Phase 11 — USB analyzer results

**Capture level: URB / host-controller observation; not a physical bus-level USB analyzer; live usbmon unverified.**

Hardware: **no USB analyzer, and no usbmon access** (no `/dev/usbmon*`, debugfs
denied, not root, no sudo). The live backend is written but has never run. This
phase is synthetic-, file- and replay-validated.

| Item | Result |
|---|---|
| usbmon mmapped header encode/decode (link type 220) | tests |
| usbmon text parser (setup, placeholders, status:interval, iso, data, 32-bit µs unwrap) and its inverse | tests |
| descriptors: device, configuration (interfaces, endpoints, truncation flag), header-only probe, SET_ADDRESS handling | tests |
| class decoders: HID, CDC (line coding, DTR/RTS, ASCII data), DFU, MSC CBW/CSW with SCSI opcode names | tests |
| analysis: S/C pairing, unpaired completions, still-pending URBs, error counts, per-endpoint latency | tests |
| plugin: synthetic, text file (EOF + bad lines), FIFO cancel, missing file, snaplen, peek during capture | tests, race detector on |
| pcapng artifact read back with `pcap.ReadNG`; decoded by tshark as `usb` | tests (tshark present) |
| analysis exposure: `hwlab usb analyze`, MCP `usb_analyze`, `app.AnalyzeUSB` (filters, limit, errors) | tests |
| replay disconnected | pass: `testdata/synthetic-usbcap` (labelled synthetic), `TestSyntheticUSBSessionReplaysWithoutHardware` |
| CLI end to end | `HWLAB_USBCAP_SYNTH=1 hwlab run --yes examples/usbcap-synth.yaml`: 92 packets, 46 transactions, 3 devices, 2 errors |
| live usbmon backend | **not run**: validation pending (needs root or readable debugfs) |
| hardware bus analyzer | **not implemented**: needs its own link type and backend |

## Issues found while building

- The endpoint transfer type in descriptors (0 control, 1 isochronous, 2 bulk,
  3 interrupt) differs from the usbmon numbering (iso 0, interrupt 1, control 2,
  bulk 3); a test caught the wrong mapping.
- A host's first configuration read is the 9-byte header only; it was reported
  as "descriptor truncated". It is now reported as a header read with the total
  length.
- The usbmon text format omits the status for setup submissions and the data
  limit is 32 bytes; the parser and the analysis account for both.
- Timestamps are 32-bit microseconds and wrap; the parser unwraps them and
  tolerates stragglers.

## Not validated / limits

Timing accuracy, live capture, isochronous data, high-speed split transactions
and anything below the URB level.
