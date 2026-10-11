package scpi

import (
	"fmt"
	"io"
	"math"
	"strconv"
)

// Waveform is one acquired trace in volts.
type Waveform struct {
	Channel int
	Source  string  // the instrument's own name for the channel, e.g. CHAN1
	T0      float64 // time of sample 0 in seconds relative to the instrument's time reference
	DT      float64 // seconds between samples
	Volts   []float32
	// Note records assumptions made while scaling (unverified time reference...).
	Note string
}

// Stats summarises a waveform.
type Stats struct {
	N                        int
	Min, Max, Mean, RMS, Vpp float64
}

// Stats computes the summary in one pass.
func (w *Waveform) Stats() Stats {
	s := Stats{N: len(w.Volts)}
	if s.N == 0 {
		return s
	}
	s.Min, s.Max = math.Inf(1), math.Inf(-1)
	var sum, sq float64
	for _, v32 := range w.Volts {
		v := float64(v32)
		s.Min, s.Max = math.Min(s.Min, v), math.Max(s.Max, v)
		sum += v
		sq += v * v
	}
	s.Mean = sum / float64(s.N)
	s.RMS = math.Sqrt(sq / float64(s.N))
	s.Vpp = s.Max - s.Min
	return s
}

// Envelope is a min/max reduction of a waveform to at most len(Min) buckets,
// small enough to hand to an LLM or a chart.
type Envelope struct {
	T0, BucketDT float64
	Min, Max     []float64
}

// Envelope reduces the waveform to at most n buckets (n >= 1).
func (w *Waveform) Envelope(n int) Envelope {
	if n < 1 {
		n = 1
	}
	total := len(w.Volts)
	if total == 0 {
		return Envelope{T0: w.T0}
	}
	if n > total {
		n = total
	}
	e := Envelope{T0: w.T0, BucketDT: w.DT * float64(total) / float64(n), Min: make([]float64, n), Max: make([]float64, n)}
	for b := 0; b < n; b++ {
		lo, hi := b*total/n, (b+1)*total/n
		mn, mx := math.Inf(1), math.Inf(-1)
		for _, v := range w.Volts[lo:hi] {
			mn, mx = math.Min(mn, float64(v)), math.Max(mx, float64(v))
		}
		e.Min[b], e.Max[b] = mn, mx
	}
	return e
}

// WriteCSV writes "time_s,volts" rows.
func (w *Waveform) WriteCSV(out io.Writer) error {
	if _, err := fmt.Fprintf(out, "time_s,ch%d_volts\n", w.Channel); err != nil {
		return err
	}
	buf := make([]byte, 0, 64)
	for i, v := range w.Volts {
		buf = strconv.AppendFloat(buf[:0], w.T0+float64(i)*w.DT, 'g', 9, 64)
		buf = append(buf, ',')
		buf = strconv.AppendFloat(buf, float64(v), 'g', 7, 32)
		buf = append(buf, '\n')
		if _, err := out.Write(buf); err != nil {
			return err
		}
	}
	return nil
}
