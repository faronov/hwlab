package backend

import (
	"context"
	"math"
)

// Fake is a SYNTHETIC four-channel scope. Nothing it returns was measured: the
// inputs are fixed mathematical signals, so tests and fixtures can check
// measurements against known answers.
//
//	A  square 1 kHz, 0..3.3 V, 50 % duty, 2 µs rise, 4 µs fall
//	B  sine 10 kHz, ±1 V
//	C  DC 1.2 V
//	D  sine 50 Hz, ±15 V (overflows every range up to 10 V)
//
// Limits mimic the way real scopes trade rate and memory for channels and
// resolution, with made-up numbers.
type Fake struct {
	serial string
	closed bool
}

// NewFake returns the synthetic scope.
func NewFake(serial string) *Fake { return &Fake{serial: serial} }

// FakeSerial is the serial number the plugin gives its synthetic device.
const FakeSerial = "SYNTH0001"

// Info implements Backend.
func (f *Fake) Info() Info {
	return Info{Vendor: "hwlab", Model: "SYNTHETIC-PS4000", Serial: f.serial, Firmware: "synthetic", Driver: "synthetic",
		Channels: []string{"A", "B", "C", "D"}, ResolutionsBits: []int{8, 12}, Ranges: Ranges, MaxMemory: 8_000_000, Synthetic: true}
}

// Limits implements Backend.
func (f *Fake) Limits(enabled, bits int) Limits {
	rate := 1e9
	if bits == 12 {
		rate = 250e6
	}
	if enabled > 2 {
		rate /= 2
	}
	return Limits{MaxSampleRateHz: rate, MaxSamples: f.Info().MaxMemory / enabled}
}

// Plan implements Backend.
func (f *Fake) Plan(r Request) (Resolved, error) { return CheckRequest(f.Info(), f.Limits, r) }

// Close implements Backend.
func (f *Fake) Close() error { f.closed = true; return nil }

// signal is the synthetic input voltage on a channel at time t (seconds).
func signal(ch string, t float64) float64 {
	switch ch {
	case "A":
		const period, rise, fall, high = 1e-3, 2e-6, 4e-6, 0.5e-3
		p := math.Mod(t, period)
		switch {
		case p < rise:
			return 3.3 * p / rise
		case p < high:
			return 3.3
		case p < high+fall:
			return 3.3 * (1 - (p-high)/fall)
		}
		return 0
	case "B":
		return math.Sin(2 * math.Pi * 10e3 * t)
	case "C":
		return 1.2
	case "D":
		return 15 * math.Sin(2*math.Pi*50*t)
	}
	return 0
}

// searchStepS is the resolution of the synthetic trigger search; the real
// trigger of a scope works on the analogue signal, not on samples.
const searchStepS = 0.25e-6

const defaultTimeoutS = 1.0

// Acquire implements Backend.
func (f *Fake) Acquire(ctx context.Context, r Request) (*Block, error) {
	res, err := f.Plan(r)
	if err != nil {
		return nil, err
	}
	dt := res.SampleIntervalS
	preSamples := int(math.Round(r.PreTrigger * float64(res.Samples)))
	tStart, trigIdx, triggered := 0.0, 0, false
	if r.Trigger.Enabled {
		var src *Channel
		for i := range r.Channels {
			if r.Channels[i].Name == r.Trigger.Source {
				src = &r.Channels[i]
			}
		}
		timeout := r.Trigger.TimeoutS
		if timeout == 0 {
			timeout = defaultTimeoutS
		}
		first := float64(preSamples) * dt
		prev := signal(src.Name, first)
		deadline := first + timeout
		var tTrig float64
		for t := first + searchStepS; t <= deadline; t += searchStepS {
			if int(t/searchStepS)%4096 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			v := signal(src.Name, t)
			up := prev < r.Trigger.LevelV && v >= r.Trigger.LevelV
			down := prev > r.Trigger.LevelV && v <= r.Trigger.LevelV
			if (r.Trigger.Slope == "rising" && up) || (r.Trigger.Slope == "falling" && down) || (r.Trigger.Slope == "either" && (up || down)) {
				tTrig, triggered = t, true
				break
			}
			prev = v
		}
		switch {
		case triggered:
			trigIdx = preSamples
			tStart = tTrig - float64(preSamples)*dt
		case r.Trigger.Auto:
			// auto timeout: a free-running block, as a real scope does
		default:
			return nil, ErrNoTrigger
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	blk := &Block{SampleIntervalS: dt, Samples: res.Samples, Triggered: triggered, TriggerIndex: trigIdx}
	maxADC := int16(((1 << (r.ResolutionBits - 1)) - 1) << (16 - r.ResolutionBits))
	for _, ch := range r.Channels {
		if !ch.Enabled {
			continue
		}
		cd := ChannelData{Name: ch.Name, MaxADC: maxADC, RangeV: ch.RangeV, OffsetV: ch.OffsetV, ADC: make([]int16, res.Samples)}
		steps := float64(int(1)<<(r.ResolutionBits-1) - 1)
		for i := range cd.ADC {
			v := signal(ch.Name, tStart+float64(i)*dt)
			if ch.Coupling == "ac" {
				v -= acMean(ch.Name)
			}
			x := (v + ch.OffsetV) / ch.RangeV
			if x > 1 || x < -1 {
				cd.Overflow = true
				x = math.Max(-1, math.Min(1, x))
			}
			cd.ADC[i] = int16(math.Round(x*steps)) << (16 - r.ResolutionBits)
		}
		blk.Channels = append(blk.Channels, cd)
	}
	return blk, nil
}

// acMean is the DC component an AC-coupled input removes.
func acMean(ch string) float64 {
	switch ch {
	case "A":
		return 3.3 * (0.5e-3 - 1e-6 + 2e-6) / 1e-3
	case "C":
		return 1.2
	}
	return 0
}
