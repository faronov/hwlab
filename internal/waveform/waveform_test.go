package waveform

import (
	"math"
	"testing"
)

func near(t *testing.T, name string, got *float64, want, tol float64) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s: not measured, want %g", name, want)
	}
	if math.Abs(*got-want) > tol {
		t.Fatalf("%s = %g, want %g ± %g", name, *got, want, tol)
	}
}

func sine(n int, dt, freq, amp, offset float64) Trace {
	v := make([]float32, n)
	for i := range v {
		v[i] = float32(offset + amp*math.Sin(2*math.Pi*freq*float64(i)*dt))
	}
	return Trace{DT: dt, V: v}
}

// trapezoid is a periodic pulse with linear edges: low, rise, high, fall.
func trapezoid(n int, dt, period, rise, fall, duty, lo, hi float64) Trace {
	v := make([]float32, n)
	for i := range v {
		p := math.Mod(float64(i)*dt, period)
		highEnd := period * duty
		var x float64
		switch {
		case p < rise:
			x = p / rise
		case p < highEnd:
			x = 1
		case p < highEnd+fall:
			x = 1 - (p-highEnd)/fall
		}
		v[i] = float32(lo + (hi-lo)*x)
	}
	return Trace{DT: dt, V: v}
}

func TestSineMetrics(t *testing.T) {
	tr := sine(100000, 1e-6, 1000, 1.0, 0.5) // 100 ms: 100 cycles
	m, err := Measure(tr)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(m.Mean-0.5) > 1e-3 || math.Abs(m.Vpp-2) > 1e-3 || math.Abs(m.Max-1.5) > 1e-3 || math.Abs(m.Min+0.5) > 1e-3 {
		t.Fatalf("level stats %+v", m)
	}
	wantRMS := math.Sqrt(0.5*0.5 + 0.5)
	if math.Abs(m.RMS-wantRMS) > 1e-3 {
		t.Fatalf("rms %g want %g", m.RMS, wantRMS)
	}
	near(t, "frequency", m.FrequencyHz, 1000, 0.5)
	if m.Cycles < 98 || m.Cycles > 100 {
		t.Fatalf("cycles %d", m.Cycles)
	}
	near(t, "duty", m.Duty, 0.5, 0.01)
}

func TestSquareEdgesAndPulses(t *testing.T) {
	// 100 kHz, 30 % duty, linear 1 µs rise and 2 µs fall, 0..3.3 V, 10 MS/s.
	tr := trapezoid(10000, 1e-7, 10e-6, 1e-6, 2e-6, 0.3, 0, 3.3)
	m, err := Measure(tr)
	if err != nil {
		t.Fatal(err)
	}
	near(t, "frequency", m.FrequencyHz, 100e3, 100)
	// 10 %-90 % of a linear edge is 80 % of its duration.
	near(t, "rise", m.RiseTimeS, 0.8e-6, 0.02e-6)
	near(t, "fall", m.FallTimeS, 1.6e-6, 0.03e-6)
	// 50 % crossings: rise mid 0.5 µs, high until 3 µs + 1 µs (fall mid at highEnd+1µs)
	near(t, "pulse high", m.PulseHighS, 3.5e-6, 0.05e-6)
	near(t, "duty", m.Duty, 0.35, 0.01)
	near(t, "base", m.Base, 0, 0.02)
	near(t, "top", m.Top, 3.3, 0.02)
	if m.RisingEdges < 99 || m.FallingEdges < 99 || m.RiseEdgesUsed < 98 {
		t.Fatalf("edges %+v", m)
	}
}

func TestFlatAndErrors(t *testing.T) {
	m, err := Measure(Trace{DT: 1e-3, V: []float32{1.2, 1.2, 1.2, 1.2}})
	if err != nil || m.FrequencyHz != nil || m.Base != nil || len(m.Notes) == 0 {
		t.Fatalf("flat %+v %v", m, err)
	}
	if m.Mean != float64(float32(1.2)) || m.Vpp != 0 {
		t.Fatalf("flat stats %+v", m)
	}
	if _, err := Measure(Trace{DT: 1e-3}); err == nil {
		t.Fatal("empty window must be an error")
	}
	if _, err := Measure(Trace{DT: 0, V: []float32{1}}); err == nil {
		t.Fatal("zero period must be an error")
	}
	if _, err := Measure(Trace{DT: 1, V: []float32{1, float32(math.NaN())}}); err == nil {
		t.Fatal("NaN must be refused")
	}
}

func TestSingleEdgeHasNoFrequencyAndPartialEdgesAreSkipped(t *testing.T) {
	v := make([]float32, 100)
	for i := 50; i < 100; i++ {
		v[i] = 1
	}
	m, err := Measure(Trace{DT: 1e-6, V: v})
	if err != nil {
		t.Fatal(err)
	}
	if m.RisingEdges != 1 || m.FrequencyHz != nil {
		t.Fatalf("%+v", m)
	}
	// An ideal step has no samples inside the edge: the rise time is limited
	// by the sample rate and must say so.
	if m.RiseTimeS == nil || len(m.Notes) == 0 {
		t.Fatalf("%+v", m)
	}

	// A window that starts inside a rising edge does not count that edge.
	cut := Trace{DT: 1e-6, V: []float32{0.7, 1, 1, 1, 0, 0, 0, 0}}
	m, err = Measure(cut)
	if err != nil {
		t.Fatal(err)
	}
	if m.RisingEdges != 0 || m.RiseTimeS != nil || m.FallingEdges != 1 {
		t.Fatalf("an edge cut by the window start must not be measured: %+v", m)
	}
}

func TestWindowAndEnvelope(t *testing.T) {
	tr := Trace{T0: 10, DT: 0.5, V: []float32{0, 1, 2, 3, 4, 5, 6, 7}}
	w := tr.Window(11, 13)
	if len(w.V) != 4 || w.T0 != 11 || w.V[0] != 2 {
		t.Fatalf("%+v", w)
	}
	if w := tr.Window(0, 0); len(w.V) != 8 {
		t.Fatalf("open window %+v", w)
	}
	if w := tr.Window(20, 30); len(w.V) != 0 {
		t.Fatalf("outside %+v", w)
	}
	e := MakeEnvelope(tr, 4)
	if len(e.Min) != 4 || e.Min[0] != 0 || e.Max[0] != 1 || e.Max[3] != 7 || e.BucketDT != 1 {
		t.Fatalf("%+v", e)
	}
	if e := MakeEnvelope(tr, 100); len(e.Min) != 8 {
		t.Fatalf("envelope must not exceed the sample count: %d", len(e.Min))
	}
}
