package correlation

import "testing"

func TestTrueDeltaBothBounded(t *testing.T) {
	a := Bound{Known: true, MinNs: -10, MinKnown: true, MaxNs: 5, MaxKnown: true}
	b := Bound{Known: true, MinNs: -2, MinKnown: true, MaxNs: 3, MaxKnown: true}
	r := TrueDelta(100, a, b)
	// eB-eA in [-2-5, 3+10] = [-7, 13]  => true in [87, 107]
	if !r.LowKnown || !r.HighKnown || r.LowNs != 87 || r.HighNs != 107 {
		t.Fatalf("%+v", r)
	}
	if w, ok := r.Width(); !ok || w != 20 {
		t.Fatalf("width %d %v", w, ok)
	}
	if r.Order() != "after" {
		t.Fatal(r.Order())
	}
}

func TestTrueDeltaSnifferLateUnbounded(t *testing.T) {
	// Power: early by 0..10 us. Sniffer: late by an unknown amount >= 0.
	power := Bound{Known: true, MinNs: -10_000, MinKnown: true, MaxNs: 0, MaxKnown: true}
	radio := Bound{Known: true, MinNs: 0, MinKnown: true}
	r := TrueDelta(900_000, power, radio)
	if r.LowKnown {
		t.Fatal("radio lateness is unbounded, so the true delta has no lower bound")
	}
	if !r.HighKnown || r.HighNs != 900_000+0 {
		t.Fatalf("upper = %+v", r)
	}
	// The radio may have truly happened before the power event.
	if r.Order() != "undetermined" {
		t.Fatal(r.Order())
	}
	if _, ok := r.Width(); ok {
		t.Fatal("width must be unbounded")
	}
}

func TestTrueDeltaBeforeCertain(t *testing.T) {
	a := Bound{Known: true, MinKnown: true, MaxKnown: true}
	b := Bound{Known: true, MinKnown: true, MaxKnown: true}
	if r := TrueDelta(-5, a, b); r.Order() != "before" {
		t.Fatal(r.Order())
	}
}

func TestTrueDeltaUnknownClock(t *testing.T) {
	r := TrueDelta(50, Bound{}, Bound{Known: true, MinKnown: true, MaxKnown: true})
	if r.LowKnown || r.HighKnown || r.Order() != "undetermined" {
		t.Fatalf("%+v", r)
	}
}

func TestTrueDeltaOffsetShiftsBounds(t *testing.T) {
	a := Bound{Known: true, MinKnown: true, MaxKnown: true}
	b := Bound{Known: true, MinKnown: true, MaxKnown: true, OffsetAppliedNs: 40}
	r := TrueDelta(100, a, b)
	if r.LowNs != 60 || r.HighNs != 60 {
		t.Fatalf("%+v", r)
	}
}
