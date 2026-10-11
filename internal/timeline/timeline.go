package timeline

import (
	"sort"
	"strings"
	"sync"
)

// Timeline is a concurrency-safe, lazily sorted collection of events.
// Appends are O(1); the first query after an append sorts if needed.
type Timeline struct {
	mu     sync.RWMutex
	events []Event
	sorted bool
	seq    uint64
}

// New returns an empty timeline.
func New() *Timeline { return &Timeline{sorted: true} }

// FromEvents builds a timeline from existing events.
func FromEvents(evs []Event) *Timeline {
	t := &Timeline{events: append([]Event(nil), evs...)}
	for i := range t.events {
		if t.events[i].Seq == 0 {
			t.seq++
			t.events[i].Seq = t.seq
		} else if t.events[i].Seq > t.seq {
			t.seq = t.events[i].Seq
		}
	}
	t.sorted = false
	return t
}

// Append adds an event and returns it with its sequence number assigned.
func (t *Timeline) Append(e Event) Event {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seq++
	e.Seq = t.seq
	if n := len(t.events); n > 0 && less(e, t.events[n-1]) {
		t.sorted = false
	}
	t.events = append(t.events, e)
	return e
}

func less(a, b Event) bool {
	if a.TimestampNs != b.TimestampNs {
		return a.TimestampNs < b.TimestampNs
	}
	return a.Seq < b.Seq
}

// Len returns the number of events.
func (t *Timeline) Len() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.events)
}

// snapshot returns a sorted view. Callers must not modify the slice.
func (t *Timeline) snapshot() []Event {
	t.mu.RLock()
	if t.sorted {
		ev := t.events
		t.mu.RUnlock()
		return ev
	}
	t.mu.RUnlock()
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.sorted {
		sort.SliceStable(t.events, func(i, j int) bool { return less(t.events[i], t.events[j]) })
		t.sorted = true
	}
	return t.events
}

// All returns a copy of all events in time order.
func (t *Timeline) All() []Event {
	return append([]Event(nil), t.snapshot()...)
}

// Bounds returns the first and last timestamps; ok is false when empty.
func (t *Timeline) Bounds() (first, last int64, ok bool) {
	ev := t.snapshot()
	if len(ev) == 0 {
		return 0, 0, false
	}
	last = ev[len(ev)-1].TimestampNs
	for _, e := range ev {
		if end := e.TimestampNs + e.DurationNs; end > last {
			last = end
		}
	}
	return ev[0].TimestampNs, last, true
}

// Filter selects events. Empty slices match everything.
type Filter struct {
	Types    []string
	Sources  []string
	Devices  []string
	Streams  []string
	Contains string // case-sensitive substring of Summary
}

func (f Filter) match(e *Event) bool {
	return in(f.Types, e.Type) && in(f.Sources, e.SourceID) && in(f.Devices, e.DeviceID) &&
		in(f.Streams, e.StreamID) && (f.Contains == "" || strings.Contains(e.Summary, f.Contains))
}

func in(set []string, v string) bool {
	if len(set) == 0 {
		return true
	}
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

// lowerBound returns the first index with TimestampNs >= ts.
func lowerBound(ev []Event, ts int64) int {
	return sort.Search(len(ev), func(i int) bool { return ev[i].TimestampNs >= ts })
}

// Between returns events with from <= t < to matching f, at most limit
// (limit <= 0 means unlimited).
func (t *Timeline) Between(from, to int64, f Filter, limit int) []Event {
	ev := t.snapshot()
	var out []Event
	for i := lowerBound(ev, from); i < len(ev) && ev[i].TimestampNs < to; i++ {
		if f.match(&ev[i]) {
			out = append(out, ev[i])
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out
}

// Nearest returns the event closest in time to ts matching f.
func (t *Timeline) Nearest(ts int64, f Filter) (Event, bool) {
	ev := t.snapshot()
	i := lowerBound(ev, ts)
	var best Event
	bestD := int64(-1)
	for j := i; j < len(ev); j++ { // forward: first match is nearest on this side
		if f.match(&ev[j]) {
			best, bestD = ev[j], ev[j].TimestampNs-ts
			break
		}
	}
	for j := i - 1; j >= 0; j-- {
		if f.match(&ev[j]) {
			if d := ts - ev[j].TimestampNs; bestD < 0 || d < bestD {
				best, bestD = ev[j], d
			}
			break
		}
	}
	return best, bestD >= 0
}

// Before returns up to n matching events strictly before ts, nearest first.
func (t *Timeline) Before(ts int64, f Filter, n int) []Event {
	ev := t.snapshot()
	var out []Event
	for j := lowerBound(ev, ts) - 1; j >= 0 && (n <= 0 || len(out) < n); j-- {
		if f.match(&ev[j]) {
			out = append(out, ev[j])
		}
	}
	return out
}

// After returns up to n matching events at or after ts, nearest first.
func (t *Timeline) After(ts int64, f Filter, n int) []Event {
	ev := t.snapshot()
	var out []Event
	for j := lowerBound(ev, ts); j < len(ev) && (n <= 0 || len(out) < n); j++ {
		if f.match(&ev[j]) {
			out = append(out, ev[j])
		}
	}
	return out
}

// Within returns matching events with |t - ts| <= window, ordered by time.
func (t *Timeline) Within(ts, window int64, f Filter) []Event {
	return t.Between(ts-window, ts+window+1, f, 0)
}

// Count returns per-type event counts.
func (t *Timeline) Count() map[string]int {
	out := map[string]int{}
	for _, e := range t.snapshot() {
		out[e.Type]++
	}
	return out
}

// Sources returns the distinct source ids.
func (t *Timeline) Sources() []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range t.snapshot() {
		if !seen[e.SourceID] {
			seen[e.SourceID] = true
			out = append(out, e.SourceID)
		}
	}
	sort.Strings(out)
	return out
}
