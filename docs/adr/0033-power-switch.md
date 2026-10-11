# ADR 0033: PowerSwitch, target power control independent of the debug probe

Status: proposed (architecture only; no code beyond the existing PPK2 source
capability)

## Context

Phase 27 ([note](../notes/phase27-lp-xds110et-target-power.md)) measured that the
LP-XDS110ET cannot switch its 3V3 pin: `XDS_SET_SUPPLY` is acknowledged and does
nothing. A target that wedges in a way a reset does not clear therefore cannot be
power cycled through the probe. Even on probes that do have a supply (the stand-alone
XDS110), tying recovery to the debug probe is the wrong coupling: the thing that
switches power is a different piece of lab hardware from the thing that debugs.

hwlab already has one real power-source capability: `PowerSource.set_dut_power` on
the PPK2 plugin (source mode, DUT supply on/off, `supply_mv` and `dut_power` are
dangerous config keys).

## Decision

Introduce a generic capability, `PowerSwitch`, that any plugin may implement. A
target recovery workflow asks for `target.power_cycle` and never names a probe.

    PowerSwitch (capability)
      info()                    -> kind, channels, max voltage/current, verified: bool,
                                   state_readback: none | measured | reported
      state(channel)            -> on/off/unknown and, if readable, measured millivolts
      set(channel, on)          -> confirm-gated, never automatic
      power_cycle(channel, off_ms, settle_ms)
                                -> off, wait, on, and report the measured voltage
                                   before and after when the provider can read it

A power cycle is only reported as done when the provider can show the rail left its
operating range (readback), or the result says `unverified` and recovery may not
treat the target as power cycled.

Providers (each a plugin, none required):

| Provider | Switching element | Voltage readback |
|---|---|---|
| PPK2 source output | `set_dut_power` (exists) | PPK2 measures the rail |
| Programmable PSU over SCPI | `OUTP` on/off | `MEAS:VOLT?` |
| USB power switch | per-port power (uhubctl-style / PPPS hubs) | none or hub reported |
| GPIO / load switch | one GPIO driving a load switch | none unless an ADC watches the rail |
| Relay / MOSFET fixture | relay board or MOSFET on the target rail | none unless measured |

Binding: a target profile names the switch (`power: ppk2/<id>#ch0`), not the probe.
The XDS110 plugin gets no PowerSwitch and no `target_power` on the LP-XDS110ET. The
stand-alone XDS110 supply (id 0x21) could become one provider later, but only if
exercised on hardware.

## Recovery workflow

    target.recover:
      1. reset (nRESET, then SYSRESETREQ)        # existing
      2. if the debug port does not come back and a PowerSwitch is bound:
           target.power_cycle(off_ms >= a measured discharge time, settle_ms)
      3. re-attach, verify DPIDR/CPUID/identity
      4. otherwise report POWER_CYCLE_UNAVAILABLE (not a silent skip)

The off time must be taken from a measurement of the rail with that target, not a
constant: on the CC2340 board the decay of a switched supply was never measured
because no switch existed.

## Rules

* Switching is dangerous: opt-in per device at open time, and a confirm phrase per
  call; never invoked implicitly by a capture or a connect.
* A provider that cannot confirm the rail state says so; `power_cycle` then returns
  `unverified`.
* The probe plugins stay probe-only. No `target_power` on hardware without a supply
  (UNSUPPORTED_BY_ATTACHED_HW, not a no-op acknowledgement).
* Fixtures: replay of each provider's real traffic where available; simulators are
  labelled SYNTHETIC.

## Consequences

* CC2340 "power-cycle recovery" stays NOT_IMPLEMENTED until a provider is attached and
  a real cycle is measured.
* The PPK2 `set_dut_power` capability is the first candidate to move behind the
  interface once the interface exists.
* A cheap first fixture is a relay or MOSFET on the CC2340 board VCC wire (the user
  already has the 3V3 wire on a breadboard) driven by a GPIO; the PPK2 in source
  mode is the other, since it also measures the rail.
