package dfu

import (
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// transcriptBytes is how many payload bytes of each entry are kept.
const transcriptBytes = 32

// Entry is one recorded serial event.
type Entry struct {
	At   time.Duration // since the transcript started
	Dir  string        // "tx" (request, before SLIP), "rx" (raw bytes read), "err" or "note"
	Len  int           // full payload length
	Data []byte        // first transcriptBytes bytes of the payload
	Note string
}

func (e Entry) String() string {
	s := fmt.Sprintf("%9.3fms %-4s", float64(e.At.Microseconds())/1000, e.Dir)
	if e.Len > 0 {
		s += fmt.Sprintf(" len=%d %s", e.Len, hex.EncodeToString(e.Data))
		if e.Len > len(e.Data) {
			s += "…"
		}
	}
	if e.Note != "" {
		s += " " + e.Note
	}
	return s
}

// Transcript is a bounded record of a DFU exchange, used to diagnose
// transfers that stall on real hardware. It keeps the most recent Max
// entries; a nil *Transcript records nothing.
type Transcript struct {
	Max int // entries kept; 4096 when zero

	mu      sync.Mutex
	start   time.Time
	ring    []Entry
	next    int
	dropped int
}

func (t *Transcript) add(dir string, p []byte, note string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if t.start.IsZero() {
		t.start = now
	}
	e := Entry{At: now.Sub(t.start), Dir: dir, Len: len(p), Note: note}
	if len(p) > 0 {
		e.Data = append([]byte(nil), p[:min(len(p), transcriptBytes)]...)
	}
	limit := t.Max
	if limit <= 0 {
		limit = 4096
	}
	if len(t.ring) < limit {
		t.ring = append(t.ring, e)
		return
	}
	t.ring[t.next] = e
	t.next = (t.next + 1) % limit
	t.dropped++
}

// Note records a free-form event (e.g. port setup by the caller).
func (t *Transcript) Note(format string, args ...any) {
	t.add("note", nil, fmt.Sprintf(format, args...))
}

// Entries returns the kept entries in order and how many older ones were
// dropped.
func (t *Transcript) Entries() ([]Entry, int) {
	if t == nil {
		return nil, 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := append(append([]Entry(nil), t.ring[t.next:]...), t.ring[:t.next]...)
	return out, t.dropped
}

// Tail formats the last n entries, one per line.
func (t *Transcript) Tail(n int) string {
	es, _ := t.Entries()
	if len(es) > n {
		es = es[len(es)-n:]
	}
	lines := make([]string, len(es))
	for i, e := range es {
		lines[i] = e.String()
	}
	return strings.Join(lines, "\n")
}

// WriteTo writes the whole kept transcript.
func (t *Transcript) WriteTo(w io.Writer) (int64, error) {
	es, dropped := t.Entries()
	var sb strings.Builder
	if dropped > 0 {
		fmt.Fprintf(&sb, "# %d earlier entries dropped\n", dropped)
	}
	for _, e := range es {
		sb.WriteString(e.String())
		sb.WriteByte('\n')
	}
	n, err := io.WriteString(w, sb.String())
	return int64(n), err
}
