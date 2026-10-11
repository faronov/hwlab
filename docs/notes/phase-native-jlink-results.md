# Phase: native J-Link (`jlink-native`) results

Scope actually tested: ONE probe, fictional J-Link serial alias `90000001` (SAM-ICE class,
nickname `jlinkv9`), wired to an **EFR32MG1PxxxF256**. The nRF52840 and the
second probe were not touched (the nRF52840 serves as a sniffer), so the
Nordic and multi-probe acceptance items are **deferred, not done**.

No SEGGER process or library is in the native path (`jlink-segger` was used
only as an oracle for comparisons).

## Probe (as reported by the probe)

| item | value |
|---|---|
| USB | `1366:0105`, bcdDevice 0100, full speed |
| interface / endpoints | vendor interface 2 (class ff), bulk OUT 0x03, bulk IN 0x83, max packet 64 |
| version string | `J-Link V9 compiled May  7 2021 16:26:12`, HW V9.70 |
| capabilities | 0xB9FF7BBF (bit names not claimed) |
| target interfaces mask | 0x7B |
| speeds | base clock 12 MHz, max 12000 kHz |
| VTref | about 3.32 V |

## Results on the EFR32MG1P (native only)

| area | result |
|---|---|
| discovery, serial selection, probe info, VTref | REAL_HW_VERIFIED |
| identify | DPIDR 0x2ba01477, AP IDR 0x24770011, Cortex-M4 r0p1, CPUID 0x410fc241, ROM table, 6 FPB + 4 DWT comparators |
| halt / resume / status / all registers | verified |
| register write + restore, SRAM write 8/16/32 + restore | verified |
| 100 single steps | verified |
| FPB breakpoints | 5/5 actual hits, set/clear verified |
| DWT watchpoint | actual hit (a core-executed SRAM stub touches the watched address), set/clear verified |
| resets | sysreset, sysreset+halt, VECTRESET (core); reconnect afterwards |
| repeated open/connect/close | 10 rounds; second open -> BUSY |
| physical unplug/replug | read after unplug -> `DEVICE_GONE: PROBE_GONE`; an active RTT capture ended with DEVICE_GONE; replug returned the same serial with identical DPIDR/CPUID/AP IDR; USB device number changed 124 -> 125 |
| RTT (target-memory, `internal/rtt`) | control block at 0x20003684 (up 128 B, down 16 B); output digest identical to `jlink-segger` (3 lines each). The block only exists for part of the firmware life cycle; a sysreset recreates it. `rtt_write` is simulator-only |
| reset pin | UNSUPPORTED_BY_ATTACHED_HW (nRESET is not wired to this target) |
| JTAG | IMPLEMENTED_UNTESTED (modelled chain only; never run on hardware, to avoid driving the SWD-wired pins in JTAG mode) |
| trace/SWO, flash | NOT_IMPLEMENTED (flash is out of scope by design) |
| multi-probe isolation | IMPLEMENTED_UNTESTED (deferred; one probe available) |

## SWD speed ladder (16 KiB read, byte-identical at every rate)

| requested | time |
|---|---|
| 100 kHz | 2.145 s |
| 400 kHz | 593 ms |
| 1 MHz | 281 ms |
| 2 MHz | 174 ms |
| 4 MHz | 122 ms |
| 8 MHz | 112 ms |
| 12 MHz | rejected by the target: `PROTOCOL_ERROR: SWD ACK: invalid ACK 0b011`; the target recovered |

The programmed value is the probe's configuration, not a measured SWCLK. Above
about 2 MHz full-speed USB dominates.

## Performance vs `jlink-segger` (best of 3, same target)

| operation | native | segger |
|---|---|---|
| connect | 12 ms | 160 ms |
| 16 B read | 13 ms | 176 ms |
| 2 KiB read | 52 ms | 175 ms |
| 64 KiB read @1 MHz | 1.15 s | 0.63 s |
| 64 KiB read @4 MHz | 0.49 s | not measured |
| 100 single steps | about 0.61-0.64 s (about 6 ms/step) | under 30 ms in one session |
| 20x resume+halt | about 115 ms | about 0.57-0.59 s (prints registers) |

Segger per-call timings include a `JLinkExe` process spawn; the step and
halt/resume rows come from one scripted session. Native single-step is slower
because the shared `internal/cortexm` Step makes several USB round trips.
Memory bytes were identical.

## Shared fixes found on the way

* `internal/cortexm`: `Reset` accepts DFSR.VCATCH for reset-and-halt
  (`ResetResult.VectorCatch`).
* The subsequent maintenance pass replaced duplicated DWT snapshots and
  per-adapter ownership caches with a shared DWT ownership ledger. The fix
  applies to native J-Link, CMSIS-DAP, XDS110 and ST-Link; it is no longer a
  native-only cache workaround. See the maintenance results below.

## Incidents (honest log)

* Twice during reconnaissance the probe fell into its bootloader (`1366:0101`)
  after a command was sent while a reply was pending or a reply was read
  short; only a physical replug recovered it. Hence the exact-reply rule.
* An aborted test once left DWT/TRCENA enabled on the target (cleared only by
  power-on); repaired with `JLinkExe`. Shared transactional cleanup and the
  native maintenance hardware test now cover this failure class.
* An earlier assistant shell command, `rm -f /tmp/*.out`, deleted unrelated
  files. This command was not repository code and must never be repeated.
  The maintenance audit found no executable wildcard `/tmp` cleanup in the
  repository; it did find related file-ownership violations described below.
  This pass does not recover previously deleted files.

## Maintenance: cleanup ownership and debugger state

Scope: correctness only, on the same EFR32/native J-Link. No flash writes,
new backend, additional J-Link commands, performance work or nRF52840 changes.

### Host ownership -- FIXED_AND_TEST_VERIFIED

`internal/ownedtemp` creates unique directories, rejects replacement-directory
cleanup, and publishes output through exclusively created sibling staging
files. Failed writes preserve existing user output and neighboring reference
files. Exclusive no-overwrite publication also prevents a late-arriving user
file from being overwritten.

Repository fixes:

* ESP hardware restore checks no longer pre-delete the fixed shared
  `/tmp/hwlab-esp-restore-check.bin`; they use `t.TempDir()`.
* ST-Link memory/flash-read and ESP memory-read failures no longer remove the
  caller's existing output. ESP flash-read staging no longer uses the
  predictable caller path plus `.part`.
* CC-Debugger output publication rejects an output appearing after its initial
  existence check, rather than renaming over it.
* Root and ESP acceptance-firmware `make clean` no longer recursively delete
  their output directories. They remove only exact outputs registered by the
  build with matching SHA-256 digests. Modified, symlinked and unregistered
  files are retained; a missing ownership manifest means no cleanup.
* SEGGER commander temporary files are scoped to an allocated
  directory. Hardware unplug logs default to `t.TempDir()`; an explicitly
  selected operator output path remains an operator-owned artifact.
* ESP hardware banner reads restore the saved CDC terminal settings.

`TestCleanupPreservesNeighbor` creates its own sentinel outside the owned
directory and proves exact contents survive cleanup after success, error,
cancellation and DEVICE_GONE. `TestPublishNeverDeletesReferences` covers
those same outcomes with existing output and `.part` references.
`TestBuildCleanupUsesOwnershipMetadata` covers registered outputs, modified
outputs, an unrelated `.out` neighbor and a directory without metadata.
Discovery globs, backup/reference files and the existing allocated
socket/MCP/doctor cleanup paths were audited without a broad-delete issue.

### Shared Cortex-M -- FIXED_AND_TEST_VERIFIED

Root cause: ownership lived in duplicated adapter caches/full snapshots;
hardware verification failures, cache disposal and cancelled transport
contexts could leave comparators armed or make cleanup lose the state needed
to disable them. Restoring full old snapshots could also overwrite foreign
debugger state.

* Set verifies FUNCTION, COMP and MASK. Any write/read/verify/cancel failure
  attempts rollback with a fresh bounded context. Failed rollback remains
  conservatively tracked, but is never reported as a successful installation.
* Clear checks live ownership, disables and verifies FUNCTION, restores only
  the allocated originally-disabled slot, and then removes cached ownership.
  A hardware-cleanup failure retains retryable state. A changed foreign
  comparator is left untouched and reported as an ownership error.
* Set reconciles disabled/reset or pending failed-install slots before reuse,
  preventing duplicate address ownership and wrong-slot clears. Listing an
  unsupported FUNCTION after failed rollback does not discard pending cleanup.
* Reset first cleans owned triggers; reconnect creates fresh target state.
  A closed DWT handle is not reused, and live TRCENA is checked rather than
  trusted from a cache. Cleanup preserves unrelated DEMCR bits and does not
  disable trace underneath foreign comparators.
* FPB cleanup now restores only owned slots instead of a whole stale
  comparator snapshot, refuses adoption of foreign breakpoints, and does not
  disable FPB underneath foreign active comparators.
* All four adapters use the shared cleanup on operational error/cancel/close.
  Rejected invalid requests do not discard previously installed triggers.
  Primary DEVICE_GONE/TIMEOUT/PROTOCOL_ERROR/cancellation/UNSUPPORTED errors
  retain their classification when a cleanup failure is attached.
* ADIv5 forwards the fresh cleanup context to context-aware transports.
  Native USB cancellation is not reclassified as PROTOCOL_ERROR; a cancelled
  exchange is marked for pending-reply drainage.

The shared simulator regression runs through each adapter's actual simulated
wire path: native J-Link, ST-Link, CMSIS-DAP and XDS110. It covers 20 reuse
cycles, set verification failure, clear failure and retry, reset/reopen,
cancellation, timeout cleanup, conflicting settings and foreign comparators.
Shared fault-bus tests additionally cover pending rollback and external-reset
reconciliation. Native real-wire fixture replay remains byte-exact.

### Native EFR32 -- FIXED_AND_REAL_HW_VERIFIED

Final `TestHWWatchpointCleanup` run passed on the probe recorded under fictional
serial alias `90000001` in 1.43 s:

* 20 actual DWT write-watchpoint hits from the core-executed SRAM stub.
* 20 clears with FUNCTION checked while TRCENA is explicitly enabled.
* 20 executions of the same store without a watchpoint: the store completed,
  the core stayed running, and no stale DWT halt cause was seen.
* An intentionally cancelled operation with an installed watchpoint returned
  a cancellation/deadline class and left its comparator disabled using the
  fresh cleanup context.
* SRAM at `0x20007000`/`0x20007100` and r0/r1/PC were restored, owned triggers
  removed, and the final target status confirmed running.

An earlier maintenance run exposed cancellation being misclassified; the
transport/context fix above was applied before the passing recheck.

### Validation -- FIXED_AND_TEST_VERIFIED

Formatting of the changed Go packages, `go vet ./...`,
`go vet -tags=hardware ./...`, `go test -race -count=1 ./...`,
`CGO_ENABLED=0 go build ./...` and `make check-cgo` all passed.
The full race run includes ST-Link, CMSIS-DAP, XDS110, ESP USB-JTAG, FTDI,
CC-Debugger, J-Link SEGGER and MCP. `make check-cgo` covers Linux amd64/arm64,
Windows amd64 and the project's documented Darwin arm64 CGO-free subset.
An actual `make build`/`make clean` round trip in a newly allocated output
directory also passed, including directory removal after registered outputs
were cleaned.

### Limits -- REMAINING_GAP

Cleanup is best-effort if the probe/target is physically unreachable; a
DEVICE_GONE failure is returned, not claimed as a disabled comparator.
SIGKILL, host power loss or USB removal cannot execute software cleanup.
Other adapters were regression-tested in simulators, not rechecked on real
hardware during this pass. Unrelated historical `/tmp` deletions were not
recovered.

## Fixtures

* `testdata/real-jlink-native/wire.json`: 318 real exchanges (info, connect,
  AP scan, status, halt, registers, memory read, resume, disconnect),
  sanitized (no serial, no paths). Replay is byte-exact and needs no probe:
  `go test ./plugins/jlinknative/ -run 'Fixture'`.
* Re-record: `HWLAB_JLINKNATIVE_HW=1 HWLAB_JLINKNATIVE_RECORD=<file> go test -tags=hardware ./plugins/jlinknative/ -run TestHWRecordWireFixture`.
* RTT payloads are never committed (they contain a device address).

## Remaining gaps

nRF52840 acceptance; two-probe isolation and concurrency; JTAG on a real
chain; reset pin; trace/SWO; other probe generations (V10/V11/OB); `rtt_write`
on hardware; step performance (needs a cortexm batching change).
