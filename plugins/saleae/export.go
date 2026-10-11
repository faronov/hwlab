package saleae

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

const (
	maxEdgesPerBatch   = 4096
	maxFramesPerBatch  = 4096
	maxDecodedPerBatch = 256
)

func secondsToNs(s string) (int64, error) {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, err
	}
	return int64(math.Round(f * 1e9)), nil
}

func newCSV(r io.Reader) *csv.Reader {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.ReuseRecord = true
	cr.TrimLeadingSpace = true
	return cr
}

// digitalEdges is one batch of changes. Offsets are nanoseconds from the
// capture start (Logic 2 time zero); States hold the full channel word after
// each change.
type digitalEdges struct {
	Initial  uint32
	StartNs  int64 // time of the first change in this batch (the first batch starts at the initial-state row)
	Offsets  []int64
	States   []uint32
	anchored bool
}

// readDigitalCSV parses Logic 2's digital.csv ("Time [s],Channel 0,Channel 1",
// one row per change, the first row being the initial state). Columns map to
// bits by position, in the order of the channels that were captured, because
// Logic 2 writes user-assigned channel names in the header.
func readDigitalCSV(r io.Reader, nch int, emit func(*digitalEdges) error) error {
	cr := newCSV(r)
	if _, err := cr.Read(); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return fmt.Errorf("digital csv header: %w", err)
	}
	var (
		prevNs  int64
		first   = true
		cur     *digitalEdges
		initial uint32
	)
	flush := func() error {
		if cur == nil || len(cur.Offsets) == 0 {
			return nil
		}
		err := emit(cur)
		cur = nil
		return err
	}
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("digital csv: %w", err)
		}
		if len(rec) != nch+1 {
			return fmt.Errorf("digital csv: row has %d columns, expected %d (time + %d channels)", len(rec), nch+1, nch)
		}
		ns, err := secondsToNs(rec[0])
		if err != nil {
			return fmt.Errorf("digital csv time %q: %w", rec[0], err)
		}
		var word uint32
		for i := 0; i < nch; i++ {
			switch strings.TrimSpace(rec[i+1]) {
			case "1":
				word |= 1 << i
			case "0":
			default:
				return fmt.Errorf("digital csv: bad level %q", rec[i+1])
			}
		}
		if first {
			initial, first, prevNs = word, false, ns
			cur = &digitalEdges{Initial: initial, StartNs: ns, anchored: true}
			continue
		}
		if cur == nil {
			cur = &digitalEdges{Initial: initial}
		}
		if n := len(cur.Offsets); n > 0 && ns <= prevNs {
			// Two changes inside one nanosecond collapse to the latest state.
			cur.States[n-1] = word
			initial = word
			continue
		}
		if len(cur.Offsets) == 0 && !cur.anchored {
			cur.StartNs = ns
		}
		cur.Offsets = append(cur.Offsets, ns-cur.StartNs)
		cur.States = append(cur.States, word)
		initial, prevNs = word, ns
		if len(cur.Offsets) >= maxEdgesPerBatch {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

// analogFrames is a block of interleaved samples.
type analogFrames struct {
	StartNs int64
	Samples []float32
}

// readAnalogCSV parses analog.csv ("Time [s],Channel 0,..."), one row per
// (possibly downsampled) sample.
func readAnalogCSV(r io.Reader, nch int, emit func(*analogFrames) error) error {
	cr := newCSV(r)
	if _, err := cr.Read(); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return fmt.Errorf("analog csv header: %w", err)
	}
	var cur *analogFrames
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("analog csv: %w", err)
		}
		if len(rec) != nch+1 {
			return fmt.Errorf("analog csv: row has %d columns, expected %d", len(rec), nch+1)
		}
		if cur == nil {
			ns, err := secondsToNs(rec[0])
			if err != nil {
				return fmt.Errorf("analog csv time %q: %w", rec[0], err)
			}
			cur = &analogFrames{StartNs: ns, Samples: make([]float32, 0, maxFramesPerBatch*nch)}
		}
		for i := 1; i <= nch; i++ {
			v, err := strconv.ParseFloat(strings.TrimSpace(rec[i]), 32)
			if err != nil {
				return fmt.Errorf("analog csv value %q: %w", rec[i], err)
			}
			cur.Samples = append(cur.Samples, float32(v))
		}
		if len(cur.Samples) >= maxFramesPerBatch*nch {
			if err := emit(cur); err != nil {
				return err
			}
			cur = nil
		}
	}
	if cur != nil && len(cur.Samples) > 0 {
		return emit(cur)
	}
	return nil
}

// decodedRow is one analyzer result from a Logic 2 data-table export.
type decodedRow struct {
	StartNs    int64
	DurationNs int64
	Type       string
	Name       string
	Fields     map[string]any
}

// readTableCSV parses a data-table export. The first columns are the generic
// "name,type,start_time,duration"; every other column is analyzer specific and
// is carried through as a field. Header names are matched case-insensitively
// and "start_time"/"start" both identify the time column.
func readTableCSV(r io.Reader, emit func([]decodedRow) error) error {
	cr := newCSV(r)
	hdr, err := cr.Read()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return fmt.Errorf("table csv header: %w", err)
	}
	cols := make([]string, len(hdr))
	for i, h := range hdr {
		cols[i] = strings.TrimSpace(h)
	}
	idx := map[string]int{}
	for i, c := range cols {
		idx[strings.ToLower(c)] = i
	}
	ti, ok := idx["start_time"]
	if !ok {
		if ti, ok = idx["start"]; !ok {
			return fmt.Errorf("table csv has no start_time column (columns: %s)", strings.Join(cols, ","))
		}
	}
	di, hasDur := idx["duration"]
	yi, hasType := idx["type"]
	ni, hasName := idx["name"]
	var batch []decodedRow
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("table csv: %w", err)
		}
		if len(rec) <= ti {
			continue
		}
		row := decodedRow{Fields: map[string]any{}}
		if row.StartNs, err = secondsToNs(rec[ti]); err != nil {
			return fmt.Errorf("table csv start time %q: %w", rec[ti], err)
		}
		if hasDur && di < len(rec) {
			if row.DurationNs, err = secondsToNs(rec[di]); err != nil {
				row.DurationNs = 0
			}
		}
		if hasType && yi < len(rec) {
			row.Type = strings.TrimSpace(rec[yi])
		}
		if hasName && ni < len(rec) {
			row.Name = strings.TrimSpace(rec[ni])
		}
		for i, c := range cols {
			if i == ti || (hasDur && i == di) || (hasType && i == yi) || (hasName && i == ni) || i >= len(rec) {
				continue
			}
			row.Fields[c] = strings.TrimSpace(rec[i])
		}
		batch = append(batch, row)
		if len(batch) >= maxDecodedPerBatch {
			if err := emit(batch); err != nil {
				return err
			}
			batch = nil
		}
	}
	if len(batch) > 0 {
		return emit(batch)
	}
	return nil
}

// summarize renders a decoded row for the timeline.
func summarize(label string, row decodedRow) string {
	keys := make([]string, 0, len(row.Fields))
	for k := range row.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(label)
	if row.Type != "" {
		b.WriteString(" " + row.Type)
	}
	for _, k := range keys {
		if v, _ := row.Fields[k].(string); v != "" {
			fmt.Fprintf(&b, " %s=%s", k, v)
		}
	}
	return b.String()
}
