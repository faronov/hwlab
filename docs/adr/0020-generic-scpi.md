# ADR 0020: Generic SCPI as an instrument subsystem, with vendor profiles as data

Status: accepted

## Context

Bench oscilloscopes (Rigol, Siglent, and others later) share one transport and
message layer, SCPI over TCP, serial or USBTMC, but differ in channel naming,
waveform transfer and screenshot commands. Writing a driver per vendor would
repeat the transport, framing and safety logic. No instrument is attached to
the development host, so nothing here has met a real instrument.

## Decision

- **Layers.** `internal/scpi` is vendor independent and has no plugin
  dependency: `Transport` (TCP, serial through `uart.Port`, USBTMC device
  file) → `Instrument` (serialised command/query/binary query, IEEE 488.2
  block framing, error queue, `*IDN?`) → `Scope` (channels, timebase, trigger,
  acquire, measure, waveform, screenshot) driven by a `Profile`. The
  `plugins/scpi` plugin only maps these onto the plugin protocol.
- **Two capabilities.** `SCPI` (identify, query, query_binary, command) works
  on any instrument. `Oscilloscope` needs a profile with scope semantics. The
  `generic` profile has none, so an unknown instrument still gets raw SCPI and
  refuses scope calls with `FailedPrecondition`.
- **Profiles are data.** A `Profile` is a table of command templates plus two
  functions (waveform decode, screenshot). `Rigol` (DS1000Z family) and
  `Siglent` (SDS1000X-E family) are selected from `*IDN?` or forced with the
  `profile` config key. Adding a vendor means adding a profile, not a plugin.
- **Both profiles are UNVALIDATED.** A web search for the command sets returned
  contradictory channel naming (Siglent `C1` versus `CHAN1`), so the choices in
  the profiles are from public programming guides and are flagged
  `Validated=false`. `identify`, stream attributes and artifact metadata carry
  `profile_validated=false` until a real instrument runs them.
- **Safety.** Reads (`query`, `query_binary`, `channels`, `timebase`, `trigger`,
  `trigger_status`, `measure`, `waveform`) are SAFE. Anything that changes the
  instrument (`command`, `configure_channel`, `set_timebase`, `set_trigger`,
  `acquire`) is CONFIRM. `query` accepts only text in which every
  `;`-separated message has a header ending in `?` (quote aware), so a command
  cannot be smuggled through the safe method; `command` refuses queries; both
  reject control characters including newlines. Rejected requests send nothing
  to the instrument. Input impedance is not exposed. Serial ports are never
  probed with `*IDN?` during discovery.
- **Discovery.** `HWLAB_SCPI_HOSTS=name=addr,...` (`tcp://host[:port]`, bare
  `host`, `serial:/dev/x[@baud]`, `usbtmc:/dev/usbtmcN`) plus `/dev/usbtmc*`.
  LAN/VXI-11/mDNS discovery is not implemented.
- **Capture is acquire-and-export, like Saleae, not a stream.** `trigger:
  single` arms the instrument and polls for a trigger (failing on timeout);
  `none` reads whatever the instrument holds. Each channel becomes `AnalogBatch`
  events (`CH<n>`, volts, sample period from the time axis, chunks of at most
  50 000 samples) and a `waveform-chN.csv` artifact; `screenshot: true` adds an
  image. Full waveforms are never returned from `Invoke`; the `waveform` method
  returns statistics and a min/max envelope of at most 1000 buckets.
- **Honest clocks.** The instrument reports no acquisition timestamp. For
  `single` the event time is the host time the trigger was first seen plus the
  trace time axis; the lower error bound is the poll interval plus a measured
  transfer round trip and the upper bound is not claimed. For `none` the
  acquisition happened at an unknown earlier time. The instrument's time base
  accuracy was not checked.
- **Synthetic fake.** `HWLAB_SCPI_FAKE=rigol|siglent` offers the in-process
  `internal/scpi.Fake` as device `fake-<profile>`. It speaks each profile's
  commands (1 kHz sine, 1200 points, trigger state machine, screenshots, an
  error queue) and is labelled synthetic everywhere. It exists to test the
  pipeline and to record a replay fixture; it proves nothing about real
  instruments, and it was written from the same profile data it tests.

## Consequences

- The same core capture/session abstraction now carries a PPK2 live stream, a
  Saleae export, a packet capture and an oscilloscope acquire-and-export with
  no scope-specific code in the core.
- When a real Rigol or Siglent is available, the work is to run the profile,
  correct it (likely the Siglent channel naming and waveform scaling), and flip
  `Validated`. Waveforms longer than the screen memory (Rigol `RAW` mode with
  `:RUN`/`:STOP` paging, Siglent `WAV:MAXP`) are not implemented.
- A PicoScope will not use SCPI; it will share the `AnalogCapture` stream and
  CSV/analog-event conventions, not the `Oscilloscope` command layer.
