# ADR 0016 — Capability model review against five real device classes

Status: accepted. Reviewed after real runs of PPK2, the nRF 802.15.4 sniffer,
a Saleae Logic, a J-Link OB and a TI CC-Debugger.

## Question

Does the capability / plugin / session model, designed around PPK2, hold for
very different devices without vendor-specific hacks in the core?

## Evidence

| Class | Device | Shape | Core changes it needed |
|---|---|---|---|
| high-rate stream | PPK2 | live sample stream, 100 kSa/s | none (the original design) |
| packet stream | nRF 802.15.4 | live `Packet` events + PCAP artifact | none; decode is optional and external (tshark) |
| capture/export | Saleae Logic | no live stream; data arrives after the capture as files | none for ingestion (events emitted during `Stop`); two small robustness fixes below |
| command/debug probe | J-Link | request/response, plus an optional RTT stream | steps and control devices (ADR 0014); `Log.attributes` |
| restricted probe | CC-Debugger | subset of DebugProbe, no RTT | none: advertises fewer capabilities |

## Findings and decisions

1. **Capabilities as a subset work.** CC-Debugger advertises only
   `DebugProbe`+`FlashProgramming`; Saleae advertises `OfflineCapture` and no
   `LiveRawStream`. Nothing in the core special-cases a vendor. Kept.
2. **Request/response devices were not captures.** J-Link and CC-Debugger do
   useful work through `Invoke`. Timed `steps` and control devices (ADR 0014)
   put those calls on the same timeline. Done earlier; confirmed by real use.
3. **Normalized log events lacked context.** The J-Link spec requires the RTT
   channel in each event and a serial line will need its port. `Log` gained an
   additive `attributes` map (wire-compatible; surfaced as `attr.<key>` in
   timeline data like `Packet.attributes`).
4. **A capture with no events still has a duration.** An idle logic analyzer
   produced no time extent and failed the whole run at plot time. The Saleae
   reader now anchors its first batch at the initial-state row, and an
   empty plot is a warning (`plot.ErrNothingToPlot`) instead of a failure.
5. **`LiveRawStream` means "samples/bytes arrive while the capture runs".**
   The J-Link advertises it because RTT really streams; Saleae does not. The
   meaning is documented in `pkg/api/pluginv1/caps.go` terms and is not
   changed.
6. **Timing claims stay per source.** Saleae and J-Link report unbounded or
   one-sided `ClockInfo`; nothing is claimed that was not measured.

## Deferred (no evidence of a problem yet)

- **Stop timeout.** `finalize` gives `Stop` 30 s, which bounds export-oriented
  devices. A 90 s Saleae capture of a mostly idle line exported in well under a
  second, so it is not changed; a per-plugin stop budget (`PluginInfo`) is the
  fix if a Logic Pro high-rate capture overruns.
- **Analog export** (`analog.csv`) has only run against the fake: the attached
  Logic has no analog channels.
- **Danger classification** is per method (`DANGER_CONFIRM`); raw SCPI and
  similar free-form commands will need per-call metadata when SCPI lands.

## Consequences

No structural refactor. The model stays: devices advertise capabilities,
captures emit typed events, `Invoke` carries commands, steps schedule them, and
artifacts carry anything bulky.
