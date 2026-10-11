package correlation

import (
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/faronov/hwlab/internal/timeline"
)

// Affine maps one device clock onto the reference (session) clock:
//
//	session = RefSessionNs + Rate*(device - RefDeviceNs)
//
// which is session = a*device + b with a = Rate and b = RefSessionNs -
// Rate*RefDeviceNs. Rate is 1 for a pure offset. The reference point sits in
// the middle of the measured data so that the integer nanosecond fields keep
// full precision.
type Affine struct {
	RefDeviceNs  int64   `json:"ref_device_ns"`
	RefSessionNs int64   `json:"ref_session_ns"`
	Rate         float64 `json:"rate"`
}

// Identity is the mapping that changes nothing.
func Identity() Affine { return Affine{Rate: 1} }

// Apply converts a device timestamp to session time.
func (a Affine) Apply(deviceNs int64) int64 {
	return a.RefSessionNs + int64(math.Round(a.Rate*float64(deviceNs-a.RefDeviceNs)))
}

// DriftPPM is the rate error of the device clock against the reference clock:
// positive when the device clock runs slower than the reference.
func (a Affine) DriftPPM() float64 { return (a.Rate - 1) * 1e6 }

// Edge is one level change of a digital signal.
type Edge struct {
	T     int64
	Level bool // level after the edge
}

// EdgePair is the same physical edge seen by the reference device (Ref) and
// by the device being calibrated (Dev).
type EdgePair struct{ Ref, Dev int64 }

// EdgeMatch is the result of pairing the edges of one signal seen twice.
type EdgeMatch struct {
	Pairs []EdgePair
	// Shift is the index offset: Dev edge i+Shift is Ref edge i.
	Shift int
	// Score is the number of matching intervals at the chosen shift; Runner is
	// the best score of any other shift. A small margin means the signal is too
	// periodic to identify edges.
	Score, Runner int
	// Markers is the number of irregular intervals matched at the chosen shift
	// and RunnerMarkers the best count at any other shift.
	Markers, RunnerMarkers int
}

// MatchEdges pairs the edges of a signal that two devices observed. A periodic
// signal cannot be paired by time alone when the device clocks differ by more
// than a period, so the pairing compares interval sequences instead: the edge
// index shift with the most equal consecutive intervals (within tolNs, and with
// equal levels) wins. The signal therefore needs at least one irregularity
// (a stretched half-period, a missing edge, a burst) that both devices see; a
// perfectly periodic signal is reported as ambiguous.
func MatchEdges(ref, dev []Edge, maxShift int, tolNs int64) (EdgeMatch, error) {
	var m EdgeMatch
	if len(ref) < 4 || len(dev) < 4 {
		return m, errors.New("too few edges to match")
	}
	if maxShift <= 0 {
		maxShift = max(len(ref), len(dev))
	}
	// A "marker" is an interval that differs from the previous one by more than
	// 20%: the irregularity that identifies a position in an otherwise periodic
	// signal. Alignments are ranked by matched markers first, matched intervals
	// second.
	marker := make([]bool, len(ref))
	for i := 1; i+1 < len(ref); i++ {
		a, b := ref[i].T-ref[i-1].T, ref[i+1].T-ref[i].T
		marker[i] = abs(a-b)*5 > max(a, b)
	}
	type score struct{ markers, matches int }
	better := func(a, b score) bool {
		return a.markers > b.markers || a.markers == b.markers && a.matches > b.matches
	}
	best, runner := score{-1, -1}, score{-1, -1}
	bestShift := 0
	for k := -maxShift; k <= maxShift; k++ {
		var sc score
		prevOK := false
		for i := 0; i+1 < len(ref); i++ {
			j := i + k
			if j < 0 || j+1 >= len(dev) {
				continue
			}
			ok := ref[i].Level == dev[j].Level && ref[i+1].Level == dev[j+1].Level &&
				abs((ref[i+1].T-ref[i].T)-(dev[j+1].T-dev[j].T)) <= tolNs
			if ok {
				sc.matches++
				// A marker only identifies the alignment together with the
				// interval before it, which has a different length.
				if marker[i] && prevOK {
					sc.markers++
				}
			}
			prevOK = ok
		}
		switch {
		case better(sc, best):
			runner, best, bestShift = best, sc, k
		case better(sc, runner):
			runner = sc
		}
	}
	m.Shift, m.Score, m.Runner = bestShift, best.matches, max(runner.matches, 0)
	m.Markers, m.RunnerMarkers = best.markers, max(runner.markers, 0)
	if best.matches < 4 {
		return m, errors.New("no edge alignment found: the two edge sequences do not match")
	}
	if best.markers < 1 || best.markers <= m.RunnerMarkers {
		return m, fmt.Errorf("edge matching is ambiguous (%d markers matched, next best alignment %d; %d vs %d matching intervals): the signal needs a non-periodic marker seen by both devices",
			best.markers, m.RunnerMarkers, best.matches, m.Runner)
	}
	for i := range ref {
		j := i + bestShift
		if j < 0 || j >= len(dev) || ref[i].Level != dev[j].Level {
			continue
		}
		m.Pairs = append(m.Pairs, EdgePair{Ref: ref[i].T, Dev: dev[j].T})
	}
	return m, nil
}

// AffineFit is the least-squares clock mapping of one device with the evidence
// behind it. Residuals are measured against the fitted line, so they describe
// the scatter of individual shared edges (timestamp jitter of both devices
// plus the skew of the signal itself), not the absolute accuracy of the
// mapping.
type AffineFit struct {
	Map      Affine `json:"map"`
	Pairs    int    `json:"pairs"`
	Rejected int    `json:"rejected"`
	// InitialOffsetNs is session - device at the first matched edge, before any
	// rate correction.
	InitialOffsetNs int64 `json:"initial_offset_ns"`
	// OffsetOnlyNs is the best fixed offset (rate fixed at 1) and
	// OffsetOnlyP95Ns / OffsetOnlyMaxNs the residuals it leaves: the drift a
	// plain offset cannot absorb.
	OffsetOnlyNs    int64 `json:"offset_only_ns"`
	OffsetOnlyP95Ns int64 `json:"offset_only_p95_ns"`
	OffsetOnlyMaxNs int64 `json:"offset_only_max_ns"`
	// Residuals of the affine fit (absolute values).
	ResidualMedianNs int64   `json:"residual_median_ns"`
	ResidualP95Ns    int64   `json:"residual_p95_ns"`
	ResidualMaxNs    int64   `json:"residual_max_ns"`
	DriftPPM         float64 `json:"drift_ppm"`
	// DriftStdErrPPM is the statistical standard error of the rate estimate.
	DriftStdErrPPM float64 `json:"drift_stderr_ppm"`
	SpanNs         int64   `json:"span_ns"`
}

// FitAffine fits session = a*device + b to the paired edges by least squares,
// rejecting pairs further than 5 MAD (and at least minRejectNs) from the line
// and refitting once.
func FitAffine(pairs []EdgePair, minRejectNs int64) (AffineFit, error) {
	var f AffineFit
	if len(pairs) < 8 {
		return f, errors.New("fewer than 8 edge pairs: too few for a rate estimate")
	}
	use := pairs
	var fit Affine
	for pass := 0; pass < 2; pass++ {
		fit = lsq(use)
		if pass == 1 {
			break
		}
		res := residuals(use, fit)
		mags := make([]int64, len(res))
		for i, r := range res {
			mags[i] = absI(r)
		}
		sort.Slice(mags, func(i, j int) bool { return mags[i] < mags[j] })
		limit := max(5*median(mags), minRejectNs)
		var keep []EdgePair
		for i, r := range res {
			if absI(r) <= limit {
				keep = append(keep, use[i])
			}
		}
		if len(keep) == len(use) || len(keep) < 8 {
			break
		}
		f.Rejected = len(pairs) - len(keep)
		use = keep
	}
	f.Map, f.Pairs = fit, len(use)
	f.SpanNs = use[len(use)-1].Dev - use[0].Dev
	f.InitialOffsetNs = use[0].Ref - use[0].Dev

	res := residuals(use, fit)
	f.ResidualMedianNs, f.ResidualP95Ns, f.ResidualMaxNs = absStats(res)
	f.DriftPPM = fit.DriftPPM()

	var sxx, sse float64
	mx := meanDev(use)
	for i, p := range use {
		dx := float64(p.Dev) - mx
		sxx += dx * dx
		r := float64(res[i])
		sse += r * r
	}
	if sxx > 0 && len(use) > 2 {
		f.DriftStdErrPPM = math.Sqrt(sse/float64(len(use)-2)/sxx) * 1e6
	}

	// Offset-only comparison: median of (ref - dev).
	diffs := make([]int64, len(use))
	for i, p := range use {
		diffs[i] = p.Ref - p.Dev
	}
	sort.Slice(diffs, func(i, j int) bool { return diffs[i] < diffs[j] })
	f.OffsetOnlyNs = median(diffs)
	oo := make([]int64, len(use))
	for i, d := range diffs {
		oo[i] = d - f.OffsetOnlyNs
	}
	_, f.OffsetOnlyP95Ns, f.OffsetOnlyMaxNs = absStats(oo)
	return f, nil
}

func meanDev(p []EdgePair) float64 {
	var s float64
	for _, e := range p {
		s += float64(e.Dev)
	}
	return s / float64(len(p))
}

// lsq is ordinary least squares of Ref on Dev, centred on the mean for
// numerical stability (timestamps are up to 1e12 ns).
func lsq(p []EdgePair) Affine {
	n := float64(len(p))
	mx, my := meanDev(p), 0.0
	for _, e := range p {
		my += float64(e.Ref)
	}
	my /= n
	var sxx, sxy float64
	for _, e := range p {
		dx, dy := float64(e.Dev)-mx, float64(e.Ref)-my
		sxx += dx * dx
		sxy += dx * dy
	}
	rate := 1.0
	if sxx > 0 {
		rate = sxy / sxx
	}
	return Affine{RefDeviceNs: int64(math.Round(mx)), RefSessionNs: int64(math.Round(my)), Rate: rate}
}

func residuals(p []EdgePair, a Affine) []int64 {
	out := make([]int64, len(p))
	for i, e := range p {
		out[i] = e.Ref - a.Apply(e.Dev)
	}
	return out
}

// absStats returns the median, 95th percentile and maximum of |v|.
func absStats(v []int64) (med, p95, mx int64) {
	a := make([]int64, len(v))
	for i, x := range v {
		a[i] = absI(x)
	}
	sort.Slice(a, func(i, j int) bool { return a[i] < a[j] })
	if len(a) == 0 {
		return 0, 0, 0
	}
	return median(a), a[min(len(a)-1, int(math.Ceil(0.95*float64(len(a))))-1)], a[len(a)-1]
}

func absI(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// EdgesOf extracts the digital.edge events of one source (and optionally one
// channel name) as edges in timestamp order.
func EdgesOf(tl *timeline.Timeline, source, channel string) []Edge {
	var out []Edge
	f := timeline.Filter{Types: []string{timeline.TypeDigitalEdge}, Sources: []string{source}}
	for _, e := range tl.Between(-1<<62, 1<<62, f, 0) {
		if channel != "" {
			if c, _ := e.Data["channel"].(string); c != channel {
				continue
			}
		}
		lvl, _ := e.Data["level"].(bool)
		out = append(out, Edge{T: e.TimestampNs, Level: lvl})
	}
	return out
}
