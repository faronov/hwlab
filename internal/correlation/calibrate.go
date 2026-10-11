package correlation

import (
	"sort"

	"github.com/faronov/hwlab/internal/timeline"
)

// Calibration estimates the offset between two clocks from events that are
// known to be simultaneous, e.g. one hardware trigger line seen by both
// devices. It is meaningless for causally related events (a packet that
// follows a current spike has a real delay, which is not a clock offset).
type Calibration struct {
	Pairs     int   `json:"pairs"`
	Ambiguous int   `json:"ambiguous"`       // A events with more than one B candidate in the window
	Unmatched int   `json:"unmatched"`       // A events with no B in the window
	MedianNs  int64 `json:"median_delta_ns"` // tB - tA
	MinNs     int64 `json:"min_delta_ns"`
	MaxNs     int64 `json:"max_delta_ns"`
	// MADNs is the median absolute deviation of the deltas: the jitter of the
	// shared event as seen by the two devices.
	MADNs int64 `json:"mad_ns"`
}

// SuggestedOffsetNs is the offset to add to B's timestamps so that the
// calibration events coincide.
func (c Calibration) SuggestedOffsetNs() int64 { return -c.MedianNs }

// Calibrate pairs every A event with the single B event within ±window of it.
// A events with several candidates are skipped because the match would be a
// guess.
func Calibrate(tl *timeline.Timeline, a, b timeline.Filter, window int64) Calibration {
	var cal Calibration
	var deltas []int64
	for _, ea := range tl.Between(-1<<62, 1<<62, a, 0) {
		cands := tl.Within(ea.TimestampNs, window, b)
		switch len(cands) {
		case 0:
			cal.Unmatched++
		case 1:
			deltas = append(deltas, cands[0].TimestampNs-ea.TimestampNs)
		default:
			cal.Ambiguous++
		}
	}
	cal.Pairs = len(deltas)
	if len(deltas) == 0 {
		return cal
	}
	sort.Slice(deltas, func(i, j int) bool { return deltas[i] < deltas[j] })
	cal.MinNs, cal.MaxNs, cal.MedianNs = deltas[0], deltas[len(deltas)-1], median(deltas)
	dev := make([]int64, len(deltas))
	for i, d := range deltas {
		dev[i] = abs(d - cal.MedianNs)
	}
	sort.Slice(dev, func(i, j int) bool { return dev[i] < dev[j] })
	cal.MADNs = median(dev)
	return cal
}

// median of an ascending slice.
func median(v []int64) int64 {
	n := len(v)
	if n%2 == 1 {
		return v[n/2]
	}
	return (v[n/2-1] + v[n/2]) / 2
}
