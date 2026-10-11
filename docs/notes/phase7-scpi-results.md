# Phase 7 — generic SCPI results

Hardware: **none available** (no oscilloscope or other SCPI instrument on this
host). This phase is replay/synthetic-validated only; the Rigol and Siglent
profiles and the TCP, serial and USBTMC transports against real instruments are
**unvalidated**.

| Item | Result |
|---|---|
| address parsing (`tcp://`, bare host, `serial:path@baud`, `usbtmc:`) | unit tests, round trip |
| framing: text replies, IEEE 488.2 blocks (`#n`, `#0`), prefix skipping, stale bytes, size cap | unit tests |
| safety classifier: query vs command, quoting, `;` chains, control characters | unit tests; rejected requests verified to send nothing |
| scope layer on rigol and siglent profiles: channels, timebase, trigger, acquire, measure, waveform, screenshot | tests against the synthetic fake |
| waveform decode (Rigol preamble, Siglent vdiv/offset), stats, envelope, CSV | unit tests (synthetic data) |
| TCP transport | tested over a loopback listener |
| serial transport | wraps the already-tested `uart.Port`; not run against an instrument |
| plugin: listing, open/identify, profile override, transient lease, danger levels, capture validation, trigger wait, cancel, trigger timeout, second capture refused | tests (race detector on) |
| capture: analog events, CSV, screenshot, honest clock | tests |
| replay disconnected | pass: `testdata/synthetic-scpi` (labelled synthetic), `TestSyntheticSCPISessionReplaysWithoutHardware`; the plot renders |
| CLI end to end | `HWLAB_SCPI_FAKE=rigol hwlab run --yes examples/scpi-fake.yaml`: 2400 samples, 3 artifacts, SVG plot; `invoke` of `SCPI.command` without `--yes` is refused |
| real Rigol / Siglent | **not run**: unvalidated |
| USBTMC device file, VXI-11, discovery on LAN | **not run / not implemented** |

## Bugs found while testing (all fixed, with regression tests)

- `Instrument.more`/`fill` looped forever when the leftover buffer held no
  terminator.
- `IsQuery` rejected a query with parameters (`:MEAS:ITEM? VPP,CHAN1`); the rule
  is now "each message's header ends in `?`".

## Known gaps

- Memory-depth waveforms beyond the screen buffer (Rigol `RAW`, Siglent `MAXP`).
- Siglent channel naming (`C1` vs `CHAN1`) and waveform scaling are the likeliest
  places for a real instrument to disagree with the profile.
- The fake was written from the same profile data it tests, so agreement
  between them is not evidence about real hardware.

## To finish validation

```sh
export HWLAB_SCPI_HOSTS="scope=tcp://<ip>"
hwlab invoke scpi/scope SCPI.identify          # check profile detection
hwlab invoke scpi/scope Oscilloscope.measure -a item=frequency -a channel=1
hwlab run --yes examples/scpi-fake.yaml        # with device: scope
```

Compare the CSV against the instrument's screen, then flip `Validated` per
profile and record the firmware tested.
