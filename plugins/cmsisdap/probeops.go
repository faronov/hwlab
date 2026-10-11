package cmsisdapplugin

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/internal/jtag"
	"github.com/faronov/hwlab/plugins/cmsisdap/dap"
)

func toMap(v any) map[string]any {
	b, _ := json.Marshal(v)
	m := map[string]any{}
	_ = json.Unmarshal(b, &m)
	return m
}

// withPort runs f with the probe connected to port, leaving the target
// detached afterwards. A previous SWD attachment is dropped first.
func (d *device) withPort(port byte, f func() error) error {
	if err := d.detach(); err != nil {
		return err
	}
	got, err := d.probe.Connect(port)
	if err != nil {
		return err
	}
	defer func() { _ = d.probe.Disconnect() }()
	if got != port {
		return fmt.Errorf("the probe connected port %d, not %d", got, port)
	}
	return f()
}

var pinNames = []struct {
	bit  byte
	name string
}{{dap.PinSWCLK, "swclk_tck"}, {dap.PinSWDIO, "swdio_tms"}, {1 << 2, "tdi"}, {1 << 3, "tdo"}, {1 << 5, "ntrst"}, {dap.PinnRESET, "nreset"}}

func (d *device) pins() (map[string]any, error) {
	var in byte
	err := d.withPort(dap.PortSWD, func() error {
		var e error
		in, e = d.probe.Pins(0, 0, 0) // select nothing: read only
		return e
	})
	if err != nil {
		return nil, err
	}
	levels := map[string]any{}
	for _, p := range pinNames {
		levels[p.name] = in&p.bit != 0
	}
	return map[string]any{
		"raw": fmt.Sprintf("0x%02x", in), "levels": levels,
		"note": "input levels as the probe sees them; nothing was driven. This does not prove a target is wired",
	}, nil
}

// clockProbe asks for each clock and times a 256-bit SWJ sequence. The probe
// reports only OK/ERROR for DAP_SWJ_Clock, so the timing is the only evidence
// of the effective rate, and it is only meaningful when the sequence takes
// clearly longer than a USB round trip.
func (d *device) clockProbe(a map[string]any) (map[string]any, error) {
	rates := []float64{100_000, 400_000, 1_000_000, 2_000_000, 4_000_000}
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
	ones := make([]byte, 32)
	for i := range ones {
		ones[i] = 0xFF
	}
	const reps = 9
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
	var out []any
	var rtt time.Duration
	err := d.withPort(dap.PortSWD, func() error {
		var e error
		if rtt, e = median(func() error { return d.probe.Delay(1) }); e != nil {
			return e
		}
		for _, hz := range rates {
			row := map[string]any{"requested_hz": hz}
			if err := d.probe.SetClock(uint32(hz)); err != nil {
				if classify(err) != nil && IsDeviceGone(classify(err)) {
					return err
				}
				row["accepted"], row["error"] = false, err.Error()
				out = append(out, row)
				continue
			}
			row["accepted"] = true
			seq, err := median(func() error { return d.probe.SWJSequence(256, ones) })
			if err != nil {
				return err
			}
			ideal := time.Duration(256 * float64(time.Second) / hz)
			row["seq256_us"], row["ideal_us"] = float64(seq.Microseconds()), float64(ideal.Microseconds())
			// Only a transfer much longer than the USB round trip can show
			// the clock; a faster one measures USB latency, not the clock.
			resolvable := ideal > 3*rtt
			row["timing_resolvable"] = resolvable
			if resolvable {
				if excess := seq - rtt; excess > 0 {
					row["implied_hz"] = float64(256 * float64(time.Second) / float64(excess))
				}
			}
			out = append(out, row)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"clocks": out, "usb_round_trip_us": float64(rtt.Microseconds()),
		"note": "DAP_SWJ_Clock only answers OK/ERROR; an accepted value is not proof of that exact rate. Rates whose 256-bit sequence is not clearly longer than a USB round trip cannot be measured from the host",
	}, nil
}

func (d *device) jtagScan(ctx context.Context, o options, a map[string]any) (map[string]any, error) {
	if !d.probe.Info().Caps.JTAG {
		return nil, status.Error(codes.FailedPrecondition, "the probe does not report JTAG support (DAP_Info capabilities)")
	}
	max, err := optInt(a, "max_devices", 8, 1, 32)
	if err != nil {
		return nil, err
	}
	clk := o.clockHz
	if clk > 1_000_000 {
		clk = 1_000_000
	}
	var chain jtag.Chain
	err = d.withPort(dap.PortJTAG, func() error {
		if e := d.probe.SetClock(clk); e != nil {
			return e
		}
		if e := d.probe.Configure(0, 100, 0); e != nil {
			return e
		}
		var e error
		chain, e = jtag.ScanChain(ctx, jtag.NewDriver(dap.JTAGPort{P: d.probe}), jtag.ScanOptions{MaxDevices: max})
		return e
	})
	if err != nil {
		return nil, err
	}
	res := toMap(chain)
	res["clock_hz"] = float64(clk)
	res["note"] = "TAP enumeration only; debugging a core over JTAG is not implemented"
	return res, nil
}
