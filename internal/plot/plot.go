// Package plot renders a session's tracks (power, analog, digital, packets,
// events, markers) on a shared time axis as SVG or PNG.
package plot

import (
	"context"
	"errors"
	"fmt"
	"image/color"
	"math"
	"sort"

	"github.com/faronov/hwlab/internal/analysis"
	"github.com/faronov/hwlab/internal/session"
	"github.com/faronov/hwlab/internal/timeline"
)

// Track kinds.
const (
	KindPower   = "power"
	KindAnalog  = "analog"
	KindDigital = "digital"
	KindPacket  = "packet"
	KindEvent   = "event"
)

// TrackSpec selects what a track shows. Empty Spec.Tracks means "auto".
type TrackSpec struct {
	Kind   string   `json:"kind" yaml:"kind"`
	Source string   `json:"source,omitempty" yaml:"source"` // device alias
	Stream string   `json:"stream,omitempty" yaml:"stream"`
	Types  []string `json:"types,omitempty" yaml:"types"` // event/packet types
	Title  string   `json:"title,omitempty" yaml:"title"`
}

// Spec describes one plot.
type Spec struct {
	Title    string      `json:"title,omitempty" yaml:"title"`
	StartNs  int64       `json:"start_ns" yaml:"start_ns"` // EndNs <= StartNs: whole session
	EndNs    int64       `json:"end_ns" yaml:"end_ns"`
	Width    int         `json:"width,omitempty" yaml:"width"`
	Format   string      `json:"format,omitempty" yaml:"format"` // "svg" (default) or "png"
	LogPower bool        `json:"log_power,omitempty" yaml:"log_power"`
	Markers  *bool       `json:"markers,omitempty" yaml:"markers"` // default true
	Tracks   []TrackSpec `json:"tracks,omitempty" yaml:"tracks"`
}

// Result is a rendered plot.
type Result struct {
	Data    []byte
	Format  string
	MIME    string
	StartNs int64
	EndNs   int64
	Tracks  int
	Width   int
	Height  int
}

var palette = []color.RGBA{
	{31, 119, 180, 255}, {214, 39, 40, 255}, {44, 160, 44, 255}, {148, 103, 189, 255},
	{255, 127, 14, 255}, {23, 190, 207, 255}, {140, 86, 75, 255}, {127, 127, 127, 255},
}

var (
	colGrid   = color.RGBA{225, 225, 225, 255}
	colAxis   = color.RGBA{80, 80, 80, 255}
	colText   = color.RGBA{30, 30, 30, 255}
	colMarker = color.RGBA{200, 0, 120, 255}
)

// ErrNothingToPlot means the session has no samples or events to draw, e.g. a
// logic analyzer that only watched static lines.
var ErrNothingToPlot = errors.New("nothing to plot: session has no samples or events")

const (
	marginL, marginR, marginT, marginB = 90.0, 20.0, 34.0, 36.0
	trackGap                           = 10.0
)

type layoutTrack struct {
	spec  TrackSpec
	y, h  float64
	lanes []string // digital lanes
}

// Render draws the session according to spec.
func Render(ctx context.Context, sess *session.Session, spec Spec) (*Result, error) {
	if spec.Width <= 0 {
		spec.Width = 1200
	}
	if spec.Format == "" {
		spec.Format = "svg"
	}
	if spec.Format != "svg" && spec.Format != "png" {
		return nil, fmt.Errorf("unsupported plot format %q (use svg or png)", spec.Format)
	}
	start, end := spec.StartNs, spec.EndNs
	if end <= start {
		var err error
		if start, end, err = sessionBounds(sess); err != nil {
			return nil, err
		}
	}
	tl := sess.Timeline()
	tracks := spec.Tracks
	if len(tracks) == 0 {
		tracks = autoTracks(sess)
	}
	if len(tracks) == 0 {
		return nil, ErrNothingToPlot
	}

	var lts []layoutTrack
	y := marginT
	for _, ts := range tracks {
		lt := layoutTrack{spec: ts, y: y}
		switch ts.Kind {
		case KindPower, KindAnalog:
			lt.h = 150
		case KindDigital:
			lt.lanes = digitalLanes(tl, ts)
			if len(lt.lanes) == 0 {
				continue
			}
			lt.h = float64(len(lt.lanes))*16 + 8
		case KindPacket, KindEvent:
			lt.h = 34
		default:
			return nil, fmt.Errorf("unknown track kind %q", ts.Kind)
		}
		y += lt.h + trackGap
		lts = append(lts, lt)
	}
	height := int(y + marginB)
	width := spec.Width
	var cv canvas
	if spec.Format == "png" {
		cv = newPNG(width, height)
	} else {
		cv = newSVG(width, height)
		cv.Rect(0, 0, float64(width), float64(height), color.RGBA{255, 255, 255, 255})
	}
	x0, x1 := marginL, float64(width)-marginR
	span := float64(end - start)
	tx := func(t int64) float64 { return x0 + float64(t-start)/span*(x1-x0) }

	title := spec.Title
	if title == "" {
		title = sess.Meta().Name
	}
	cv.Text(x0, 18, fmt.Sprintf("%s   [%s .. %s]", title, timeline.FormatNs(start), timeline.FormatNs(end)), colText, anchorStart)

	// time grid
	unit, div := timeUnit(end - start)
	step := niceStep(float64(end-start) / 8)
	first := math.Ceil(float64(start)/step) * step
	for t := first; t <= float64(end); t += step {
		xx := tx(int64(t))
		cv.Line(xx, marginT, xx, y-trackGap, colGrid, 1, false)
		cv.Text(xx, y+12, trimFloat(t/div), colText, anchorMiddle)
	}
	cv.Text(x1, y+26, "time ("+unit+")", colText, anchorEnd)
	cv.Line(x0, y-trackGap, x1, y-trackGap, colAxis, 1, false)

	nColors := 0
	for _, lt := range lts {
		cv.Rect(x0, lt.y, x1-x0, lt.h, color.RGBA{250, 250, 252, 255})
		cv.Line(x0, lt.y, x0, lt.y+lt.h, colAxis, 1, false)
		label := lt.spec.Title
		if label == "" {
			label = lt.spec.Source
			if lt.spec.Stream != "" && lt.spec.Kind != KindDigital {
				label += "/" + lt.spec.Stream
			}
			if lt.spec.Kind == KindPacket || lt.spec.Kind == KindEvent {
				label += " " + lt.spec.Kind + "s"
			}
		}
		col := palette[nColors%len(palette)]
		nColors++
		var err error
		switch lt.spec.Kind {
		case KindPower:
			err = drawPower(ctx, cv, sess, lt, label, col, start, end, x0, x1, tx, spec.LogPower)
		case KindAnalog:
			err = drawAnalog(ctx, cv, sess, lt, label, col, start, end, x0, x1, tx)
		case KindDigital:
			drawDigital(cv, tl, lt, col, start, end, x0, x1, tx)
		case KindPacket, KindEvent:
			drawTicks(cv, tl, lt, label, col, start, end, x0, x1, tx)
		}
		if err != nil {
			return nil, err
		}
	}

	if spec.Markers == nil || *spec.Markers {
		for _, lt := range lts {
			if lt.spec.Kind == KindPower {
				drawSpikeMarks(cv, tl, lt, y-trackGap, start, end, tx)
			}
		}
		for _, m := range tl.Between(start, end, timeline.Filter{Types: []string{timeline.TypeMarker}}, 200) {
			xx := tx(m.TimestampNs)
			cv.Line(xx, marginT, xx, y-trackGap, colMarker, 1, true)
			cv.Text(xx+3, marginT+10, m.Summary, colMarker, anchorStart)
		}
	}

	data, err := cv.Bytes()
	if err != nil {
		return nil, err
	}
	mime := "image/svg+xml"
	if spec.Format == "png" {
		mime = "image/png"
	}
	return &Result{Data: data, Format: spec.Format, MIME: mime, StartNs: start, EndNs: end, Tracks: len(lts), Width: width, Height: height}, nil
}

func sessionBounds(sess *session.Session) (int64, int64, error) {
	var lo, hi int64
	have := false
	upd := func(s, e int64) {
		if !have || s < lo {
			lo = s
		}
		if !have || e > hi {
			hi = e
		}
		have = true
	}
	if s, e, ok := sess.Timeline().Bounds(); ok {
		upd(s, e)
	}
	for _, c := range sess.Meta().Captures {
		for _, sm := range c.Streams {
			if sm.File == "" {
				continue
			}
			rd, err := sess.OpenStream(sm)
			if err != nil {
				return 0, 0, err
			}
			s, e := rd.Bounds()
			if err := rd.Close(); err != nil {
				return 0, 0, fmt.Errorf("close stream %s: %w", sm.ID, err)
			}
			upd(s, e)
		}
	}
	if !have || hi <= lo {
		return 0, 0, ErrNothingToPlot
	}
	return lo, hi, nil
}

func autoTracks(sess *session.Session) []TrackSpec {
	var out []TrackSpec
	for _, kind := range []string{session.KindPower, session.KindAnalog} {
		for _, sm := range sess.StreamsByKind(kind, "") {
			out = append(out, TrackSpec{Kind: kind, Source: sm.Alias, Stream: sm.ID})
		}
	}
	tl := sess.Timeline()
	perSource := map[string]map[string]bool{}
	var order []string
	for _, e := range tl.All() {
		if perSource[e.SourceID] == nil {
			perSource[e.SourceID] = map[string]bool{}
			order = append(order, e.SourceID)
		}
		perSource[e.SourceID][e.Type] = true
	}
	for _, src := range order {
		types := perSource[src]
		if types[timeline.TypeDigitalEdge] {
			out = append(out, TrackSpec{Kind: KindDigital, Source: src})
		}
		if types[timeline.TypePacket] {
			out = append(out, TrackSpec{Kind: KindPacket, Source: src, Types: []string{timeline.TypePacket}})
		}
		var other []string
		for t := range types {
			switch t {
			case timeline.TypeDigitalEdge, timeline.TypePacket, timeline.TypeMarker, "warning", "error":
			default:
				other = append(other, t)
			}
		}
		if len(other) > 0 {
			sort.Strings(other)
			out = append(out, TrackSpec{Kind: KindEvent, Source: src, Types: other})
		}
	}
	return out
}

// ---------------------------------------------------------------- tracks

func yScale(lt layoutTrack) (top, bottom float64) { return lt.y + 14, lt.y + lt.h - 6 }

func drawPower(ctx context.Context, cv canvas, sess *session.Session, lt layoutTrack, label string, col color.RGBA,
	start, end int64, x0, x1 float64, tx func(int64) float64, logScale bool) error {
	var sm *session.StreamMeta
	for _, s := range sess.StreamsByKind(session.KindPower, lt.spec.Source) {
		if lt.spec.Stream == "" || s.ID == lt.spec.Stream {
			s := s
			sm = &s
			break
		}
	}
	if sm == nil {
		return fmt.Errorf("power stream %s/%s not found", lt.spec.Source, lt.spec.Stream)
	}
	rd, err := sess.OpenStream(*sm)
	if err != nil {
		return err
	}
	n := int(x1 - x0)
	bs, err := analysis.Envelope(ctx, rd, start, end, n)
	// Envelope owns its output, so release the read-only stream before drawing.
	if closeErr := rd.Close(); closeErr != nil {
		err = errors.Join(err, fmt.Errorf("close power stream: %w", closeErr))
	}
	if err != nil {
		return err
	}
	maxV, minV := 0.0, math.Inf(1)
	for _, b := range bs {
		if b.N == 0 {
			continue
		}
		maxV = math.Max(maxV, b.MaxNA)
		if p := math.Max(b.MinNA, 1); p < minV {
			minV = p
		}
	}
	if maxV <= 0 {
		maxV = 1
	}
	top, bottom := yScale(lt)
	var ymap func(float64) float64
	if logScale {
		lo, hi := math.Log10(math.Max(minV, 1)), math.Log10(math.Max(maxV, 10))
		if hi-lo < 1 {
			hi = lo + 1
		}
		ymap = func(v float64) float64 {
			return bottom - (math.Log10(math.Max(v, 1))-lo)/(hi-lo)*(bottom-top)
		}
		for d := math.Ceil(lo); d <= hi; d++ {
			yy := ymap(math.Pow(10, d))
			cv.Line(x0, yy, x1, yy, colGrid, 1, false)
			cv.Text(x0-4, yy+4, analysis.FormatCurrent(math.Pow(10, d)), colText, anchorEnd)
		}
	} else {
		hi := maxV * 1.05
		ymap = func(v float64) float64 { return bottom - v/hi*(bottom-top) }
		for i := 0; i <= 3; i++ {
			v := hi * float64(i) / 3
			yy := ymap(v)
			cv.Line(x0, yy, x1, yy, colGrid, 1, false)
			cv.Text(x0-4, yy+4, analysis.FormatCurrent(v), colText, anchorEnd)
		}
	}
	cv.Text(x0+4, lt.y+11, label+"  (min/max envelope + mean)", col, anchorStart)
	fill := color.RGBA{col.R, col.G, col.B, 90}
	var px, py float64
	havePrev := false
	for i, b := range bs {
		if b.N == 0 {
			havePrev = false
			continue
		}
		xx := x0 + float64(i)
		yHi, yLo := ymap(b.MaxNA), ymap(b.MinNA)
		cv.Rect(xx, yHi, 1, math.Max(yLo-yHi, 1), fill)
		ym := ymap(b.MeanNA)
		if havePrev {
			cv.Line(px, py, xx, ym, col, 1, false)
		}
		px, py, havePrev = xx, ym, true
	}
	return nil
}

func drawAnalog(ctx context.Context, cv canvas, sess *session.Session, lt layoutTrack, label string, col color.RGBA,
	start, end int64, x0, x1 float64, tx func(int64) float64) error {
	var sm *session.StreamMeta
	for _, s := range sess.StreamsByKind(session.KindAnalog, lt.spec.Source) {
		if lt.spec.Stream == "" || s.ID == lt.spec.Stream {
			s := s
			sm = &s
			break
		}
	}
	if sm == nil {
		return fmt.Errorf("analog stream %s/%s not found", lt.spec.Source, lt.spec.Stream)
	}
	rd, err := sess.OpenStream(*sm)
	if err != nil {
		return err
	}
	nch := len(sm.Channels)
	if nch == 0 {
		nch = 1
	}
	n := int(x1 - x0)
	width := float64(end-start) / float64(n)
	mins := make([][]float64, nch)
	maxs := make([][]float64, nch)
	for c := range mins {
		mins[c], maxs[c] = make([]float64, n), make([]float64, n)
		for i := range mins[c] {
			mins[c][i], maxs[c][i] = math.Inf(1), math.Inf(-1)
		}
	}
	lo, hi := math.Inf(1), math.Inf(-1)
	err = rd.ReadAnalog(ctx, start, end, func(c *session.AnalogChunk) error {
		for i := 0; i < c.N(); i++ {
			t := c.StartNs + int64(i)*c.PeriodNs
			if t < start || t >= end {
				continue
			}
			bi := int(float64(t-start) / width)
			if bi >= n {
				bi = n - 1
			}
			for ch := 0; ch < c.Channels && ch < nch; ch++ {
				v := float64(c.Samples[i*c.Channels+ch])
				mins[ch][bi] = math.Min(mins[ch][bi], v)
				maxs[ch][bi] = math.Max(maxs[ch][bi], v)
				lo, hi = math.Min(lo, v), math.Max(hi, v)
			}
		}
		return nil
	})
	err = errors.Join(err, rd.Close())
	if err != nil {
		return err
	}
	if math.IsInf(lo, 1) {
		return nil
	}
	if hi-lo < 1e-12 {
		hi = lo + 1
	}
	top, bottom := yScale(lt)
	ymap := func(v float64) float64 { return bottom - (v-lo)/(hi-lo)*(bottom-top) }
	for i := 0; i <= 3; i++ {
		v := lo + (hi-lo)*float64(i)/3
		cv.Line(x0, ymap(v), x1, ymap(v), colGrid, 1, false)
		cv.Text(x0-4, ymap(v)+4, fmt.Sprintf("%.4g %s", v, sm.Unit), colText, anchorEnd)
	}
	cv.Text(x0+4, lt.y+11, label, col, anchorStart)
	for ch := 0; ch < nch; ch++ {
		c := palette[(ch+1)%len(palette)]
		for i := 0; i < n; i++ {
			if math.IsInf(mins[ch][i], 1) {
				continue
			}
			yHi, yLo := ymap(maxs[ch][i]), ymap(mins[ch][i])
			cv.Rect(x0+float64(i), yHi, 1, math.Max(yLo-yHi, 1), c)
		}
	}
	return nil
}

func digitalLanes(tl *timeline.Timeline, ts TrackSpec) []string {
	type lane struct {
		name string
		bit  int
	}
	seen := map[string]lane{}
	f := timeline.Filter{Types: []string{timeline.TypeDigitalEdge}}
	if ts.Source != "" {
		f.Sources = []string{ts.Source}
	}
	for _, e := range tl.Between(math.MinInt64/2, math.MaxInt64/2, f, 0) {
		name, _ := e.Data["channel"].(string)
		bit, _ := e.Data["bit"].(float64)
		if b, ok := e.Data["bit"].(int); ok {
			bit = float64(b)
		}
		if _, ok := seen[name]; !ok {
			seen[name] = lane{name, int(bit)}
		}
	}
	lanes := make([]lane, 0, len(seen))
	for _, l := range seen {
		lanes = append(lanes, l)
	}
	sort.Slice(lanes, func(i, j int) bool { return lanes[i].bit < lanes[j].bit })
	out := make([]string, len(lanes))
	for i, l := range lanes {
		out[i] = l.name
	}
	return out
}

func drawDigital(cv canvas, tl *timeline.Timeline, lt layoutTrack, col color.RGBA, start, end int64, x0, x1 float64, tx func(int64) float64) {
	f := timeline.Filter{Types: []string{timeline.TypeDigitalEdge}}
	if lt.spec.Source != "" {
		f.Sources = []string{lt.spec.Source}
	}
	all := tl.Between(math.MinInt64/2, math.MaxInt64/2, f, 0)
	for li, name := range lt.lanes {
		laneTop := lt.y + 4 + float64(li)*16
		hiY, loY := laneTop+2, laneTop+12
		cv.Text(x0-4, loY, name, colText, anchorEnd)
		cv.Line(x0, loY, x1, loY, colGrid, 1, false)
		// Level before the window: opposite of the first edge seen inside
		// or the last edge before it.
		level := false
		known := false
		var edges []timeline.Event
		for _, e := range all {
			if n, _ := e.Data["channel"].(string); n != name {
				continue
			}
			if e.TimestampNs < start {
				level, known = e.Data["level"] == true, true
				continue
			}
			if e.TimestampNs > end {
				break
			}
			edges = append(edges, e)
		}
		if !known && len(edges) > 0 {
			level = edges[0].Data["level"] != true
		}
		prevX := x0
		yy := func(l bool) float64 {
			if l {
				return hiY
			}
			return loY
		}
		for _, e := range edges {
			xx := tx(e.TimestampNs)
			cv.Line(prevX, yy(level), xx, yy(level), col, 1.5, false)
			level = e.Data["level"] == true
			cv.Line(xx, loY, xx, hiY, col, 1.5, false)
			prevX = xx
		}
		cv.Line(prevX, yy(level), x1, yy(level), col, 1.5, false)
	}
}

var colSpike = color.RGBA{230, 120, 0, 255}

// maxSpikeMarks bounds the clutter: with more spikes than this the lines stop
// being readable and only the envelope is shown.
const maxSpikeMarks = 40

// drawSpikeMarks draws a numbered vertical line at the start of every detected
// current spike of the track's source, across all tracks, so that packets can be
// compared against spikes on one time axis. The numbers match `hwlab explain`.
func drawSpikeMarks(cv canvas, tl *timeline.Timeline, lt layoutTrack, bottom float64, start, end int64, tx func(int64) float64) {
	f := timeline.Filter{Types: []string{timeline.TypePowerRise}}
	if lt.spec.Source != "" {
		f.Sources = []string{lt.spec.Source}
	}
	all := tl.Between(-1<<62, 1<<62, f, 0)
	if len(all) == 0 || len(all) > maxSpikeMarks {
		return
	}
	for i, e := range all {
		if e.TimestampNs < start || e.TimestampNs > end {
			continue
		}
		xx := tx(e.TimestampNs)
		cv.Line(xx, lt.y, xx, bottom, colSpike, 1, true)
		cv.Text(xx+2, lt.y+24, fmt.Sprintf("#%d", i+1), colSpike, anchorStart)
	}
}

func drawTicks(cv canvas, tl *timeline.Timeline, lt layoutTrack, label string, col color.RGBA, start, end int64, x0, x1 float64, tx func(int64) float64) {
	f := timeline.Filter{Types: lt.spec.Types}
	if lt.spec.Source != "" {
		f.Sources = []string{lt.spec.Source}
	}
	evs := tl.Between(start, end, f, 0)
	cv.Text(x0+4, lt.y+11, fmt.Sprintf("%s (%d)", label, len(evs)), col, anchorStart)
	lastLabel := -1e9
	for _, e := range evs {
		xx := tx(e.TimestampNs)
		cv.Line(xx, lt.y+16, xx, lt.y+lt.h-3, col, 1.5, false)
		if len(evs) <= 40 && xx-lastLabel > 70 {
			s := e.Summary
			if len(s) > 14 {
				s = s[:14]
			}
			cv.Text(xx+2, lt.y+lt.h-5, s, colText, anchorStart)
			lastLabel = xx
		}
	}
}

// ---------------------------------------------------------------- axis

func timeUnit(span int64) (string, float64) {
	switch {
	case span >= 2_000_000_000:
		return "s", 1e9
	case span >= 2_000_000:
		return "ms", 1e6
	}
	return "µs", 1e3
}

func niceStep(raw float64) float64 {
	if raw <= 0 {
		return 1
	}
	exp := math.Pow(10, math.Floor(math.Log10(raw)))
	for _, m := range []float64{1, 2, 5, 10} {
		if m*exp >= raw {
			return m * exp
		}
	}
	return 10 * exp
}

func trimFloat(v float64) string {
	return fmt.Sprintf("%g", math.Round(v*1000)/1000)
}
