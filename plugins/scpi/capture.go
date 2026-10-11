package scpiplugin

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
)

// StreamAnalog is the stream id of every waveform event.
const StreamAnalog = "analog"

const (
	pollEvery      = 50 * time.Millisecond
	maxChunk       = 50_000 // samples per analog event
	defaultTrigger = 10 * time.Second
	emitTimeout    = 5 * time.Second
)

// capSettings are the capture-level settings.
type capSettings struct {
	channels   []int
	trigger    string // "none" reads what the instrument holds; "single" arms and waits
	trigWait   time.Duration
	screenshot bool
	waveform   bool
}

func parseCapture(cfg map[string]any, nch int) (capSettings, error) {
	cs := capSettings{channels: []int{1}, trigger: "none", trigWait: defaultTrigger, waveform: true}
	if err := checkKeys(cfg); err != nil {
		return cs, err
	}
	if v, ok := cfg["channels"]; ok {
		cs.channels = nil
		list, isList := v.([]any)
		if !isList {
			list = []any{v}
		}
		seen := map[int]bool{}
		for _, e := range list {
			f, isNum := e.(float64)
			if !isNum || f != math.Trunc(f) || f < 1 || int(f) > nch {
				return cs, fmt.Errorf("channels: %v is not a channel number 1..%d", e, nch)
			}
			if !seen[int(f)] {
				seen[int(f)] = true
				cs.channels = append(cs.channels, int(f))
			}
		}
		if len(cs.channels) == 0 {
			return cs, errors.New("channels is empty")
		}
	}
	if s, err := optString(cfg, "trigger"); err != nil {
		return cs, err
	} else if s != "" {
		if s != "none" && s != "single" {
			return cs, fmt.Errorf("trigger %q: want none (read the instrument's current memory) or single (arm and wait for a trigger)", s)
		}
		cs.trigger = s
	}
	if v, ok, err := optNumber(cfg, "trigger_timeout_s"); err != nil {
		return cs, err
	} else if ok {
		if v <= 0 || v > 3600 {
			return cs, fmt.Errorf("trigger_timeout_s %v: want 0..3600", v)
		}
		cs.trigWait = time.Duration(v * float64(time.Second))
	}
	if b, err := optBool(cfg, "screenshot"); err != nil {
		return cs, err
	} else if b != nil {
		cs.screenshot = *b
	}
	if b, err := optBool(cfg, "waveform"); err != nil {
		return cs, err
	} else if b != nil {
		cs.waveform = *b
	}
	if !cs.waveform && !cs.screenshot {
		return cs, errors.New("nothing to capture: waveform and screenshot are both off")
	}
	return cs, nil
}

// capture is one acquire-and-export run: the instrument is armed (optionally),
// each channel's waveform is transferred, and the result becomes analog events
// plus CSV artifacts. Like Saleae it is capture/export oriented, not a live
// stream.
type capture struct {
	dev     *device
	release func()
	req     pluginsdk.CaptureRequest
	cs      capSettings
	sink    *pluginsdk.Sink
	synth   bool
	done    chan struct{}

	mu        sync.Mutex
	err       error
	artifacts []*pluginv1.ArtifactRef
	events    uint64
	samples   uint64
	refMode   string
	waited    time.Duration
	rttMax    time.Duration
}

func (c *capture) fail(err error) {
	c.mu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.mu.Unlock()
}

// StartCapture implements pluginsdk.Plugin.
func (p *Plugin) StartCapture(ctx context.Context, req pluginsdk.CaptureRequest, sink *pluginsdk.Sink) (pluginsdk.Capture, *pluginsdk.CaptureStart, error) {
	d, release, err := p.lease(req.DeviceID, req.Config)
	if err != nil {
		return nil, nil, err
	}
	fail := func(e error) (pluginsdk.Capture, *pluginsdk.CaptureStart, error) {
		release()
		return nil, nil, e
	}
	if d.scope.P.Scopeless() {
		return fail(status.Errorf(codes.FailedPrecondition, "%s has no oscilloscope profile (%s): captures need rigol or siglent semantics", req.DeviceID, d.scope.P.ID))
	}
	cs, err := parseCapture(req.Config, d.scope.Channels)
	if err != nil {
		return fail(argErr(err))
	}
	if req.ArtifactDir != "" {
		if err := os.MkdirAll(req.ArtifactDir, 0o755); err != nil {
			return fail(status.Error(codes.Internal, err.Error()))
		}
	} else {
		return fail(status.Error(codes.FailedPrecondition, "a capture needs an artifact directory"))
	}
	d.mu.Lock()
	if d.cap != nil {
		d.mu.Unlock()
		return fail(status.Errorf(codes.FailedPrecondition, "%s already has a running capture", req.DeviceID))
	}
	c := &capture{dev: d, release: release, req: req, cs: cs, sink: sink, synth: d.entry.Fake != nil, done: make(chan struct{})}
	d.cap = c
	d.mu.Unlock()

	go c.run(ctx)

	attrs := map[string]string{"profile": d.scope.P.ID, "profile_validated": fmt.Sprint(d.scope.P.Validated), "unit": "V"}
	if c.synth {
		attrs["synthetic"] = "true"
	}
	streams := []*pluginv1.StreamInfo{}
	if cs.waveform {
		streams = append(streams, &pluginv1.StreamInfo{StreamId: StreamAnalog, Kind: "analog", Attributes: attrs})
	}
	return c, &pluginsdk.CaptureStart{Streams: streams}, nil
}

func (c *capture) run(ctx context.Context) {
	defer close(c.done)
	defer func() {
		c.dev.mu.Lock()
		if c.dev.cap == c {
			c.dev.cap = nil
		}
		c.dev.mu.Unlock()
		c.release()
	}()
	sc := c.dev.scope
	ref := c.req.Clock.Now()
	c.refMode = "read"
	if c.cs.trigger == "single" {
		t0 := time.Now()
		if _, err := sc.Acquire("single"); err != nil {
			c.fail(fmt.Errorf("arm: %w", err))
			return
		}
		if _, err := sc.WaitDone(ctx, c.cs.trigWait, pollEvery); err != nil {
			c.fail(err)
			return
		}
		c.waited = time.Since(t0)
		ref = c.req.Clock.Now()
		c.refMode = "trigger"
	}
	if c.cs.waveform {
		for _, ch := range c.cs.channels {
			if ctx.Err() != nil {
				return
			}
			if err := c.oneChannel(ctx, ch, ref); err != nil {
				c.fail(fmt.Errorf("channel %d: %w", ch, err))
				return
			}
		}
	}
	if c.cs.screenshot && ctx.Err() == nil {
		if err := c.shot(); err != nil {
			c.fail(fmt.Errorf("screenshot: %w", err))
		}
	}
}

func (c *capture) oneChannel(ctx context.Context, ch int, ref int64) error {
	start := time.Now()
	w, err := c.dev.scope.Waveform(ch)
	if err != nil {
		return err
	}
	if rtt := time.Since(start); rtt > c.rttMax {
		c.rttMax = rtt
	}
	periodNs := int64(math.Round(w.DT * 1e9))
	if periodNs < 1 {
		return fmt.Errorf("sample period %.3g s is below the 1 ns the timeline can represent", w.DT)
	}
	name := fmt.Sprintf("CH%d", ch)
	first := ref + int64(math.Round(w.T0*1e9))
	if first < 0 {
		first = 0
	}
	for off := 0; off < len(w.Volts); off += maxChunk {
		end := off + maxChunk
		if end > len(w.Volts) {
			end = len(w.Volts)
		}
		ev := &pluginv1.Event{TimestampNs: first + int64(off)*periodNs, StreamId: StreamAnalog,
			Payload: &pluginv1.Event_Analog{Analog: &pluginv1.AnalogBatch{
				SamplePeriodNs: periodNs, Channels: []string{name}, Unit: "V", Samples: w.Volts[off:end]}}}
		ectx, cancel := context.WithTimeout(ctx, emitTimeout)
		err := c.sink.Emit(ectx, ev)
		cancel()
		if err != nil {
			return fmt.Errorf("emit: %w", err)
		}
		c.mu.Lock()
		c.events++
		c.mu.Unlock()
	}
	c.mu.Lock()
	c.samples += uint64(len(w.Volts))
	c.mu.Unlock()

	path := filepath.Join(c.req.ArtifactDir, fmt.Sprintf("waveform-ch%d.csv", ch))
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := w.WriteCSV(f); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	meta := map[string]string{"channel": fmt.Sprint(ch), "source": w.Source, "samples": fmt.Sprint(len(w.Volts)), "dt_s": fmt.Sprint(w.DT),
		"t0_s": fmt.Sprint(w.T0), "profile": c.dev.scope.P.ID, "profile_validated": fmt.Sprint(c.dev.scope.P.Validated), "unit": "V"}
	if w.Note != "" {
		meta["note"] = w.Note
	}
	if c.synth {
		meta["synthetic"] = "true"
	}
	c.mu.Lock()
	c.artifacts = append(c.artifacts, &pluginv1.ArtifactRef{Name: filepath.Base(path), Kind: "csv", MimeType: "text/csv", Path: path, Metadata: meta})
	c.mu.Unlock()
	return nil
}

func (c *capture) shot() error {
	img, mime, err := c.dev.scope.Screenshot()
	if err != nil {
		return err
	}
	ext := map[string]string{"image/png": ".png", "image/bmp": ".bmp", "image/jpeg": ".jpg"}[mime]
	if ext == "" {
		ext = ".bin"
	}
	path := filepath.Join(c.req.ArtifactDir, "screenshot"+ext)
	if err := os.WriteFile(path, img, 0o644); err != nil {
		return err
	}
	meta := map[string]string{"profile": c.dev.scope.P.ID, "bytes": fmt.Sprint(len(img))}
	if c.synth {
		meta["synthetic"] = "true"
	}
	c.mu.Lock()
	c.artifacts = append(c.artifacts, &pluginv1.ArtifactRef{Name: filepath.Base(path), Kind: "screenshot", MimeType: mime, Path: path, Metadata: meta})
	c.mu.Unlock()
	return nil
}

// Wait implements pluginsdk.Capture.
func (c *capture) Wait() error {
	<-c.done
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Result implements pluginsdk.Capture.
func (c *capture) Result() *pluginv1.StopCaptureResponse {
	c.mu.Lock()
	defer c.mu.Unlock()
	return &pluginv1.StopCaptureResponse{
		Artifacts: c.artifacts,
		Stats:     &pluginv1.CaptureStats{Events: c.events, Samples: c.samples},
		Clock:     c.clockInfo(),
	}
}

// clockInfo is deliberately modest. The instrument reports no timestamp for its
// acquisition, so every event time is a host time plus the trace's own time
// axis. Whether the instrument's time base is accurate was not measured.
func (c *capture) clockInfo() *pluginv1.ClockInfo {
	ci := &pluginv1.ClockInfo{ResolutionNs: 1, ErrorMinNs: 0, ErrorMinBounded: true}
	switch {
	case c.synth:
		ci.Source = "host clock (synthetic instrument: no real acquisition)"
		ci.Note = "SYNTHETIC oscilloscope; times only order the events"
	case c.refMode == "trigger":
		ci.Source = "host clock when the trigger state was first seen, plus the trace time axis (relative to the trigger point)"
		ci.ErrorMaxNs = (pollEvery + c.rttMax).Nanoseconds()
		ci.Note = fmt.Sprintf("the trigger fired between two %s status polls, so the stamp is late by at least the poll interval plus a reply round trip (%s seen for a transfer); "+
			"the instrument's own latency to report the trigger was not measured, so the upper bound is not claimed. Sample spacing is the instrument's time base, not verified against a reference",
			pollEvery, c.rttMax.Round(time.Millisecond))
	default:
		ci.Source = "host clock at waveform transfer, plus the trace time axis"
		ci.Note = "the instrument acquired earlier, at an unknown time; the stamp is only an upper bound on the acquisition time. Sample spacing is the instrument's time base, not verified against a reference"
	}
	return ci
}
