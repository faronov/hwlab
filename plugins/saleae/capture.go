package saleae

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
	"github.com/faronov/hwlab/plugins/saleae/internal/saleaepb"
)

const (
	streamDigital  = "digital"
	streamAnalog   = "analog"
	streamAnalyzer = "analyzers"

	// timerGrace is how long past a timed capture's own end hwlab waits for
	// Logic 2 to finish before it calls StopCapture.
	timerGrace = 3 * time.Second
	// exportTimeout bounds one save/export RPC; Logic 2 writes the files.
	exportTimeout = 10 * time.Minute
)

type capture struct {
	p       *Plugin
	req     pluginsdk.CaptureRequest
	cfg     *config
	sink    *pluginsdk.Sink
	conn    *grpc.ClientConn
	mgr     saleaepb.ManagerClient
	unlock  func()
	timed   bool
	started time.Time
	startNs int64
	latency time.Duration
	capID   uint64

	done chan struct{}

	mu        sync.Mutex
	artifacts []*pluginv1.ArtifactRef
	events    uint64
	samples   uint64
	warnings  []string
	err       error
}

// StartCapture implements pluginsdk.Plugin. A capture with a Duration is a
// Logic 2 timed capture; without one it is manual and ends when the context
// is cancelled. The data is only available after the capture ends, so events
// are emitted during the post-capture export, not live.
func (p *Plugin) StartCapture(ctx context.Context, req pluginsdk.CaptureRequest, sink *pluginsdk.Sink) (pluginsdk.Capture, *pluginsdk.CaptureStart, error) {
	cfg, err := parseConfig(req.Config)
	if err != nil {
		return nil, nil, err
	}
	if req.ArtifactDir == "" {
		return nil, nil, errors.New("no artifact directory: Logic 2 needs a directory to write to")
	}
	if err := os.MkdirAll(req.ArtifactDir, 0o755); err != nil {
		return nil, nil, err
	}
	unlock, err := p.lock(req.DeviceID)
	if err != nil {
		return nil, nil, err
	}
	conn, mgr, err := p.dial(ctx)
	if err != nil {
		unlock()
		return nil, nil, err
	}
	c := &capture{p: p, req: req, cfg: cfg, sink: sink, conn: conn, mgr: mgr, unlock: unlock,
		timed: req.Duration > 0, done: make(chan struct{})}

	sreq := &saleaepb.StartCaptureRequest{
		DeviceId: req.DeviceID,
		DeviceConfiguration: &saleaepb.StartCaptureRequest_LogicDeviceConfiguration{
			LogicDeviceConfiguration: cfg.deviceConfig()},
		CaptureConfiguration: &saleaepb.CaptureConfiguration{BufferSizeMegabytes: cfg.BufferMB},
	}
	if c.timed {
		sreq.CaptureConfiguration.CaptureMode = &saleaepb.CaptureConfiguration_TimedCaptureMode{
			TimedCaptureMode: &saleaepb.TimedCaptureMode{DurationSeconds: req.Duration.Seconds()}}
	} else {
		sreq.CaptureConfiguration.CaptureMode = &saleaepb.CaptureConfiguration_ManualCaptureMode{
			ManualCaptureMode: &saleaepb.ManualCaptureMode{}}
	}
	t0 := time.Now()
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	reply, err := mgr.StartCapture(rctx, sreq)
	cancel()
	t1 := time.Now()
	if err != nil {
		closeErr := conn.Close()
		unlock()
		return nil, nil, errors.Join(fmt.Errorf("logic 2 StartCapture on %s: %w", p.addr, err), closeErr)
	}
	c.capID = reply.GetCaptureInfo().GetCaptureId()
	c.started, c.latency = t1, t1.Sub(t0)
	c.startNs = req.Clock.At(t1)

	var streams []*pluginv1.StreamInfo
	if len(cfg.Digital) > 0 {
		streams = append(streams, &pluginv1.StreamInfo{StreamId: streamDigital, Kind: "digital",
			Attributes: map[string]string{"sample_rate": fmt.Sprint(cfg.DigitalRate)}})
	}
	if len(cfg.Analog) > 0 {
		streams = append(streams, &pluginv1.StreamInfo{StreamId: streamAnalog, Kind: "analog",
			Attributes: map[string]string{"sample_rate": fmt.Sprint(cfg.AnalogRate), "downsample": fmt.Sprint(cfg.AnalogDownsample)}})
	}
	if len(cfg.Analyzers) > 0 {
		streams = append(streams, &pluginv1.StreamInfo{StreamId: streamAnalyzer, Kind: "decoded"})
	}
	go c.run(ctx)
	return c, &pluginsdk.CaptureStart{Streams: streams}, nil
}

func (c *config) deviceConfig() *saleaepb.LogicDeviceConfiguration {
	d := &saleaepb.LogicDeviceConfiguration{
		EnabledChannels: &saleaepb.LogicDeviceConfiguration_LogicChannels{
			LogicChannels: &saleaepb.LogicChannels{DigitalChannels: c.Digital, AnalogChannels: c.Analog}},
		DigitalSampleRate:     c.DigitalRate,
		AnalogSampleRate:      c.AnalogRate,
		DigitalThresholdVolts: c.Threshold,
	}
	for _, g := range c.Glitch {
		d.GlitchFilters = append(d.GlitchFilters, &saleaepb.GlitchFilterEntry{ChannelIndex: g.Channel, PulseWidthSeconds: g.Seconds})
	}
	return d
}

func (c *capture) warn(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	c.mu.Lock()
	c.warnings = append(c.warnings, msg)
	c.mu.Unlock()
}

func (c *capture) setErr(err error) {
	c.mu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.mu.Unlock()
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
	note := fmt.Sprintf("Logic 2 buffers the capture and returns it after it ends; sample times are offsets from "+
		"Logic 2's capture start, anchored at the host time the StartCapture RPC returned (RPC latency %s). "+
		"The delay between that RPC and the device actually sampling is not measured, so no error bound is claimed.",
		c.latency.Round(time.Millisecond))
	res := &pluginv1.StopCaptureResponse{
		Artifacts: c.artifacts,
		Stats:     &pluginv1.CaptureStats{Events: c.events, Samples: c.samples},
		Clock: &pluginv1.ClockInfo{
			Source: "Logic 2 capture time offset, anchored at host time of StartCapture RPC return",
			Note:   note,
		},
	}
	if rate := c.cfg.DigitalRate; len(c.cfg.Digital) > 0 && rate > 0 {
		res.Clock.ResolutionNs = int64(1e9 / float64(rate))
	}
	return res
}

// run drives the capture to its end and then exports it. All post-capture
// work uses a context that survives the cancellation that ended the capture.
func (c *capture) run(ctx context.Context) {
	defer close(c.done)
	defer c.unlock()
	defer func() {
		if err := c.conn.Close(); err != nil {
			c.mu.Lock()
			c.err = errors.Join(c.err, fmt.Errorf("close Logic 2 connection: %w", err))
			c.mu.Unlock()
		}
	}()
	bg := context.WithoutCancel(ctx)

	if err := c.waitEnd(ctx, bg); err != nil {
		c.setErr(err)
		c.closeCapture(bg)
		return
	}
	c.export(bg)
	c.closeCapture(bg)
}

// waitEnd returns once Logic 2 has stopped capturing.
func (c *capture) waitEnd(ctx, bg context.Context) error {
	wctx, wcancel := context.WithCancel(bg)
	defer wcancel()
	waited := make(chan error, 1)
	if c.timed {
		go func() {
			_, err := c.mgr.WaitCapture(wctx, &saleaepb.WaitCaptureRequest{CaptureId: c.capID})
			waited <- err
		}()
	}
	select {
	case err := <-waited:
		return wrapRPC("WaitCapture", err)
	case <-ctx.Done():
	}
	// A timed capture whose timer just expired ends by itself; give Logic 2 a
	// moment to say so. A cancellation before the deadline stops it now.
	if c.timed && time.Since(c.started) >= c.req.Duration-200*time.Millisecond {
		select {
		case err := <-waited:
			return wrapRPC("WaitCapture", err)
		case <-time.After(timerGrace):
		}
	}
	sctx, cancel := context.WithTimeout(bg, c.p.stopTimeout())
	defer cancel()
	_, err := c.mgr.StopCapture(sctx, &saleaepb.StopCaptureRequest{CaptureId: c.capID})
	return wrapRPC("StopCapture", err)
}

func wrapRPC(what string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("logic 2 %s: %w", what, err)
}

func (p *Plugin) stopTimeout() time.Duration {
	if p.timeout > 10*time.Second {
		return p.timeout
	}
	return 10 * time.Second
}

func (c *capture) closeCapture(bg context.Context) {
	cctx, cancel := context.WithTimeout(bg, c.p.stopTimeout())
	defer cancel()
	if _, err := c.mgr.CloseCapture(cctx, &saleaepb.CloseCaptureRequest{CaptureId: c.capID}); err != nil {
		c.warn("CloseCapture: %v", err)
	}
}

func (c *capture) addArtifact(name, kind, mime, path string, meta map[string]string) {
	c.mu.Lock()
	c.artifacts = append(c.artifacts, &pluginv1.ArtifactRef{Name: name, Kind: kind, MimeType: mime, Path: path, Metadata: meta})
	c.mu.Unlock()
}

func (c *capture) emit(ctx context.Context, evs ...*pluginv1.Event) error {
	if err := c.sink.Emit(ctx, evs...); err != nil {
		return err
	}
	c.mu.Lock()
	c.events += uint64(len(evs))
	c.mu.Unlock()
	return nil
}

// export saves the native capture, exports raw data and analyzers and turns
// everything into events. A failed export is a warning on the timeline, not a
// capture failure, unless nothing at all could be exported.
func (c *capture) export(bg context.Context) {
	var failures int
	fail := func(format string, a ...any) {
		failures++
		c.warn(format, a...)
		c.logWarn(bg, fmt.Sprintf(format, a...))
	}
	call := func() (context.Context, context.CancelFunc) { return context.WithTimeout(bg, exportTimeout) }

	if c.cfg.SaveNative {
		path := filepath.Join(c.req.ArtifactDir, "capture.sal")
		ctx, cancel := call()
		_, err := c.mgr.SaveCapture(ctx, &saleaepb.SaveCaptureRequest{CaptureId: c.capID, Filepath: path})
		cancel()
		if err != nil {
			fail("SaveCapture: %v", err)
		} else {
			c.addArtifact("capture.sal", "native-capture", "application/octet-stream", path,
				map[string]string{"opens_with": "Saleae Logic 2"})
		}
	}

	rawDir := filepath.Join(c.req.ArtifactDir, "raw")
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		fail("create %s: %v", rawDir, err)
	} else {
		ctx, cancel := call()
		_, err := c.mgr.ExportRawDataCsv(ctx, &saleaepb.ExportRawDataCsvRequest{
			CaptureId: c.capID, Directory: rawDir,
			Channels: &saleaepb.ExportRawDataCsvRequest_LogicChannels{
				LogicChannels: &saleaepb.LogicChannels{DigitalChannels: c.cfg.Digital, AnalogChannels: c.cfg.Analog}},
			AnalogDownsampleRatio: c.cfg.AnalogDownsample,
		})
		cancel()
		if err != nil {
			fail("ExportRawDataCsv: %v", err)
		} else {
			if len(c.cfg.Digital) > 0 {
				if err := c.ingestDigital(bg, filepath.Join(rawDir, "digital.csv")); err != nil {
					fail("digital export: %v", err)
				}
			}
			if len(c.cfg.Analog) > 0 {
				if err := c.ingestAnalog(bg, filepath.Join(rawDir, "analog.csv")); err != nil {
					fail("analog export: %v", err)
				}
			}
		}
	}

	for i, a := range c.cfg.Analyzers {
		if err := c.ingestAnalyzer(bg, i, a); err != nil {
			fail("analyzer %q: %v", a.Label, err)
		}
	}
	if failures > 0 && c.events == 0 && len(c.artifacts) == 0 {
		c.setErr(fmt.Errorf("logic 2 export produced nothing: %s", c.firstWarning()))
	}
}

func (c *capture) firstWarning() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.warnings) == 0 {
		return "unknown error"
	}
	return c.warnings[0]
}

func (c *capture) logWarn(ctx context.Context, msg string) {
	// Not counted in c.events: that counter decides whether anything was exported.
	_ = c.sink.Emit(ctx, &pluginv1.Event{TimestampNs: c.startNs, StreamId: streamAnalyzer,
		Payload: &pluginv1.Event_Log{Log: &pluginv1.Log{Level: "warn", Source: "saleae", Message: msg}}})
}

func (c *capture) ingestDigital(ctx context.Context, path string) (retErr error) {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, f.Close()) }()
	names := make([]string, len(c.cfg.Digital))
	for i, ch := range c.cfg.Digital {
		names[i] = c.cfg.channelName(ch)
	}
	c.addArtifact("digital.csv", "csv", "text/csv", path, map[string]string{"channels": fmt.Sprint(c.cfg.Digital)})
	return readDigitalCSV(f, len(names), func(b *digitalEdges) error {
		return c.emit(ctx, &pluginv1.Event{
			TimestampNs: c.startNs + b.StartNs, StreamId: streamDigital,
			Payload: &pluginv1.Event_Digital{Digital: &pluginv1.DigitalBatch{Channels: names,
				Encoding: &pluginv1.DigitalBatch_Edges{Edges: &pluginv1.DigitalEdges{
					InitialState: b.Initial, OffsetNs: b.Offsets, State: b.States}}}}})
	})
}

func (c *capture) ingestAnalog(ctx context.Context, path string) (retErr error) {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, f.Close()) }()
	names := make([]string, len(c.cfg.Analog))
	for i, ch := range c.cfg.Analog {
		names[i] = fmt.Sprintf("A%d", ch)
		if n := c.cfg.Names[ch]; n != "" {
			names[i] = n
		}
	}
	period := int64(float64(c.cfg.AnalogDownsample) * 1e9 / float64(c.cfg.AnalogRate))
	c.addArtifact("analog.csv", "csv", "text/csv", path, map[string]string{"channels": fmt.Sprint(c.cfg.Analog),
		"downsample": fmt.Sprint(c.cfg.AnalogDownsample)})
	return readAnalogCSV(f, len(names), func(b *analogFrames) error {
		c.mu.Lock()
		c.samples += uint64(len(b.Samples) / len(names))
		c.mu.Unlock()
		return c.emit(ctx, &pluginv1.Event{
			TimestampNs: c.startNs + b.StartNs, StreamId: streamAnalog,
			Payload: &pluginv1.Event_Analog{Analog: &pluginv1.AnalogBatch{
				SamplePeriodNs: period, Channels: names, Unit: "V", Samples: b.Samples}}})
	})
}

func (c *capture) ingestAnalyzer(ctx context.Context, i int, a analyzerSpec) (retErr error) {
	actx, cancel := context.WithTimeout(ctx, exportTimeout)
	defer cancel()
	added, err := c.mgr.AddAnalyzer(actx, &saleaepb.AddAnalyzerRequest{
		CaptureId: c.capID, AnalyzerName: a.Name, AnalyzerLabel: a.Label, Settings: a.Settings})
	if err != nil {
		return fmt.Errorf("AddAnalyzer: %w", err)
	}
	path := filepath.Join(c.req.ArtifactDir, fmt.Sprintf("analyzer-%d-%s.csv", i, sanitize(a.Label)))
	_, err = c.mgr.ExportDataTableCsv(actx, &saleaepb.ExportDataTableCsvRequest{
		CaptureId: c.capID, Filepath: path,
		Analyzers: []*saleaepb.DataTableAnalyzerConfiguration{{AnalyzerId: added.GetAnalyzerId(),
			RadixType: saleaepb.RadixType_RADIX_TYPE_HEXADECIMAL}},
	})
	if err != nil {
		return fmt.Errorf("ExportDataTableCsv: %w", err)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, f.Close()) }()
	c.addArtifact(filepath.Base(path), "csv", "text/csv", path, map[string]string{"analyzer": a.Name, "label": a.Label})
	proto := a.Kind
	if proto == "" {
		proto = a.Name
	}
	return readTableCSV(f, func(rows []decodedRow) error {
		evs := make([]*pluginv1.Event, 0, len(rows))
		for _, r := range rows {
			fields := map[string]any{"analyzer": a.Label}
			if r.Type != "" {
				fields["type"] = r.Type
			}
			for k, v := range r.Fields {
				fields[k] = v
			}
			st, err := structpb.NewStruct(fields)
			if err != nil {
				return err
			}
			evs = append(evs, &pluginv1.Event{TimestampNs: c.startNs + r.StartNs, StreamId: streamAnalyzer,
				Payload: &pluginv1.Event_Decoded{Decoded: &pluginv1.DecodedProtocolEvent{
					Protocol: proto, Summary: summarize(a.Label, r), Fields: st, DurationNs: r.DurationNs}}})
		}
		return c.emit(ctx, evs...)
	})
}

func sanitize(s string) string {
	out := []rune(s)
	for i, r := range out {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			out[i] = '_'
		}
	}
	return string(out)
}
