package picoscopeplugin

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
)

// StreamAnalog is the stream id of every waveform event.
const StreamAnalog = "analog"

const (
	maxChunk    = 50_000 // frames (samples per channel) per analog event
	emitTimeout = 5e9    // ns
)

// capture is one block acquisition turned into analog events and a CSV
// artifact. Like SCPI and Saleae it is acquisition/export oriented, not a live
// stream.
type capture struct {
	dev     *device
	release func()
	req     pluginsdk.CaptureRequest
	sink    *pluginsdk.Sink
	set     settings
	done    chan struct{}

	mu        sync.Mutex
	err       error
	artifacts []*pluginv1.ArtifactRef
	events    uint64
	samples   uint64
	triggered bool
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
	d, _, release, err := p.lease(req.DeviceID, req.Config)
	if err != nil {
		return nil, nil, err
	}
	fail := func(e error) (pluginsdk.Capture, *pluginsdk.CaptureStart, error) {
		release()
		return nil, nil, e
	}
	if req.ArtifactDir == "" {
		return fail(status.Error(codes.FailedPrecondition, "a capture needs an artifact directory"))
	}
	if err := os.MkdirAll(req.ArtifactDir, 0o755); err != nil {
		return fail(status.Error(codes.Internal, err.Error()))
	}
	d.mu.Lock()
	if d.busy {
		d.mu.Unlock()
		return fail(errf(codes.FailedPrecondition, "%s already has a running capture", req.DeviceID))
	}
	s, err := parseSettings(d.set, req.Config, allKeys)
	if err != nil {
		d.mu.Unlock()
		return fail(argErr(err))
	}
	if _, _, err := d.plan(s); err != nil {
		d.mu.Unlock()
		return fail(err)
	}
	d.busy = true
	d.mu.Unlock()

	c := &capture{dev: d, release: release, req: req, sink: sink, set: s, done: make(chan struct{})}
	go c.run(ctx)

	attrs := map[string]string{"unit": "V", "mode": "block", "model": d.info.Model, "channels": fmt.Sprint(s.enabledNames())}
	if d.info.Synthetic {
		attrs["synthetic"] = "true"
	}
	return c, &pluginsdk.CaptureStart{Streams: []*pluginv1.StreamInfo{{StreamId: StreamAnalog, Kind: "analog", Attributes: attrs}}}, nil
}

func (c *capture) run(ctx context.Context) {
	defer close(c.done)
	defer func() {
		c.dev.mu.Lock()
		c.dev.busy = false
		c.dev.mu.Unlock()
		c.release()
	}()
	d := c.dev
	res, _, err := d.plan(c.set)
	if err != nil {
		c.fail(err)
		return
	}
	actx, cancel := context.WithTimeout(ctx, triggerWait(c.set))
	defer cancel()
	blk, err := d.b.Acquire(actx, c.set.request())
	if ctx.Err() != nil {
		return // cancelled: not an error
	}
	if err != nil {
		c.fail(err)
		return
	}
	ref := c.req.Clock.Now()
	acq := newAcquisition(blk, c.set, res)
	d.mu.Lock()
	d.last = acq
	d.mu.Unlock()
	c.mu.Lock()
	c.triggered = blk.Triggered
	c.mu.Unlock()
	if err := c.emit(ctx, acq, ref); err != nil {
		if ctx.Err() == nil {
			c.fail(err)
		}
		return
	}
	if err := c.csv(acq); err != nil {
		c.fail(err)
	}
}

func (c *capture) emit(ctx context.Context, acq *acquisition, ref int64) error {
	blk := acq.blk
	periodNs := int64(math.Round(blk.SampleIntervalS * 1e9))
	if periodNs < 1 {
		return fmt.Errorf("sample interval %.3g s is below the 1 ns the timeline can represent", blk.SampleIntervalS)
	}
	names := acq.channelNames()
	vols := make([][]float32, len(blk.Channels))
	for i, ch := range blk.Channels {
		vols[i] = ch.Volts()
	}
	trig := int64(0)
	if blk.Triggered {
		trig = int64(blk.TriggerIndex)
	}
	for off := 0; off < blk.Samples; off += maxChunk {
		end := min(off+maxChunk, blk.Samples)
		buf := make([]float32, 0, (end-off)*len(vols))
		for i := off; i < end; i++ {
			for _, v := range vols {
				buf = append(buf, v[i])
			}
		}
		first := ref + (int64(off)-trig)*periodNs
		ev := &pluginv1.Event{TimestampNs: first, StreamId: StreamAnalog,
			Payload: &pluginv1.Event_Analog{Analog: &pluginv1.AnalogBatch{SamplePeriodNs: periodNs, Channels: names, Unit: "V", Samples: buf}}}
		ectx, cancel := context.WithTimeout(ctx, emitTimeout)
		err := c.sink.Emit(ectx, ev)
		cancel()
		if err != nil {
			return fmt.Errorf("emit: %w", err)
		}
		c.mu.Lock()
		c.events++
		c.samples += uint64(end - off) // frames: samples per channel, as the saleae plugin counts
		c.mu.Unlock()
	}
	return nil
}

func (c *capture) csv(acq *acquisition) error {
	blk := acq.blk
	path := filepath.Join(c.req.ArtifactDir, "waveform.csv")
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	hdr := "time_s"
	for _, n := range acq.channelNames() {
		hdr += "," + n
	}
	_, err = w.WriteString(hdr + "\n")
	vols := make([][]float32, len(blk.Channels))
	for i, ch := range blk.Channels {
		vols[i] = ch.Volts()
	}
	trig := 0
	if blk.Triggered {
		trig = blk.TriggerIndex
	}
	var line []byte
	for i := 0; err == nil && i < blk.Samples; i++ {
		line = strconv.AppendFloat(line[:0], float64(i-trig)*blk.SampleIntervalS, 'g', 10, 64)
		for _, v := range vols {
			line = append(line, ',')
			line = strconv.AppendFloat(line, float64(v[i]), 'g', 7, 32)
		}
		line = append(line, '\n')
		_, err = w.Write(line)
	}
	if err == nil {
		err = w.Flush()
	}
	if err = errors.Join(err, f.Close()); err != nil {
		return fmt.Errorf("write waveform.csv: %w", err)
	}
	meta := map[string]string{"samples": fmt.Sprint(blk.Samples), "dt_s": fmt.Sprint(blk.SampleIntervalS), "channels": fmt.Sprint(acq.channelNames()),
		"triggered": fmt.Sprint(blk.Triggered), "unit": "V", "time_zero": "trigger point (first sample when untriggered)"}
	if c.dev.info.Synthetic {
		meta["synthetic"] = "true"
	}
	c.mu.Lock()
	c.artifacts = append(c.artifacts, &pluginv1.ArtifactRef{Name: "waveform.csv", Kind: "csv", MimeType: "text/csv", Path: path, Metadata: meta})
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

// clockInfo is deliberately modest: the instrument supplies no timestamp, so
// event times are the host clock when the block was returned plus the block's
// own time axis (zero at the trigger point).
func (c *capture) clockInfo() *pluginv1.ClockInfo {
	ci := &pluginv1.ClockInfo{ResolutionNs: 1, ErrorMinNs: 0, ErrorMinBounded: true,
		Source: "host clock when the block was returned, plus the block time axis (zero at the trigger point)"}
	if c.dev.info.Synthetic {
		ci.Source = "host clock (synthetic instrument: no real acquisition)"
		ci.Note = "SYNTHETIC oscilloscope; times only order the events"
		return ci
	}
	ci.Note = "the block was acquired before it was returned, at an unknown latency; the stamp is only an upper bound on the trigger time. Sample spacing is the instrument's time base, not verified against a reference"
	return ci
}
