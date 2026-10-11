package xds110plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/internal/jtag"
	"github.com/faronov/hwlab/internal/usb"
	"github.com/faronov/hwlab/plugins/xds110/energytrace"
	"github.com/faronov/hwlab/plugins/xds110/xds"
)

// powerConfirm is the exact phrase that enables the target supply.
const powerConfirm = "enable-target-power"

func toMap(v any) map[string]any {
	b, _ := json.Marshal(v)
	m := map[string]any{}
	_ = json.Unmarshal(b, &m)
	return m
}

// usbIdentity re-reads the probe's descriptors and kernel driver bindings from
// sysfs, so the report shows the bindings as they are now (for example the
// cdc_acm interfaces next to the interface hwlab holds).
func (d *device) usbIdentity() map[string]any {
	if d.entry.USB == nil {
		return nil
	}
	u := *d.entry.USB
	for _, c := range usb.Scan("/sys", xds.Match) {
		if DeviceID(c) == d.id {
			u = c
		}
	}
	claimed := -1
	if l, ok := xds.LayoutFor(u.VID, u.PID); ok {
		claimed = l.Interface
	}
	var ifs []any
	for _, i := range u.Interfaces {
		row := map[string]any{"number": float64(i.Number), "class": fmt.Sprintf("0x%02x", i.Class), "kernel_driver": i.Driver, "claimed_by_hwlab": i.Number == claimed}
		if i.TTY != "" {
			row["tty"] = i.TTY
		}
		if i.Name != "" {
			row["name"] = i.Name
		}
		var eps []any
		for _, e := range i.Endpoints {
			eps = append(eps, fmt.Sprintf("0x%02x %s %d", e.Address, e.Type, e.MaxPacket))
		}
		row["endpoints"] = eps
		ifs = append(ifs, row)
	}
	return map[string]any{
		"vid": fmt.Sprintf("%04x", u.VID), "pid": fmt.Sprintf("%04x", u.PID), "serial": u.Serial,
		"manufacturer": u.Manufacturer, "product": u.Product, "bcd_device": fmt.Sprintf("0x%04x", u.BCD),
		"configuration": float64(u.Configuration), "speed_mbps": float64(u.SpeedMbps),
		"interfaces": ifs, "claimed_interface": float64(claimed),
	}
}

func (d *device) energyTraceInfo() (map[string]any, error) {
	supported := supportsEnergyTrace(d.entry)
	res := map[string]any{
		"supported": supported, "status": "PROTOCOL_PARTIAL",
		"sample_rate_hz": float64(energytrace.SampleRateHz), "sample_period_ns": float64(energytrace.SamplePeriodNs),
		"samples_per_packet": float64(energytrace.SamplesPerPacket),
		"data_interface":     float64(energytrace.DataInterface), "data_endpoint": fmt.Sprintf("0x%02x", energytrace.DataIn),
		"decoded_range":      "0 (about 7 mA class, verified against the vendor tool at one operating point)",
		"not_decoded":        "ranges 4 and 1 (low-current), voltage, energy counters, target-side meaning of the unknown commands",
		"gating":             "capture needs allow_target_power at open time and the core's confirmation. This is conservative: measured on an LP-XDS110ET, the 3V3 pin is already live with the jumper on XDS and the stream does not switch it; the effect of the unknown start commands is not established",
		"allow_target_power": d.o.allowPower, "capturing": d.cap != nil,
		"doc": "docs/xds110-energytrace-protocol.md",
	}
	return res, nil
}

func (d *device) info() (map[string]any, error) {
	in, err := d.probe.Version() // a fresh round trip, not the cached open-time copy
	if err != nil {
		return nil, err
	}
	res := map[string]any{
		"transport": d.probe.Describe(), "firmware": in.FirmwareText, "firmware_raw": fmt.Sprintf("0x%08x", in.Firmware),
		"hardware_id": fmt.Sprintf("0x%02x", in.Hardware), "target_supply_capable": in.SupplyCapable,
		"jtag_driven": "chain-scan-only", "validated": d.entry.Sim == nil,
		"note": "the hardware id is reported as read; target_supply_capable is true only for the stand-alone id 0x21. On the LP-XDS110ET (0x23) the 3V3 pin is the probe rail selected by jumper P9 and cannot be switched by software",
	}
	if d.entry.Sim != nil {
		res["synthetic"] = true
		res["validated"] = false
	}
	if id := d.usbIdentity(); id != nil {
		res["usb"] = id
	}
	return res, nil
}

// clockProbe asks the probe for each clock and times TCK cycles from the host.
// The probe never reports the effective rate; the plan shows the encoding sent
// and the nominal frequency it implies, and the timing is only evidence when
// the cycle burst is clearly longer than a USB round trip.
func (d *device) clockProbe(a map[string]any) (map[string]any, error) {
	rates := []float64{100_000, 400_000, 1_000_000, 2_500_000, 5_500_000, 8_500_000, 10_000_000, 12_000_000, 14_000_000}
	if v, ok := a["rates_hz"]; ok && v != nil {
		list, isList := v.([]any)
		if !isList || len(list) == 0 || len(list) > 16 {
			return nil, argErr(fmt.Errorf("rates_hz: want 1..16 numbers"))
		}
		rates = rates[:0]
		for _, x := range list {
			f, ok := x.(float64)
			if !ok || f < 1000 || f > 50_000_000 || f != float64(uint32(f)) {
				return nil, argErr(fmt.Errorf("rates_hz entry %v: want an integer 1000..50000000", x))
			}
			rates = append(rates, f)
		}
	}
	if err := d.detach(); err != nil {
		return nil, err
	}
	const reps, cycles = 5, 20000
	median := func(f func() error) (time.Duration, error) {
		var ts []time.Duration
		for i := 0; i < reps; i++ {
			t0 := time.Now()
			if err := f(); err != nil {
				return 0, err
			}
			ts = append(ts, time.Since(t0))
		}
		sort.Slice(ts, func(i, j int) bool { return ts[i] < ts[j] })
		return ts[reps/2], nil
	}
	if _, err := d.probe.SetClock(14000); err != nil {
		return nil, err
	}
	rtt, err := median(func() error { return d.probe.CycleTCK(1) })
	if err != nil {
		return nil, err
	}
	var out []any
	for _, hz := range rates {
		row := map[string]any{"requested_hz": hz}
		plan, err := d.probe.SetClock(uint32(hz / 1000))
		if err != nil {
			if c := classify(err); IsDeviceGone(c) {
				return nil, err
			}
			row["accepted"], row["error"] = false, err.Error()
			out = append(out, row)
			continue
		}
		row["accepted"], row["delay_count"] = true, float64(plan.Delay)
		row["nominal_hz"], row["clamped"] = float64(plan.NominalKHz)*1000, plan.Clamped
		burst, err := median(func() error { return d.probe.CycleTCK(cycles) })
		if err != nil {
			return nil, err
		}
		ideal := time.Duration(float64(cycles) * float64(time.Second) / (float64(plan.NominalKHz) * 1000))
		row["burst_us"], row["ideal_us"] = float64(burst.Microseconds()), float64(ideal.Microseconds())
		resolvable := ideal > 3*rtt
		row["timing_resolvable"] = resolvable
		if resolvable {
			if excess := burst - rtt; excess > 0 {
				row["implied_hz"] = float64(cycles) * float64(time.Second) / float64(excess)
			}
		}
		out = append(out, row)
	}
	return map[string]any{
		"clocks": out, "cycles_per_burst": float64(cycles), "usb_round_trip_us": float64(rtt.Microseconds()),
		"note": "the probe answers OK/ERROR only; an accepted value is not proof of that exact rate. nominal_hz is what the encoding is meant to give. Bursts not clearly longer than a USB round trip cannot be measured from the host",
	}, nil
}

func (d *device) jtagScan(ctx context.Context, o options, a map[string]any) (map[string]any, error) {
	max, err := optInt(a, "max_devices", 8, 1, 32)
	if err != nil {
		return nil, err
	}
	if err := d.detach(); err != nil {
		return nil, err
	}
	clk := o.clockHz
	if clk > 1_000_000 {
		clk = 1_000_000
	}
	if _, err := d.probe.SetClock(clk / 1000); err != nil {
		return nil, err
	}
	if err := d.probe.CJTAGConnect(); err != nil {
		return nil, err
	}
	defer func() { _ = d.probe.CJTAGDisconnect() }()
	chain, err := jtag.ScanChain(ctx, jtag.NewDriver(xds.NewJTAGPort(d.probe)), jtag.ScanOptions{MaxDevices: max})
	if err != nil {
		return nil, err
	}
	res := toMap(chain)
	res["clock_hz"] = float64(clk)
	res["note"] = "TAP enumeration only; debugging a core over JTAG (including TI ICEPick routing) is not implemented"
	return res, nil
}

// targetPower switches the probe's target supply. It is the only method that
// can put voltage on the connector, so it is refused unless the device was
// opened with allow_target_power and, to enable, the exact confirm phrase is
// given. Disabling needs the device setting but no phrase.
func (d *device) targetPower(o options, a map[string]any) (map[string]any, error) {
	if !o.allowPower {
		return nil, status.Error(codes.PermissionDenied, "target power is disabled: open the device with allow_target_power=true to permit it")
	}
	enable, set, err := optBool(a, "enable")
	if err != nil {
		return nil, argErr(err)
	}
	if !set {
		return nil, argErr(fmt.Errorf("enable is required (true or false)"))
	}
	if !d.probe.Info.SupplyCapable {
		return nil, classify(fmt.Errorf("%w: only the stand-alone XDS110 (hardware id 0x%02x) has one; this probe reports 0x%02x. The LP-XDS110ET 3V3 pin is wired to the probe's rail through jumper P9 and no command changes it; use an external PowerSwitch (docs/adr/0033-power-switch.md)", xds.ErrNoSupply, xds.HWStandAlone, d.probe.Info.Hardware))
	}
	if !enable {
		if err := d.probe.SetSupply(0, false); err != nil {
			return nil, err
		}
		d.supplyOn = false
		return map[string]any{"enabled": false}, nil
	}
	if c, _ := a["confirm"].(string); c != powerConfirm {
		return nil, status.Errorf(codes.PermissionDenied, "enabling the target supply needs confirm=%q", powerConfirm)
	}
	mv, err := optInt(a, "millivolts", 0, 0, 5000)
	if err != nil {
		return nil, err
	}
	if mv < int(xds.SupplyMinMV) || mv > int(xds.SupplyMaxMV) {
		return nil, argErr(fmt.Errorf("millivolts %d: want %d..%d", mv, xds.SupplyMinMV, xds.SupplyMaxMV))
	}
	if err := d.probe.SetSupply(uint32(mv), true); err != nil {
		return nil, err
	}
	d.supplyOn = true
	return map[string]any{"enabled": true, "millivolts": float64(mv), "note": "the probe confirms the command only; the voltage was not measured. hwlab switches it off again when the device is closed"}, nil
}
