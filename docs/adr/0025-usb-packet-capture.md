# ADR 0025: USB packet capture at URB level (usbmon), vendor-neutral

Capture level: **URB / host-controller observation; not a physical bus-level USB analyzer; live usbmon unverified.**

Status: accepted (**synthetic, text-file and FIFO sources tested; the live Linux
usbmon backend has never run on this host**)

## Context

A USB analyzer abstraction must not fit only one backend. Sources differ in what
they can see:

- a **hardware bus analyzer** sees packets on the wire (tokens, handshakes, SOF,
  bus-level timing);
- **Linux usbmon** sees URBs as the host kernel submits and completes them;
- vendor tools export their own formats.

The development host has no usbmon access: no `/dev/usbmon*`, debugfs is not
readable, the user is not root and there is no sudo. Live capture therefore
cannot be validated here.

## Decision

1. **The normalized model is the URB**, because it is the lowest common level of
   the sources that exist: `Packet{timestamp, URB id, event S/C/E, transfer type,
   bus, device, endpoint, direction, status, length, setup, data}`. It uses the
   usbmon numbering and is stored as one `packet` timeline event with protocol
   `usb` (`hex` = usbmon mmapped 64-byte header + data) plus a pcapng artifact
   (link type 220, `LINKTYPE_USB_LINUX_MMAPPED`) that Wireshark decodes natively.
2. **A hardware bus analyzer is explicitly not covered.** Its packets are not
   URBs. It needs its own link type and backend (for example
   `USB_2_0` link types) and its own analysis; forcing it into the URB model would
   invent fields. It can emit `packet` events with another protocol and reuse the
   capability, the timeline and replay unchanged.
3. **Reuse existing capabilities.** `USBPacketCapture` (new name), `PacketCapture`,
   `TimedCapture`, `ArtifactExport`. No core change.
4. **Sources of the `usbcap` plugin.**
   - `usbmon` (live): debugfs `/sys/kernel/debug/usb/usbmon/Nu`, listed only if
     openable. **Unvalidated.**
   - `file` (offline): usbmon text captures or FIFOs registered with
     `HWLAB_USBCAP_SOURCES=name=/path`. Replayed with `speed`. Tested.
   - `synthetic`: `HWLAB_USBCAP_SYNTH=1`: a HID mouse, a mass-storage stick and a
     CDC serial port, with a stall and a bus error. Labelled synthetic everywhere.
5. **Analysis is offline and in the service layer** (`app.AnalyzeUSB`, CLI
   `hwlab usb analyze`, MCP `usb_analyze`): pairs submissions with completions,
   learns devices, configurations, interfaces and endpoint-to-interface maps from
   enumeration traffic, and decodes control requests, HID, CDC (line coding,
   control lines, ASCII data), DFU and mass-storage CBW/CSW (SCSI opcode names).
   Classes are never guessed without descriptors: a device enumerated before the
   capture starts stays unlabelled. Results are bounded (`limit`, filters by
   device, errors and time window); raw packets are not sent through MCP.
6. **Privacy.** USB payloads can contain keystrokes and secrets. `snaplen`
   (default 256, `0` = headers only) limits what is stored.
7. **Time.** Timestamps are kernel microsecond differences anchored at the first
   packet; latencies are kernel URB submit-to-complete times, not bus times. The
   accuracy against other devices is unmeasured and the clock note says so.

## Protocol roadmap

Decoded now: control (standard requests, descriptors), HID, CDC-ACM, DFU, mass
storage (BOT). Vendor-specific transfers are labelled only. Not done: HID report
descriptor parsing, UVC/UAC, CDC-ECM/NCM, MSC beyond named opcodes, USB-PD.

## Consequences

- The usbmon text API keeps at most 32 data bytes, so large descriptors can be
  truncated; the analysis flags it. The binary usbmon interface would remove the
  limit but needs ioctls and has not been written.
- No SOF, no tokens, no handshakes, no bus-level timing; do not claim them.
- Validation still needed: a live usbmon run (root or readable debugfs) and a
  hardware analyzer backend.
