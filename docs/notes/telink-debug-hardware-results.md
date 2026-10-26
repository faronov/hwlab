# Telink debug (halt, step, breakpoints) on real hardware

Same board and programmer as phase 15: TB-04-style TLSR825x (SWire chip id
0x5562, rev 0x02), pvvx SWire firmware 0x0008 on a CH340
(`/dev/cu.usbserial-EXAMPLE`), running a zigbee-rs TLSR8258 router image. All
calls went through the hwlab MCP server (`target_op` / `device_invoke`), with
`config.layout=tb04-512k-nofactory`.

## Register behaviour observed

| Register | Observation |
|---|---|
| 0x602 control | `0x06` stall keeps PC and registers. `0x05` holds the core at the reset vector: PC and the whole register file read 0 and resume boots the firmware from reset. SWire activation (`attach`) leaves the core in `0x05`. After a comparator stop the byte still reads `0x08`. |
| 0x610 dword 0 | bit 31 is set while the core is stopped in debug: in stall (`0x06`) and after a comparator stop. Go (`0x08`/`0x84`) clears it; it is clear in `0x05`. Rewriting the breakpoint block does not clear it. |
| 0x610 dwords 1..3 | three comparators, `addr | 0x01000000`. All three fired on the board (a stop at the address written in dword 3 was observed). |
| comparators | live as soon as written: a core running with plain go (`0x08`) stopped at a freshly written comparator without `0x84`. |
| 0x613 step | steps only after the control byte is `0x05`/`0x06`; after a comparator stop (control `0x08`) the core must first be put in stall. |
| 0x6bc PC | a write was not accepted in stall mode (read back unchanged) nor in stop mode (reads 0). PC write is therefore not offered. |
| below 0x800000 | SWire reads there return the core/debug register block (e.g. 0x602, 0x6bc), not flash: a read at a code address differed from `FlashProgramming.read` of the same address. |

## Sequences run

| Sequence | Result |
|---|---|
| `breakpoint-set 0x468cc run_wait_ms=3000` on the running router | hit; `status` halted=true, halt_reason=breakpoint, control 0x08, PC stable |
| `step count=3` from the comparator stop | PC 0x468cc -> 0x468d2 |
| arm 0x468d4 and 0x467de while halted, then a 4th | three armed; the 4th refused (ResourceExhausted); `list_breakpoints` shows all three |
| `resume` + `wait_halt` | the CPU ran (PC moving), then stopped at 0x467de (dword 3) |
| `clear_breakpoint address=0x468d4`, then `all=true`, `resume` | slots empty, CPU running, status word 0 |
| plain `halt` (stall) | halt_reason=request, status word 0x80000000 |
| running CPU: `breakpoint-set` (no wait) -> `wait_halt` -> `step 2` -> `resume` -> `wait_halt` -> `resume` (step-over) -> `wait_halt` -> clear all -> `resume` | every stop at 0x468cc detected; the step-over resume left the PC moving before the next stop; final state running with no comparators |
| `MemoryAccess.read 0x4d570` | refused with a pointer to `FlashProgramming.read` |

An earlier halt at the router's idle PC (0x4d574) accepted step writes without
moving the PC: the core was parked in a hardware MCU stall. `step` now reports
`pc_unchanged` with a hint in that case (simulator-tested; not reproduced in
this run).

## Not validated

PC write (not accepted by the hardware; not offered), SRAM memory write, the
meaning of `0x84` versus `0x08` beyond both letting comparators fire.
