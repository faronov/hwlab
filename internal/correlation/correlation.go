// Package correlation answers cross-device timing questions on a timeline:
// what happened around an event, which event precedes another, how long
// between two kinds of events.
package correlation

import (
	"sort"

	"github.com/faronov/hwlab/internal/timeline"
)

// Hit is an event with its signed offset from the query anchor
// (negative = before the anchor).
type Hit struct {
	Event   timeline.Event `json:"event"`
	DeltaNs int64          `json:"delta_ns"`
}

// Around returns events within ±window of ts, ordered by absolute distance.
func Around(tl *timeline.Timeline, ts, window int64, f timeline.Filter, limit int) []Hit {
	evs := tl.Within(ts, window, f)
	hits := make([]Hit, len(evs))
	for i, e := range evs {
		hits[i] = Hit{Event: e, DeltaNs: e.TimestampNs - ts}
	}
	sort.SliceStable(hits, func(i, j int) bool { return abs(hits[i].DeltaNs) < abs(hits[j].DeltaNs) })
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}

// Nearest returns the event closest to ts.
func Nearest(tl *timeline.Timeline, ts int64, f timeline.Filter) (Hit, bool) {
	e, ok := tl.Nearest(ts, f)
	if !ok {
		return Hit{}, false
	}
	return Hit{Event: e, DeltaNs: e.TimestampNs - ts}, true
}

// Preceding returns up to n matching events before ts within window
// (window <= 0: unbounded), nearest first.
func Preceding(tl *timeline.Timeline, ts, window int64, f timeline.Filter, n int) []Hit {
	var out []Hit
	for _, e := range tl.Before(ts, f, 0) {
		if window > 0 && ts-e.TimestampNs > window {
			break
		}
		out = append(out, Hit{Event: e, DeltaNs: e.TimestampNs - ts})
		if n > 0 && len(out) >= n {
			break
		}
	}
	return out
}

// Following returns up to n matching events at/after ts within window.
func Following(tl *timeline.Timeline, ts, window int64, f timeline.Filter, n int) []Hit {
	var out []Hit
	for _, e := range tl.After(ts, f, 0) {
		if window > 0 && e.TimestampNs-ts > window {
			break
		}
		out = append(out, Hit{Event: e, DeltaNs: e.TimestampNs - ts})
		if n > 0 && len(out) >= n {
			break
		}
	}
	return out
}

// Delta is the time from a to b.
func Delta(a, b timeline.Event) int64 { return b.TimestampNs - a.TimestampNs }

// Pair links an event of kind A with the first following event of kind B.
type Pair struct {
	A       timeline.Event `json:"a"`
	B       timeline.Event `json:"b"`
	DeltaNs int64          `json:"delta_ns"`
}

// Pairs matches every A event with the first B event that follows within
// maxDelta (<=0: unbounded). Unmatched A events are skipped.
func Pairs(tl *timeline.Timeline, a, b timeline.Filter, maxDelta int64) []Pair {
	var out []Pair
	for _, ea := range tl.Between(-1<<62, 1<<62, a, 0) {
		next := tl.After(ea.TimestampNs, b, 1)
		if len(next) == 0 {
			continue
		}
		if d := next[0].TimestampNs - ea.TimestampNs; maxDelta <= 0 || d <= maxDelta {
			out = append(out, Pair{A: ea, B: next[0], DeltaNs: d})
		}
	}
	return out
}

// Summary is a latency summary of pairs.
type Summary struct {
	Count  int   `json:"count"`
	MinNs  int64 `json:"min_ns"`
	MaxNs  int64 `json:"max_ns"`
	MeanNs int64 `json:"mean_ns"`
}

// Summarize computes min/max/mean of pair deltas.
func Summarize(ps []Pair) Summary {
	if len(ps) == 0 {
		return Summary{}
	}
	s := Summary{Count: len(ps), MinNs: ps[0].DeltaNs, MaxNs: ps[0].DeltaNs}
	var sum int64
	for _, p := range ps {
		if p.DeltaNs < s.MinNs {
			s.MinNs = p.DeltaNs
		}
		if p.DeltaNs > s.MaxNs {
			s.MaxNs = p.DeltaNs
		}
		sum += p.DeltaNs
	}
	s.MeanNs = sum / int64(len(ps))
	return s
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
