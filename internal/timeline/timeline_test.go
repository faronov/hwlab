package timeline

import (
	"testing"
)

func mk(ts int64, src, typ string) Event { return Event{TimestampNs: ts, SourceID: src, Type: typ} }

func TestOrderingIsStableAndSortedAfterOutOfOrderAppend(t *testing.T) {
	tl := New()
	for _, e := range []Event{mk(30, "a", "x"), mk(10, "a", "x"), mk(20, "b", "x"), mk(10, "b", "x")} {
		tl.Append(e)
	}
	got := tl.All()
	for i := 1; i < len(got); i++ {
		if got[i].TimestampNs < got[i-1].TimestampNs {
			t.Fatalf("unsorted: %v", got)
		}
	}
	// Equal timestamps keep arrival order.
	if got[0].SourceID != "a" || got[1].SourceID != "b" {
		t.Errorf("tie order = %s,%s, want a,b", got[0].SourceID, got[1].SourceID)
	}
}

func TestBetweenIsHalfOpenAndFiltered(t *testing.T) {
	tl := New()
	for i := int64(0); i < 10; i++ {
		typ := "even"
		if i%2 == 1 {
			typ = "odd"
		}
		tl.Append(mk(i*10, "s", typ))
	}
	if got := tl.Between(20, 50, Filter{}, 0); len(got) != 3 {
		t.Errorf("[20,50) returned %d events, want 3", len(got))
	}
	if got := tl.Between(0, 100, Filter{Types: []string{"odd"}}, 0); len(got) != 5 {
		t.Errorf("odd filter returned %d", len(got))
	}
	if got := tl.Between(0, 100, Filter{}, 4); len(got) != 4 {
		t.Errorf("limit not applied: %d", len(got))
	}
}

func TestNearestBeforeAfterWithin(t *testing.T) {
	tl := New()
	for _, ts := range []int64{100, 200, 400} {
		tl.Append(mk(ts, "s", "e"))
	}
	if e, ok := tl.Nearest(290, Filter{}); !ok || e.TimestampNs != 200 {
		t.Errorf("nearest(290) = %v %v", e, ok)
	}
	if e, ok := tl.Nearest(310, Filter{}); !ok || e.TimestampNs != 400 {
		t.Errorf("nearest(310) = %v %v", e, ok)
	}
	if got := tl.Before(250, Filter{}, 5); len(got) != 2 {
		t.Errorf("before = %v", got)
	}
	if got := tl.After(250, Filter{}, 1); len(got) != 1 || got[0].TimestampNs != 400 {
		t.Errorf("after = %v", got)
	}
	if got := tl.Within(200, 150, Filter{}); len(got) != 2 { // 100..350 contains 100 and 200
		t.Errorf("within = %v", got)
	}
	if _, ok := New().Nearest(1, Filter{}); ok {
		t.Error("empty timeline has no nearest event")
	}
}

func TestBoundsCountSources(t *testing.T) {
	tl := FromEvents([]Event{mk(5, "b", "x"), mk(1, "a", "y"), mk(9, "a", "x")})
	first, last, ok := tl.Bounds()
	if !ok || first != 1 || last != 9 {
		t.Errorf("bounds = %d %d %v", first, last, ok)
	}
	if c := tl.Count(); c["x"] != 2 || c["y"] != 1 {
		t.Errorf("count = %v", c)
	}
	if s := tl.Sources(); len(s) != 2 || s[0] != "a" {
		t.Errorf("sources = %v", s)
	}
}

func TestFormatNs(t *testing.T) {
	for ns, want := range map[int64]string{0: "0.000000 ms", 1_500_000: "1.500000 ms"} {
		if got := FormatNs(ns); got != want {
			t.Errorf("FormatNs(%d) = %q, want %q", ns, got, want)
		}
	}
}
