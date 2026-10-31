package clockmap

import (
	"math"
	"testing"
)

func TestClockMapJitterAndMonotonic(t *testing.T) {
	c := NewClockMap()
	const trueOffset = 7_000_000_000 // device timer is 7 s behind session time
	var prev int64
	for i := 0; i < 5000; i++ {
		devUs := int64(1_000_000 + i*10_000)    // a frame every 10 ms
		jitter := int64((i * 7919) % 2_000_000) // up to 2 ms of host-side delay
		host := devUs*1000 + trueOffset + 300_000 + jitter
		ts := c.Map(devUs, host)
		if ts < prev {
			t.Fatalf("non-monotonic at %d: %d < %d", i, ts, prev)
		}
		prev = ts
	}
	s := c.Stats()
	if s.LatencyMinNs != 0 && s.LatencyMinNs > 1000 {
		t.Errorf("min latency %d", s.LatencyMinNs)
	}
	if s.LatencyMaxNs > 2_000_000 || s.LatencyMaxNs < 1_000_000 {
		t.Errorf("max latency = %d, want ~2ms", s.LatencyMaxNs)
	}
	// The mapping must land within the minimum transit time (0.3 ms) of the truth.
	if bias := s.OffsetNs - trueOffset; bias < 0 || bias > 300_000+10_000 {
		t.Errorf("offset bias = %d ns", bias)
	}
}

func TestClockMapTracksDrift(t *testing.T) {
	c := NewClockMap()
	const ppm = 40.0 // device timer runs 40 ppm slow
	for i := 0; i < 60*100; i++ {
		hostNs := int64(i) * 10_000_000
		devUs := int64(float64(hostNs)/(1+ppm*1e-6)) / 1000
		jitter := int64((i * 104729) % 500_000)
		c.Map(devUs, hostNs+jitter)
	}
	s := c.Stats()
	if math.Abs(s.DriftPPM-ppm) > 8 {
		t.Errorf("drift = %.1f ppm, want ~%.0f", s.DriftPPM, ppm)
	}
	if s.Windows < 55 {
		t.Errorf("windows = %d", s.Windows)
	}
}

func TestClockMapDetectsReboot(t *testing.T) {
	c := NewClockMap()
	c.Map(5_000_000, 10_000_000_000)
	c.Map(5_010_000, 10_010_000_000)
	ts := c.Map(100, 20_000_000_000) // device rebooted
	if c.Stats().Resets != 1 {
		t.Fatalf("resets = %d", c.Stats().Resets)
	}
	if ts < 10_010_000_000 {
		t.Errorf("timestamp went backwards after reboot: %d", ts)
	}
}
