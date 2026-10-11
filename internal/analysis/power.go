// Package analysis computes statistics, envelopes and events from stored
// samples. All routines stream over chunks and never load a whole capture.
package analysis

import (
	"context"
	"fmt"
	"math"

	"github.com/faronov/hwlab/internal/session"
)

// PowerSource is the read side of a power stream. session.StreamReader
// implements it.
type PowerSource interface {
	ReadPower(ctx context.Context, from, to int64, fn func(*session.PowerChunk) error) error
	Bounds() (start, end int64)
}

// FormatCurrent renders a current given in nanoamps.
func FormatCurrent(nA float64) string {
	a := math.Abs(nA)
	switch {
	case a >= 1e9:
		return fmt.Sprintf("%.3f A", nA/1e9)
	case a >= 1e6:
		return fmt.Sprintf("%.3f mA", nA/1e6)
	case a >= 1e3:
		return fmt.Sprintf("%.3f µA", nA/1e3)
	}
	return fmt.Sprintf("%.1f nA", nA)
}

// Bucket is one display bucket of an envelope. Downsampling by min/max/mean
// (never by decimation) keeps short spikes visible.
type Bucket struct {
	StartNs int64
	EndNs   int64
	MinNA   float64
	MaxNA   float64
	MeanNA  float64
	N       int
}

// Envelope reduces [from,to) to n buckets.
func Envelope(ctx context.Context, src PowerSource, from, to int64, n int) ([]Bucket, error) {
	if n <= 0 || to <= from {
		return nil, fmt.Errorf("invalid envelope request (n=%d, range=[%d,%d))", n, from, to)
	}
	width := float64(to-from) / float64(n)
	bs := make([]Bucket, n)
	sums := make([]float64, n)
	for i := range bs {
		bs[i] = Bucket{StartNs: from + int64(float64(i)*width), EndNs: from + int64(float64(i+1)*width),
			MinNA: math.Inf(1), MaxNA: math.Inf(-1)}
	}
	err := src.ReadPower(ctx, from, to, func(c *session.PowerChunk) error {
		for i, v := range c.CurrentNA {
			t := c.StartNs + int64(i)*c.PeriodNs
			if t < from || t >= to {
				continue
			}
			bi := int(float64(t-from) / width)
			if bi >= n {
				bi = n - 1
			}
			f := float64(v)
			b := &bs[bi]
			if f < b.MinNA {
				b.MinNA = f
			}
			if f > b.MaxNA {
				b.MaxNA = f
			}
			sums[bi] += f
			b.N++
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for i := range bs {
		if bs[i].N > 0 {
			bs[i].MeanNA = sums[i] / float64(bs[i].N)
		} else {
			bs[i].MinNA, bs[i].MaxNA = math.NaN(), math.NaN()
			bs[i].MeanNA = math.NaN()
		}
	}
	return bs, nil
}

// Stats summarises a time range of a power stream.
type Stats struct {
	StartNs  int64   `json:"start_ns"`
	EndNs    int64   `json:"end_ns"`
	Samples  int64   `json:"samples"`
	MinNA    float64 `json:"min_na"`
	MaxNA    float64 `json:"max_na"`
	MeanNA   float64 `json:"mean_na"`
	PeakAtNs int64   `json:"peak_at_ns"`
	ChargeUC float64 `json:"charge_uc"`
	// EnergyUJ is valid only when HasEnergy: it needs per-sample voltage or
	// a nominal voltage.
	EnergyUJ  float64 `json:"energy_uj"`
	HasEnergy bool    `json:"has_energy"`
}

// ComputeStats integrates a range. nominalV (volts) is used when the stream
// has no voltage samples; 0 disables the energy result in that case.
func ComputeStats(ctx context.Context, src PowerSource, from, to int64, nominalV float64) (Stats, error) {
	st := Stats{StartNs: from, EndNs: to, MinNA: math.Inf(1), MaxNA: math.Inf(-1)}
	var sum, charge, energy float64
	var energySamples int64
	err := src.ReadPower(ctx, from, to, func(c *session.PowerChunk) error {
		dt := float64(c.PeriodNs) * 1e-9
		for i, v := range c.CurrentNA {
			t := c.StartNs + int64(i)*c.PeriodNs
			if t < from || t >= to {
				continue
			}
			f := float64(v)
			if f < st.MinNA {
				st.MinNA = f
			}
			if f > st.MaxNA {
				st.MaxNA, st.PeakAtNs = f, t
			}
			sum += f
			st.Samples++
			amps := f * 1e-9
			charge += amps * dt
			switch {
			case len(c.VoltageUV) == len(c.CurrentNA):
				energy += amps * float64(c.VoltageUV[i]) * 1e-6 * dt
				energySamples++
			case nominalV > 0:
				energy += amps * nominalV * dt
				energySamples++
			}
		}
		return nil
	})
	if err != nil {
		return st, err
	}
	if st.Samples == 0 {
		return Stats{StartNs: from, EndNs: to}, nil
	}
	st.MeanNA = sum / float64(st.Samples)
	st.ChargeUC = charge * 1e6
	st.HasEnergy = energySamples == st.Samples
	if st.HasEnergy {
		st.EnergyUJ = energy * 1e6
	}
	return st, nil
}
