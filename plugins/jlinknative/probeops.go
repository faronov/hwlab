package jlinknativeplugin

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/internal/jtag"
	"github.com/faronov/hwlab/plugins/jlinknative/jtagport"
	"github.com/faronov/hwlab/plugins/jlinknative/protocol"
	jusb "github.com/faronov/hwlab/plugins/jlinknative/usb"
)

// Feature statuses. They are claims about this backend and must stay in step
// with docs/notes/phase-native-jlink-results.md.
const (
	StatusRealHW         = "REAL_HW_VERIFIED"
	StatusUntested       = "IMPLEMENTED_UNTESTED"
	StatusSimulated      = "SIMULATED_ONLY"
	StatusUnsupportedHW  = "UNSUPPORTED_BY_ATTACHED_HW"
	StatusNotImplemented = "NOT_IMPLEMENTED"
)

// Features is the per-feature status of the native backend.
func Features() map[string]string {
	return map[string]string{
		"usb_discovery":       StatusRealHW,
		"serial_selection":    StatusRealHW,
		"probe_info":          StatusRealHW,
		"target_voltage":      StatusRealHW,
		"swd_transport":       StatusRealHW,
		"swd_clock":           StatusRealHW,
		"jtag_transport":      StatusUntested,
		"reset_pin":           StatusUnsupportedHW,
		"reconnect":           StatusRealHW,
		"multi_probe":         StatusUntested,
		"cortexm_via_swd":     StatusRealHW,
		"rtt":                 StatusRealHW,
		"trace_swo":           StatusNotImplemented,
		"flash_programming":   StatusNotImplemented,
		"notes":               "reconnect = after target reset and after a physical USB unplug/replug (same serial); rtt = target-memory RTT up-channel read, byte-identical to jlink-segger on the EFR32 (rtt_write is simulator-tested only); multi_probe is deferred (one probe attached); jtag_transport was run against a modelled chain only",
		"unsupported_probe_*": "the probe's other commands are not implemented; unknown values are kept in diagnostics",
	}
}

func (d *device) info(ctx context.Context) (map[string]any, error) {
	c := d.client
	caps, err := c.Caps(ctx)
	if err != nil {
		return nil, err
	}
	ext, err := c.ExtCaps(ctx)
	if err != nil {
		return nil, err
	}
	hw, err := c.HWVersion(ctx)
	if err != nil {
		return nil, err
	}
	st, err := c.HWStatus(ctx)
	if err != nil {
		return nil, err
	}
	mask, err := c.Interfaces(ctx)
	if err != nil {
		return nil, err
	}
	sp, err := c.Speeds(ctx)
	if err != nil {
		return nil, err
	}
	var ifaces []any
	for _, i := range []protocol.Interface{protocol.IfJTAG, protocol.IfSWD} {
		if mask&(1<<i) != 0 {
			ifaces = append(ifaces, i.String())
		}
	}
	res := map[string]any{
		"backend": "jlink-native", "transport": d.tr.Describe(),
		"firmware": d.version.Raw, "firmware_full": d.version.Full,
		"hardware_version": hw.String(), "hardware_version_raw": float64(hw.Raw),
		"capabilities_raw": fmt.Sprintf("0x%08x", caps), "ext_capabilities_raw": hex.EncodeToString(ext[:]),
		"target_voltage_mv": float64(st.VTargetMV), "hw_status_raw": hex.EncodeToString(st.Raw[:]),
		"interfaces_mask": fmt.Sprintf("0x%x", mask), "interfaces_offered": ifaces,
		"speed_base_hz": float64(sp.BaseHz), "speed_min_divider": float64(sp.MinDivider), "speed_max_khz": float64(sp.MaxKHz()),
		"capability_bits_note": "the individual capability bits are reported raw: their names were not established independently",
		"features":             featuresAny(),
		"validated":            d.entry.Sim == nil, "synthetic": d.entry.Sim != nil,
		"validated_scope": "one SAM-ICE class probe (J-Link V9, HW 9.70) wired to an EFR32MG1P only; see features for the per-capability status",
	}
	if u := d.entry.USB; u != nil {
		res["usb_serial"] = u.Serial
		res["usb_interface"], res["endpoint_out"], res["endpoint_in"], res["max_packet"] = float64(u.Interface), fmt.Sprintf("0x%02x", u.Out), fmt.Sprintf("0x%02x", u.In), float64(u.MaxPacket)
	}
	if unk := c.Unknown(); len(unk) > 0 {
		list := make([]any, len(unk))
		for i, s := range unk {
			list[i] = s
		}
		res["unknown_values"] = list
	}
	return res, nil
}

func featuresAny() map[string]any {
	out := map[string]any{}
	for k, v := range Features() {
		out[k] = v
	}
	return out
}

// clockProbe programs each rate and times an input-only sequence, so nothing
// is driven on the target. The programmed value is what the probe was told;
// the timing shows what it did, limited by the USB round trip.
func (d *device) clockProbe(ctx context.Context, a map[string]any) (map[string]any, error) {
	rates := []float64{100e3, 400e3, 1e6, 2e6, 4e6, 8e6}
	if v, ok := a["rates_hz"].([]any); ok && len(v) > 0 {
		rates = rates[:0]
		for _, x := range v {
			f, ok := x.(float64)
			if !ok || f < 1000 || f != float64(uint32(f)) {
				return nil, argErr(fmt.Errorf("rates_hz: want whole numbers of Hz >= 1000, got %v", x))
			}
			rates = append(rates, f)
		}
	}
	if _, err := d.client.SelectInterface(ctx, protocol.IfSWD); err != nil {
		return nil, err
	}
	sp, err := d.client.Speeds(ctx)
	if err != nil {
		return nil, err
	}
	const bits = protocol.MaxSequenceBits
	zero := make([]byte, (bits+7)/8)
	small := make([]byte, 1)
	seq := protocol.Sequence{Bits: bits, A: zero, B: zero}
	base := protocol.Sequence{Bits: 8, A: small, B: small}
	var rows []any
	for _, r := range rates {
		khz := uint32(r) / 1000
		row := map[string]any{"requested_hz": r}
		if max := sp.MaxKHz(); khz > max {
			row["accepted"], row["error"] = false, fmt.Sprintf("above the probe's %d kHz maximum; not programmed", max)
			rows = append(rows, row)
			continue
		}
		if err := d.client.SetSpeed(ctx, uint16(khz)); err != nil {
			return nil, err
		}
		timeIt := func(s protocol.Sequence, n int) (time.Duration, error) {
			t0 := time.Now()
			for i := 0; i < n; i++ {
				if _, err := d.client.Run(ctx, s); err != nil {
					return 0, err
				}
			}
			return time.Since(t0) / time.Duration(n), nil
		}
		rtt, err := timeIt(base, 10)
		if err != nil {
			return nil, err
		}
		long, err := timeIt(seq, 5)
		if err != nil {
			return nil, err
		}
		row["programmed_khz"] = float64(khz)
		row["roundtrip_8bit_us"], row["sequence_bits"], row["sequence_us"] = float64(rtt.Microseconds()), float64(bits), float64(long.Microseconds())
		wire := time.Duration(float64(bits) / r * float64(time.Second))
		row["expected_wire_us"] = float64(wire.Microseconds())
		if long > rtt && long-rtt > 0 {
			eff := float64(bits) / (long - rtt).Seconds()
			row["effective_hz"] = eff
		}
		row["distinguishable"] = wire > 3*rtt
		if wire <= 3*rtt {
			row["note"] = "the wire time is below the USB round trip: this rate cannot be told from the timing"
		}
		rows = append(rows, row)
	}
	// Leave the probe at a defined, safe rate.
	_ = d.client.SetSpeed(ctx, 1000)
	return map[string]any{"rates": rows, "speed_max_khz": float64(sp.MaxKHz()), "basis": "input-only 8192-bit sequences; the programmed value is the probe's configuration, not a measurement of SWCLK"}, nil
}

// jtagScan selects JTAG, resets the TAPs and enumerates the chain through the
// shared internal/jtag driver. The SWD attachment is dropped first and the
// probe is put back on SWD afterwards. IMPLEMENTED_UNTESTED: the vectors were
// only exercised against a modelled chain.
func (d *device) jtagScan(ctx context.Context, a map[string]any) (map[string]any, error) {
	max := 8
	if v, ok := a["max_devices"]; ok {
		f, isNum := v.(float64)
		if !isNum || f < 1 || f > 64 || f != float64(int(f)) {
			return nil, argErr(fmt.Errorf("max_devices %v: want a whole number 1..64", v))
		}
		max = int(f)
	}
	khz := uint32(1000)
	if v, ok := a["clock_hz"]; ok {
		f, isNum := v.(float64)
		if !isNum || f < 1000 || f != float64(uint32(f)) {
			return nil, argErr(fmt.Errorf("clock_hz %v: want whole Hz >= 1000", v))
		}
		khz = uint32(f) / 1000
	}
	if err := d.detach(); err != nil {
		return nil, err
	}
	defer func() { _, _ = d.client.SelectInterface(context.WithoutCancel(ctx), protocol.IfSWD) }()
	if _, err := d.client.SelectInterface(ctx, protocol.IfJTAG); err != nil {
		return nil, classify(err)
	}
	if err := d.setClock(ctx, khz*1000); err != nil {
		return nil, classify(err)
	}
	chain, err := jtag.ScanChain(ctx, jtag.NewDriver(jtagport.New(d.client)), jtag.ScanOptions{MaxDevices: max, MeasureIR: true})
	if err != nil {
		return nil, classify(err)
	}
	b, err := json.Marshal(chain)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	var res map[string]any
	if err := json.Unmarshal(b, &res); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	res["clock_khz"] = float64(khz)
	res["status_of_feature"] = StatusUntested
	res["note"] = "TAP enumeration only; no JTAG target was available to verify the probe's JTAG vectors on hardware"
	return res, nil
}

var _ = sort.Strings
var _ jusb.Transport
