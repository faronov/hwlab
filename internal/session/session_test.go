package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/faronov/hwlab/internal/timeline"
)

func writeFile(path string, b []byte) error { return os.WriteFile(path, b, 0o644) }

func TestSessionLifecycle(t *testing.T) {
	root := t.TempDir()
	s, err := Create(root, "my run/with:odd chars", "test")
	if err != nil {
		t.Fatal(err)
	}
	for i, ts := range []int64{300, 100, 200} { // out of order arrival
		if _, err := s.AppendEvent(timeline.Event{TimestampNs: ts, SourceID: "a", Type: "marker", Summary: string(rune('x' + i))}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	s.Update(func(m *Meta) { m.Status = "complete"; m.DurationNs = 300 })
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Name is sanitised in the directory name, status persisted.
	if filepath.Dir(s.Dir) != root {
		t.Errorf("session dir %q is not directly under root", s.Dir)
	}
	raw, err := os.ReadFile(filepath.Join(s.Dir, "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m Meta
	if err := json.Unmarshal(raw, &m); err != nil || m.Status != "complete" {
		t.Fatalf("session.json: %v / %+v", err, m)
	}

	// Reopen read-only: timeline is sorted by time.
	r, err := Open(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	evs := r.Timeline().All()
	if len(evs) != 3 || evs[0].TimestampNs != 100 || evs[2].TimestampNs != 300 {
		t.Fatalf("events = %+v", evs)
	}

	metas, err := List(root)
	if err != nil || len(metas) != 1 {
		t.Fatalf("List: %v %v", metas, err)
	}
	dir, err := Resolve(root, "latest")
	if err != nil || dir != s.Dir {
		t.Errorf("Resolve(latest) = %q, %v", dir, err)
	}
	if _, err := Resolve(root, "does-not-exist"); err == nil {
		t.Error("Resolve of an unknown session must fail")
	}
}

func TestTornTimelineLineIsTolerated(t *testing.T) {
	root := t.TempDir()
	s, err := Create(root, "torn", "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEvent(timeline.Event{TimestampNs: 1, SourceID: "a", Type: "marker"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash mid-write: a truncated final JSON line.
	f, err := os.OpenFile(filepath.Join(s.Dir, "timeline.jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"t_ns":2,"source":"a","ty`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := Open(s.Dir)
	if err != nil {
		t.Fatalf("a torn last line must not make the session unreadable: %v", err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	if n := r.Timeline().Len(); n != 1 {
		t.Errorf("events = %d, want 1", n)
	}
}
