# Phase 5 — generic UART results

Port: nRF52840 DK on-board J-Link OB virtual COM port (`/dev/ttyACM0`,
1366:1025). The DK firmware is silent on the UART.

| Item | Result |
|---|---|
| discovery | pass (`uart/ttyACM0`) |
| open / configure (115200 8N1) | pass on real port |
| write (CONFIRM-gated) | pass; `tx` packet on the timeline, 7 B |
| read / capture RX | **not validated on real hardware** (no loopback wired); pty + fake only |
| flow control, stop bits | pty only (termios bits asserted) |
| parity, data bits | fakes only (a pty forces 8N) |
| replay disconnected | pass: `testdata/real-uart`, `TestRealUARTSessionReplaysWithoutHardware` (TX-only) |
| policy | `run` without `--yes` correctly refused |

## Bugs found by tests

- Chunk slices were reused after emit while the sink retains them.
- Opening a second handle for termios fails with EBUSY (exclusive port), hence
  flow control through the open handle.

## To finish validation

Jumper P0.06↔P0.08 on the DK and run
`HWLAB_UART_LOOPBACK=1 go test -tags=hardware ./plugins/uart/ -run Hardware -v`,
or use a USB-UART adapter for parity/data-bit checks.
