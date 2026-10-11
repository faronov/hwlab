package correlation

import (
	"math"
	"math/rand"
	"testing"
)

// Synthetic: a 5 ms half-period square wave with an 8 ms marker section, seen
// by a device whose clock has a known rate error and offset plus jitter.
func synthEdges(rate float64, offsetNs int64, jitterNs int64, markers bool, drop int) (ref, dev []Edge) {
	rng := rand.New(rand.NewSource(1))
	t := int64(1_000_000_000)
	level := false
	for i := 0; i < 2000; i++ {
		level = !level
		half := int64(5_000_000)
		if markers && ((i > 300 && i < 380) || (i > 1500 && i < 1560)) {
			half = 8_000_000
		}
		t += half
		ref = append(ref, Edge{T: t, Level: level})
		if i < drop {
			continue
		}
		d := int64(float64(t-offsetNs)/rate) + rng.Int63n(2*jitterNs+1) - jitterNs
		dev = append(dev, Edge{T: d, Level: level})
	}
	return
}

func TestFitAffineRecoversKnownRateAndOffset(t *testing.T) {
	ref, dev := synthEdges(1-90e-6, 40_000_000, 3_000, true, 57)
	m, err := MatchEdges(ref, dev, 0, 20_000)
	if err != nil {
		t.Fatal(err)
	}
	if m.Shift != -57 {
		t.Errorf("shift %d", m.Shift)
	}
	f, err := FitAffine(m.Pairs, 20_000)
	if err != nil {
		t.Fatal(err)
	}
	// session = rate*device with rate = 1-90e-6
	if math.Abs(f.DriftPPM+90) > 0.5 {
		t.Errorf("drift %.3f ppm", f.DriftPPM)
	}
	if f.ResidualP95Ns > 6_000 {
		t.Errorf("p95 residual %d ns for 3 us jitter", f.ResidualP95Ns)
	}
	if f.OffsetOnlyP95Ns < 10*f.ResidualP95Ns {
		t.Errorf("offset-only should leave far more: %d vs %d", f.OffsetOnlyP95Ns, f.ResidualP95Ns)
	}
}

func TestMatchEdgesRejectsPeriodicSignal(t *testing.T) {
	ref, dev := synthEdges(1, 0, 3_000, false, 57)
	if _, err := MatchEdges(ref, dev, 0, 20_000); err == nil {
		t.Fatal("a perfectly periodic signal cannot be paired: want an ambiguity error")
	}
}

func TestFitAffineRejectsOutliers(t *testing.T) {
	ref, dev := synthEdges(1, 10_000_000, 1_000, true, 0)
	dev[700].T += 400_000
	m, err := MatchEdges(ref, dev, 0, 20_000)
	if err != nil {
		t.Fatal(err)
	}
	f, err := FitAffine(m.Pairs, 20_000)
	if err != nil {
		t.Fatal(err)
	}
	if f.Rejected < 1 || math.Abs(f.DriftPPM) > 1 {
		t.Errorf("rejected %d, drift %.3f ppm", f.Rejected, f.DriftPPM)
	}
}

func TestAffineApplyAndIdentity(t *testing.T) {
	if Identity().Apply(12345) != 12345 {
		t.Error("identity changed a timestamp")
	}
	a := Affine{RefDeviceNs: 1_000_000_000, RefSessionNs: 1_100_000_000, Rate: 1.0001}
	if got := a.Apply(2_000_000_000); got != 2_100_100_000 {
		t.Errorf("apply: %d", got)
	}
}
