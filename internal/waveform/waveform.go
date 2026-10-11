// Package waveform measures evenly sampled analog traces: the summary numbers
// an engineer reads off an oscilloscope (min/max/mean/RMS, frequency, pulse
// width, rise and fall time) and a min/max envelope small enough for a chart
// or an LLM. It is vendor independent and works on live acquisitions and on
// recorded sessions alike.
//
// Timing metrics come from the sample grid with linear interpolation between
// samples; they cannot be better than the trace's own time base and are not a
// substitute for a calibrated instrument.
package waveform

import (
	"fmt"
	"math"
)

// Trace is one evenly sampled channel. Times are seconds on the trace's own
// axis; sample i is at T0 + i*DT.
type Trace struct {
	T0 float64
	DT float64
	V  []float32
}

// End is the time one sample period after the last sample.
func (t Trace) End() float64 { return t.T0 + float64(len(t.V))*t.DT }

// Window returns the samples with from <= time < to (to <= from: to the end).
func (t Trace) Window(from, to float64) Trace {
	if t.DT <= 0 || len(t.V) == 0 {
		return Trace{T0: t.T0, DT: t.DT}
	}
	lo := 0
	if from > t.T0 {
		lo = int(math.Ceil((from - t.T0) / t.DT))
	}
	hi := len(t.V)
	if to > from {
		hi = int(math.Ceil((to - t.T0) / t.DT))
	}
	if lo < 0 {
		lo = 0
	}
	if hi > len(t.V) {
		hi = len(t.V)
	}
	if lo >= hi {
		return Trace{T0: t.T0 + float64(lo)*t.DT, DT: t.DT}
	}
	return Trace{T0: t.T0 + float64(lo)*t.DT, DT: t.DT, V: t.V[lo:hi]}
}

// Measurements are the metrics of one trace window. Optional metrics are nil
// when they cannot be determined, with the reason in Notes.
type Measurements struct {
	Samples      int     `json:"samples"`
	StartS       float64 `json:"start_s"`
	DurationS    float64 `json:"duration_s"`
	SampleRateHz float64 `json:"sample_rate_hz"`

	Min    float64 `json:"min"`
	Max    float64 `json:"max"`
	Mean   float64 `json:"mean"`
	RMS    float64 `json:"rms"`
	Vpp    float64 `json:"vpp"`
	MinAtS float64 `json:"min_at_s"`
	MaxAtS float64 `json:"max_at_s"`

	// Base and Top are the two most common levels of a two-level signal (the
	// modes of the lower and upper half of the histogram), the reference for
	// the edge metrics. Nil when the signal does not swing.
	Base *float64 `json:"base,omitempty"`
	Top  *float64 `json:"top,omitempty"`

	RisingEdges  int `json:"rising_edges"`
	FallingEdges int `json:"falling_edges"`

	Cycles      int      `json:"cycles,omitempty"`
	PeriodS     *float64 `json:"period_s,omitempty"`
	FrequencyHz *float64 `json:"frequency_hz,omitempty"`
	// PulseHighS and PulseLowS are mean widths measured between 50 % crossings
	// (complete pulses only).
	PulseHighS *float64 `json:"pulse_high_s,omitempty"`
	PulseLowS  *float64 `json:"pulse_low_s,omitempty"`
	Duty       *float64 `json:"duty,omitempty"`
	// RiseTimeS and FallTimeS are means of the 10 %-90 % transition time over
	// the complete edges (RiseEdgesUsed/FallEdgesUsed of them).
	RiseTimeS     *float64 `json:"rise_time_s,omitempty"`
	FallTimeS     *float64 `json:"fall_time_s,omitempty"`
	RiseEdgesUsed int      `json:"rise_edges_used,omitempty"`
	FallEdgesUsed int      `json:"fall_edges_used,omitempty"`

	Notes []string `json:"notes,omitempty"`
}

// MinSwing is the smallest peak-to-peak (in the trace's unit) treated as a
// signal when looking for edges; below it the trace is considered flat.
const MinSwing = 1e-9

func ptr(v float64) *float64 { return &v }

// Measure computes the metrics of the trace. NaN and infinite samples are
// refused, because they would silently poison every statistic.
func Measure(t Trace) (*Measurements, error) {
	n := len(t.V)
	if n == 0 {
		return nil, fmt.Errorf("the window holds no samples")
	}
	if !(t.DT > 0) || math.IsInf(t.DT, 0) {
		return nil, fmt.Errorf("sample period %v is not positive", t.DT)
	}
	m := &Measurements{Samples: n, StartS: t.T0, DurationS: float64(n) * t.DT, SampleRateHz: 1 / t.DT}
	m.Min, m.Max = math.Inf(1), math.Inf(-1)
	var sum, sq float64
	minI, maxI := 0, 0
	for i, f := range t.V {
		v := float64(f)
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, fmt.Errorf("sample %d is not finite", i)
		}
		if v < m.Min {
			m.Min, minI = v, i
		}
		if v > m.Max {
			m.Max, maxI = v, i
		}
		sum += v
		sq += v * v
	}
	m.Mean = sum / float64(n)
	m.RMS = math.Sqrt(sq / float64(n))
	m.Vpp = m.Max - m.Min
	m.MinAtS, m.MaxAtS = t.T0+float64(minI)*t.DT, t.T0+float64(maxI)*t.DT

	if m.Vpp < MinSwing {
		m.Notes = append(m.Notes, "the signal does not swing: no edge, frequency or pulse metrics")
		return m, nil
	}
	if n < 3 {
		m.Notes = append(m.Notes, "too few samples for edge metrics")
		return m, nil
	}
	base, top := levels(t.V, m.Min, m.Max)
	m.Base, m.Top = ptr(base), ptr(top)
	edges(t, m, base, top)
	return m, nil
}

// levels finds the modal low and high level with a 256-bin histogram.
func levels(v []float32, lo, hi float64) (base, top float64) {
	const bins = 256
	var h [bins]int
	span := hi - lo
	for _, f := range v {
		b := int((float64(f) - lo) / span * bins)
		if b >= bins {
			b = bins - 1
		}
		h[b]++
	}
	mode := func(from, to int) int {
		best := from
		for b := from; b < to; b++ {
			if h[b] > h[best] {
				best = b
			}
		}
		return best
	}
	centre := func(b int) float64 { return lo + (float64(b)+0.5)/bins*span }
	return centre(mode(0, bins/2)), centre(mode(bins/2, bins))
}

type crossing struct {
	t      float64
	idx    int // sample at or just before the 50 % crossing
	rising bool
}

// edges finds Schmitt-trigger transitions (mid level, 10 % hysteresis of the
// swing) and derives the timing metrics.
func edges(t Trace, m *Measurements, base, top float64) {
	amp := top - base
	if amp < MinSwing {
		m.Notes = append(m.Notes, "the two signal levels coincide: no edge metrics")
		return
	}
	mid := base + amp/2
	hiTh, loTh := mid+amp*0.1, mid-amp*0.1
	at := func(i int) float64 { return float64(t.V[i]) }

	// crossTime interpolates the time at which the trace passes level between
	// samples j and j+1.
	crossTime := func(j int, level float64) float64 {
		a, b := at(j), at(j+1)
		f := 0.0
		if b != a {
			f = (level - a) / (b - a)
		}
		return t.T0 + (float64(j)+f)*t.DT
	}

	var cs []crossing
	state := 0 // 0 unknown, -1 low, +1 high
	for i, f := range t.V {
		v := float64(f)
		switch {
		case v >= hiTh && state != 1:
			if state == -1 {
				j := i - 1
				for j > 0 && at(j) >= mid {
					j--
				}
				cs = append(cs, crossing{t: crossTime(j, mid), idx: j, rising: true})
			}
			state = 1
		case v <= loTh && state != -1:
			if state == 1 {
				j := i - 1
				for j > 0 && at(j) <= mid {
					j--
				}
				cs = append(cs, crossing{t: crossTime(j, mid), idx: j, rising: false})
			}
			state = -1
		}
	}

	var rises, highs, lows []float64
	for i, c := range cs {
		if c.rising {
			m.RisingEdges++
			rises = append(rises, c.t)
		} else {
			m.FallingEdges++
		}
		if i == 0 {
			continue
		}
		prev := cs[i-1]
		switch {
		case prev.rising && !c.rising:
			highs = append(highs, c.t-prev.t)
		case !prev.rising && c.rising:
			lows = append(lows, c.t-prev.t)
		}
	}
	if len(rises) >= 2 {
		period := (rises[len(rises)-1] - rises[0]) / float64(len(rises)-1)
		m.Cycles = len(rises) - 1
		m.PeriodS, m.FrequencyHz = ptr(period), ptr(1/period)
		if len(rises) < 3 {
			m.Notes = append(m.Notes, "frequency comes from a single period")
		}
	} else if m.RisingEdges+m.FallingEdges > 0 {
		m.Notes = append(m.Notes, "fewer than two rising edges in the window: no frequency")
	}
	if len(highs) > 0 {
		m.PulseHighS = ptr(mean(highs))
	}
	if len(lows) > 0 {
		m.PulseLowS = ptr(mean(lows))
	}
	switch {
	case m.PulseHighS != nil && m.PulseLowS != nil:
		m.Duty = ptr(*m.PulseHighS / (*m.PulseHighS + *m.PulseLowS))
	case m.PulseHighS != nil && m.PeriodS != nil:
		m.Duty = ptr(*m.PulseHighS / *m.PeriodS)
	}

	// 10 %-90 % transition times of complete edges: walk from the mid crossing
	// back to the starting level and forward to the ending level.
	t10, t90 := base+0.1*amp, base+0.9*amp
	var rt, ft []float64
	for _, c := range cs {
		start, end := t10, t90
		if !c.rising {
			start, end = t90, t10
		}
		// beyondStart: v is still on the far side of the edge's start level;
		// beforeEnd: v has not yet reached the edge's end level.
		beyondStart := func(v float64) bool {
			if c.rising {
				return v > start
			}
			return v < start
		}
		beforeEnd := func(v float64) bool {
			if c.rising {
				return v < end
			}
			return v > end
		}
		a := c.idx
		for a > 0 && beyondStart(at(a)) {
			a--
		}
		if beyondStart(at(a)) {
			continue // the edge starts before the window
		}
		b := c.idx + 1
		for b < len(t.V)-1 && beforeEnd(at(b)) {
			b++
		}
		if beforeEnd(at(b)) {
			continue // the edge ends after the window
		}
		ta, tb := crossTime(a, start), crossTime(b-1, end)
		if tb < ta {
			continue
		}
		if c.rising {
			rt = append(rt, tb-ta)
		} else {
			ft = append(ft, tb-ta)
		}
	}
	if len(rt) > 0 {
		m.RiseTimeS, m.RiseEdgesUsed = ptr(mean(rt)), len(rt)
	}
	if len(ft) > 0 {
		m.FallTimeS, m.FallEdgesUsed = ptr(mean(ft)), len(ft)
	}
	if (m.RisingEdges > 0 && len(rt) == 0) || (m.FallingEdges > 0 && len(ft) == 0) {
		m.Notes = append(m.Notes, "no complete edge in the window for a rise or fall time")
	}
	for _, v := range []*float64{m.RiseTimeS, m.FallTimeS} {
		if v != nil && *v < 3*t.DT {
			m.Notes = append(m.Notes, fmt.Sprintf("an edge spans fewer than 3 samples (%.3g s period): the rise/fall time is limited by the sample rate", t.DT))
			break
		}
	}
}

func mean(v []float64) float64 {
	s := 0.0
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}

// Envelope is a min/max reduction of a trace to at most len(Min) buckets.
type Envelope struct {
	T0       float64   `json:"t0_s"`
	BucketDT float64   `json:"bucket_dt_s"`
	Min      []float64 `json:"min"`
	Max      []float64 `json:"max"`
}

// MakeEnvelope reduces the trace to at most n buckets (n >= 1).
func MakeEnvelope(t Trace, n int) Envelope {
	if n < 1 {
		n = 1
	}
	total := len(t.V)
	if total == 0 {
		return Envelope{T0: t.T0}
	}
	if n > total {
		n = total
	}
	e := Envelope{T0: t.T0, BucketDT: t.DT * float64(total) / float64(n), Min: make([]float64, n), Max: make([]float64, n)}
	for b := 0; b < n; b++ {
		lo, hi := b*total/n, (b+1)*total/n
		mn, mx := math.Inf(1), math.Inf(-1)
		for _, v := range t.V[lo:hi] {
			mn, mx = math.Min(mn, float64(v)), math.Max(mx, float64(v))
		}
		e.Min[b], e.Max[b] = mn, mx
	}
	return e
}
