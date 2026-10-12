package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/faronov/hwlab/internal/session"
	"github.com/faronov/hwlab/internal/waveform"
)

const (
	defaultWaveformPoints = 200
	maxWaveformPoints     = 1000
	maxWaveformSegments   = 20
	// maxWaveformSamples bounds the samples held in memory for one channel.
	maxWaveformSamples = 20_000_000
)

// WaveformQuery asks for measurements of a recorded analog stream (an
// oscilloscope capture, a Saleae analog capture, ...), live or replayed.
type WaveformQuery struct {
	Session string
	Source  string // device alias; empty = every analog stream
	Channel string // channel name; empty = the first channel of each stream
	FromNs  int64
	ToNs    int64 // <= FromNs: to the end
	Points  int   // envelope buckets per segment (default 200, max 1000)
}

// WaveformSegment is one contiguous run of samples. A block-mode instrument
// records one segment per acquisition; a gap in the timeline starts a new one.
type WaveformSegment struct {
	StartNs      int64                  `json:"start_ns"`
	EndNs        int64                  `json:"end_ns"`
	Samples      int                    `json:"samples"`
	PeriodNs     int64                  `json:"sample_period_ns"`
	Measurements *waveform.Measurements `json:"measurements,omitempty"`
	Envelope     *waveform.Envelope     `json:"envelope,omitempty"`
	Error        string                 `json:"error,omitempty"`
}

// WaveformStream is the analysis of one channel of one stream.
type WaveformStream struct {
	Stream   string            `json:"stream"`
	Source   string            `json:"source"`
	Channel  string            `json:"channel"`
	Channels []string          `json:"channels"`
	Unit     string            `json:"unit"`
	Segments []WaveformSegment `json:"segments"`
	// Truncated is set when more segments exist than were measured.
	Truncated bool `json:"truncated,omitempty"`
}

// WaveformReport is the result of MeasureWaveform.
type WaveformReport struct {
	Session string           `json:"session"`
	Streams []WaveformStream `json:"streams"`
	Note    string           `json:"note"`
}

const waveformNote = "Measured offline from the recorded samples, never from the instrument. Times in a segment's envelope and measurements are seconds from the segment start (start_ns is session time). Edge counts ignore partial edges, and a flat signal reports no frequency. Raw samples are never returned: the envelope is a min/max reduction."

// MeasureWaveform measures a recorded analog stream. It works on replays with
// no hardware.
func (a *App) MeasureWaveform(ctx context.Context, q WaveformQuery) (*WaveformReport, error) {
	if q.Points <= 0 {
		q.Points = defaultWaveformPoints
	}
	if q.Points > maxWaveformPoints {
		return nil, fmt.Errorf("points %d: want 1..%d", q.Points, maxWaveformPoints)
	}
	s, err := a.OpenSession(q.Session)
	if err != nil {
		return nil, err
	}
	metas := s.StreamsByKind(session.KindAnalog, q.Source)
	if len(metas) == 0 {
		return nil, fmt.Errorf("session has no analog streams%s", suffix(q.Source))
	}
	rep := &WaveformReport{Session: q.Session, Note: waveformNote}
	for _, sm := range metas {
		idx := 0
		if q.Channel != "" {
			idx = -1
			for i, n := range sm.Channels {
				if strings.EqualFold(n, q.Channel) {
					idx = i
				}
			}
			if idx < 0 {
				if len(metas) == 1 {
					return nil, fmt.Errorf("stream %s has no channel %q (have %s)", sm.ID, q.Channel, strings.Join(sm.Channels, ", "))
				}
				continue
			}
		}
		ws, err := a.measureStream(ctx, s, sm, idx, q)
		if err != nil {
			return nil, err
		}
		rep.Streams = append(rep.Streams, *ws)
	}
	if len(rep.Streams) == 0 {
		return nil, fmt.Errorf("no analog stream has channel %q", q.Channel)
	}
	return rep, nil
}

func (a *App) measureStream(ctx context.Context, s *session.Session, sm session.StreamMeta, idx int, q WaveformQuery) (_ *WaveformStream, retErr error) {
	rd, err := s.OpenStream(sm)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, rd.Close()) }()
	from, to := q.FromNs, q.ToNs
	start, end := rd.Bounds()
	if to <= from {
		to = end + 1
		if from < start {
			from = start
		}
	}
	name := fmt.Sprintf("ch%d", idx)
	if idx < len(sm.Channels) {
		name = sm.Channels[idx]
	}
	out := &WaveformStream{Stream: sm.ID, Source: sm.Alias, Channel: name, Channels: sm.Channels, Unit: sm.Unit}

	type seg struct {
		start, period int64
		v             []float32
	}
	var segs []seg
	total := 0
	err = rd.ReadAnalog(ctx, from, to, func(c *session.AnalogChunk) error {
		if c.Channels == 0 || c.PeriodNs <= 0 || idx >= c.Channels {
			return nil
		}
		n := c.N()
		for i := 0; i < n; i++ {
			t := c.StartNs + int64(i)*c.PeriodNs
			if t < from || t >= to {
				continue
			}
			var cur *seg
			if len(segs) > 0 {
				cur = &segs[len(segs)-1]
			}
			next := int64(-1)
			if cur != nil {
				next = cur.start + int64(len(cur.v))*cur.period
			}
			if cur == nil || cur.period != c.PeriodNs || t-next > cur.period/2+1 || next-t > cur.period/2+1 {
				if len(segs) >= maxWaveformSegments {
					out.Truncated = true
					return errStopWaveform
				}
				segs = append(segs, seg{start: t, period: c.PeriodNs})
				cur = &segs[len(segs)-1]
			}
			cur.v = append(cur.v, c.Samples[i*c.Channels+idx])
			if total++; total > maxWaveformSamples {
				return fmt.Errorf("more than %d samples in the window: narrow it with from/to", maxWaveformSamples)
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStopWaveform) {
		return nil, err
	}
	for _, sg := range segs {
		ws := WaveformSegment{StartNs: sg.start, EndNs: sg.start + int64(len(sg.v))*sg.period, Samples: len(sg.v), PeriodNs: sg.period}
		tr := waveform.Trace{DT: float64(sg.period) / 1e9, V: sg.v}
		if ms, err := waveform.Measure(tr); err != nil {
			ws.Error = err.Error()
		} else {
			ws.Measurements = ms
			env := waveform.MakeEnvelope(tr, q.Points)
			ws.Envelope = &env
		}
		out.Segments = append(out.Segments, ws)
	}
	if len(out.Segments) == 0 {
		return nil, fmt.Errorf("stream %s holds no samples in the window", sm.ID)
	}
	return out, nil
}

var errStopWaveform = fmt.Errorf("stop")
