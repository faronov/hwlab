# CC23xx ROM serial bootloader: architecture boundary

hwlab does not implement the CC23xx ROM serial bootloader (status
NOT_IMPLEMENTED). It is documented here so nobody confuses it with the SACI path.

| | SACI (implemented) | ROM serial bootloader (not implemented) |
|---|---|---|
| Transport | SWD, CFG-AP mailbox, through a debug probe | UART / SPI pins of the device |
| Entered by | reset with debug access (nRESET or SYSRESETREQ), then SACI | a separate boot path described in the TRM (not verified here) |
| Used for | identify, CCFG, whole-chip erase, program, verify, restore | factory-style programming without a debug probe |
| Layer | `internal/ti/cc23xx` | would need its own transport plugin (serial) |

Reasons for the boundary:

- It needs a different transport (a serial port and boot-pin control), not a debug
  probe, so it would be a separate plugin rather than part of the CC23xx target layer.
- It was never exercised on the attached hardware (no boot-pin or UART wiring
  to the CC2340 was used).
- Its security behaviour depends on the CCFG; enabling it must never weaken debug
  security.

A future plugin can reuse `internal/ti/cc23xx/ccfg` to decide whether the ROM
bootloader is enabled before offering it.
