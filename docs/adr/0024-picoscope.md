# ADR 0024: PicoScope as a block-mode contract with a backend seam

Status: accepted (**no real driver; never run on hardware**)

## Context

PicoScope is high-rate analog acquisition. No PicoScope is attached to the
development host, and the vendor driver (PicoSDK: `ps2000a`, `ps4000a`,
`ps5000a`, `ps6000a`, ...) is a C library. A hwlab plugin is a separate Go
binary, and the repository builds with `CGO_ENABLED=0`, so linking a vendor
library is a decision with a cost, not a detail. Without hardware there is no
way to verify any driver call sequence.

The existing capability model was already shown to fit other instruments:
`scpi` (acquire-and-export oscilloscope) and `saleae` (capture/export). A
PicoScope block capture has the same shape.

## Decision

1. **Reuse the existing capabilities.** The plugin advertises `Oscilloscope`,
   `AnalogCapture`, `TimedCapture` and `ArtifactExport`, with attributes
   `mode=block` and `streaming=not-implemented`. No new capability and no core
   change: a capture produces analog events on stream `analog` (interleaved
   channels, unit V) and a `waveform.csv` artifact, exactly like the other analog
   sources, so plots, replay, the timeline and MCP work unchanged.
2. **A backend seam, no vendor code.** `plugins/picoscope/backend` defines
   `Backend` (`Info`, `Limits`, `Plan`, `Acquire`, `Close`) and the request
   validation every driver must obey (ranges, offset, coupling, resolution, rate
   grid, memory, trigger). The plugin never imports a vendor library. A future
   PicoSDK backend, in a CGO build or a helper process, implements that
   interface; nothing above it changes.
3. **Only a synthetic backend exists.** `HWLAB_PICOSCOPE_FAKE=1` lists
   `picoscope/fake-ps` (SYNTHETIC-PS4000): fixed mathematical signals, 8/12-bit
   quantisation, memory shared by enabled channels, a rate that falls with
   channel count, clipping with an overflow flag, edge triggers with a pre-trigger
   fraction and timeout. Its outputs are labelled `synthetic` everywhere
   (listing, results, stream attributes, artifact metadata, clock note, fixture).
   Nothing is listed without the variable, so a host with no scope sees no
   device.
4. **Block mode only.** One acquisition per capture. Streaming mode and
   rapid-block are not implemented, and the capability attributes say so. A
   block is capped at 4 M samples across all channels because it is held in
   memory and becomes session events and a CSV.
5. **Honest limits.** A requested sample rate is rounded to the instrument grid
   and the plan returned by every setting call states requested versus achieved
   rate, interval, samples and duration. The maximum rate and memory depend on
   the enabled channels and the resolution, so they are reported for the current
   setup. A trigger timeout is an error unless `trigger_auto` is set, in which
   case the untriggered block is marked untriggered.
6. **Waveforms never travel through RPC.** `acquire` returns a plan and
   per-channel measurements; `measure` returns measurements of a window;
   `waveform` returns statistics and a min/max envelope of at most 1000
   buckets. Full data belongs in the capture artifacts.
7. **Analysis is offline and vendor-neutral.** `internal/waveform` computes
   min/max/mean/RMS/peak-to-peak, base and top level, edge counts, frequency,
   period, duty, pulse widths and 10-90 % rise and fall time (Schmitt-trigger
   edge detection with linear interpolation; partial edges are not counted; a
   flat signal reports no frequency). `app.MeasureWaveform` applies it to any
   recorded analog stream, so replayed sessions, and the SCPI and Saleae analog
   streams, are measurable with no hardware: `hwlab waveform measure`, MCP
   `waveform_measure`. FFT is future work.

### Danger levels

Every method is SAFE. The setting methods (`configure_channel`, `set_timebase`,
`set_trigger`) only change host-side state that is applied at the next
acquisition; a PicoScope input range or trigger cannot damage a target. They
require an opened device, because settings on a transient call would be lost;
a transient `acquire` takes its settings as arguments instead. During a running
capture, methods that would touch the instrument or change its setup are
refused and reads continue.

### Timestamps

The instrument supplies no wall time. Event times are the host clock when the
block was returned plus the block's own time axis (zero at the trigger point),
and the clock note says the acquisition happened earlier by an unknown latency.
No accuracy is claimed.

## Consequences

- Real PicoScope support needs a backend. The recommended route is a helper
  process (or a separate CGO build) that wraps PicoSDK and speaks a narrow
  protocol to the plugin, keeping the default build pure Go. Choosing it needs
  hardware to validate against, so it is deferred.
- Real-device behaviours the synthetic scope cannot show: driver-specific
  timebase grids, ADC counts versus ranges, overflow semantics, resolution
  switching and power-supply/USB errors. All are **unvalidated**.
- The measurement library is validated against generated signals with known
  answers only, never a real probe.
