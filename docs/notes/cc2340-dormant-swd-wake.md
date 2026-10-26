# CC2340: dormant SW-DP after power-on, automatic wake-up

Status: **implemented and verified on real hardware** (E104-BT55SP CC2340 module,
LP-XDS110ET firmware 3.0.0.32, Linux host; both the `xds110` and `cmsisdap`
backends).

## Symptom

After the target lost power (USB re-plug, jumper P9 change, supply cycle) SWD did
not answer on either backend:

* `xds110`: `XDS110 command 0x0f: SWD protocol error (-615)` at every clock
  (100 kHz, 400 kHz, 1 MHz), also after an nRESET pulse and SACI entry;
* `cmsisdap`: "no target answered on SWD", DPIDR reads 0;
* JTAG scan: TDO stuck low;
* pins at rest: nRESET 1, SWDIO 1, SWCLK 0 (no repurposing by the application).

Re-seating or swapping the SWDIO/SWCLK wires "sometimes" made it work. Phase 27
recorded the same outage with "cause unknown".

## Cause

The CC2340 debug port is an SW-DP v2 (DPIDR `0x6BA02477`, multi-drop capable). It
comes out of power-on reset in the **dormant** state (ADIv5.2 B5.3). A dormant DP
ignores the JTAG-to-SWD switch (`0xE79E`) and line resets; only the dormant-to-SWD
sequence activates it:

1. at least 8 cycles with SWDIO high,
2. the 128-bit selection alert `0x19BC0EA2_E3DDAFE9_86852D95_6209F392`
   (LSB first),
3. 4 cycles low,
4. activation code `0x1A` (SWD),
5. a line reset (>= 50 high) and idle cycles.

Once activated the DP stays in SWD until power is lost; nRESET and SYSRESETREQ do
not return it to dormant. Wire-fiddling occasionally produced enough noise on the
lines to look like the sequence, which is why it "eventually" worked.

## Evidence

* Raw CMSIS-DAP `DAP_SWJ_Sequence` with JTAG-to-SWD only: no ACK. With the
  dormant-to-SWD sequence: DPIDR `0x6BA02477` immediately.
* Explicit SWD-to-dormant (`0xE3BC`) put the DP back to dormant (no ACK, XDS110
  -615 again), reproducing the outage on demand without touching power.
* XDS110 firmware cannot clock arbitrary SWJ bits in SWD mode, but in JTAG mode
  SWDIO is TMS. `OCD_PATHMOVE` (command `0x3c`: u32 count + one XDS state id per
  TCK) walks the TAP through states whose transitions encode any TMS bit pattern.
  Starting from Test-Logic-Reset, the 204-bit wake sequence as a state path
  activated the DP; a plain SWD connect then succeeded (Cortex-M0+ r0p1, CPUID
  `0x410CC601`, AP IDR `0x84770001`).

## Implementation

* `internal/adiv5`: `Reinit` first tries the normal line-up (line reset,
  JTAG-to-SWD, line reset, DPIDR read). If that fails it runs the dormant-to-SWD
  wake-up and retries. The wake-up uses the link's optional `DormantWaker`
  interface when present, otherwise `SWJSequence`. On success
  `Target.WokeFromDormant` is set; on failure the original error is kept
  (`errors.Is` still works) with the wake attempt appended.
* `xds110`: `SWDLink.WakeDormant` (firmware >= 0x02030011, same threshold as
  OpenOCD's optimized DAP commands): SWD disconnect, TAP to Reset, then
  `PathMove` with the TMS path of `adiv5.DormantWakeTMS()`.
* `cmsisdap`: uses the generic `SWJSequence` path.
* `DebugProbe.connect` returns `"woke_from_dormant": true` when the wake-up was
  needed (`xds110`, `cmsisdap`, `jlinknative`).
* Simulators: the XDS110 and CMSIS-DAP simulators model a dormant DP (connect
  fails until the wake sequence is seen; `0xE3BC` returns to dormant).
  Tests: `TestConnectWakesDormantDP`, `TestConnectUsesLinkDormantWaker`,
  `TestAttachWakesDormantDP`, `TestDormantDPIsWokenThroughPathMove`,
  `TestPathMoveRejectsNonAdjacentStates`.

## Hardware verification

For each backend: DP forced dormant with `0xE3BC` over CMSIS-DAP (no ACK
confirmed), then `DebugProbe.connect`:

| Backend | Previous build (`273c219`) | This change |
|---|---|---|
| `xds110` | `-615` | Cortex-M0+, DPIDR `0x6ba02477`, `woke_from_dormant: true` |
| `cmsisdap` | "no target answered on SWD" | Cortex-M0+, DPIDR `0x6ba02477`, `woke_from_dormant: true` |

Not verified: a physical power cycle with the new build (the forced-dormant path
is the same state), J-Link and ST-Link against a dormant DP (they use the generic
`SWJSequence` path), XDS110 firmware older than 0x02030011.
