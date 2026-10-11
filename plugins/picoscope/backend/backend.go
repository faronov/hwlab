// Package backend is the seam between the picoscope plugin and a PicoScope
// driver. The plugin never touches a vendor library: it talks to a Backend, so
// the PicoSDK (a C library that needs CGO or a helper process) can be added
// later without changing the plugin, the capability model or the core.
//
// Only the synthetic backend in this package exists today. No real PicoScope
// has been available, so no hardware backend is provided and none is claimed.
package backend

import (
	"context"
	"errors"
	"fmt"
	"math"
)

// Standard PicoScope input ranges (± full scale, volts).
var Ranges = []float64{0.01, 0.02, 0.05, 0.1, 0.2, 0.5, 1, 2, 5, 10, 20}

// ErrNoTrigger is returned when a triggered block saw no trigger event.
var ErrNoTrigger = errors.New("no trigger event before the timeout")

// Info identifies the instrument and what it can do.
type Info struct {
	Vendor, Model, Serial, Firmware string
	Driver                          string // e.g. "synthetic", "ps5000a"
	Channels                        []string
	ResolutionsBits                 []int
	Ranges                          []float64
	// MaxMemory is the sample memory shared by the enabled channels.
	MaxMemory int
	Synthetic bool
}

// Limits depend on how many channels are enabled and on the resolution, as on
// real scopes.
type Limits struct {
	MaxSampleRateHz float64
	MaxSamples      int // per channel
}

// Channel is the setup of one input.
type Channel struct {
	Name     string
	Enabled  bool
	RangeV   float64 // ± full scale
	OffsetV  float64 // added to the input before digitising
	Coupling string  // "dc" or "ac"
}

// Trigger is a simple edge trigger.
type Trigger struct {
	Enabled bool
	Source  string
	LevelV  float64
	Slope   string // rising, falling, either
	// TimeoutS is how long to wait for the trigger; 0 waits for the backend's
	// default.
	TimeoutS float64
	// Auto returns an untriggered block when the timeout expires instead of
	// failing with ErrNoTrigger.
	Auto bool
}

// Request describes one block acquisition.
type Request struct {
	Channels       []Channel
	ResolutionBits int
	SampleRateHz   float64
	Samples        int     // per channel
	PreTrigger     float64 // fraction of the block before the trigger, 0..1
	Trigger        Trigger
}

// Resolved is what a request becomes after the backend applied its limits.
type Resolved struct {
	SampleIntervalS float64
	SampleRateHz    float64
	Samples         int
	Enabled         int
}

// ChannelData is one channel of a block as the instrument delivers it.
type ChannelData struct {
	Name    string
	ADC     []int16
	MaxADC  int16 // ADC count of +full scale
	RangeV  float64
	OffsetV float64
	// Overflow is set when the input exceeded the range during the block.
	Overflow bool
}

// Volts converts the counts: v = adc/maxADC*range - offset.
func (c ChannelData) Volts() []float32 {
	out := make([]float32, len(c.ADC))
	scale := c.RangeV / float64(c.MaxADC)
	for i, a := range c.ADC {
		out[i] = float32(float64(a)*scale - c.OffsetV)
	}
	return out
}

// Block is one acquisition.
type Block struct {
	SampleIntervalS float64
	Samples         int
	// Triggered is false for a free-running or auto-timeout block;
	// TriggerIndex is the sample at the trigger point when Triggered.
	Triggered    bool
	TriggerIndex int
	Channels     []ChannelData
}

// Backend drives one instrument. Implementations must be safe to call from one
// goroutine at a time; the plugin serialises access.
type Backend interface {
	Info() Info
	Limits(enabled, resolutionBits int) Limits
	// Plan validates a request and reports what the instrument would do.
	Plan(Request) (Resolved, error)
	// Acquire runs one block acquisition and returns when it is complete. It
	// honours ctx cancellation (a trigger wait must be interruptible).
	Acquire(context.Context, Request) (*Block, error)
	Close() error
}

// ValidRange reports whether v is one of the instrument's input ranges.
func ValidRange(v float64, ranges []float64) bool {
	for _, r := range ranges {
		if math.Abs(r-v) < 1e-12 {
			return true
		}
	}
	return false
}

// CheckRequest is the validation every backend shares.
func CheckRequest(info Info, lim func(enabled, bits int) Limits, r Request) (Resolved, error) {
	enabled := 0
	for _, ch := range r.Channels {
		if !ch.Enabled {
			continue
		}
		enabled++
		if !ValidRange(ch.RangeV, info.Ranges) {
			return Resolved{}, fmt.Errorf("channel %s: range %g V is not one of %v", ch.Name, ch.RangeV, info.Ranges)
		}
		if math.Abs(ch.OffsetV) > ch.RangeV {
			return Resolved{}, fmt.Errorf("channel %s: offset %g V exceeds the ±%g V range", ch.Name, ch.OffsetV, ch.RangeV)
		}
		if ch.Coupling != "dc" && ch.Coupling != "ac" {
			return Resolved{}, fmt.Errorf("channel %s: coupling %q: want dc or ac", ch.Name, ch.Coupling)
		}
	}
	if enabled == 0 {
		return Resolved{}, errors.New("no channel is enabled")
	}
	okBits := false
	for _, b := range info.ResolutionsBits {
		okBits = okBits || b == r.ResolutionBits
	}
	if !okBits {
		return Resolved{}, fmt.Errorf("resolution %d bit: want one of %v", r.ResolutionBits, info.ResolutionsBits)
	}
	l := lim(enabled, r.ResolutionBits)
	if !(r.SampleRateHz > 0) || math.IsInf(r.SampleRateHz, 0) {
		return Resolved{}, fmt.Errorf("sample rate %g Hz is not positive", r.SampleRateHz)
	}
	if r.Samples < 1 {
		return Resolved{}, fmt.Errorf("samples %d: want at least 1", r.Samples)
	}
	if r.Samples > l.MaxSamples {
		return Resolved{}, fmt.Errorf("samples %d exceed the %d the instrument can hold per channel with %d channel(s) at %d bit", r.Samples, l.MaxSamples, enabled, r.ResolutionBits)
	}
	if r.PreTrigger < 0 || r.PreTrigger > 1 {
		return Resolved{}, fmt.Errorf("pre_trigger %g: want a fraction 0..1", r.PreTrigger)
	}
	if t := r.Trigger; t.Enabled {
		found := false
		for _, ch := range r.Channels {
			found = found || (ch.Name == t.Source && ch.Enabled)
		}
		if !found {
			return Resolved{}, fmt.Errorf("trigger source %q is not an enabled channel", t.Source)
		}
		switch t.Slope {
		case "rising", "falling", "either":
		default:
			return Resolved{}, fmt.Errorf("trigger slope %q: want rising, falling or either", t.Slope)
		}
		if t.TimeoutS < 0 {
			return Resolved{}, errors.New("trigger timeout must not be negative")
		}
	}
	// The achieved rate is the nearest rate the grid offers, never above the maximum.
	grid := l.MaxSampleRateHz
	div := math.Max(1, math.Round(grid/r.SampleRateHz))
	rate := grid / div
	return Resolved{SampleIntervalS: 1 / rate, SampleRateHz: rate, Samples: r.Samples, Enabled: enabled}, nil
}
