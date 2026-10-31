package wstk

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/faronov/hwlab/internal/clockmap"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
	"github.com/faronov/hwlab/plugins/wstk/protocol"
)

// Capture modes.
const (
	ModeAEM = "aem" // current/voltage
	ModePTI = "pti" // packet trace, Ethernet only
)

const (
	powerStream  = "current"
	packetStream = "packets"
	// samplePeriodNs is the AEM rate the kit reports (10 kHz).
	samplePeriodNs = 100_000
	// aemPoll is the pause between AEM channel reads; a buffer of 256
	// samples fills every 25.6 ms.
	aemPoll = 10 * time.Millisecond
	// aemStall ends a capture when the kit stops sending buffers.
	aemStall = 3 * time.Second
)

type options struct {
	mode    string
	pollMs  int // Ethernet AEM: admin console poll period
	channel int // PTI: channel when the trace does not tell
}

func parseOptions(cfg map[string]any, isNet bool) (options, error) {
	o := options{mode: ModeAEM, pollMs: 100}
	if v, ok := cfg["mode"]; ok {
		s, _ := v.(string)
		if s != ModeAEM && s != ModePTI {
			return o, fmt.Errorf("mode must be %q or %q", ModeAEM, ModePTI)
		}
		o.mode = s
	}
	if o.mode == ModePTI && !isNet {
		return o, errors.New("mode pti needs the kit on Ethernet (the USB PTI path is not implemented); set " + HostsEnv)
	}
	if v, ok := cfg["poll_ms"]; ok {
		n, ok := toInt(v)
		if !ok || n < 20 || n > 10_000 {
			return o, errors.New("poll_ms must be an integer in [20,10000]")
		}
		o.pollMs = n
	}
	if v, ok := cfg["channel"]; ok {
		n, ok := toInt(v)
		if !ok || n < 0 || n > 63 {
			return o, errors.New("channel must be an integer in [0,63]")
		}
		o.channel = n
	}
	return o, nil
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), n == math.Trunc(n)
	case int:
		return n, true
	case int64:
		return int(n), true
	}
	return 0, false
}

// StartCapture implements pluginsdk.Plugin.
func (p *Plugin) StartCapture(ctx context.Context, req pluginsdk.CaptureRequest, sink *pluginsdk.Sink) (pluginsdk.Capture, *pluginsdk.CaptureStart, error) {
	t, err := p.resolve(req.DeviceID)
	if err != nil {
		return nil, nil, err
	}
	cfg := map[string]any{}
	p.mu.Lock()
	for k, v := range p.open[req.DeviceID] {
		cfg[k] = v
	}
	p.mu.Unlock()
	for k, v := range req.Config {
		cfg[k] = v
	}
	opts, err := parseOptions(cfg, t.usb == nil)
	if err != nil {
		return nil, nil, invalidArg(err)
	}
	if err := p.markBusy(t.id); err != nil {
		return nil, nil, err
	}
	var c pluginsdk.Capture
	var st *pluginsdk.CaptureStart
	switch {
	case t.usb != nil:
		c, st, err = p.startUSBAEM(ctx, t, req, sink)
	case opts.mode == ModePTI:
		c, st, err = p.startPTI(ctx, t, opts, req, sink)
	default:
		c, st, err = p.startNetAEM(ctx, t, opts, req, sink)
	}
	if err != nil {
		p.clearBusy(t.id)
		return nil, nil, err
	}
	return c, st, nil
}

// aemStream starts the AEM stream and returns the kit's configuration
// answer. Data messages queued before it (from an earlier session) are
// skipped by the caller using the configuration's kit time.
func aemStream(ctx context.Context, e *protocol.Emucom, sp *protocol.AEMSplitter) (protocol.AEMConfig, error) {
	if err := e.Write(ctx, protocol.ChanAEM, protocol.AEMStartRequest()); err != nil {
		return protocol.AEMConfig{}, fmt.Errorf("AEM start: %w", err)
	}
	deadline := time.Now().Add(kitTimeout)
	for time.Now().Before(deadline) {
		b, err := e.Read(ctx, protocol.ChanAEM, protocol.AEMMaxRead)
		if err != nil {
			return protocol.AEMConfig{}, fmt.Errorf("AEM read: %w", err)
		}
		for _, m := range sp.Feed(b) {
			if m.Type == protocol.AEMTypeConfig {
				return protocol.ParseAEMConfig(m)
			}
		}
		if len(b) == 0 {
			if err := sleepCtx(ctx, aemPoll); err != nil {
				return protocol.AEMConfig{}, err
			}
		}
	}
	return protocol.AEMConfig{}, fmt.Errorf("%w: no AEM configuration answer within %v", errNoData, kitTimeout)
}

// readOneBuffer returns the first complete AEM buffer after starting the
// stream.
func readOneBuffer(ctx context.Context, e *protocol.Emucom) (protocol.AEMBuffer, error) {
	var sp protocol.AEMSplitter
	cfg, err := aemStream(ctx, e, &sp)
	if err != nil {
		return protocol.AEMBuffer{}, err
	}
	deadline := time.Now().Add(kitTimeout)
	for time.Now().Before(deadline) {
		b, err := e.Read(ctx, protocol.ChanAEM, protocol.AEMMaxRead)
		if err != nil {
			return protocol.AEMBuffer{}, err
		}
		for _, m := range sp.Feed(b) {
			if m.Type != protocol.AEMTypeData || m.KitMs < cfg.KitMs {
				continue
			}
			return protocol.ParseAEMData(m)
		}
		if err := sleepCtx(ctx, aemPoll); err != nil {
			return protocol.AEMBuffer{}, err
		}
	}
	return protocol.AEMBuffer{}, fmt.Errorf("%w within %v", errNoData, kitTimeout)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (p *Plugin) startUSBAEM(ctx context.Context, t target, req pluginsdk.CaptureRequest, sink *pluginsdk.Sink) (pluginsdk.Capture, *pluginsdk.CaptureStart, error) {
	e, release, err := p.openKit(t)
	if err != nil {
		return nil, nil, err
	}
	// Identification is a few small transfers; it also proves the EMUCOM
	// path works before the capture is reported as started.
	m, err := identify(ctx, e)
	if err != nil {
		release()
		return nil, nil, fmt.Errorf("wstk %s: %w", t.id, err)
	}
	p.setMeta(t.id, m)
	c := &aemCapture{p: p, id: t.id, e: e, release: release, sink: sink, clock: req.Clock, cm: clockmap.NewClockMap(),
		duration: req.Duration, done: make(chan struct{}), meta: m}
	if c.cfg, err = aemStream(ctx, e, &c.sp); err != nil {
		release()
		return nil, nil, fmt.Errorf("wstk %s: %w", t.id, err)
	}
	if c.cfg.SampleRate != 0 && c.cfg.SampleRate != 1e9/samplePeriodNs {
		release()
		return nil, nil, fmt.Errorf("wstk %s: AEM reports %d Hz; only %d Hz has been validated", t.id, c.cfg.SampleRate, int(1e9/samplePeriodNs))
	}
	go c.run(ctx)
	attrs := map[string]string{
		"sample_period_ns": fmt.Sprint(samplePeriodNs), "source": "AEM over J-Link EMUCOM (USB)",
		"firmware": m.info.Firmware, "voltage": "per 25.6 ms buffer",
		"clock": "kit timer mapped to session time with a windowed minimum-offset estimator",
	}
	if len(m.boards) > 0 {
		attrs["boards"] = metaResult(m)["boards"].(string)
	}
	return c, &pluginsdk.CaptureStart{Streams: []*pluginv1.StreamInfo{{StreamId: powerStream, Kind: "power", Attributes: attrs}}}, nil
}

type aemCapture struct {
	p        *Plugin
	id       string
	e        *protocol.Emucom
	release  func()
	sink     *pluginsdk.Sink
	clock    *pluginsdk.Clock
	cm       *clockmap.ClockMap
	duration time.Duration
	meta     *kitMeta

	cfg       protocol.AEMConfig
	sp        protocol.AEMSplitter
	tl        protocol.AEMTimeline
	done      chan struct{}
	err       error
	emitted   int64 // samples
	limit     int64
	events    uint64
	nextNs    int64 // earliest start of the next batch
	firstNs   int64
	gaps      int
	gapNs     int64
	lastUs    int64
	lastLenUs int64
	have      bool
}

func (c *aemCapture) Wait() error { <-c.done; return c.err }

func (c *aemCapture) Result() *pluginv1.StopCaptureResponse {
	<-c.done
	return &pluginv1.StopCaptureResponse{Stats: &pluginv1.CaptureStats{
		Events: c.events, Samples: uint64(c.emitted), DroppedBatches: c.sink.Dropped()}, Clock: c.clockInfo()}
}

func (c *aemCapture) run(ctx context.Context) {
	defer close(c.done)
	defer c.p.clearBusy(c.id)
	defer c.release()
	if c.duration > 0 {
		c.limit = int64((c.duration + samplePeriodNs - 1) / samplePeriodNs)
	}
	last := time.Now()
	for ctx.Err() == nil && (c.limit == 0 || c.emitted < c.limit) {
		b, err := c.e.Read(ctx, protocol.ChanAEM, protocol.AEMMaxRead)
		if err != nil {
			if ctx.Err() == nil {
				c.err = fmt.Errorf("wstk AEM read: %w", err)
			}
			break
		}
		rx := time.Now()
		for _, m := range c.sp.Feed(b) {
			if m.Type != protocol.AEMTypeData || m.KitMs < c.cfg.KitMs {
				continue
			}
			buf, err := protocol.ParseAEMData(m)
			if err != nil {
				c.emitLog("warn", err.Error())
				continue
			}
			if c.onBuffer(ctx, buf, c.clock.At(rx)) {
				last = rx
			}
		}
		if time.Since(last) > aemStall {
			c.err = fmt.Errorf("wstk AEM: no new buffer for %v", aemStall)
			break
		}
		if len(b) < protocol.AEMMaxRead {
			if sleepCtx(ctx, aemPoll) != nil {
				break
			}
		}
	}
	if c.gaps > 0 {
		c.emitLog("warn", fmt.Sprintf("%d gaps in the AEM stream (%.1f ms of samples missing); the kit drops buffers the host does not fetch in time",
			c.gaps, float64(c.gapNs)/1e6))
	}
	if n := c.tl.Stale; n > 0 {
		c.emitLog("warn", fmt.Sprintf("%d out-of-order AEM buffers were dropped", n))
	}
}

// onBuffer stamps and emits one buffer. The kit stamps each buffer's first
// sample with its microsecond timer and each message with the millisecond
// time it was queued; the queue time is mapped to the session clock (the
// host receive time is a late-biased observation of it), and that offset
// places the samples.
func (c *aemCapture) onBuffer(ctx context.Context, b protocol.AEMBuffer, hostNs int64) bool {
	startUs, fresh := c.tl.Place(b)
	if !fresh {
		return false
	}
	c.cm.Map(int64(b.KitMs)*1000, hostNs)
	off := c.cm.Stats().OffsetNs
	ts := startUs*1000 + off
	if c.have {
		if missing := startUs - c.lastUs - c.lastLenUs; missing > 50 {
			c.gaps++
			c.gapNs += missing * 1000
		}
	}
	c.lastUs, c.have = startUs, true
	c.lastLenUs = int64(len(b.CurrentmA)) * 1_000_000 / int64(b.SampleRate)
	if ts < c.nextNs {
		ts = c.nextNs
	}
	cur := b.CurrentmA
	if c.limit > 0 {
		cur = cur[:min(int64(len(cur)), c.limit-c.emitted)]
	}
	if len(cur) == 0 {
		return true
	}
	na := make([]int32, len(cur))
	uv := make([]uint32, len(cur))
	vuv := uint32(math.Round(float64(b.VoltageV) * 1e6))
	for i, v := range cur {
		na[i] = int32(math.Round(float64(v) * 1e6))
		uv[i] = vuv
	}
	if c.emitted == 0 {
		c.firstNs = ts
	}
	ev := &pluginv1.Event{TimestampNs: ts, StreamId: powerStream, Payload: &pluginv1.Event_Power{Power: &pluginv1.PowerBatch{
		SamplePeriodNs: samplePeriodNs, CurrentNa: na, VoltageUv: uv}}}
	c.emitted += int64(len(cur))
	c.nextNs = ts + int64(len(cur))*samplePeriodNs
	final := c.limit > 0 && c.emitted >= c.limit
	if final {
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		if c.sink.Emit(fctx, ev) == nil {
			c.events++
		}
	} else if c.sink.TryEmit(ev) {
		c.events++
	}
	return true
}

func (c *aemCapture) emitLog(level, msg string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = c.sink.Emit(ctx, &pluginv1.Event{TimestampNs: c.clock.Now(), StreamId: powerStream,
		Payload: &pluginv1.Event_Log{Log: &pluginv1.Log{Level: level, Source: "wstk", Message: msg}}})
}

// clockInfo: the kit's queue time has millisecond resolution and is never
// later than the send, and the host receives after the send, so the offset
// estimate (host minus kit time) is too large, never too small: stamps can
// be late (by up to 1 ms plus the minimum USB transit), never early.
func (c *aemCapture) clockInfo() *pluginv1.ClockInfo {
	s := c.cm.Stats()
	note := "sample spacing is the kit's 10 kHz AEM clock; absolute alignment is late by up to 1 ms (millisecond queue-time resolution) plus the unknown minimum USB transit; that the message time is the queue time is inferred from observation"
	if s.Samples > 0 {
		note += fmt.Sprintf("; host receive minus mapped queue time was %.1f ms mean, %.1f ms max over %d buffers",
			float64(s.LatencyAvgNs)/1e6, float64(s.LatencyMaxNs)/1e6, s.Samples)
	}
	ci := &pluginv1.ClockInfo{Source: "kit timer (AEM buffer start, us; message queue time, ms) mapped by minimum-offset estimator",
		ResolutionNs: samplePeriodNs, ErrorMinNs: 0, ErrorMinBounded: true, DriftPpm: s.DriftPPM, Note: note}
	if s.Samples > 0 {
		ci.JitterNs = s.LatencyMaxNs - s.LatencyMinNs
	}
	return ci
}
