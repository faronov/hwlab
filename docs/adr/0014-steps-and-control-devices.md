# ADR 0014 — Recorded experiment steps and control devices

Status: accepted. Capability-model change found during Phase 3 hardware
bring-up (input to the Phase 4 review).

## Context

Two real device classes did not fit the capture-only model:

- **Command/response devices** (J-Link, CC-Debugger, later SCPI, CMSIS-DAP):
  they stream nothing, so they had no capture capability and could not be part
  of a run. J-Link only worked after we made it advertise `LiveRawStream`, which
  is a false claim.
- **Invocation results were never recorded.** `hwlab invoke` returned the answer
  and forgot it, so a probe session could not be replayed or queried later.

## Decision

1. **Control devices.** In `app.resolveTargets` a device without
   `TimedCapture`/`LiveRawStream`/`OfflineCapture` is accepted if its plugin
   declares at least one method on one of the device's capabilities. It is opened
   and closed with the run, listed in `session.json` devices, and has no
   `captures` entry.
2. **Steps.** An experiment may list timed calls:

   ```yaml
   steps:
     - {name: identify, at: 100ms, device: dbg, call: DebugProbe.info, args: {}}
   ```

   Steps may target control or capture devices (e.g. a PPK2 `set_voltage`). Each
   is executed at `run epoch + at` and recorded as a timeline event of type
   `invoke` with `capability`, `method`, `args`, `ok`, `result` (replaced by a
   `truncated` marker above 64 KB), `error` and `step`; the event duration is the
   call duration.
3. **Safety.** Methods are checked against the plugin's declared methods, and any
   non-SAFE call needs the run's confirmation, before the session is created.
4. **Failure.** A failing step is recorded and counted, produces a finalisation
   warning, and does not abort the run. A control-only run ends when its steps
   finish; a run with no capture device and no steps is refused.
5. **Replay.** `invoke` events are ordinary timeline events, so replay reproduces
   them without the plugin. `replay --rerun` does not re-fire steps.

The core still has no vendor concepts: it knows only capability and method
names declared by plugins. `jlink` keeps `LiveRawStream` because it really
streams RTT.

## Timing caveat

`invoke` events are stamped on the host monotonic clock when the call starts.
The uncertainty of that stamp relative to any device clock is **not measured**
and no uncertainty is claimed for it.
