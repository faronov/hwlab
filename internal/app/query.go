package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/faronov/hwlab/internal/analysis"
	"github.com/faronov/hwlab/internal/apperr"
	"github.com/faronov/hwlab/internal/correlation"
	"github.com/faronov/hwlab/internal/plot"
	"github.com/faronov/hwlab/internal/session"
	"github.com/faronov/hwlab/internal/timeline"
)

func toStruct(m map[string]any) (*structpb.Struct, error) {
	s, err := structpb.NewStruct(m)
	if err != nil {
		return nil, apperr.Wrap(apperr.InvalidArgument, err)
	}
	return s, nil
}

// ParseTime parses a session time: a Go duration ("12.5ms", "1s", "250us"),
// or a bare integer meaning nanoseconds.
func ParseTime(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n, nil
	}
	d, err := time.ParseDuration(strings.ReplaceAll(s, "µ", "u"))
	if err != nil {
		return 0, apperr.Newf(apperr.InvalidArgument, "invalid time %q (use e.g. 12.5ms, 1s, or integer nanoseconds)", s)
	}
	return int64(d), nil
}

// ResolveSession maps a path, id, unique id prefix or "latest" to a directory.
func (a *App) ResolveSession(ref string) (string, error) {
	dir, err := session.Resolve(a.Cfg.SessionsDir, ref)
	if err != nil {
		return "", apperr.Wrap(apperr.NotFound, fmt.Errorf("%w: %w", errNoSuchSession, err))
	}
	return dir, nil
}

// OpenSession opens a session read-only.
func (a *App) OpenSession(ref string) (*session.Session, error) {
	dir, err := a.ResolveSession(ref)
	if err != nil {
		return nil, err
	}
	return session.Open(dir)
}

// ListSessions returns stored sessions, newest first.
func (a *App) ListSessions() ([]session.Meta, error) { return session.List(a.Cfg.SessionsDir) }

// SessionInfo is a compact description of a session.
type SessionInfo struct {
	Meta      session.Meta   `json:"meta"`
	Dir       string         `json:"dir"`
	Events    map[string]int `json:"events_by_type"`
	Sources   []string       `json:"sources"`
	Streams   []StreamInfo   `json:"streams"`
	Artifacts []string       `json:"artifacts"`
	Plots     []string       `json:"plots"`
}

// StreamInfo describes a stored sample stream.
type StreamInfo struct {
	ID      string `json:"id"`
	Alias   string `json:"alias"`
	Kind    string `json:"kind"`
	Samples int64  `json:"samples"`
	StartNs int64  `json:"start_ns"`
	EndNs   int64  `json:"end_ns"`
}

// SessionInfo describes a session.
func (a *App) SessionInfo(ref string) (*SessionInfo, error) {
	s, err := a.OpenSession(ref)
	if err != nil {
		return nil, err
	}
	return describe(s), nil
}

func describe(s *session.Session) *SessionInfo {
	info := &SessionInfo{Meta: s.Meta(), Dir: s.Dir, Events: s.Timeline().Count(), Sources: s.Timeline().Sources()}
	for _, c := range info.Meta.Captures {
		for _, st := range c.Streams {
			si := StreamInfo{ID: st.ID, Alias: st.Alias, Kind: st.Kind}
			if rd, err := s.OpenStream(st); err == nil {
				si.Samples = rd.Samples()
				si.StartNs, si.EndNs = rd.Bounds()
				if err := rd.Close(); err != nil {
					slog.Warn("close stream while describing session", "err", err)
				}
			}
			info.Streams = append(info.Streams, si)
		}
	}
	for _, art := range s.Artifacts.List() {
		info.Artifacts = append(info.Artifacts, art.Path)
	}
	plots, _ := filepath.Glob(filepath.Join(s.Dir, "plots", "*"))
	for _, p := range plots {
		info.Plots = append(info.Plots, filepath.ToSlash(filepath.Join("plots", filepath.Base(p))))
	}
	return info
}

// TimelineQuery selects events.
type TimelineQuery struct {
	Session  string
	FromNs   int64
	ToNs     int64 // <= FromNs: to the end
	Types    []string
	Sources  []string
	Contains string
	Limit    int // default 200
}

// Filter converts to a timeline filter.
func (q TimelineQuery) Filter() timeline.Filter {
	return timeline.Filter{Types: q.Types, Sources: q.Sources, Contains: q.Contains}
}

// QueryTimeline returns events in a time range.
func (a *App) QueryTimeline(q TimelineQuery) ([]timeline.Event, error) {
	s, err := a.OpenSession(q.Session)
	if err != nil {
		return nil, err
	}
	to := q.ToNs
	if to <= q.FromNs {
		to = math.MaxInt64
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 200
	}
	return s.Timeline().Between(q.FromNs, to, q.Filter(), limit), nil
}

// NearestResult is the closest event to a point in time.
type NearestResult struct {
	Found bool           `json:"found"`
	Event timeline.Event `json:"event,omitempty"`
	Delta int64          `json:"delta_ns"`
}

// Nearest finds the event closest to ts matching the filter.
func (a *App) Nearest(ref string, ts int64, f timeline.Filter) (NearestResult, error) {
	s, err := a.OpenSession(ref)
	if err != nil {
		return NearestResult{}, err
	}
	h, ok := correlation.Nearest(s.Timeline(), ts, f)
	if !ok {
		return NearestResult{}, nil
	}
	return NearestResult{Found: true, Event: h.Event, Delta: h.DeltaNs}, nil
}

// CorrelateQuery asks what happens near events of kind A.
type CorrelateQuery struct {
	Session  string
	A        timeline.Filter // anchor events (e.g. power rises)
	B        timeline.Filter // related events (e.g. packets)
	WindowNs int64           // search window around/after each anchor (default 5ms)
	Limit    int             // anchors reported (default 50)
}

// Correlation is one anchor with its related events.
type Correlation struct {
	Anchor  timeline.Event    `json:"anchor"`
	Related []correlation.Hit `json:"related"`
}

// CorrelationResult groups correlations and a latency summary of the first
// B event following each anchor.
type CorrelationResult struct {
	Anchors      int                 `json:"anchors"`
	Matched      int                 `json:"matched"`
	Correlations []Correlation       `json:"correlations"`
	FollowDelay  correlation.Summary `json:"first_following_delay"`
}

// Correlate relates events of two kinds.
func (a *App) Correlate(q CorrelateQuery) (*CorrelationResult, error) {
	s, err := a.OpenSession(q.Session)
	if err != nil {
		return nil, err
	}
	window := q.WindowNs
	if window <= 0 {
		window = 5_000_000
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	tl := s.Timeline()
	anchors := tl.Between(math.MinInt64, math.MaxInt64, q.A, 0)
	res := &CorrelationResult{Anchors: len(anchors)}
	for _, an := range anchors {
		hits := correlation.Around(tl, an.TimestampNs, window, q.B, 20)
		if len(hits) > 0 {
			res.Matched++
		}
		if len(res.Correlations) < limit {
			res.Correlations = append(res.Correlations, Correlation{Anchor: an, Related: hits})
		}
	}
	res.FollowDelay = correlation.Summarize(correlation.Pairs(tl, q.A, q.B, window))
	return res, nil
}

// CalibrateQuery selects the shared events used to estimate a clock offset.
type CalibrateQuery struct {
	Session  string
	A        timeline.Filter // reference device events
	B        timeline.Filter // events of the device to be aligned to A
	WindowNs int64           // default 20 ms
}

// CalibrateResult is a clock-offset estimate with its evidence.
type CalibrateResult struct {
	correlation.Calibration
	SuggestedOffsetNs int64    `json:"suggested_clock_offset_ns"`
	Warnings          []string `json:"warnings,omitempty"`
}

// Calibrate estimates the offset of B's clock relative to A's from events that
// both devices saw at the same instant (a shared trigger).
func (a *App) Calibrate(q CalibrateQuery) (*CalibrateResult, error) {
	s, err := a.OpenSession(q.Session)
	if err != nil {
		return nil, err
	}
	window := q.WindowNs
	if window <= 0 {
		window = 20_000_000
	}
	c := correlation.Calibrate(s.Timeline(), q.A, q.B, window)
	res := &CalibrateResult{Calibration: c, SuggestedOffsetNs: c.SuggestedOffsetNs()}
	switch {
	case c.Pairs == 0:
		res.Warnings = append(res.Warnings, "no unambiguous event pairs: check the filters and the window")
		res.SuggestedOffsetNs = 0
	case c.Pairs < 3:
		res.Warnings = append(res.Warnings, "fewer than 3 pairs: the estimate is not trustworthy")
	}
	if c.Ambiguous > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%d reference events had several candidates and were skipped; use a sparser trigger or a smaller window", c.Ambiguous))
	}
	if c.Pairs > 0 && c.MADNs > c.MedianNs/4 && c.MADNs > 100_000 {
		res.Warnings = append(res.Warnings, "the deltas scatter widely: the events are probably not simultaneous (causal delays are not clock offsets)")
	}
	return res, nil
}

// AnalyzeOptions controls power analysis.
type AnalyzeOptions struct {
	Session        string
	Source         string // alias; empty = all power streams
	FromNs, ToNs   int64  // ToNs <= FromNs: whole stream
	NominalVoltage float64
	Spikes         bool
}

// PowerAnalysis is the analysis of one power stream.
type PowerAnalysis struct {
	Stream  string           `json:"stream"`
	Source  string           `json:"source"`
	Stats   analysis.Stats   `json:"stats"`
	Spikes  []analysis.Spike `json:"spikes,omitempty"`
	MeanStr string           `json:"mean"`
	PeakStr string           `json:"peak"`
}

// Analyze computes power statistics (and optionally spikes) for a session.
func (a *App) Analyze(ctx context.Context, o AnalyzeOptions) ([]PowerAnalysis, error) {
	s, err := a.OpenSession(o.Session)
	if err != nil {
		return nil, err
	}
	var out []PowerAnalysis
	for _, sm := range s.StreamsByKind(session.KindPower, o.Source) {
		rd, err := s.OpenStream(sm)
		if err != nil {
			return nil, err
		}
		from, to := o.FromNs, o.ToNs
		start, end := rd.Bounds()
		if to <= from {
			from, to = start, end+1
		}
		st, err := analysis.ComputeStats(ctx, rd, from, to, o.NominalVoltage)
		pa := PowerAnalysis{Stream: sm.ID, Source: sm.Alias, Stats: st}
		if err == nil {
			pa.MeanStr, pa.PeakStr = analysis.FormatCurrent(st.MeanNA), analysis.FormatCurrent(st.MaxNA)
			if o.Spikes {
				pa.Spikes, err = analysis.DetectSpikes(ctx, rd, from, to, o.NominalVoltage, analysis.SpikeOptions{})
			}
		}
		err = errors.Join(err, rd.Close())
		if err != nil {
			return nil, err
		}
		out = append(out, pa)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("session has no power streams%s", suffix(o.Source))
	}
	return out, nil
}

func suffix(src string) string {
	if src == "" {
		return ""
	}
	return " for source " + src
}

// PlotResult is a rendered plot stored in the session.
type PlotResult struct {
	Path    string `json:"path"`
	Format  string `json:"format"`
	MIME    string `json:"mime"`
	StartNs int64  `json:"start_ns"`
	EndNs   int64  `json:"end_ns"`
	Tracks  int    `json:"tracks"`
	Width   int    `json:"width"`
	Height  int    `json:"height"`
	Bytes   int    `json:"bytes"`
	Data    []byte `json:"-"`
}

// RenderPlot renders a plot of a recorded session into its plots/ directory
// (or to out, when given).
func (a *App) RenderPlot(ctx context.Context, ref, name, out string, spec plot.Spec) (*PlotResult, error) {
	s, err := a.OpenSession(ref)
	if err != nil {
		return nil, err
	}
	r, err := plot.Render(ctx, s, spec)
	if err != nil {
		return nil, err
	}
	path := out
	if path == "" {
		if name == "" {
			name = "plot"
		}
		path = filepath.Join(s.Dir, "plots", sanitizeName(name)+"."+r.Format)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, r.Data, 0o644); err != nil {
		return nil, err
	}
	return &PlotResult{Path: path, Format: r.Format, MIME: r.MIME, StartNs: r.StartNs, EndNs: r.EndNs,
		Tracks: r.Tracks, Width: r.Width, Height: r.Height, Bytes: len(r.Data), Data: r.Data}, nil
}

// writePlot renders into an open (live) session and returns the relative path.
func (a *App) writePlot(ctx context.Context, s *session.Session, name string, spec plot.Spec) (string, error) {
	r, err := plot.Render(ctx, s, spec)
	if err != nil {
		return "", err
	}
	rel := filepath.Join("plots", sanitizeName(name)+"."+r.Format)
	if err := os.MkdirAll(filepath.Join(s.Dir, "plots"), 0o755); err != nil {
		return "", err
	}
	return rel, os.WriteFile(filepath.Join(s.Dir, rel), r.Data, 0o644)
}

func sanitizeName(n string) string {
	n = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		}
		return '_'
	}, n)
	if n == "" {
		return "plot"
	}
	return n
}

// ClockCalibrationQuery selects the shared digital signal used to measure the
// clock mapping between two devices.
type ClockCalibrationQuery struct {
	Session string
	Ref     string // reference device alias
	Dev     string // device whose clock is mapped onto the reference
	RefChan string // channel name filter (optional)
	DevChan string
	// TolNs is the interval tolerance for edge matching; 0 = automatic from
	// the coarser of the two clock resolutions.
	TolNs int64
	Save  bool
}

// ClockCalibrationResult is a measured clock mapping with its evidence.
type ClockCalibrationResult struct {
	Calibration session.ClockCalibration `json:"calibration"`
	Fit         correlation.AffineFit    `json:"fit"`
	RefEdges    int                      `json:"ref_edges"`
	DevEdges    int                      `json:"dev_edges"`
	EdgeShift   int                      `json:"edge_shift"`
	MatchScore  int                      `json:"match_score"`
	MatchRunner int                      `json:"match_runner_up"`
	Markers     int                      `json:"markers_matched"`
	Warnings    []string                 `json:"warnings,omitempty"`
	Saved       bool                     `json:"saved"`
}

// CalibrateClocks measures an affine clock mapping (session = a*t + b) of one
// device against another from digital edges both of them observed. Unlike
// Calibrate it uses the whole edge sequence, so it also measures clock-rate
// differences. The timeline is not modified; with Save the mapping is stored in
// the session metadata as evidence.
func (a *App) CalibrateClocks(q ClockCalibrationQuery) (*ClockCalibrationResult, error) {
	dir, err := a.ResolveSession(q.Session)
	if err != nil {
		return nil, err
	}
	var s *session.Session
	if q.Save {
		s, err = session.OpenWritable(dir)
	} else {
		s, err = session.Open(dir)
	}
	if err != nil {
		return nil, err
	}
	if q.Ref == "" || q.Dev == "" {
		return nil, fmt.Errorf("both a reference and a device alias are required")
	}
	ref := correlation.EdgesOf(s.Timeline(), q.Ref, q.RefChan)
	dev := correlation.EdgesOf(s.Timeline(), q.Dev, q.DevChan)
	if len(ref) == 0 || len(dev) == 0 {
		return nil, fmt.Errorf("no digital edges: %s has %d, %s has %d", q.Ref, len(ref), q.Dev, len(dev))
	}
	resNs := func(alias string) int64 {
		var r int64
		for _, c := range s.Meta().Captures {
			if c.Alias == alias && c.Clock != nil && c.Clock.ResolutionNs > r {
				r = c.Clock.ResolutionNs
			}
		}
		return r
	}
	rRef, rDev := resNs(q.Ref), resNs(q.Dev)
	tol := q.TolNs
	if tol <= 0 {
		tol = 2 * max(rRef, rDev, 1000)
	}
	// Interval comparison tolerates the timestamp quantisation of both clocks
	// on both ends of the interval, plus a little clock-rate error.
	m, err := correlation.MatchEdges(ref, dev, 0, 2*tol)
	res := &ClockCalibrationResult{RefEdges: len(ref), DevEdges: len(dev),
		EdgeShift: m.Shift, MatchScore: m.Score, MatchRunner: m.Runner, Markers: m.Markers}
	if err != nil {
		return res, err
	}
	fit, err := correlation.FitAffine(m.Pairs, 4*tol)
	if err != nil {
		return res, err
	}
	res.Fit = fit
	unc := fit.ResidualMaxNs + rRef/2 + rDev/2
	cal := session.ClockCalibration{
		Alias: q.Dev, Reference: q.Ref, Method: "shared-digital-edges/least-squares-affine",
		Signal: q.RefChan + "~" + q.DevChan, Pairs: fit.Pairs, Rejected: fit.Rejected, SpanNs: fit.SpanNs,
		RefDeviceNs: fit.Map.RefDeviceNs, RefSessionNs: fit.Map.RefSessionNs, Rate: fit.Map.Rate,
		InitialOffsetNs: fit.InitialOffsetNs, DriftPPM: fit.DriftPPM, DriftStdErrPPM: fit.DriftStdErrPPM,
		ResidualMedianNs: fit.ResidualMedianNs, ResidualP95Ns: fit.ResidualP95Ns, ResidualMaxNs: fit.ResidualMaxNs,
		UncertaintyNs: unc, OffsetOnlyP95Ns: fit.OffsetOnlyP95Ns,
		Note: "uncertainty = worst residual + half of each timestamp resolution; common-mode delays and probe-point skew are not observable",
	}
	res.Calibration = cal
	if fit.Pairs < 100 {
		res.Warnings = append(res.Warnings, "fewer than 100 matched edges: the rate estimate is weak")
	}
	if fit.DriftStdErrPPM > 0 && math.Abs(fit.DriftPPM) < 3*fit.DriftStdErrPPM {
		res.Warnings = append(res.Warnings, "measured drift is within 3 standard errors of zero: not distinguishable from no drift")
	}
	if q.Save {
		s.Update(func(m *session.Meta) {
			out := m.ClockCalibrations[:0]
			for _, c := range m.ClockCalibrations {
				if c.Alias != cal.Alias || c.Reference != cal.Reference {
					out = append(out, c)
				}
			}
			m.ClockCalibrations = append(out, cal)
		})
		if err := s.Save(); err != nil {
			return res, err
		}
		res.Saved = true
	}
	return res, nil
}
