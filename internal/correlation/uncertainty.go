package correlation

import "fmt"

// Bound is the range of (stamped time - true time) of one clock, in ns. A side
// that is not known has no limit, and the value there is only a floor.
type Bound struct {
	MinNs, MaxNs        int64
	MinKnown, MaxKnown  bool
	OffsetAppliedNs     int64
	Known               bool // false: the source reported nothing about its clock
	Source, Description string
}

func (b Bound) lo() (int64, bool) { return b.MinNs + b.OffsetAppliedNs, b.Known && b.MinKnown }
func (b Bound) hi() (int64, bool) { return b.MaxNs + b.OffsetAppliedNs, b.Known && b.MaxKnown }

// Range is where the true delta between two events can lie.
type Range struct {
	LowNs, HighNs       int64
	LowKnown, HighKnown bool
}

// TrueDelta bounds the real time difference between event B on clock b and
// event A on clock a, given the stamped difference measured = tB - tA.
//
// With stamped = true + e and e in [min,max] for each clock:
//
//	true = measured - (eB - eA),  eB - eA in [Bmin - Amax, Bmax - Amin]
//
// so the lower end needs Bmax and Amin, the upper end needs Bmin and Amax.
func TrueDelta(measured int64, a, b Bound) Range {
	aMin, aMinOK := a.lo()
	aMax, aMaxOK := a.hi()
	bMin, bMinOK := b.lo()
	bMax, bMaxOK := b.hi()
	return Range{
		LowNs: measured - (bMax - aMin), LowKnown: bMaxOK && aMinOK,
		HighNs: measured - (bMin - aMax), HighKnown: bMinOK && aMaxOK,
	}
}

// Order reports whether B happened strictly after A ("after"), strictly before
// ("before"), or whether the timestamps cannot tell ("undetermined").
func (r Range) Order() string {
	switch {
	case r.LowKnown && r.LowNs > 0:
		return "after"
	case r.HighKnown && r.HighNs < 0:
		return "before"
	}
	return "undetermined"
}

// Width is the size of the range, or false when a side is unbounded.
func (r Range) Width() (int64, bool) {
	if !r.LowKnown || !r.HighKnown {
		return 0, false
	}
	return r.HighNs - r.LowNs, true
}

// String renders the range with explicit infinities.
func (r Range) String() string {
	lo, hi := "-inf", "+inf"
	if r.LowKnown {
		lo = fmt.Sprintf("%+dns", r.LowNs)
	}
	if r.HighKnown {
		hi = fmt.Sprintf("%+dns", r.HighNs)
	}
	return "[" + lo + ", " + hi + "]"
}
