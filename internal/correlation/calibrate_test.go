package correlation

import (
	"testing"

	"github.com/faronov/hwlab/internal/timeline"
)

func TestCalibrateFindsOffset(t *testing.T) {
	tl := timeline.New()
	// Device B's clock is 3 ms ahead of A's with a little jitter.
	for i, jit := range []int64{0, 10_000, -10_000, 5_000, 0} {
		ts := int64(i+1) * 1_000_000_000
		tl.Append(timeline.Event{TimestampNs: ts, SourceID: "a", Type: "trigger"})
		tl.Append(timeline.Event{TimestampNs: ts + 3_000_000 + jit, SourceID: "b", Type: "trigger"})
	}
	c := Calibrate(tl, timeline.Filter{Sources: []string{"a"}}, timeline.Filter{Sources: []string{"b"}}, 50_000_000)
	if c.Pairs != 5 || c.MedianNs != 3_000_000 || c.SuggestedOffsetNs() != -3_000_000 {
		t.Fatalf("%+v", c)
	}
	if c.MADNs != 5_000 || c.MinNs != 2_990_000 || c.MaxNs != 3_010_000 {
		t.Fatalf("spread %+v", c)
	}
}

func TestCalibrateSkipsAmbiguousAndUnmatched(t *testing.T) {
	tl := timeline.New()
	tl.Append(timeline.Event{TimestampNs: 1_000_000, SourceID: "a", Type: "trigger"})
	tl.Append(timeline.Event{TimestampNs: 1_100_000, SourceID: "b", Type: "trigger"})
	tl.Append(timeline.Event{TimestampNs: 1_200_000, SourceID: "b", Type: "trigger"})   // two candidates
	tl.Append(timeline.Event{TimestampNs: 900_000_000, SourceID: "a", Type: "trigger"}) // no candidate
	c := Calibrate(tl, timeline.Filter{Sources: []string{"a"}}, timeline.Filter{Sources: []string{"b"}}, 10_000_000)
	if c.Pairs != 0 || c.Ambiguous != 1 || c.Unmatched != 1 {
		t.Fatalf("%+v", c)
	}
}

func TestMedianEven(t *testing.T) {
	if m := median([]int64{1, 3, 5, 9}); m != 4 {
		t.Fatal(m)
	}
}
