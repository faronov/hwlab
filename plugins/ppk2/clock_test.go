package ppk2

import (
	"testing"

	"github.com/faronov/hwlab/plugins/ppk2/protocol"
)

func TestStartBoundBracketsTrueStart(t *testing.T) {
	// True start S = 1_000_000 ns after the write. The first data arrives
	// 3 ms after S; later reads arrive with variable delay >= 0.
	const w, t0 = 10_000_000, 10_050_000
	const s = w + 1_000_000
	b := newStartBound(w, t0)
	for i, delay := range []int64{3_000_000, 1_200_000, 5_000_000, 700_000, 2_000_000} {
		total := int64(i+1) * 1000
		arrive := s + (total-1)*protocol.SamplePeriod + delay
		b.observe(arrive, total)
	}
	ci := b.clockInfo()
	trueErr := int64(t0 - s) // stamped - true for sample 0
	if !ci.ErrorMinBounded || !ci.ErrorMaxBounded {
		t.Fatal("both sides must be bounded")
	}
	if trueErr < ci.ErrorMinNs || trueErr > ci.ErrorMaxNs {
		t.Fatalf("true error %d outside [%d,%d]", trueErr, ci.ErrorMinNs, ci.ErrorMaxNs)
	}
	if ci.ErrorMinNs != trueErr-700_000 {
		t.Fatalf("lower bound = %d, want %d (tightest arrival delay)", ci.ErrorMinNs, trueErr-700_000)
	}
	if ci.ErrorMaxNs != t0-w {
		t.Fatalf("max = %d", ci.ErrorMaxNs)
	}
}

func TestStartBoundIgnoresLateData(t *testing.T) {
	b := newStartBound(0, 10)
	b.observe(1_000_000, 100)
	before := b.upperNs
	b.observe(9_000_000_000, 500_000) // beyond the 2 s window
	if b.upperNs != before {
		t.Fatal("late arrivals must not change the bound")
	}
}

func TestStartBoundInconsistentNotClaimed(t *testing.T) {
	b := newStartBound(0, 1_000_000)
	b.observe(10, 1) // arrived "before" t0: assumptions broken
	ci := b.clockInfo()
	if ci.ErrorMinBounded {
		t.Fatalf("must not claim a bound: %+v", ci)
	}
}

func TestStartBoundNilSafe(t *testing.T) {
	var b *startBound
	b.observe(1, 1)
	if b.clockInfo() != nil {
		t.Fatal("nil bound has no clock info")
	}
}
