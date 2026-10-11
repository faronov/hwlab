package correlation

import (
	"testing"

	"github.com/faronov/hwlab/internal/timeline"
)

func ev(ts int64, typ string) timeline.Event {
	return timeline.Event{TimestampNs: ts, SourceID: "s", Type: typ}
}

func TestPairsAndSummary(t *testing.T) {
	tl := timeline.FromEvents([]timeline.Event{
		ev(1000, "rise"), ev(1100, "pkt"), ev(1600, "ack"),
		ev(5000, "rise"), ev(5200, "pkt"),
		ev(9000, "rise"), // no packet nearby
	})
	a := timeline.Filter{Types: []string{"rise"}}
	b := timeline.Filter{Types: []string{"pkt"}}
	pairs := Pairs(tl, a, b, 500)
	if len(pairs) != 2 { // the unmatched third rise is skipped
		t.Fatalf("pairs = %d", len(pairs))
	}
	if pairs[0].DeltaNs != 100 || pairs[1].DeltaNs != 200 {
		t.Errorf("pairs = %+v", pairs)
	}
	s := Summarize(pairs)
	if s.Count != 2 || s.MinNs != 100 || s.MaxNs != 200 || s.MeanNs != 150 {
		t.Errorf("summary = %+v", s)
	}
}

func TestPrecedingFollowingNearest(t *testing.T) {
	tl := timeline.FromEvents([]timeline.Event{ev(100, "x"), ev(200, "x"), ev(900, "x")})
	f := timeline.Filter{}
	if got := Preceding(tl, 250, 200, f, 5); len(got) != 2 || got[0].DeltaNs >= 0 {
		t.Errorf("preceding = %+v", got)
	}
	if got := Following(tl, 250, 100, f, 5); len(got) != 0 {
		t.Errorf("following within 100ns = %+v", got)
	}
	if h, ok := Nearest(tl, 850, f); !ok || h.Event.TimestampNs != 900 || h.DeltaNs != 50 {
		t.Errorf("nearest = %+v %v", h, ok)
	}
	if Delta(ev(10, "a"), ev(35, "b")) != 25 {
		t.Error("Delta must be b-a")
	}
}
