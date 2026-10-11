package xds110plugin

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/faronov/hwlab/internal/usb"
	pluginv1 "github.com/faronov/hwlab/pkg/api/pluginv1"
	"github.com/faronov/hwlab/pkg/pluginsdk"
	"github.com/faronov/hwlab/plugins/xds110/energytrace"
)

// etPID is the only USB product the EnergyTrace stream was observed on.
const etPID = 0xbef3

const (
	// batchPackets is how many 256-sample packets make one PowerBatch (16 ms).
	batchPackets = 16
	// stopDrain bounds how long the stream is read after the stop command so
	// that in-flight packets are not lost.
	stopDrain = 400 * time.Millisecond
	// firstSamplePackets: the first data packet is dropped (see energytrace.Reader),
	// so sample 0 of the stream is this many samples after the start.
	firstSampleOffset = energytrace.SamplesPerPacket
	// startWindowNs limits how long arrival times feed the start bound.
	startWindowNs = 2_000_000_000
)

// DataLink is the stream interface of the probe.
type DataLink interface {
	energytrace.DataPort
	Close() error
}

// DataOpener opens the EnergyTrace data interface of an entry.
type DataOpener func(Entry) (DataLink, error)

func defaultOpenData(e Entry) (DataLink, error) {
	if e.USB == nil {
		return nil, status.Error(codes.Unimplemented, "EnergyTrace needs a real LP-XDS110ET; the simulator has no stream")
	}
	port, err := usb.Open(*e.USB, energytrace.DataInterface, usb.OpenOptions{})
	if err != nil {
		return nil, err
	}
	if hc, ok := port.(usb.HaltClearer); ok {
		_ = hc.ClearHalt(energytrace.DataIn)
	}
	return port, nil
}

// SetDataOpener replaces the data interface opener (tests).
func (p *Plugin) SetDataOpener(o DataOpener) { p.openData = o }

func supportsEnergyTrace(e Entry) bool { return e.USB != nil && e.USB.PID == etPID }

func etAttributes() map[string]string {
	return map[string]string{
		"energytrace":      "PROTOCOL_PARTIAL",
		"sample_rate_hz":   fmt.Sprint(energytrace.SampleRateHz),
		"sample_period_ns": fmt.Sprint(energytrace.SamplePeriodNs),
		"range":            "0 only",
		"voltage":          "not measured",
	}
}

// StartCapture implements pluginsdk.Plugin: it starts the EnergyTrace stream
// of an LP-XDS110ET. The device must have been opened with allow_target_power
// (the stream start may energize the target connector; whether it does was
// not established), which the core guards with a confirmation.
func (p *Plugin) StartCapture(ctx context.Context, req pluginsdk.CaptureRequest, sink *pluginsdk.Sink) (pluginsdk.Capture, *pluginsdk.CaptureStart, error) {
	p.mu.Lock()
	d := p.devs[req.DeviceID]
	p.mu.Unlock()
	if d == nil {
		return nil, nil, status.Errorf(codes.FailedPrecondition, "open %s first", req.DeviceID)
	}
	if !supportsEnergyTrace(d.entry) {
		return nil, nil, status.Error(codes.Unimplemented, "EnergyTrace is implemented for the LP-XDS110ET (0451:bef3) only")
	}
	if _, err := parseOptions(req.Config); err != nil {
		return nil, nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if !d.o.allowPower {
		return nil, nil, status.Error(codes.PermissionDenied, "EnergyTrace measures the target rail and its start commands are only partly understood: open the device with allow_target_power=true (and confirm) to permit it")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cap != nil {
		return nil, nil, status.Error(codes.FailedPrecondition, "an EnergyTrace capture is already running on this probe")
	}
	if err := d.detach(); err != nil {
		return nil, nil, err
	}

	link, err := p.openData(d.entry)
	if err != nil {
		return nil, nil, classify(err)
	}
	restore := func() {
		_ = energytrace.Disconnect(d.probe)
		_, _ = d.probe.Connect()
	}
	cal, err := energytrace.Prepare(d.probe)
	if err != nil {
		_ = link.Close()
		restore()
		return nil, nil, classify(err)
	}
	rd, err := energytrace.NewReader(link, cal.Range0)
	if err != nil {
		_ = link.Close()
		restore()
		return nil, nil, classify(err)
	}
	writeNs := req.Clock.Now()
	if err := energytrace.Begin(d.probe); err != nil {
		// Unknown whether the probe started: stop quietly, then restore.
		_ = energytrace.StopStream(d.probe)
		_ = energytrace.ReleaseStream(d.probe)
		_ = link.Close()
		restore()
		return nil, nil, classify(err)
	}
	t0 := req.Clock.Now()

	cctx, cancel := context.WithCancel(ctx)
	c := &etCapture{
		p: p, d: d, link: link, rd: rd, sink: sink, clock: req.Clock,
		t0: t0, writeNs: writeNs, cancel: cancel, done: make(chan struct{}),
	}
	if req.Duration > 0 {
		c.limit = int64(req.Duration) * energytrace.SampleRateHz / int64(time.Second)
		if c.limit == 0 {
			c.limit = 1
		}
	}
	d.cap = c
	go c.run(cctx)
	return c, &pluginsdk.CaptureStart{Streams: []*pluginv1.StreamInfo{{StreamId: "current", Kind: "power", Attributes: etAttributes()}}}, nil
}

type etCapture struct {
	p      *Plugin
	d      *device
	link   DataLink
	rd     *energytrace.Reader
	sink   *pluginsdk.Sink
	clock  *pluginsdk.Clock
	t0     int64
	limit  int64 // samples; 0 = until stopped
	cancel context.CancelFunc
	done   chan struct{}

	writeNs int64
	upperNs int64
	seen    bool

	err     error
	pending []int32
	pendAt  uint64 // StartSample of pending[0]
	emitted int64  // samples handed to the sink, including dropped batches
	events  uint64
	lost    uint64
	reached bool
}

func (c *etCapture) Wait() error { <-c.done; return c.err }

func (c *etCapture) Result() *pluginv1.StopCaptureResponse {
	return &pluginv1.StopCaptureResponse{
		Stats: &pluginv1.CaptureStats{Events: c.events, Samples: uint64(c.emitted), DroppedBatches: c.sink.Dropped() + c.lost},
		Clock: c.clockInfo(),
	}
}

// stop cancels the capture and waits for the probe to be released.
func (c *etCapture) stop() {
	c.cancel()
	<-c.done
}

func (c *etCapture) run(ctx context.Context) {
	defer close(c.done)
	rctx, stopRead := context.WithCancel(ctx)
	defer stopRead()
	err := c.rd.Run(rctx, 0, func(b energytrace.Batch) {
		c.handle(b)
		if c.reached {
			stopRead()
		}
	})
	gone := errors.Is(err, usb.ErrDisconnected)
	if err != nil {
		c.err = fmt.Errorf("EnergyTrace stream: %w", classify(err))
	}

	if !gone {
		// Stop, collect what is in flight, then release: the vendor tool also
		// stops, drains for about a second, then releases.
		c.d.mu.Lock()
		if e := energytrace.StopStream(c.d.probe); e != nil && c.err == nil {
			c.err = classify(e)
		}
		dctx, cancel := context.WithTimeout(context.Background(), stopDrain)
		_ = c.rd.Run(dctx, stopDrain, func(b energytrace.Batch) { c.handle(b) })
		cancel()
		if e := energytrace.ReleaseStream(c.d.probe); e != nil && c.err == nil {
			c.err = classify(e)
		}
		_ = energytrace.Disconnect(c.d.probe)
		_, _ = c.d.probe.Connect()
		c.d.mu.Unlock()
	}
	_ = c.link.Close()

	fctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	c.flush(fctx, true)
	cancel()
	if st, _ := c.rd.Stats(); st.LostPackets > c.lost {
		c.lost = st.LostPackets
	}
	c.reportOutcome()

	c.d.mu.Lock()
	c.d.cap = nil
	c.d.mu.Unlock()
	if gone {
		c.p.drop(c.d)
	}
}

// reportOutcome puts a failed or lossy capture on the timeline. The SDK only
// returns the plugin error from StopCapture when there is no result, so
// without this a probe that vanished mid-capture would look like a clean run.
func (c *etCapture) reportOutcome() {
	var level, msg string
	switch {
	case c.err != nil:
		level, msg = "error", "EnergyTrace capture ended early: "+c.err.Error()
	case c.lost > 0:
		level, msg = "warning", fmt.Sprintf("EnergyTrace lost %d data packets (%d samples)", c.lost, c.lost*energytrace.SamplesPerPacket)
	default:
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = c.sink.Emit(ctx, &pluginv1.Event{
		TimestampNs: c.clock.Now(), StreamId: "current",
		Payload: &pluginv1.Event_Log{Log: &pluginv1.Log{Level: level, Source: "energytrace", Message: msg}},
	})
}

// handle accumulates one packet. Lost packets break contiguity, so the
// pending samples are flushed before a gap.
func (c *etCapture) handle(b energytrace.Batch) {
	if c.reached {
		return
	}
	c.observe(b)
	if b.Lost > 0 {
		c.lost += uint64(b.Lost)
		c.flush(context.Background(), false)
	}
	cur := b.CurrentNA
	if c.limit > 0 {
		have := int64(b.StartSample) + int64(len(cur))
		if have >= c.limit {
			cur = cur[:max(0, int(c.limit-int64(b.StartSample)))]
			c.reached = true
		}
	}
	if len(cur) == 0 {
		return
	}
	if len(c.pending) == 0 {
		c.pendAt = b.StartSample
	}
	c.pending = append(c.pending, cur...)
	if c.reached || len(c.pending) >= batchPackets*energytrace.SamplesPerPacket {
		c.flush(context.Background(), false)
	}
}

// flush sends the pending samples. While streaming it never blocks (the probe
// cannot be paused); the final flush does.
func (c *etCapture) flush(ctx context.Context, block bool) {
	if len(c.pending) == 0 {
		return
	}
	// sample index s is taken s+256 periods after the start (the first packet
	// is dropped); the exact period is 15625/4 ns.
	ts := c.t0 + int64(c.pendAt+firstSampleOffset)*15625/4
	ev := &pluginv1.Event{
		TimestampNs: ts, StreamId: "current",
		Payload: &pluginv1.Event_Power{Power: &pluginv1.PowerBatch{
			SamplePeriodNs: energytrace.SamplePeriodNs,
			CurrentNa:      append([]int32(nil), c.pending...),
		}},
	}
	c.emitted += int64(len(c.pending))
	c.pending = c.pending[:0]
	if block {
		if err := c.sink.Emit(ctx, ev); err != nil {
			return
		}
	} else if !c.sink.TryEmit(ev) {
		return
	}
	c.events++
}

// observe tightens the bound on when sampling really started: packet k (0
// based, counting the dropped one) cannot arrive before its last sample was
// taken.
func (c *etCapture) observe(b energytrace.Batch) {
	end := int64(b.StartSample) + int64(len(b.CurrentNA)) + firstSampleOffset
	if end*15625/4 > startWindowNs {
		return
	}
	arrive := c.clock.At(b.Arrival)
	up := arrive - end*15625/4
	if !c.seen || up < c.upperNs {
		c.upperNs, c.seen = up, true
	}
}

func (c *etCapture) clockInfo() *pluginv1.ClockInfo {
	ci := &pluginv1.ClockInfo{
		Source:       "sample count x 3.90625 us from the host time of the stream-start command",
		ResolutionNs: energytrace.SamplePeriodNs,
		Note: "the probe sends no timestamps; sample 0 is stamped when the stream-start reply arrived and " +
			"bounded by the host time before the command (late side) and by data arrival (early side); " +
			"device clock drift is not measured",
		ErrorMaxNs: c.t0 - c.writeNs, ErrorMaxBounded: true,
	}
	if c.seen {
		ci.ErrorMinNs, ci.ErrorMinBounded = c.t0-c.upperNs, true
		if ci.ErrorMinNs > 0 {
			ci.ErrorMinNs, ci.ErrorMinBounded = 0, false
			ci.Note += "; the arrival bound was inconsistent and is not claimed"
		}
	}
	return ci
}
